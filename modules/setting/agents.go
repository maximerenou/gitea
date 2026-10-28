// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package setting

import (
	"errors"
	"fmt"
	"time"
)

// Agents settings for the Codex CLI agent sessions, which run on Gitea Actions runners
var Agents = struct {
	Enabled                bool          `ini:"ENABLED"`
	RunsOn                 string        `ini:"RUNS_ON"`
	ContainerImage         string        `ini:"CONTAINER_IMAGE"`
	ContainerOptions       string        `ini:"CONTAINER_OPTIONS"`
	CodexVersion           string        `ini:"CODEX_VERSION"`
	JobTimeout             time.Duration `ini:"JOB_TIMEOUT"`
	CheckpointInterval     time.Duration `ini:"CHECKPOINT_INTERVAL"`
	MaxModelRequestsPerRun int           `ini:"MAX_MODEL_REQUESTS_PER_RUN"`
	MaxModelOutputTokens   int           `ini:"MAX_MODEL_OUTPUT_TOKENS"`
	MaxActiveRunsPerUser   int           `ini:"MAX_ACTIVE_RUNS_PER_USER"`
	AllowedHostList        string        `ini:"ALLOWED_HOST_LIST"`
	Firewall               bool          `ini:"FIREWALL"`
	SetupAsRoot            bool          `ini:"SETUP_AS_ROOT"`
	FirewallAllowedHosts   []string      `ini:"FIREWALL_ALLOWED_HOSTS"`
	MaxCIFixAttempts       int           `ini:"MAX_CI_FIX_ATTEMPTS"`
	EnableByDefault        bool          `ini:"ENABLE_BY_DEFAULT"`
}{
	Enabled:                false,
	RunsOn:                 "ubuntu-latest",
	CodexVersion:           "latest",
	JobTimeout:             60 * time.Minute,
	CheckpointInterval:     45 * time.Second,
	MaxActiveRunsPerUser:   10,
	MaxModelRequestsPerRun: 2048,
	MaxModelOutputTokens:   32768,
	MaxCIFixAttempts:       2,
	EnableByDefault:        true,
}

func loadAgentsFrom(rootCfg ConfigProvider) error {
	if err := rootCfg.Section("agents").MapTo(&Agents); err != nil {
		return fmt.Errorf("failed to map Agents settings: %w", err)
	}
	if Agents.Enabled && !Actions.Enabled {
		return errors.New("[agents] ENABLED requires [actions] ENABLED")
	}
	if Agents.Firewall && Agents.ContainerImage == "" {
		return errors.New("[agents] FIREWALL requires CONTAINER_IMAGE, the job container needs the NET_ADMIN capability")
	}
	Agents.JobTimeout = min(max(Agents.JobTimeout, time.Minute), 6*time.Hour)
	Agents.CheckpointInterval = max(Agents.CheckpointInterval, 10*time.Second)
	Agents.MaxActiveRunsPerUser = max(Agents.MaxActiveRunsPerUser, 1)
	Agents.MaxModelRequestsPerRun = max(Agents.MaxModelRequestsPerRun, 1)
	Agents.MaxModelOutputTokens = max(Agents.MaxModelOutputTokens, 1)
	return nil
}
