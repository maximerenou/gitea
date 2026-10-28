// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package depscan

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	depscan_model "gitea.dev/models/depscan"
	"gitea.dev/modules/egress/policy"
	"gitea.dev/modules/json"
	"gitea.dev/modules/proxy"
	"gitea.dev/modules/setting"

	"github.com/hashicorp/golang-lru/v2/expirable"
)

const osvBatchSize = 1000

// vulnCache keeps the vulnerability records fetched lately, scans and reviews of many repositories share most of them
var vulnCache = expirable.NewLRU[string, *OSVVuln](2000, nil, time.Hour)

type osvClient struct {
	base string
	http *http.Client
	// maxFetches bounds the records a scan or a review fetches, so a pull request adding thousands of vulnerable packages
	// does not hold the queue
	maxFetches, fetches int
}

func newOSVClient(maxFetches int) *osvClient {
	mode := policy.Lax
	if setting.Security.EgressMode == "strict" {
		mode = policy.Strict
	}
	p := policy.NewPolicy("dependency scanning", mode,
		policy.WithAllow(setting.DependencyScanning.AllowedHostList, "dependency_scanning.ALLOWED_HOST_LIST"),
		policy.WithLocalNeedsIPAllow(),
		policy.WithProxy(proxy.Proxy()))
	return &osvClient{base: setting.DependencyScanning.OSVURL, http: &http.Client{Transport: p.NewHTTPTransport(), Timeout: time.Minute}, maxFetches: maxFetches}
}

func (c *osvClient) do(ctx context.Context, method, route string, body, result any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+route, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("OSV %s %s: HTTP %d %s", method, route, resp.StatusCode, strings.TrimSpace(string(excerpt)))
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 64*1024*1024)).Decode(result)
}

type osvQuery struct {
	Package struct {
		Name      string `json:"name"`
		Ecosystem string `json:"ecosystem"`
	} `json:"package"`
	Version   string `json:"version"`
	PageToken string `json:"page_token,omitempty"`
}

// QueryBatch returns the IDs of the vulnerabilities affecting each package
func (c *osvClient) QueryBatch(ctx context.Context, packages []Package) ([][]string, error) {
	result := make([][]string, len(packages))
	for start := 0; start < len(packages); start += osvBatchSize {
		chunk := packages[start:min(start+osvBatchSize, len(packages))]
		queries := make([]osvQuery, len(chunk))
		for i, p := range chunk {
			queries[i].Package.Name, queries[i].Package.Ecosystem, queries[i].Version = p.Name, p.Ecosystem, p.Version
		}
		pending := make([]int, len(chunk))
		for i := range pending {
			pending[i] = i
		}
		for page := 0; len(pending) > 0 && page < 10; page++ {
			batch := make([]osvQuery, len(pending))
			for i, index := range pending {
				batch[i] = queries[index]
			}
			var resp struct {
				Results []struct {
					Vulns []struct {
						ID string `json:"id"`
					} `json:"vulns"`
					NextPageToken string `json:"next_page_token"`
				} `json:"results"`
			}
			if err := c.do(ctx, http.MethodPost, "/v1/querybatch", map[string]any{"queries": batch}, &resp); err != nil {
				return nil, err
			}
			next := pending[:0:0]
			for i, r := range resp.Results {
				if i >= len(pending) {
					break
				}
				index := pending[i]
				for _, v := range r.Vulns {
					result[start+index] = append(result[start+index], v.ID)
				}
				if r.NextPageToken != "" {
					queries[index].PageToken = r.NextPageToken
					next = append(next, index)
				}
			}
			pending = next
		}
	}
	return result, nil
}

// OSVVuln is the part of an OSV record alerts use
type OSVVuln struct {
	ID       string   `json:"id"`
	Summary  string   `json:"summary"`
	Details  string   `json:"details"`
	Aliases  []string `json:"aliases"`
	Severity []struct {
		Type  string `json:"type"`
		Score string `json:"score"`
	} `json:"severity"`
	Affected []struct {
		Package struct {
			Ecosystem string `json:"ecosystem"`
			Name      string `json:"name"`
		} `json:"package"`
		Ranges []struct {
			Type   string              `json:"type"`
			Events []map[string]string `json:"events"`
		} `json:"ranges"`
		DatabaseSpecific  map[string]any `json:"database_specific"`
		EcosystemSpecific map[string]any `json:"ecosystem_specific"`
	} `json:"affected"`
	References []struct {
		URL string `json:"url"`
	} `json:"references"`
	DatabaseSpecific map[string]any `json:"database_specific"`
}

func (c *osvClient) GetVuln(ctx context.Context, id string) (*OSVVuln, error) {
	key := c.base + "\x00" + id
	if v, ok := vulnCache.Get(key); ok {
		return v, nil
	}
	if c.fetches++; c.fetches > c.maxFetches {
		return nil, fmt.Errorf("more than %d vulnerability records to fetch", c.maxFetches)
	}
	v := &OSVVuln{}
	if err := c.do(ctx, http.MethodGet, "/v1/vulns/"+url.PathEscape(id), nil, v); err != nil {
		return nil, err
	}
	vulnCache.Add(key, v)
	return v, nil
}

