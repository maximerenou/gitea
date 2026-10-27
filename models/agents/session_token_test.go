// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"strings"
	"testing"
	"time"

	actions_model "gitea.dev/models/actions"
	auth_model "gitea.dev/models/auth"
	"gitea.dev/models/db"
	"gitea.dev/models/unittest"
	"gitea.dev/modules/timeutil"
	"gitea.dev/modules/util"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setRunStatus bypasses ActionRun's optimistic lock, which would make a partial struct update match no row
func setRunStatus(t *testing.T, runID int64, status actions_model.Status) {
	res, err := db.GetEngine(t.Context()).Exec("UPDATE action_run SET status = ? WHERE id = ?", status, runID)
	require.NoError(t, err)
	affected, err := res.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, affected)
}

func newRun(t *testing.T, index int64, status actions_model.Status) *actions_model.ActionRun {
	run := &actions_model.ActionRun{RepoID: 1, OwnerID: 2, Index: index, WorkflowID: "agent.yml", Status: status, TriggerUserID: 2}
	require.NoError(t, db.Insert(t.Context(), run))
	return run
}

func TestSessionToken(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := t.Context()
	run := newRun(t, 9001, actions_model.StatusRunning)
	opts := CreateSessionTokenOptions{BotUserID: 100, TriggerUserID: 2, SessionID: 7, ActionRunID: run.ID}

	token, st, err := CreateSessionToken(ctx, opts)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(token, SessionTokenPrefix))
	assert.NotContains(t, st.TokenHash, token) // only a hash is stored
	maxScope, err := MaxSessionTokenScope.Normalize()
	require.NoError(t, err)
	assert.EqualValues(t, maxScope, st.Scope)

	got, err := GetUsableSessionToken(ctx, token)
	require.NoError(t, err)
	assert.Equal(t, int64(2), got.TriggerUserID)
	assert.Equal(t, int64(7), got.SessionID)

	t.Run("GarbageIsRefused", func(t *testing.T) {
		for _, bad := range []string{"", "gsess_", "gsess_" + strings.Repeat("a", 40), strings.Replace(token, SessionTokenPrefix, "xxxxx_", 1)} {
			_, err := GetUsableSessionToken(ctx, bad)
			assert.ErrorIs(t, err, ErrSessionTokenInvalid, bad)
		}
	})

	t.Run("RunFinishedRevokesImplicitly", func(t *testing.T) {
		for _, done := range []actions_model.Status{actions_model.StatusSuccess, actions_model.StatusFailure, actions_model.StatusCancelled} {
			setRunStatus(t, run.ID, done)
			_, err := GetUsableSessionToken(ctx, token)
			assert.ErrorIs(t, err, ErrSessionTokenInvalid, done.String())
		}
		setRunStatus(t, run.ID, actions_model.StatusRunning)
		_, err = GetUsableSessionToken(ctx, token)
		assert.NoError(t, err)
	})

	t.Run("Expiry", func(t *testing.T) {
		_, err := db.GetEngine(ctx).ID(st.ID).Cols("expired_unix").Update(&SessionToken{ExpiredUnix: timeutil.TimeStampNow().Add(-1)})
		require.NoError(t, err)
		_, err = GetUsableSessionToken(ctx, token)
		assert.ErrorIs(t, err, ErrSessionTokenInvalid)
	})

	t.Run("RevokeByRunAndAll", func(t *testing.T) {
		tokenA, _, err := CreateSessionToken(ctx, opts)
		require.NoError(t, err)
		otherRun := newRun(t, 9002, actions_model.StatusRunning)
		tokenB, _, err := CreateSessionToken(ctx, CreateSessionTokenOptions{BotUserID: 100, TriggerUserID: 2, SessionID: 8, ActionRunID: otherRun.ID})
		require.NoError(t, err)

		require.NoError(t, RevokeSessionTokensOfRun(ctx, run.ID))
		_, err = GetUsableSessionToken(ctx, tokenA)
		assert.ErrorIs(t, err, ErrSessionTokenInvalid)
		_, err = GetUsableSessionToken(ctx, tokenB) // another run is untouched
		assert.NoError(t, err)

		require.NoError(t, RevokeAllSessionTokens(ctx))
		_, err = GetUsableSessionToken(ctx, tokenB)
		assert.ErrorIs(t, err, ErrSessionTokenInvalid)
	})
}

func TestSessionTokenCreationRules(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := t.Context()
	run := newRun(t, 9003, actions_model.StatusRunning)
	base := CreateSessionTokenOptions{BotUserID: 100, TriggerUserID: 2, SessionID: 7, ActionRunID: run.ID}

	// no human behind the run (scheduled scan) means no token at all
	noHuman := base
	noHuman.TriggerUserID = 0
	_, _, err := CreateSessionToken(ctx, noHuman)
	assert.ErrorIs(t, err, util.ErrInvalidArgument)
	noRun := base
	noRun.ActionRunID = 0
	_, _, err = CreateSessionToken(ctx, noRun)
	assert.ErrorIs(t, err, util.ErrInvalidArgument)

	// a session can never be given administration, organization, user or settings scopes
	for _, scope := range []auth_model.AccessTokenScope{
		auth_model.AccessTokenScopeAll, auth_model.AccessTokenScopeWriteAdmin, auth_model.AccessTokenScopeReadAdmin,
		auth_model.AccessTokenScopeWriteOrganization, auth_model.AccessTokenScopeWriteUser, auth_model.AccessTokenScopeWriteMisc,
		"write:repository,write:admin", "public-only,write:repository",
	} {
		opts := base
		opts.Scope = scope
		_, _, err := CreateSessionToken(ctx, opts)
		assert.ErrorIs(t, err, util.ErrInvalidArgument, scope)
	}
	narrow := base
	narrow.Scope = auth_model.AccessTokenScopeReadRepository
	_, st, err := CreateSessionToken(ctx, narrow)
	require.NoError(t, err)
	assert.EqualValues(t, "read:repository", st.Scope)

	// the lifetime is capped
	long := base
	long.Lifetime = 1000 * time.Hour
	_, st, err = CreateSessionToken(ctx, long)
	require.NoError(t, err)
	assert.LessOrEqual(t, int64(st.ExpiredUnix), int64(timeutil.TimeStampNow().Add(int64(SessionTokenMaxLifetime/time.Second)+5)))
}
