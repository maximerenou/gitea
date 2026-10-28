// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package helper

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// prepare fetches the run context, puts the repository on the right ref and writes the prompt.
func prepare(ctx context.Context, e Env, out io.Writer) error {
	rc, err := newClient(e).context(ctx)
	if err != nil {
		return fmt.Errorf("fetching the run context: %w", err)
	}
	if err := os.MkdirAll(e.StateDir, 0o700); err != nil {
		return err
	}
	if err := e.writeJSON("context.json", rc); err != nil {
		return err
	}

	if _, err := os.Stat(filepath.Join(e.Workspace, ".git")); err != nil {
		// the checkout step normally did this; clone ourselves when it was skipped
		if _, err := git(ctx, e, filepath.Dir(e.Workspace), "clone", rc.Repository.CloneURL, e.Workspace); err != nil {
			return err
		}
	}
	for _, kv := range [][2]string{{"user.name", rc.Agent.Name}, {"user.email", rc.Agent.Name + "@users.noreply.invalid"}} {
		if _, err := git(ctx, e, e.Workspace, "config", kv[0], kv[1]); err != nil {
			return err
		}
	}

	if rc.CheckoutRef != "" {
		// a pull request is read at its head, detached: there is no branch to push
		if _, err := git(ctx, e, e.Workspace, "fetch", "origin", rc.CheckoutRef); err != nil {
			return err
		}
		if _, err := git(ctx, e, e.Workspace, "checkout", "--detach", "FETCH_HEAD"); err != nil {
			return err
		}
	} else {
		// continue the session's branch when it exists, otherwise start it from where the checkout is
		if _, err := git(ctx, e, e.Workspace, "fetch", "origin", "refs/heads/"+rc.Branch); err == nil {
			_, err = git(ctx, e, e.Workspace, "checkout", "-B", rc.Branch, "FETCH_HEAD")
			if err != nil {
				return err
			}
		} else if _, err := git(ctx, e, e.Workspace, "checkout", "-B", rc.Branch); err != nil {
			return err
		}
	}
	sha, err := git(ctx, e, e.Workspace, "rev-parse", "HEAD")
	if err != nil {
		return err
	}

	prompt := rc.SystemPrompt + "\n\n---\n\n" + rc.Prompt +
		"\n\nWrite your result as JSON to this exact path, not inside the repository: " + e.resultPath() +
		"\n\nThe JSON schema of the result:\n" + rc.ResultSchema + "\n"
	if err := os.WriteFile(e.path("prompt.txt"), []byte(prompt), 0o600); err != nil {
		return err
	}
	if err := e.writeJSON("state.json", &state{StartSHA: sha, ConversationID: rc.ConversationID}); err != nil {
		return err
	}
	fmt.Fprintf(out, "prepared run %d for %s at %.12s\n", e.RunID, rc.Repository.FullName, sha)
	return nil
}
