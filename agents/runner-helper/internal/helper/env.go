// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package helper

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Env is what the workflow hands to the helper; it is a value so tests can build one without touching the process.
type Env struct {
	RunID       int64
	APIURL      string
	Token       string
	LogEvents   string // summary (default), verbose or off
	Workspace   string // the checked out repository
	StateDir    string
	StatusEvery int // seconds between cancellation checks, 10 by default
	Extra       []string
}

func EnvFromOS() Env {
	id, _ := strconv.ParseInt(os.Getenv("AGENT_RUN_ID"), 10, 64)
	home, _ := os.UserHomeDir()
	e := Env{
		RunID:     id,
		APIURL:    strings.TrimRight(os.Getenv("AGENT_API_URL"), "/"),
		Token:     os.Getenv("AGENT_SESSION_TOKEN"),
		LogEvents: os.Getenv("AGENT_LOG_EVENTS"),
		Workspace: os.Getenv("GITHUB_WORKSPACE"),
	}
	if e.Workspace == "" {
		e.Workspace, _ = os.Getwd()
	}
	// outside the repository, so that nothing the agent writes here can be committed by accident
	e.StateDir = filepath.Join(home, ".agent-run", strconv.FormatInt(id, 10))
	return e
}

func (e Env) validate() error {
	switch {
	case e.RunID <= 0:
		return fmt.Errorf("AGENT_RUN_ID is not set")
	case e.APIURL == "":
		return fmt.Errorf("AGENT_API_URL is not set")
	case e.Token == "":
		return fmt.Errorf("AGENT_SESSION_TOKEN is not set: the run has no session token (disabled agents, or a run without a person)")
	}
	return nil
}

func (e Env) logLevel() string {
	switch e.LogEvents {
	case "verbose", "off":
		return e.LogEvents
	}
	return "summary"
}

func (e Env) statusEvery() int {
	if e.StatusEvery > 0 {
		return e.StatusEvery
	}
	return 10
}
