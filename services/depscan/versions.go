// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package depscan

import (
	"regexp"
	"strings"
)

var versionToken = regexp.MustCompile(`[0-9]+|[a-z]+`)

// compareVersions orders the versions of every ecosystem closely enough to pick ranges and lowest versions:
// numbers compare numerically, and a suffix such as "-rc.1" or "b2" sorts before the release, except a ".post" one
func compareVersions(a, b string) int {
	ta, tb := versionTokens(a), versionTokens(b)
	for i := range min(len(ta), len(tb)) {
		if c := compareVersionTokens(ta[i], tb[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(ta) > len(tb):
		return extraTokenSign(ta[len(tb)])
	case len(ta) < len(tb):
		return -extraTokenSign(tb[len(ta)])
	}
	return 0
}

func versionTokens(v string) []string {
	v = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(v)), "v")
	v, _, _ = strings.Cut(v, "+") // build metadata does not order versions
	return versionToken.FindAllString(v, -1)
}

func isNumber(token string) bool {
	return token != "" && token[0] >= '0' && token[0] <= '9'
}

func compareVersionTokens(a, b string) int {
	switch an, bn := isNumber(a), isNumber(b); {
	case an && bn:
		a, b = strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
		if len(a) != len(b) {
			return len(a) - len(b)
		}
		return strings.Compare(a, b)
	case an:
		return 1
	case bn:
		return -1
	}
	return strings.Compare(a, b)
}

// extraTokenSign tells whether a version extended by token sorts after the version without it
func extraTokenSign(token string) int {
	if isNumber(token) || token == "post" {
		return 1
	}
	return -1
}

var pypiSeparators = regexp.MustCompile(`[-_.]+`)

// normalizeName returns the form under which a registry considers two package names the same
func normalizeName(ecosystem, name string) string {
	switch ecosystem {
	case EcosystemPyPI: // PEP 503
		return pypiSeparators.ReplaceAllString(strings.ToLower(name), "-")
	case EcosystemPackagist:
		return strings.ToLower(name)
	case EcosystemCrates:
		return strings.ReplaceAll(strings.ToLower(name), "_", "-")
	}
	return name
}
