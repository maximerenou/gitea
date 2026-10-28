// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package helper implements the three steps of an agent run inside an Actions job.
package helper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Run executes one step: prepare, exec or finish.
func Run(ctx context.Context, step string, e Env, stdout, stderr io.Writer) error {
	if err := e.validate(); err != nil {
		return err
	}
	switch step {
	case "prepare":
		return prepare(ctx, e, stdout)
	case "exec":
		return execAgent(ctx, e, stdout, stderr)
	case "finish":
		return finish(ctx, e, stdout)
	}
	return fmt.Errorf("unknown step %q", step)
}

// state is what the steps hand to each other; it lives outside the repository.
type state struct {
	StartSHA       string `json:"start_sha"`
	ConversationID string `json:"conversation_id"`
	AgentExit      int    `json:"agent_exit"`
	AgentRan       bool   `json:"agent_ran"`
	Cancelled      bool   `json:"cancelled"`
	AgentError     string `json:"agent_error,omitempty"`
}

func (e Env) path(name string) string { return filepath.Join(e.StateDir, name) }

func (e Env) writeJSON(name string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(e.path(name), data, 0o600) // the run context holds the model token
}

func (e Env) readJSON(name string, v any) error {
	data, err := os.ReadFile(e.path(name))
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func (e Env) loadAll() (*RunContext, *state, error) {
	rc, st := &RunContext{}, &state{}
	if err := e.readJSON("context.json", rc); err != nil {
		return nil, nil, fmt.Errorf("reading the run context (did prepare run?): %w", err)
	}
	if err := e.readJSON("state.json", st); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}
	return rc, st, nil
}

func (e Env) resultPath() string { return e.path("result.json") }

func shorten(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