// FixedVersions lists the versions fixing the vulnerability for a package version: those ending the ranges it is in,
// or else every fixed version above it, when the ranges use an order the comparison misses
func (v *OSVVuln) FixedVersions(p Package) []string {
	var fixed, above []string
	for _, a := range v.Affected {
		if !strings.EqualFold(a.Package.Ecosystem, p.Ecosystem) || normalizeName(p.Ecosystem, a.Package.Name) != normalizeName(p.Ecosystem, p.Name) {
			continue
		}
		for _, r := range a.Ranges {
			if r.Type == "GIT" { // commit hashes, not versions
				continue
			}
			introduced, open := "", false
			for _, event := range r.Events {
				if i, ok := event["introduced"]; ok {
					introduced, open = i, true
				} else if f := event["fixed"]; f != "" {
					if open && (introduced == "0" || compareVersions(p.Version, introduced) >= 0) && compareVersions(p.Version, f) < 0 {
						fixed = append(fixed, f)
					}
					if compareVersions(f, p.Version) > 0 {
						above = append(above, f)
					}
					open = false
				} else {
					open = false // last_affected or limit
				}
			}
		}
	}
	if len(fixed) == 0 {
		fixed = above
	}
	slices.SortFunc(fixed, compareVersions)
	return slices.Compact(fixed)
}

// Rate returns the severity label and a 0-10 score used to sort alerts
func (v *OSVVuln) Rate() (depscan_model.Severity, float64) {
	score := 0.0
	for _, s := range v.Severity {
		if strings.HasPrefix(s.Type, "CVSS_V3") {
			score = max(score, cvss3BaseScore(s.Score))
		}
	}
	label := severityLabel(v.DatabaseSpecific["severity"])
	for _, a := range v.Affected {
		if label != depscan_model.SeverityUnknown {
			break
		}
		label = severityLabel(a.DatabaseSpecific["severity"])
		if label == depscan_model.SeverityUnknown {
			label = severityLabel(a.EcosystemSpecific["severity"])
		}
	}
	switch {
	case label == depscan_model.SeverityUnknown && score > 0:
		label = severityFromScore(score)
	case score == 0:
		score = map[depscan_model.Severity]float64{
			depscan_model.SeverityCritical: 9.5, depscan_model.SeverityHigh: 8,
			depscan_model.SeverityModerate: 5, depscan_model.SeverityLow: 2,
		}[label]
	}
	return label, score
}

func severityLabel(value any) depscan_model.Severity {
	s, _ := value.(string)
	switch strings.ToUpper(s) {
	case "CRITICAL":
		return depscan_model.SeverityCritical
	case "HIGH":
		return depscan_model.SeverityHigh
	case "MODERATE", "MEDIUM":
		return depscan_model.SeverityModerate
	case "LOW":
		return depscan_model.SeverityLow
	}
	return depscan_model.SeverityUnknown
}

func severityFromScore(score float64) depscan_model.Severity {
	switch {
	case score >= 9:
		return depscan_model.SeverityCritical
	case score >= 7:
		return depscan_model.SeverityHigh
	case score >= 4:
		return depscan_model.SeverityModerate
	case score > 0:
		return depscan_model.SeverityLow
	}
	return depscan_model.SeverityUnknown
}

// cvss3BaseScore computes the base score of a CVSS v3.x vector, 0 when it is not one
func cvss3BaseScore(vector string) float64 {
	metrics := map[string]string{}
	for part := range strings.SplitSeq(vector, "/") {
		if k, v, ok := strings.Cut(part, ":"); ok {
			metrics[k] = v
		}
	}
	weights := map[string]map[string]float64{
		"AV": {"N": 0.85, "A": 0.62, "L": 0.55, "P": 0.2},
		"AC": {"L": 0.77, "H": 0.44},
		"UI": {"N": 0.85, "R": 0.62},
		"C":  {"H": 0.56, "L": 0.22, "N": 0},
		"I":  {"H": 0.56, "L": 0.22, "N": 0},
		"A":  {"H": 0.56, "L": 0.22, "N": 0},
	}
	value := func(metric string) (float64, bool) {
		w, ok := weights[metric][metrics[metric]]
		return w, ok
	}
	changed := metrics["S"] == "C"
	pr := map[string]float64{"N": 0.85, "L": 0.62, "H": 0.27}
	if changed {
		pr = map[string]float64{"N": 0.85, "L": 0.68, "H": 0.5}
	}
	prValue, okPR := pr[metrics["PR"]]
	av, okAV := value("AV")
	ac, okAC := value("AC")
	ui, okUI := value("UI")
	c, okC := value("C")
	i, okI := value("I")
	a, okA := value("A")
	if !strings.HasPrefix(vector, "CVSS:3") || !okPR || !okAV || !okAC || !okUI || !okC || !okI || !okA || (metrics["S"] != "U" && !changed) {
		return 0
	}
	iss := 1 - (1-c)*(1-i)*(1-a)
	impact := 6.42 * iss
	if changed {
		impact = 7.52*(iss-0.029) - 3.25*math.Pow(iss-0.02, 15)
	}
	if impact <= 0 {
		return 0
	}
	exploitability := 8.22 * av * ac * prValue * ui
	if changed {
		return roundUp(min(1.08*(impact+exploitability), 10))
	}
	return roundUp(min(impact+exploitability, 10))
}

// roundUp is the CVSS v3.1 Roundup function
func roundUp(v float64) float64 {
	n := int64(math.Round(v * 100000))
	if n%10000 == 0 {
		return float64(n) / 100000
	}
	return (math.Floor(float64(n)/10000) + 1) / 10
}
