// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package depscan checks the dependencies pinned by repositories against an OSV vulnerability database, like GitHub's Dependabot alerts
package depscan

import (
	"context"
	"fmt"
	"slices"

	activities_model "gitea.dev/models/activities"
	"gitea.dev/models/db"
	depscan_model "gitea.dev/models/depscan"
	access_model "gitea.dev/models/perm/access"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/modules/git"
	"gitea.dev/modules/globallock"
	"gitea.dev/modules/log"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/timeutil"
	"gitea.dev/modules/util"
)

// IsEnabled reports whether dependency scanning is available on this instance
func IsEnabled() bool {
	return setting.DependencyScanning.Enabled
}

// IsRepoScanned reports whether a repository's administrators keep it scanned
func IsRepoScanned(ctx context.Context, repo *repo_model.Repository) bool {
	if !IsEnabled() || repo.IsEmpty {
		return false
	}
	rs, err := depscan_model.GetRepoSetting(ctx, repo.ID)
	if err != nil {
		log.Error("depscan: setting of repository %d: %v", repo.ID, err)
		return false
	}
	return rs.Enabled
}

type finding struct {
	vulnID   string
	pkg      Package
	manifest string
}

func (f finding) key() string {
	return fmt.Sprintf("%s\x00%s\x00%s\x00%s", f.vulnID, f.pkg.Ecosystem, f.pkg.Name, f.manifest)
}

func alertKey(a *depscan_model.Alert) string {
	return finding{a.VulnID, Package{a.Ecosystem, a.Package, ""}, a.Manifest}.key()
}

// ScanRepo reads the manifests of the default branch, stores its dependencies and updates its alerts
func ScanRepo(ctx context.Context, repo *repo_model.Repository) error {
	var opened []*depscan_model.Alert
	err := globallock.LockAndDo(ctx, fmt.Sprintf("depscan_repo_%d", repo.ID), func(ctx context.Context) error {
		rs, err := depscan_model.GetRepoSetting(ctx, repo.ID)
		if err != nil || !rs.Enabled || repo.IsEmpty {
			return err
		}
		if rs.ID == 0 {
			rs.FollowsDefault = true // enabled by default, which the instance can change later
		}
		var sha string
		var deps int
		var scanErr error
		sha, deps, opened, scanErr = scan(ctx, repo, newOSVClient(scanMaxVulnFetches))
		rs.LastScanUnix = timeutil.TimeStampNow()
		rs.LastScanError = ""
		if scanErr != nil {
			rs.LastScanError = util.EllipsisDisplayString(scanErr.Error(), 1024)
		} else {
			rs.LastScanSHA, rs.LastScanDependencies = sha, deps
		}
		if err := depscan_model.SaveRepoSetting(ctx, rs, "last_scan_unix", "last_scan_error", "last_scan_sha", "last_scan_dependencies"); err != nil {
			return err
		}
		return scanErr
	})
	startAlertUpdates(ctx, repo, opened)
	return err
}

func readManifests(ctx context.Context, repo *repo_model.Repository) (string, []*depscan_model.Dependency, error) {
	gitRepo, err := git.OpenRepository(ctx, repo)
	if err != nil {
		return "", nil, err
	}
	defer gitRepo.Close()
	commit, err := gitRepo.GetBranchCommit(ctx, repo.DefaultBranch)
	if err != nil {
		return "", nil, err
	}
	deps, err := readCommitManifests(ctx, repo, gitRepo, commit)
	return commit.ID.String(), deps, err
}

// maxDependencies bounds the packages read from a commit, and so the OSV queries of a scan or a review
const maxDependencies = 20000

// the vulnerability records a scan or a review may fetch besides those cached
const (
	scanMaxVulnFetches   = 5000
	reviewMaxVulnFetches = 500
)

