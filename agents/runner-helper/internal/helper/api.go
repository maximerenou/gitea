// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package helper

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// RunContext mirrors the API response of GET /api/v1/agents/runs/{id}/context.
type RunContext struct {
	RunID     int64  `json:"run_id"`
	SessionID int64  `json:"session_id"`
	Action    string `json:"action"`
	Event     string `json:"event"`
	Agent     struct {
		Name     string `json:"name"`
		Model    string `json:"model"`
		Language string `json:"language"`
		Provider string `json:"provider"`
		BaseURL  string `json:"base_url"`
		Token    string `json:"token"`
		CLI      string `json:"cli"`
	} `json:"agent"`
	Repository struct {
		FullName      string `json:"full_name"`
		CloneURL      string `json:"clone_url"`
		DefaultBranch string `json:"default_branch"`
	} `json:"repository"`
	Branch          string `json:"branch"`
	CheckoutRef     string `json:"checkout_ref"`
	SystemPrompt    string `json:"system_prompt"`
	Prompt          string `json:"prompt"`
	ConversationID  string `json:"conversation_id"`
	ResultSchema    string `json:"result_schema"`
	ConfigCommitSHA string `json:"config_commit_sha"`
}

// APIError carries the HTTP status so callers can tell "the run is over" from a transient failure.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body) }

// RunEnded reports a response that means the helper must stop: the token died, the run was cancelled or finished.
func (e *APIError) RunEnded() bool {
	return e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden || e.Status == http.StatusNotFound || e.Status == http.StatusConflict
}

type client struct {
	base  string
	token string
	runID int64
	http  *http.Client
}

func newClient(e Env) *client {
	return &client{base: e.APIURL, token: e.Token, runID: e.RunID, http: &http.Client{Timeout: 60 * time.Second}}
}

func (c *client) do(ctx context.Context, method, path string, body []byte, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, fmt.Sprintf("%s/api/v1/agents/runs/%d%s", c.base, c.runID, path), rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 300 {
		return &APIError{Status: resp.StatusCode, Body: string(bytes.TrimSpace(data))}
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (c *client) context(ctx context.Context) (*RunContext, error) {
	rc := &RunContext{}
	return rc, c.do(ctx, http.MethodGet, "/context", nil, rc)
}

type progress struct {
	Text           string `json:"text,omitempty"`
	ConversationID string `json:"conversation_id,omitempty"`
}

func (c *client) progress(ctx context.Context, p progress) error {
	body, _ := json.Marshal(p)
	return c.do(ctx, http.MethodPost, "/progress", body, nil)
}

type runStatus struct {
	Status    string `json:"status"`
	Cancelled bool   `json:"cancelled"`
}

func (c *client) status(ctx context.Context) (*runStatus, error) {
	st := &runStatus{}
	return st, c.do(ctx, http.MethodGet, "/status", nil, st)
}

type resultResponse struct {
	Status string   `json:"status"`
	Notes  []string `json:"notes"`
}

func (c *client) result(ctx context.Context, raw []byte) (*resultResponse, error) {
	rr := &resultResponse{}
	return rr, c.do(ctx, http.MethodPost, "/result", raw, rr)
}
