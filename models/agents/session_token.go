// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	actions_model "gitea.dev/models/actions"
	auth_model "gitea.dev/models/auth"
	"gitea.dev/models/db"
	"gitea.dev/modules/timeutil"
	"gitea.dev/modules/util"
)

const (
	// SessionTokenPrefix lets the authentication chain recognise a session token without any database lookup.
	SessionTokenPrefix = "gsess_"

	// MaxSessionTokenScope is the widest scope a session token may carry: never administration, organization, user or settings.
	MaxSessionTokenScope auth_model.AccessTokenScope = "write:repository,write:issue,write:package"

	// SessionTokenMaxLifetime caps a token even if its run never reports completion.
	SessionTokenMaxLifetime = 24 * time.Hour
)

var ErrSessionTokenInvalid = util.NewInvalidArgumentErrorf("agent session token is invalid, expired or revoked")

// SessionToken is the short-lived credential of one agent run started by a person.
// It is only valid while its Actions run is not finished, and it is bound to the person who triggered the run:
// the effective permission is the intersection of the bot, that person and the token scope.
type SessionToken struct {
	ID            int64              `xorm:"pk autoincr"`
	TokenHash     string             `xorm:"VARCHAR(64) UNIQUE NOT NULL"`
	BotUserID     int64              `xorm:"INDEX NOT NULL"`
	TriggerUserID int64              `xorm:"NOT NULL"` // always a real user: runs without one get no token
	SessionID     int64              `xorm:"INDEX NOT NULL"`
	ActionRunID   int64              `xorm:"INDEX NOT NULL"`
	Scope         string             `xorm:"TEXT NOT NULL"`
	ExpiredUnix   timeutil.TimeStamp `xorm:"NOT NULL"`
	RevokedUnix   timeutil.TimeStamp `xorm:"NOT NULL DEFAULT 0"`
	CreatedUnix   timeutil.TimeStamp `xorm:"created NOT NULL"`
}

func init() {
	db.RegisterModel(new(SessionToken))
}

func (*SessionToken) TableName() string {
	return "agent_session_token"
}

func hashSessionToken(token string) string {
	sum := sha256.Sum256([]byte(token)) // the token is 160 random bits, a slow KDF would only cost every request
	return hex.EncodeToString(sum[:])
}

type CreateSessionTokenOptions struct {
	BotUserID     int64
	TriggerUserID int64
	SessionID     int64
	ActionRunID   int64
	Scope         auth_model.AccessTokenScope // empty means MaxSessionTokenScope
	Lifetime      time.Duration               // zero or above the maximum means SessionTokenMaxLifetime
}

// CreateSessionToken returns the plain token, which is never stored.
func CreateSessionToken(ctx context.Context, opts CreateSessionTokenOptions) (string, *SessionToken, error) {
	if opts.BotUserID <= 0 || opts.TriggerUserID <= 0 || opts.SessionID <= 0 || opts.ActionRunID <= 0 {
		return "", nil, util.NewInvalidArgumentErrorf("a session token needs a bot, a triggering user, a session and a run")
	}
	scope := opts.Scope
	if scope == "" {
		scope = MaxSessionTokenScope
	}
	scope, err := scope.Normalize()
	if err != nil {
		return "", nil, fmt.Errorf("CreateSessionToken: %w", err)
	}
	if ok, err := MaxSessionTokenScope.CanCreateChildScope(scope); err != nil || !ok {
		return "", nil, util.NewInvalidArgumentErrorf("scope %q exceeds what an agent session may have", scope)
	}
	if public, err := scope.PublicOnly(); err != nil || public {
		return "", nil, util.NewInvalidArgumentErrorf("public-only scope is not supported for agent sessions")
	}
	lifetime := opts.Lifetime
	if lifetime <= 0 || lifetime > SessionTokenMaxLifetime {
		lifetime = SessionTokenMaxLifetime
	}

	token := SessionTokenPrefix + util.CryptoRandomString(40)
	t := &SessionToken{
		TokenHash:     hashSessionToken(token),
		BotUserID:     opts.BotUserID,
		TriggerUserID: opts.TriggerUserID,
		SessionID:     opts.SessionID,
		ActionRunID:   opts.ActionRunID,
		Scope:         string(scope),
		ExpiredUnix:   timeutil.TimeStampNow().Add(int64(lifetime / time.Second)),
	}
	if err := db.Insert(ctx, t); err != nil {
		return "", nil, err
	}
	return token, t, nil
}

// GetUsableSessionToken resolves a presented token. Every call re-checks revocation, expiry and the state of the
// Actions run, so nothing is cached and a finished run stops authenticating immediately.
func GetUsableSessionToken(ctx context.Context, token string) (*SessionToken, error) {
	if !strings.HasPrefix(token, SessionTokenPrefix) || len(token) != len(SessionTokenPrefix)+40 {
		return nil, ErrSessionTokenInvalid
	}
	t := &SessionToken{}
	has, err := db.GetEngine(ctx).Where("token_hash = ?", hashSessionToken(token)).Get(t)
	if err != nil {
		return nil, err
	} else if !has || t.RevokedUnix != 0 || timeutil.TimeStampNow() >= t.ExpiredUnix {
		return nil, ErrSessionTokenInvalid
	}
	run, exist, err := db.GetByID[actions_model.ActionRun](ctx, t.ActionRunID)
	if err != nil {
		return nil, err
	} else if !exist || run.Status.IsDone() {
		return nil, ErrSessionTokenInvalid
	}
	return t, nil
}

// RevokeSessionTokensOfRun is called when an Actions run ends.
func RevokeSessionTokensOfRun(ctx context.Context, runID int64) error {
	_, err := db.GetEngine(ctx).Where("action_run_id = ? AND revoked_unix = 0", runID).
		Cols("revoked_unix").Update(&SessionToken{RevokedUnix: timeutil.TimeStampNow()})
	return err
}

// RevokeAllSessionTokens is called when the agents feature is switched off.
func RevokeAllSessionTokens(ctx context.Context) error {
	_, err := db.GetEngine(ctx).Where("revoked_unix = 0").
		Cols("revoked_unix").Update(&SessionToken{RevokedUnix: timeutil.TimeStampNow()})
	return err
}

func (t *SessionToken) AccessScope() auth_model.AccessTokenScope {
	return auth_model.AccessTokenScope(t.Scope)
}