func readCommitManifests(ctx context.Context, repo *repo_model.Repository, gitRepo *git.Repository, commit *git.Commit) ([]*depscan_model.Dependency, error) {
	entries, err := commit.Tree().ListEntriesRecursiveFast(ctx, gitRepo)
	if err != nil {
		return nil, err
	}
	var deps []*depscan_model.Dependency
	manifests := 0
	for _, entry := range entries {
		if !entry.IsRegular() || !IsManifest(entry.Name()) {
			continue
		}
		if manifests++; manifests > setting.DependencyScanning.MaxManifests {
			return nil, fmt.Errorf("the commit has more than %d manifests", setting.DependencyScanning.MaxManifests)
		}
		if size := entry.GetSize(ctx, gitRepo); size > setting.DependencyScanning.MaxManifestSize {
			return nil, fmt.Errorf("manifest %q exceeds MAX_MANIFEST_SIZE (%d bytes)", entry.Name(), size)
		}
		content, err := entry.Blob(gitRepo).GetBlobContent(ctx, setting.DependencyScanning.MaxManifestSize)
		if err != nil {
			return nil, err
		}
		packages, err := Parse(entry.Name(), []byte(content))
		if err != nil {
			return nil, fmt.Errorf("cannot parse manifest %q: %w", entry.Name(), err)
		}
		if len(deps)+len(packages) > maxDependencies {
			return nil, fmt.Errorf("the manifests pin more than %d dependencies", maxDependencies)
		}
		for _, p := range packages {
			deps = append(deps, &depscan_model.Dependency{RepoID: repo.ID, Manifest: entry.Name(), Ecosystem: p.Ecosystem, Name: p.Name, Version: p.Version})
		}
	}
	return deps, nil
}

// scan returns the scanned commit, its number of dependencies and the alerts it opened
func scan(ctx context.Context, repo *repo_model.Repository, client *osvClient) (string, int, []*depscan_model.Alert, error) {
	sha, deps, err := readManifests(ctx, repo)
	if err != nil {
		return "", 0, nil, err
	}
	if err := depscan_model.ReplaceDependencies(ctx, repo.ID, deps); err != nil {
		return "", 0, nil, err
	}

	unique := make([]Package, 0, len(deps))
	index := map[Package]int{}
	for _, d := range deps {
		p := Package{d.Ecosystem, d.Name, d.Version}
		if _, ok := index[p]; !ok {
			index[p] = len(unique)
			unique = append(unique, p)
		}
	}
	vulnIDs, err := client.QueryBatch(ctx, unique)
	if err != nil {
		return "", 0, nil, err
	}

	findings := map[string]finding{}
	vulns := map[string]*OSVVuln{}
	for _, d := range deps {
		p := Package{d.Ecosystem, d.Name, d.Version}
		for _, id := range vulnIDs[index[p]] {
			if _, ok := vulns[id]; !ok {
				if vulns[id], err = client.GetVuln(ctx, id); err != nil {
					return "", 0, nil, err
				}
			}
			f := finding{id, p, d.Manifest}
			// a lockfile can pin several affected versions, the lowest is kept so the alert does not flip between scans
			if prev, ok := findings[f.key()]; !ok || compareVersions(p.Version, prev.pkg.Version) < 0 {
				findings[f.key()] = f
			}
		}
	}
	opened, err := syncAlerts(ctx, repo.ID, findings, vulns)
	return sha, len(deps), opened, err
}

