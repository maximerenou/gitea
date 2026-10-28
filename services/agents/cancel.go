// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"context"
	"fmt"

	actions_model "gitea.dev/models/actions"
	agents_model "gitea.dev/models/agents"
	actions_service "gitea.dev/services/actions"
)

// CancelRun ends the run as cancelled and stops its Actions run. It is safe to call more than once.
func CancelRun(ctx context.Context, run *agents_model.Run) error {
	if _, err := agents_model.FinishRun(ctx, run.ID, agents_model.RunCancelled, "", "", "", ""); err != nil {
		return fmt.Errorf("CancelRun: %w", err)
	}
	if run.ActionRunID == 0 {
		return nil
	}
	if err := agents_model.RevokeSessionTokensOfRun(ctx, run.ActionRunID); err != nil {
		return fmt.Errorf("CancelRun: %w", err)
	}
	actionRun, err := actions_model.GetRunByRepoAndID(ctx, run.RepoID, run.ActionRunID)
	if err != nil {
		return fmt.Errorf("CancelRun: %w", err)
	}
	if actionRun.Status.IsDone() {
		return nil
	}
	jobs, err := actions_model.GetLatestAttemptJobsByRun(ctx, actionRun)
	if err != nil {
		return fmt.Errorf("CancelRun: %w", err)
	}
	if _, err := actions_service.CancelRun(ctx, actionRun, jobs); err != nil {
		return fmt.Errorf("CancelRun: %w", err)
	}
	return nil
}
