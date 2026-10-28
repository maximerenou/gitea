// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package depscan

import (
	"bufio"
	"bytes"
	"cmp"
	"path"
	"regexp"
	"slices"
	"strings"

	"gitea.dev/modules/json"

	"go.yaml.in/yaml/v4"
)

// OSV ecosystem names, see https://ossf.github.io/osv-schema/#affectedpackage-field
const (
	EcosystemGo        = "Go"
	EcosystemNpm       = "npm"
	EcosystemPyPI      = "PyPI"
	EcosystemPackagist = "Packagist"
	EcosystemCrates    = "crates.io"
	EcosystemRubyGems  = "RubyGems"
)

// Package is a pinned package version
type Package struct {
	Ecosystem string
	Name      string
	Version   string
}

type parser func(content []byte) ([]Package, error)

// parsers handle lockfiles, which pin the versions a build really uses, and the manifests pinning exact versions
var parsers = map[string]parser{
	"go.mod":            parseGoMod,
	"package-lock.json": parsePackageLock,
	"yarn.lock":         parseYarnLock,
	"pnpm-lock.yaml":    parsePnpmLock,
	"composer.lock":     parseComposerLock,
	"requirements.txt":  parseRequirements,
	"Pipfile.lock":      parsePipfileLock,
	"poetry.lock":       parseTOMLPackages(EcosystemPyPI, false),
	"Cargo.lock":        parseTOMLPackages(EcosystemCrates, true),
	"Gemfile.lock":      parseGemfileLock,
}

// IsManifest reports whether a repository file is a manifest the scanner reads, vendored and installed dependencies are skipped
func IsManifest(filePath string) bool {
	if _, ok := parsers[path.Base(filePath)]; !ok {
		return false
	}
	for dir := range strings.SplitSeq(path.Dir(filePath), "/") {
		if dir == "node_modules" || dir == "vendor" {
			return false
		}
	}
	return true
}

// Parse returns the packages a manifest pins, deduplicated and sorted, as several parsers read maps
func Parse(filePath string, content []byte) ([]Package, error) {
	parse, ok := parsers[path.Base(filePath)]
	if !ok {
		return nil, nil
	}
	packages, err := parse(content)
	if err != nil {
		return nil, err
	}
	packages = slices.DeleteFunc(packages, func(p Package) bool { return p.Name == "" || p.Version == "" })
	slices.SortFunc(packages, func(a, b Package) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), compareVersions(a.Version, b.Version), strings.Compare(a.Version, b.Version))
	})
	return slices.Compact(packages), nil
}

func lines(content []byte) []string {
	var result []string
	scanner := bufio.NewScanner(bytes.NewReader(content))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		result = append(result, scanner.Text())
	}
	return result
}

func parseGoMod(content []byte) ([]Package, error) {
	var packages []Package
	inRequire := false
	for _, line := range lines(content) {
		line, _, _ = strings.Cut(line, "//")
		fields := strings.Fields(line)
		switch {
		case len(fields) == 0:
		case fields[0] == "require" && len(fields) == 2 && fields[1] == "(":
			inRequire = true
		case inRequire && fields[0] == ")":
			inRequire = false
		case inRequire && len(fields) >= 2:
			packages = append(packages, Package{EcosystemGo, fields[0], strings.TrimPrefix(fields[1], "v")})
		case fields[0] == "require" && len(fields) >= 3:
			packages = append(packages, Package{EcosystemGo, fields[1], strings.TrimPrefix(fields[2], "v")})
		}
	}
	return packages, nil
}

func parsePackageLock(content []byte) ([]Package, error) {
	var lock struct {
		Packages map[string]struct {
			Version string `json:"version"`
			Link    bool   `json:"link"`
		} `json:"packages"`
		Dependencies map[string]npmLockV1Dependency `json:"dependencies"`
	}
	if err := json.Unmarshal(content, &lock); err != nil {
		return nil, err
	}
	var packages []Package
	if len(lock.Packages) > 0 {
		for key, p := range lock.Packages {
			_, name, ok := strings.Cut(key, "node_modules/")
			if !ok || p.Link {
				continue
			}
			if i := strings.LastIndex(name, "node_modules/"); i >= 0 {
				name = name[i+len("node_modules/"):]
			}
			packages = append(packages, Package{EcosystemNpm, name, p.Version})
		}
		return packages, nil
	}
	var walk func(deps map[string]npmLockV1Dependency)
	walk = func(deps map[string]npmLockV1Dependency) {
		for name, d := range deps {
			packages = append(packages, Package{EcosystemNpm, name, d.Version})
			walk(d.Dependencies)
		}
	}
	walk(lock.Dependencies)
	return packages, nil
}

type npmLockV1Dependency struct {
	Version      string                         `json:"version"`
	Dependencies map[string]npmLockV1Dependency `json:"dependencies"`
}

// npmName extracts the package name of a "name@range" or "@scope/name@range" specification
func npmName(spec string) string {
	spec = strings.Trim(strings.TrimSpace(spec), `"`)
	if i := strings.LastIndex(spec, "@"); i > 0 {
		return spec[:i]
	}
	return spec
}

