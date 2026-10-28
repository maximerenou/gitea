// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"strings"
	"testing"

	"gitea.dev/models/unittest"
	"gitea.dev/modules/util"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionAndRun(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := t.Context()

	assert.ErrorIs(t, CreateSession(ctx, &Session{}), util.ErrInvalidArgument)
	s := &Session{AgentID: 1, RepoID: 1, IssueID: 10, TriggerUserID: 2}
	require.NoError(t, CreateSession(ctx, s))
	second := &Session{AgentID: 1, RepoID: 1, IssueID: 10}
	require.NoError(t, CreateSession(ctx, second))
	got, err := GetSessionBySubject(ctx, 1, 10)
	require.NoError(t, err)
	assert.Equal(t, second.ID, got.ID) // the latest session of the subject
	_, err = GetSessionBySubject(ctx, 1, 11)
	assert.ErrorIs(t, err, ErrSessionNotFound)
	require.NoError(t, SetSessionConversation(ctx, s.ID, "conv-1"))
	got, err = GetSessionByID(ctx, s.ID)
	require.NoError(t, err)
	assert.Equal(t, "conv-1", got.ConversationID)

	assert.ErrorIs(t, CreateRun(ctx, &Run{}), util.ErrInvalidArgument)
	run := &Run{SessionID: s.ID, AgentID: 1, RepoID: 1, IssueID: 10, TriggerUserID: 2}
	require.NoError(t, CreateRun(ctx, run))
	assert.Equal(t, RunPending, run.Status)

	active, err := GetActiveRunOfSession(ctx, s.ID)
	require.NoError(t, err)
	assert.Equal(t, run.ID, active.ID)

	require.NoError(t, SetRunActionRun(ctx, run.ID, 55))
	byAction, err := GetRunByActionRunID(ctx, 55)
	require.NoError(t, err)
	assert.Equal(t, run.ID, byAction.ID)

	require.NoError(t, MarkRunRunning(ctx, run.ID))
	got2, err := GetRunByID(ctx, run.ID)
	require.NoError(t, err)
	assert.Equal(t, RunRunning, got2.Status)

	t.Run("ProgressKeepsTheTail", func(t *testing.T) {
		require.NoError(t, AppendProgress(ctx, run.ID, strings.Repeat("a", MaxProgressLength)))
		require.NoError(t, AppendProgress(ctx, run.ID, "END"))
		r, err := GetRunByID(ctx, run.ID)
		require.NoError(t, err)
		assert.Len(t, r.Progress, MaxProgressLength)
		assert.True(t, strings.HasSuffix(r.Progress, "END"))
	})

	t.Run("FinishRunHappensOnce", func(t *testing.T) {
		_, err := FinishRun(ctx, run.ID, RunRunning, "", "", "", "")
		assert.ErrorIs(t, err, util.ErrInvalidArgument) // not a final status

		first, err := FinishRun(ctx, run.ID, RunSuccess, "done", "", `{"version":"v1"}`, "")
		require.NoError(t, err)
		assert.True(t, first)
		// a late failure or a cancellation must not overwrite the first decision
		for _, st := range []RunStatus{RunFailed, RunCancelled, RunNeedsHuman, RunSuccess} {
			again, err := FinishRun(ctx, run.ID, st, "other", "boom", "", "")
			require.NoError(t, err)
			assert.False(t, again, st)
		}
		r, err := GetRunByID(ctx, run.ID)
		require.NoError(t, err)
		assert.Equal(t, RunSuccess, r.Status)
		assert.Equal(t, "done", r.Summary)
		assert.NotZero(t, r.EndedUnix)

		_, err = GetActiveRunOfSession(ctx, s.ID)
		assert.ErrorIs(t, err, ErrRunNotFound)
		require.NoError(t, MarkRunRunning(ctx, run.ID)) // a finished run is never revived
		r, _ = GetRunByID(ctx, run.ID)
		assert.Equal(t, RunSuccess, r.Status)
	})

	runs, err := ListRunsOfIssue(ctx, 10)
	require.NoError(t, err)
	assert.Len(t, runs, 1)
}
