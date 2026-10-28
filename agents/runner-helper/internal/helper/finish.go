// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package helper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// finish pushes the session branch if the agent committed, then sends result.json, also when the agent failed.
func finish(ctx context.Context, e Env, out io.Writer) error {
	rc, st, err := e.loadAll()
	if err != nil {
		return err
	}
	if st.Cancelled {
		fmt.Fprintln(out, "the run was cancelled, nothing to report")
		return nil
	}

	raw, resultErr := e.readResult(st)
	var pushErr error
	if rc.CheckoutRef == "" { // a pull request under review is detached: there is nothing to push
		pushErr = pushIfCommitted(ctx, e, rc, st, out)
	}
	if pushErr != nil {
		raw = failedResult("pushing the session branch failed: " + pushErr.Error())
	}

	res, err := newClient(e).result(ctx, raw)
	var apiErr *APIError
	switch {
	case errors.As(err, &apiErr) && apiErr.Status == 409:
		fmt.Fprintln(out, "the run already ended, the result was not needed")
		return nil
	case err != nil:
		return fmt.Errorf("sending the result: %w", err)
	}
	fmt.Fprintf(out, "result accepted, run %s\n", res.Status)
	for _, n := range res.Notes {
		fmt.Fprintln(out, "note:", n)
	}
	if pushErr != nil {
		return pushErr
	}
	return resultErr
}

// readResult returns the agent's result.json, or a "failed" result that says why there is none.
func (e Env) readResult(st *state) ([]byte, error) {
	raw, err := os.ReadFile(e.resultPath())
	switch {
	case err == nil && json.Valid(raw):
		return raw, nil
	case err == nil:
		return failedResult("the agent wrote a result that is not valid JSON"), errors.New("result.json is not valid JSON")
	}
	reason := "the agent did not write a result"
	if st.AgentError != "" {
		reason += " (" + shorten(st.AgentError, 300) + ")"
	}
	if !st.AgentRan {
		reason = "the agent could not start: " + shorten(st.AgentError, 300)
	}
	return failedResult(reason), errors.New(reason)
}

func failedResult(summary string) []byte {
	b, _ := json.Marshal(map[string]string{"version": "v1", "status": "failed", "summary": summary})
	return b
}

// pushIfCommitted pushes new commits to the session branch, never forcing.
func pushIfCommitted(ctx context.Context, e Env, rc *RunContext, st *state, out io.Writer) error {
	head, err := git(ctx, e, e.Workspace, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if head == st.StartSHA {
		return nil
	}
	if strings.TrimSpace(rc.Branch) == "" {
		return errors.New("the run has no session branch")
	}
	if _, err := git(ctx, e, e.Workspace, "push", "origin", "HEAD:refs/heads/"+rc.Branch); err != nil {
		return err
	}
	fmt.Fprintf(out, "pushed %.12s to %s\n", head, rc.Branch)
	return nil
}
