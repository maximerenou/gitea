// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"context"
	"errors"
	"strconv"
	"strings"

	agents_model "gitea.dev/models/agents"
	auth_model "gitea.dev/models/auth"
	repo_model "gitea.dev/models/repo"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/util"
)

// runOfCredential returns the active run whose token a bot authenticated with
func runOfCredential(ctx context.Context, bot *user_model.User) (*agents_model.Run, error) {
	tokenID, ok := strings.CutPrefix(user_model.GetDoerCredential(bot), user_model.CredentialAccessToken+":")
	if !ok {
		return nil, util.NewNotExistErrorf("@%s is not working on a run", bot.Name)
	}
	id, _ := strconv.ParseInt(tokenID, 10, 64)
	r, err := agents_model.GetActiveRunByAccessToken(ctx, id)
	if err != nil {
		return nil, err
	}
	s, err := agents_model.GetSessionByID(ctx, r.SessionID)
	if err != nil {
		return nil, err
	}
	agent, err := agents_model.GetAgentByID(ctx, s.AgentID)
	if err != nil {
		return nil, err
	}
	if agent.BotID != bot.ID {
		return nil, util.NewNotExistErrorf("@%s is not the agent of run %d", bot.Name, r.ID)
	}
	return r, nil
}

// DelegateUser returns the user an agent bot works for: the trigger user of the run whose token it uses, in the run's repository only.
// Elsewhere, and outside of a run, it returns nil: an agent bot has no access at all, even where it was added as a collaborator.
func DelegateUser(ctx context.Context, bot *user_model.User, repo *repo_model.Repository) (*user_model.User, error) {
	if _, err := agents_model.GetAgentByBotID(ctx, bot.ID); err != nil {
		return nil, err
	}
	r, err := runOfCredential(ctx, bot)
	if errors.Is(err, util.ErrNotExist) || (err == nil && (repo == nil || r.RepoID != repo.ID)) {
		return nil, nil //nolint:nilnil // no access, not an error
	} else if err != nil {
		return nil, err
	}
	return user_model.GetUserByID(ctx, r.TriggerUserID)
}

// actingBot is the agent's bot carrying the run's credential, so what Gitea does for the run gets the trigger user's permissions
func actingBot(s *agents_model.Session, r *agents_model.Run) *user_model.User {
	bot := *s.Agent.Bot
	bot.ExtDoerData = user_model.NewTokenExtDoerData(user_model.CredentialAccessToken, r.AccessTokenID, auth_model.AccessTokenScopeWriteRepository)
	return &bot
}

// Assignable lets users assign an enabled agent wherever sessions can run, the assigner's key and permissions are used
func Assignable(ctx context.Context, user *user_model.User, repo *repo_model.Repository) bool {
	if !user.IsTypeBot() || !IsRepoUsable(ctx, repo) {
		return false
	}
	agent, err := agents_model.GetAgentByBotID(ctx, user.ID)
	return err == nil && IsAgentAllowed(ctx, repo, agent)
}
