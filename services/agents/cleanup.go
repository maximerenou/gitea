// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"context"
	"time"

	agents_model "gitea.dev/models/agents"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/log"
	"gitea.dev/modules/storage"
	"gitea.dev/modules/timeutil"
	"gitea.dev/services/cron"
)

func registerCleanupTask() {
	cron.RegisterTaskFatal("agents_cleanup", &cron.OlderThanConfig{
		BaseConfig: cron.BaseConfig{Enabled: true, Schedule: "@midnight"},
		OlderThan:  90 * 24 * time.Hour,
	}, func(ctx context.Context, _ *user_model.User, config *cron.OlderThanConfig) error {
		return Cleanup(ctx, config.OlderThan)
	})
}

// Cleanup deletes the event logs of old runs and the Codex conversations of idle sessions, sessions and runs stay as history
func Cleanup(ctx context.Context, olderThan time.Duration) error {
	before := timeutil.TimeStamp(time.Now().Add(-olderThan).Unix())
	deleted, err := agents_model.DeleteEventsOfRunsStoppedBefore(ctx, before)
	if err != nil {
		return err
	}
	sessions, err := agents_model.FindIdleSessionsWithState(ctx, before)
	if err != nil {
		return err
	}
	for _, s := range sessions {
		if err := storage.Actions.Delete(agents_model.StatePath(s.ID)); err != nil {
			log.Warn("agents: delete Codex state of session %d: %v", s.ID, err)
		}
		s.HasState, s.CodexThreadID = false, ""
		if err := agents_model.UpdateSession(ctx, s, "has_state", "codex_thread_id"); err != nil {
			return err
		}
	}
	log.Info("agents: deleted %d events and %d Codex conversations older than %s", deleted, len(sessions), olderThan)
	return nil
}
