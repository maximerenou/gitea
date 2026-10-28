// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package setting

import (
	"fmt"
	"net/url"
	"strings"
)

// DependencyScanning settings, repositories' dependencies are checked against an OSV vulnerability database
var DependencyScanning = struct {
	Enabled         bool   `ini:"ENABLED"`
	OSVURL          string `ini:"OSV_URL"`
	EnableByDefault bool   `ini:"ENABLE_BY_DEFAULT"`
	AllowedHostList string `ini:"ALLOWED_HOST_LIST"`
	MaxManifests    int    `ini:"MAX_MANIFESTS"`
	MaxManifestSize int64  `ini:"MAX_MANIFEST_SIZE"`
}{
	Enabled:         false,
	OSVURL:          "https://api.osv.dev",
	EnableByDefault: false,
	MaxManifests:    200,
	MaxManifestSize: 5 * 1024 * 1024,
}

func loadDependencyScanningFrom(rootCfg ConfigProvider) error {
	if err := rootCfg.Section("dependency_scanning").MapTo(&DependencyScanning); err != nil {
		return fmt.Errorf("failed to map DependencyScanning settings: %w", err)
	}
	DependencyScanning.OSVURL = strings.TrimSuffix(DependencyScanning.OSVURL, "/")
	if u, err := url.Parse(DependencyScanning.OSVURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("invalid [dependency_scanning] OSV_URL %q", DependencyScanning.OSVURL)
	}
	return nil
}