// syncAlerts opens alerts for new findings, reopens fixed ones that came back and closes those no longer found, dismissals are kept.
// It returns the alerts it opened or reopened.
func syncAlerts(ctx context.Context, repoID int64, findings map[string]finding, vulns map[string]*OSVVuln) (newlyOpen []*depscan_model.Alert, _ error) {
	existing, err := db.Find[depscan_model.Alert](ctx, depscan_model.FindAlertsOptions{ListOptions: db.ListOptionsAll, RepoID: repoID})
	if err != nil {
		return nil, err
	}
	defer func() { notifyAlerts(ctx, repoID, newlyOpen) }()
	byKey := make(map[string]*depscan_model.Alert, len(existing))
	for _, a := range existing {
		byKey[alertKey(a)] = a
	}
	for key, f := range findings {
		v := vulns[f.vulnID]
		severity, score := v.Rate()
		var refs []string
		for _, r := range v.References {
			refs = append(refs, r.URL)
		}
		a := byKey[key]
		if a == nil {
			a = &depscan_model.Alert{RepoID: repoID, VulnID: f.vulnID, Ecosystem: f.pkg.Ecosystem, Package: f.pkg.Name, Manifest: f.manifest, State: depscan_model.AlertOpen}
		}
		update := depscan_model.Alert{
			Version: f.pkg.Version, Severity: severity, Score: score,
			Aliases: v.Aliases, Summary: util.EllipsisDisplayString(v.Summary, 1024), Details: v.Details,
			FixedVersions: v.FixedVersions(f.pkg), References: refs,
		}
		changed := a.Version != update.Version || a.Severity != update.Severity || a.Score != update.Score ||
			!slices.Equal(a.Aliases, update.Aliases) || a.Summary != update.Summary || a.Details != update.Details ||
			!slices.Equal(a.FixedVersions, update.FixedVersions) || !slices.Equal(a.References, update.References)
		a.Version, a.Severity, a.Score = update.Version, update.Severity, update.Score
		a.Aliases, a.Summary, a.Details = update.Aliases, update.Summary, update.Details
		a.FixedVersions, a.References = update.FixedVersions, update.References
		if a.ID == 0 {
			if err := db.Insert(ctx, a); err != nil {
				return newlyOpen, err
			}
			newlyOpen = append(newlyOpen, a)
			continue
		}
		if changed {
			if err := depscan_model.UpdateAlert(ctx, a, "version", "severity", "score", "aliases", "summary", "details", "fixed_versions", "references"); err != nil {
				return newlyOpen, err
			}
		}
		if a.State == depscan_model.AlertFixed {
			a.State, a.FixedUnix = depscan_model.AlertOpen, 0
			if reopened, err := depscan_model.ChangeAlertState(ctx, a, depscan_model.AlertFixed, "state", "fixed_unix"); err != nil {
				return newlyOpen, err
			} else if reopened {
				newlyOpen = append(newlyOpen, a)
			}
		}
	}
	for key, a := range byKey {
		if _, found := findings[key]; found || a.State != depscan_model.AlertOpen {
			continue
		}
		a.State, a.FixedUnix = depscan_model.AlertFixed, timeutil.TimeStampNow()
		if _, err := depscan_model.ChangeAlertState(ctx, a, depscan_model.AlertOpen, "state", "fixed_unix"); err != nil {
			return newlyOpen, err
		}
	}
	return newlyOpen, nil
}

// notifyAlerts tells the administrators of the repository about the alerts a scan opened, from the severity it chose
func notifyAlerts(ctx context.Context, repoID int64, alerts []*depscan_model.Alert) {
	rs, err := depscan_model.GetRepoSetting(ctx, repoID)
	if err != nil {
		log.Error("depscan: notify alerts of repository %d: %v", repoID, err)
		return
	}
	alerts = slices.DeleteFunc(slices.Clone(alerts), func(a *depscan_model.Alert) bool { return !rs.NotifiesOf(a.Severity) })
	if len(alerts) == 0 {
		return
	}
	repo, err := repo_model.GetRepositoryByID(ctx, repoID)
	if err != nil {
		log.Error("depscan: notify alerts of repository %d: %v", repoID, err)
		return
	}
	candidates, err := repo_model.GetRepoAssignees(ctx, repo)
	if err != nil {
		log.Error("depscan: notify alerts of %s: %v", repo.FullName(), err)
		return
	}
	for _, u := range candidates {
		if !access_model.IsUserRepoAdmin(ctx, repo, u) {
			continue
		}
		for _, a := range alerts {
			subject := util.EllipsisDisplayString(fmt.Sprintf("%s %s: %s", a.Package, a.Version, a.Title()), 255)
			if err := activities_model.NotifySubject(ctx, u.ID, repo.ID, activities_model.NotificationSourceSecurityAlert, subject, fmt.Sprintf("security/alerts/%d", a.ID), 0); err != nil {
				log.Error("depscan: notify alert %d: %v", a.ID, err)
			}
		}
	}
}
