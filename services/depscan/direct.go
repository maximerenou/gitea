// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package depscan

import (
	"context"
	"path"
	"regexp"
	"strings"

	"gitea.dev/modules/container"
	"gitea.dev/modules/git"
	"gitea.dev/modules/json"
	"gitea.dev/modules/setting"
)

// declarations name the file declaring the direct dependencies of each lockfile, and how to read them
var declarations = map[string]struct {
	file  string
	parse func(content []byte) []string
}{
	"go.mod":            {"go.mod", directGoMod},
	"package-lock.json": {"package.json", directPackageJSON},
	"yarn.lock":         {"package.json", directPackageJSON},
	"pnpm-lock.yaml":    {"package.json", directPackageJSON},
	"composer.lock":     {"composer.json", directComposerJSON},
	"requirements.txt":  {"requirements.txt", directRequirements},
	"Pipfile.lock":      {"Pipfile", tomlDependencyNames(func(s string) bool { return s == "packages" || s == "dev-packages" })},
	"poetry.lock":       {"pyproject.toml", tomlDependencyNames(isPoetryDependencySection)},
	"Cargo.lock":        {"Cargo.toml", tomlDependencyNames(func(s string) bool { return strings.HasSuffix(s, "dependencies") })},
	"Gemfile.lock":      {"Gemfile.lock", directGemfileLock},
}

// directDependencies returns the normalized names of the packages the project of a lockfile declares itself,
// nil when the declaring file is missing: none is then known to be direct
func directDependencies(ctx context.Context, gitRepo *git.Repository, commit *git.Commit, lockfile string) container.Set[string] {
	decl, ok := declarations[path.Base(lockfile)]
	if !ok {
		return nil
	}
	blob, err := commit.GetBlobByPath(ctx, gitRepo, path.Join(path.Dir(lockfile), decl.file))
	if err != nil || blob.Size(ctx) > setting.DependencyScanning.MaxManifestSize {
		return nil
	}
	content, err := blob.GetBlobContent(ctx, setting.DependencyScanning.MaxManifestSize)
	if err != nil {
		return nil
	}
	ecosystem := lockfileEcosystem(lockfile)
	direct := container.Set[string]{}
	for _, name := range decl.parse([]byte(content)) {
		direct.Add(normalizeName(ecosystem, name))
	}
	return direct
}

func lockfileEcosystem(lockfile string) string {
	switch path.Base(lockfile) {
	case "go.mod":
		return EcosystemGo
	case "package-lock.json", "yarn.lock", "pnpm-lock.yaml":
		return EcosystemNpm
	case "composer.lock":
		return EcosystemPackagist
	case "requirements.txt", "Pipfile.lock", "poetry.lock":
		return EcosystemPyPI
	case "Cargo.lock":
		return EcosystemCrates
	case "Gemfile.lock":
		return EcosystemRubyGems
	}
	return ""
}

func directGoMod(content []byte) []string {
	var names []string
	inRequire := false
	for _, line := range lines(content) {
		code, comment, _ := strings.Cut(line, "//")
		fields := strings.Fields(code)
		indirect := strings.TrimSpace(comment) == "indirect" || strings.HasPrefix(strings.TrimSpace(comment), "indirect;")
		switch {
		case len(fields) == 0:
		case fields[0] == "require" && len(fields) == 2 && fields[1] == "(":
			inRequire = true
		case inRequire && fields[0] == ")":
			inRequire = false
		case inRequire && len(fields) >= 2 && !indirect:
			names = append(names, fields[0])
		case fields[0] == "require" && len(fields) >= 3 && !indirect:
			names = append(names, fields[1])
		}
	}
	return names
}

func jsonKeys(content []byte, sections ...string) []string {
	var manifest map[string]any
	if json.Unmarshal(content, &manifest) != nil {
		return nil
	}
	var names []string
	for _, section := range sections {
		deps, _ := manifest[section].(map[string]any)
		for name := range deps {
			names = append(names, name)
		}
	}
	return names
}

func directPackageJSON(content []byte) []string {
	return jsonKeys(content, "dependencies", "devDependencies", "optionalDependencies", "peerDependencies")
}

func directComposerJSON(content []byte) []string {
	return jsonKeys(content, "require", "require-dev")
}

func directRequirements(content []byte) []string {
	var names []string
	for _, line := range lines(content) {
		if m := pep508Name.FindString(strings.TrimSpace(line)); m != "" {
			names = append(names, m)
		}
	}
	return names
}

func directGemfileLock(content []byte) []string {
	var names []string
	inDependencies := false
	for _, line := range lines(content) {
		switch {
		case !strings.HasPrefix(line, " "):
			inDependencies = strings.TrimSpace(line) == "DEPENDENCIES"
		case inDependencies && !strings.HasPrefix(line, "    "):
			name, _, _ := strings.Cut(strings.TrimSpace(line), " ")
			names = append(names, strings.TrimSuffix(name, "!"))
		}
	}
	return names
}

func isPoetryDependencySection(s string) bool {
	return s == "tool.poetry.dependencies" || s == "tool.poetry.dev-dependencies" ||
		(strings.HasPrefix(s, "tool.poetry.group.") && strings.HasSuffix(s, ".dependencies"))
}

var (
	pep508Name   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*`)
	quotedString = regexp.MustCompile(`"([^"]*)"|'([^']*)'`)
)

// tomlDependencyNames reads the keys of the dependency tables of a TOML manifest, and the PEP 621 dependency arrays
// of pyproject.toml, without a full TOML parser
func tomlDependencyNames(isDependencySection func(section string) bool) func(content []byte) []string {
	return func(content []byte) []string {
		var names []string
		section, inArray := "", false
		addArray := func(value string) {
			for _, m := range quotedString.FindAllStringSubmatch(value, -1) {
				if name := pep508Name.FindString(m[1] + m[2]); name != "" {
					names = append(names, name)
				}
			}
			inArray = !strings.Contains(quotedString.ReplaceAllString(value, ""), "]")
		}
		for _, line := range lines(content) {
			line = strings.TrimSpace(line)
			switch {
			case line == "" || strings.HasPrefix(line, "#"):
			case inArray:
				addArray(line)
			case strings.HasPrefix(line, "["):
				section = strings.Trim(line, "[] ")
				// a dependency with its own table: [dependencies.serde]
				if i := strings.LastIndex(section, "."); i > 0 && isDependencySection(section[:i]) {
					names = append(names, strings.Trim(section[i+1:], `"'`))
				}
			default:
				key, value, ok := strings.Cut(line, "=")
				if !ok {
					continue
				}
				key, value = strings.Trim(strings.TrimSpace(key), `"'`), strings.TrimSpace(value)
				switch {
				case isDependencySection(section):
					names = append(names, key)
				case (section == "project" && key == "dependencies") || section == "project.optional-dependencies":
					addArray(value)
				}
			}
		}
		return names
	}
}
