// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package private

import (
	"net/http"
	"strconv"
	"strings"

	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"
	"gitea.dev/modules/git/gitcmd"
)

// assertAgentSessionRef restricts a push made with an agent session token: only branches named agent/<session>/...,
// never rewritten, and never the bot's own ".agent" configuration repository. It responds 403 and returns false otherwise.
func (ctx *preReceiveContext) assertAgentSessionRef(oldCommitID, newCommitID string, refFullName git.RefName) bool {
	sessionID, _, ok := user_model.GetDoerSessionToken(ctx.Doer)
	if !ok {
		return true
	}
	repo := ctx.Repo.Repository
	if repo.OwnerID == ctx.Doer.ID && repo.LowerName == ".agent" {
		ctx.PrivateUserErrorf(http.StatusForbidden, "The agent configuration repository is read-only during a run.")
		return false
	}
	prefix := "agent/" + strconv.FormatInt(sessionID, 10) + "/"
	if !refFullName.IsBranch() || !strings.HasPrefix(refFullName.BranchName(), prefix) {
		ctx.PrivateUserErrorf(http.StatusForbidden, "An agent session can only push to branches named %s...", prefix)
		return false
	}

	emptyID := ctx.Repo.GetObjectFormat().EmptyObjectID().String()
	if oldCommitID != emptyID && newCommitID != emptyID {
		// anything reachable from the old tip but not from the new one means history was rewritten
		output, _, err := gitcmd.NewCommand("rev-list", "--max-count=1").
			AddDynamicArguments(oldCommitID, "^"+newCommitID).
			WithEnv(ctx.env).WithRepo(repo).RunStdString(ctx)
		if err != nil {
			ctx.PrivateInternalErrorf("Unable to detect a force push: %v", err)
			return false
		}
		if len(strings.TrimSpace(output)) > 0 {
			ctx.PrivateUserErrorf(http.StatusForbidden, "An agent session cannot force push.")
			return false
		}
	}
	return true
}