func parseYarnLock(content []byte) ([]Package, error) {
	var packages []Package
	name := ""
	for _, line := range lines(content) {
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
		case !strings.HasPrefix(line, " ") && strings.HasSuffix(line, ":"):
			first, _, _ := strings.Cut(strings.TrimSuffix(line, ":"), ",")
			name = npmName(first)
			if name == "__metadata" {
				name = ""
			}
		case name != "" && strings.HasPrefix(strings.TrimSpace(line), "version"):
			version := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "version"))
			version = strings.Trim(strings.TrimPrefix(version, ":"), ` "`)
			if !strings.Contains(version, "use.local") {
				packages = append(packages, Package{EcosystemNpm, name, version})
			}
			name = ""
		}
	}
	return packages, nil
}

// pnpm 6+ append the peers in parentheses, pnpm 5 after an underscore following the version: "/name/1.0.0_peer@2.0.0"
var pnpmPeerSuffix = regexp.MustCompile(`\(.*$|(/\d[^/_]*)_[^/]*$`)

func parsePnpmLock(content []byte) ([]Package, error) {
	var lock struct {
		Packages map[string]any `yaml:"packages"`
	}
	if err := yaml.Unmarshal(content, &lock); err != nil {
		return nil, err
	}
	packages := make([]Package, 0, len(lock.Packages))
	for key := range lock.Packages {
		key = strings.TrimPrefix(pnpmPeerSuffix.ReplaceAllString(key, "$1"), "/")
		i := strings.LastIndex(key, "@")
		if i <= 0 { // pnpm 5 keys are "/name/version"
			i = strings.LastIndex(key, "/")
		}
		if i <= 0 {
			continue
		}
		packages = append(packages, Package{EcosystemNpm, key[:i], key[i+1:]})
	}
	return packages, nil
}

func parseComposerLock(content []byte) ([]Package, error) {
	var lock struct {
		Packages    []composerPackage `json:"packages"`
		PackagesDev []composerPackage `json:"packages-dev"`
	}
	if err := json.Unmarshal(content, &lock); err != nil {
		return nil, err
	}
	var packages []Package
	for _, p := range append(lock.Packages, lock.PackagesDev...) {
		packages = append(packages, Package{EcosystemPackagist, p.Name, strings.TrimPrefix(p.Version, "v")})
	}
	return packages, nil
}

type composerPackage struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

var requirementLine = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._-]*)(\[[^\]]*\])?\s*===?\s*([^\s;#,]+)`)

func parseRequirements(content []byte) ([]Package, error) {
	var packages []Package
	for _, line := range lines(content) {
		if m := requirementLine.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			packages = append(packages, Package{EcosystemPyPI, strings.ToLower(m[1]), m[3]})
		}
	}
	return packages, nil
}

func parsePipfileLock(content []byte) ([]Package, error) {
	var lock map[string]map[string]struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(content, &lock); err != nil {
		return nil, err
	}
	var packages []Package
	for _, section := range []string{"default", "develop"} {
		for name, p := range lock[section] {
			packages = append(packages, Package{EcosystemPyPI, strings.ToLower(name), strings.TrimPrefix(p.Version, "==")})
		}
	}
	return packages, nil
}

// parseTOMLPackages reads the [[package]] tables of poetry.lock and Cargo.lock, needing a registry source when registryOnly
func parseTOMLPackages(ecosystem string, registryOnly bool) parser {
	return func(content []byte) ([]Package, error) {
		var packages []Package
		var current *Package
		source := ""
		flush := func() {
			// Cargo.lock lists the workspace's own crates without a source, and git dependencies are not released versions
			if current != nil && (!registryOnly || (source != "" && !strings.HasPrefix(source, "git+"))) {
				packages = append(packages, *current)
			}
			current, source = nil, ""
		}
		for _, line := range lines(content) {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "[") {
				flush()
				if line == "[[package]]" {
					current = &Package{Ecosystem: ecosystem}
				}
				continue
			}
			key, value, ok := strings.Cut(line, "=")
			if current == nil || !ok {
				continue
			}
			value = strings.Trim(strings.TrimSpace(value), `"`)
			switch strings.TrimSpace(key) {
			case "name":
				current.Name = value
			case "version":
				current.Version = value
			case "source":
				source = value
			}
		}
		flush()
		return packages, nil
	}
}

var gemSpec = regexp.MustCompile(`^ {4}([^ ]+) \(([^)]+)\)$`)

func parseGemfileLock(content []byte) ([]Package, error) {
	var packages []Package
	section, inSpecs := "", false
	for _, line := range lines(content) {
		switch {
		case line == "  specs:":
			inSpecs = section == "GEM" // PATH and GIT specs are local or unreleased gems
		case !strings.HasPrefix(line, "  "):
			section, inSpecs = strings.TrimSpace(line), false
		case inSpecs:
			if m := gemSpec.FindStringSubmatch(line); m != nil {
				version, _, _ := strings.Cut(m[2], "-") // platform gems are "1.15.4-x86_64-linux"
				packages = append(packages, Package{EcosystemRubyGems, m[1], version})
			}
		}
	}
	return packages, nil
}
