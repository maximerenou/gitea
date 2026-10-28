// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package depscan

import (
	"regexp"
	"strings"

	"gitea.dev/modules/util"
)

const noMajorRule = "no-major"

// ignoreRule is an entry of the update settings' ignore list: a package name pattern with * wildcards, alone to leave
// the package as it is, followed by @no-major to keep it within its major version, or by @ and the versions to skip
type ignoreRule struct {
	pattern  *regexp.Regexp
	noMajor  bool
	versions [][]versionConstraint // any of these sets of constraints, nil for every version
}

type versionConstraint struct {
	op      string // <, <=, >, >=, = or "" for a version prefix such as 5 or 5.2.x
	version string
}

var (
	constraintOp = regexp.MustCompile(`^(<=|>=|==|<|>|=)?(.+)$`)
	spaceAfterOp = regexp.MustCompile(`([<>=])\s+`)
)

// parseIgnoreRule reads an ignore entry, such as "@types/*", "react@no-major" or "lodash@>=5" and "django@4.2.x || 5.x"
func parseIgnoreRule(entry string) (ignoreRule, error) {
	entry = strings.TrimSpace(entry)
	name, spec := entry, ""
	i := strings.Index(entry[min(1, len(entry)):], "@") // npm scopes start with @
	if i >= 0 {
		name, spec = strings.TrimSpace(entry[:i+1]), strings.TrimSpace(entry[i+2:])
	}
	if strings.Trim(name, "@") == "" || (i >= 0 && spec == "") {
		return ignoreRule{}, util.NewInvalidArgumentErrorf("%q is not a package name, optionally followed by @no-major or @ and versions", entry)
	}
	rule := ignoreRule{pattern: regexp.MustCompile("(?i)^" + strings.ReplaceAll(regexp.QuoteMeta(name), `\*`, ".*") + "$")}
	if spec == noMajorRule {
		rule.noMajor = true
		return rule, nil
	}
	if spec == "" {
		return rule, nil
	}
	for alternative := range strings.SplitSeq(spaceAfterOp.ReplaceAllString(spec, "$1"), "||") {
		var constraints []versionConstraint
		for part := range strings.FieldsFuncSeq(alternative, func(r rune) bool { return r == ',' || r == ' ' }) {
			m := constraintOp.FindStringSubmatch(part)
			version := strings.TrimSuffix(strings.TrimSuffix(m[2], ".x"), ".*")
			if version == "" || version == "*" || len(versionTokens(version)) == 0 {
				return ignoreRule{}, util.NewInvalidArgumentErrorf("%q has an invalid version %q", entry, part)
			}
			constraints = append(constraints, versionConstraint{op: strings.TrimPrefix(m[1], "="), version: version})
		}
		if len(constraints) == 0 {
			return ignoreRule{}, util.NewInvalidArgumentErrorf("%q has an empty version range", entry)
		}
		rule.versions = append(rule.versions, constraints)
	}
	return rule, nil
}

func (c versionConstraint) matches(version string) bool {
	cmp := compareVersions(version, c.version)
	switch c.op {
	case "<":
		return cmp < 0
	case "<=":
		return cmp <= 0
	case ">":
		return cmp > 0
	case ">=":
		return cmp >= 0
	}
	// "5" and "5.2.x" match the versions starting with them, "=5.2.1" too
	prefix, tokens := versionTokens(c.version), versionTokens(version)
	if len(tokens) < len(prefix) {
		return false
	}
	for i := range prefix {
		if compareVersionTokens(prefix[i], tokens[i]) != 0 {
			return false
		}
	}
	return true
}

func (r ignoreRule) matchesVersion(version string) bool {
	for _, constraints := range r.versions {
		matches := true
		for _, c := range constraints {
			matches = matches && c.matches(version)
		}
		if matches {
			return true
		}
	}
	return false
}

// ignoreRules are the parsed ignore entries of a repository, invalid ones were refused when saved
type ignoreRules []ignoreRule

func parseIgnoreRules(entries []string) ignoreRules {
	rules := make(ignoreRules, 0, len(entries))
	for _, entry := range entries {
		if rule, err := parseIgnoreRule(entry); err == nil {
			rules = append(rules, rule)
		}
	}
	return rules
}

// leaves reports whether the updates leave a package as it is
func (rules ignoreRules) leaves(name string) bool {
	for _, r := range rules {
		if !r.noMajor && r.versions == nil && r.pattern.MatchString(name) {
			return true
		}
	}
	return false
}

// keepsMajor reports whether a package stays within its major version even when the updates include major versions
func (rules ignoreRules) keepsMajor(name string) bool {
	for _, r := range rules {
		if r.noMajor && r.pattern.MatchString(name) {
			return true
		}
	}
	return false
}

// skips reports whether the updates skip a version of a package
func (rules ignoreRules) skips(name, version string) bool {
	for _, r := range rules {
		if r.versions != nil && r.pattern.MatchString(name) && r.matchesVersion(version) {
			return true
		}
	}
	return false
}

func (rules ignoreRules) hasNoMajor() bool {
	for _, r := range rules {
		if r.noMajor {
			return true
		}
	}
	return false
}

// isIgnored reports whether the updates leave a package as it is
func isIgnored(ignore []string, name string) bool {
	return parseIgnoreRules(ignore).leaves(name)
}
