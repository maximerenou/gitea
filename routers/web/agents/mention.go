// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"slices"

	agents_model "gitea.dev/models/agents"
	repo_model "gitea.dev/models/repo"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/log"
	shared_mention "gitea.dev/routers/web/shared/mention"
	agents_service "gitea.dev/services/agents"
	"gitea.dev/services/context"
)

// usableAgents returns the enabled agents the doer has a key for, the ones a mention or an assignment from them can run
func usableAgents(ctx *context.Context, repo *repo_model.Repository) ([]*agents_model.Agent, error) {
	if ctx.Doer == nil || !agents_service.IsRepoUsable(ctx, repo) {
		return nil, nil
	}
	agents, err := agents_model.FindAgents(ctx, true)
	if err != nil {
		return nil, err
	}
	keys, err := agents_model.FindUserKeys(ctx, ctx.Doer.ID)
	if err != nil {
		return nil, err
	}
	usable := make([]*agents_model.Agent, 0, len(agents))
	for _, agent := range agents {
		if keys[agent.ID] != nil && agent.Bot != nil && agents_service.IsAgentAllowed(ctx, repo, agent) {
			usable = append(usable, agent)
		}
	}
	return usable, nil
}

func ownBots(ctx *context.Context, repo *repo_model.Repository) ([]*user_model.User, error) {
	agents, err := usableAgents(ctx, repo)
	bots := make([]*user_model.User, 0, len(agents))
	for _, agent := range agents {
		bots = append(bots, agent.Bot)
	}
	return bots, err
}

func AddOwnBotMentions(ctx *context.Context, c *shared_mention.Collector) error {
	bots, err := ownBots(ctx, ctx.Repo.Repository)
	if err != nil {
		return err
	}
	c.AddUsers(ctx, bots)
	return nil
}

// AppendOwnBotAssignees offers the agents the doer has a key for as assignees, assigning one starts a session
func AppendOwnBotAssignees(ctx *context.Context, repo *repo_model.Repository, users []*user_model.User) []*user_model.User {
	bots, err := ownBots(ctx, repo)
	if err != nil {
		log.Error("agents: assignee bots: %v", err)
	}
	for _, bot := range bots {
		if !slices.ContainsFunc(users, func(u *user_model.User) bool { return u.ID == bot.ID }) {
			users = append(users, bot)
		}
	}
	return users
}
