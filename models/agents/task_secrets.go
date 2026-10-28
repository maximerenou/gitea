// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"context"
	"errors"
	"fmt"

	actions_model "gitea.dev/models/actions"
	"gitea.dev/modules/agents"
	"gitea.dev/modules/util"
)

// SessionTokenSecretName is the secret that carries the session token to the job.
const SessionTokenSecretName = "AGENT_SESSION_TOKEN"

// RestrictTaskSecrets is called when an Actions task is handed to a runner. For the task of an agent run it replaces
// every secret with the run's session token, created now so that it lives exactly as long as the job. An agent is driven
// by a language model reading untrusted text, so it must not receive the repository or owner secrets, nor the task token.
// Tasks that do not belong to an agent run are left untouched.
func RestrictTaskSecrets(ctx context.Context, task *actions_model.ActionTask, secrets map[string]string) error {
	if task.Job == nil || task.Job.Run == nil {
		return nil
	}
	run, err := GetRunByActionRunID(ctx, task.Job.RunID)
	if errors.Is(err, util.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("RestrictTaskSecrets: %w", err)
	}

	clear(secrets) // from here the task is an agent task, whatever happens next
	if !agents.IsEnabled(ctx) || run.Status.IsTerminal() || run.TriggerUserID <= 0 {
		return nil // no token: a run without a person is limited to the bot alone and gets no session credential
	}
	agent, err := GetAgentByID(ctx, run.AgentID)
	if err != nil {
		return fmt.Errorf("RestrictTaskSecrets: %w", err)
	}
	// a rerun of the same Actions run gets a fresh token and the previous one stops working
	if err := RevokeSessionTokensOfRun(ctx, run.ActionRunID); err != nil {
		return fmt.Errorf("RestrictTaskSecrets: %w", err)
	}
	token, _, err := CreateSessionToken(ctx, CreateSessionTokenOptions{
		BotUserID:     agent.BotUserID,
		TriggerUserID: run.TriggerUserID,
		SessionID:     run.SessionID,
		ActionRunID:   run.ActionRunID,
	})
	if err != nil {
		return fmt.Errorf("RestrictTaskSecrets: %w", err)
	}
	secrets[SessionTokenSecretName] = token
	return MarkRunRunning(ctx, run.ID)
}
