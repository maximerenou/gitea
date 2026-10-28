// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"testing"

	actions_model "gitea.dev/models/actions"
	"gitea.dev/models/db"
	system_model "gitea.dev/models/system"
	"gitea.dev/models/unittest"
	"gitea.dev/modules/agents"
	"gitea.dev/modules/json"
	"gitea.dev/modules/setting/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setEnabled(t *testing.T, enabled bool) {
	s := agents.DefaultSettings()
	s.Enabled = enabled
	v, err := json.Marshal(s)
	require.NoError(t, err)
	require.NoError(t, system_model.SetSettings(t.Context(), map[string]string{agents.SettingsKey(): string(v)}))
	config.GetDynGetter().InvalidateCache()
}

func TestRestrictTaskSecrets(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := t.Context()
	setEnabled(t, true)

	agent := &Agent{BotUserID: 100, Name: "bot", ConnectionID: 1, Model: "m", Action: agents.ActionReview, Events: []agents.Event{agents.EventAll}, AccessMode: AccessWriters}
	require.NoError(t, db.Insert(ctx, agent))
	session := &Session{AgentID: agent.ID, RepoID: 1, IssueID: 3}
	require.NoError(t, CreateSession(ctx, session))

	newTask := func(actionRun *actions_model.ActionRun) *actions_model.ActionTask {
		return &actions_model.ActionTask{Job: &actions_model.ActionRunJob{RunID: actionRun.ID, Run: actionRun}}
	}
	repoSecrets := func() map[string]string {
		return map[string]string{"GITEA_TOKEN": "task-token", "GITHUB_TOKEN": "task-token", "DEPLOY_KEY": "repo-secret", "ORG_SECRET": "org-secret"}
	}
	agentRun := func(index int64, triggerUserID int64) (*Run, *actions_model.ActionRun) {
		ar := newRun(t, index, actions_model.StatusWaiting)
		r := &Run{SessionID: session.ID, AgentID: agent.ID, RepoID: 1, IssueID: 3, ActionRunID: ar.ID, TriggerUserID: triggerUserID}
		require.NoError(t, CreateRun(ctx, r))
		return r, ar
	}

	t.Run("AgentTaskOnlyGetsItsSessionToken", func(t *testing.T) {
		r, ar := agentRun(9201, 2)
		secrets := repoSecrets()
		require.NoError(t, RestrictTaskSecrets(ctx, newTask(ar), secrets))
		require.Len(t, secrets, 1) // no repository secret, no organization secret, not even the task token
		token := secrets[SessionTokenSecretName]
		st, err := GetUsableSessionToken(ctx, token)
		require.NoError(t, err)
		assert.Equal(t, int64(2), st.TriggerUserID)
		assert.Equal(t, session.ID, st.SessionID)
		assert.Equal(t, agent.BotUserID, st.BotUserID)
		got, _ := GetRunByID(ctx, r.ID)
		assert.Equal(t, RunRunning, got.Status) // the job started

		// a rerun of the same Actions run gets a new token and the previous one stops working
		again := repoSecrets()
		require.NoError(t, RestrictTaskSecrets(ctx, newTask(ar), again))
		_, err = GetUsableSessionToken(ctx, token)
		assert.ErrorIs(t, err, ErrSessionTokenInvalid)
		_, err = GetUsableSessionToken(ctx, again[SessionTokenSecretName])
		assert.NoError(t, err)
	})

	t.Run("ScheduledRunGetsNoToken", func(t *testing.T) {
		_, ar := agentRun(9202, 0)
		secrets := repoSecrets()
		require.NoError(t, RestrictTaskSecrets(ctx, newTask(ar), secrets))
		assert.Empty(t, secrets) // and still none of the repository secrets
	})

	t.Run("FinishedRunGetsNothing", func(t *testing.T) {
		r, ar := agentRun(9203, 2)
		_, err := FinishRun(ctx, r.ID, RunCancelled, "", "", "", "")
		require.NoError(t, err)
		secrets := repoSecrets()
		require.NoError(t, RestrictTaskSecrets(ctx, newTask(ar), secrets))
		assert.Empty(t, secrets)
	})

	t.Run("OrdinaryTaskIsUntouched", func(t *testing.T) {
		ar := newRun(t, 9204, actions_model.StatusWaiting)
		secrets := repoSecrets()
		require.NoError(t, RestrictTaskSecrets(ctx, newTask(ar), secrets))
		assert.Equal(t, repoSecrets(), secrets)
		assert.NoError(t, RestrictTaskSecrets(ctx, &actions_model.ActionTask{}, secrets)) // nothing loaded: not an agent task either
	})

	t.Run("DisabledAgentsGetNoToken", func(t *testing.T) {
		_, ar := agentRun(9205, 2)
		setEnabled(t, false)
		secrets := repoSecrets()
		require.NoError(t, RestrictTaskSecrets(ctx, newTask(ar), secrets))
		assert.Empty(t, secrets)
		setEnabled(t, true)
	})
}
