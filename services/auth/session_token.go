// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"net/http"
	"strings"

	agents_model "gitea.dev/models/agents"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/agents"
	"gitea.dev/modules/util"
)

var _ Method = &SessionToken{}

const SessionTokenMethodName = "agent_session_token"

// SessionToken authenticates the short-lived token of an agent run, as a bearer token, a query token or a basic auth credential.
// It is inert for any other credential: only tokens carrying the session prefix are looked at.
type SessionToken struct{}

func (s *SessionToken) Name() string {
	return SessionTokenMethodName
}

func (s *SessionToken) Verify(req *http.Request, _ http.ResponseWriter, _ DataStore, _ SessionStore) (*user_model.User, error) {
	token := presentedSessionToken(req)
	if token == "" {
		return nil, nil //nolint:nilnil // the auth method is not applicable
	}
	ctx := req.Context()
	if !agents.IsEnabled(ctx) {
		return nil, agents_model.ErrSessionTokenInvalid // switching agents off kills every token at once
	}
	t, err := agents_model.GetUsableSessionToken(ctx, token)
	if err != nil {
		return nil, err
	}
	bot, err := user_model.GetValidDoerUserByID(ctx, t.BotUserID)
	if err != nil {
		return nil, err
	}
	if isBot, err := agents_model.IsAgentBot(ctx, bot.ID); err != nil {
		return nil, err
	} else if !bot.IsTypeBot() || !isBot {
		return nil, agents_model.ErrSessionTokenInvalid
	}
	// the person who started the run must still be allowed to act, otherwise the run would outlive their access
	if _, err := user_model.GetValidDoerUserByID(ctx, t.TriggerUserID); err != nil {
		return nil, agents_model.ErrSessionTokenInvalid
	}
	bot.ExtDoerData = user_model.NewSessionTokenExtDoerData(t.SessionID, t.TriggerUserID, t.AccessScope())
	return bot, nil
}

func presentedSessionToken(req *http.Request) string {
	if token, ok := parseToken(req); ok && strings.HasPrefix(token, agents_model.SessionTokenPrefix) {
		return token
	}
	if token := parseAuthBasic(req).authToken; strings.HasPrefix(token, agents_model.SessionTokenPrefix) {
		return token
	}
	return ""
}

// rejectAgentBotToken refuses personal access tokens of agent bots: a bot only acts through session tokens.
func rejectAgentBotToken(ctx context.Context, u *user_model.User) error {
	if !u.IsTypeBot() {
		return nil
	}
	isAgent, err := agents_model.IsAgentBot(ctx, u.ID)
	if err != nil {
		return err
	}
	if isAgent {
		return util.NewPermissionDeniedErrorf("agent bots do not accept personal or OAuth2 tokens")
	}
	return nil
}
