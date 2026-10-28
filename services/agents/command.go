// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"context"
	"fmt"

	agents_model "gitea.dev/models/agents"
	"gitea.dev/models/organization"
	"gitea.dev/models/perm"
	access_model "gitea.dev/models/perm/access"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unit"
	user_model "gitea.dev/models/user"
	repo_service "gitea.dev/services/repository"
)

// AppliesToRepo reports whether the agent may act on the repository: a global agent everywhere, any other one only
// on the repositories of its owner.
func AppliesToRepo(agent *agents_model.Agent, repo *repo_model.Repository) bool {
	return agent.IsGlobal() || agent.OwnerID == repo.OwnerID
}

// CanCommand reports whether a person may make the agent act on a repository. It always needs write access to the
// repository; the agent's access setting can only narrow that. A user without the right gets nothing, not even an error,
// so the caller must stay silent.
func CanCommand(ctx context.Context, agent *agents_model.Agent, repo *repo_model.Repository, user *user_model.User) (bool, error) {
	if user == nil || user.ID <= 0 || user.ID == agent.BotUserID || !AppliesToRepo(agent, repo) {
		return false, nil
	}
	p, err := access_model.GetIndividualUserRepoPermission(ctx, repo, user)
	if err != nil {
		return false, fmt.Errorf("CanCommand: %w", err)
	}
	if !p.CanWrite(unit.TypeCode) {
		return false, nil
	}
	switch agent.AccessMode {
	case agents_model.AccessWriters:
		return true, nil
	case agents_model.AccessOwner, agents_model.AccessList:
		// the explicit list of users and teams is not available yet: it fails closed to the owner
		return isAgentOwner(ctx, agent, user)
	}
	return false, nil
}

func isAgentOwner(ctx context.Context, agent *agents_model.Agent, user *user_model.User) (bool, error) {
	switch {
	case agent.IsGlobal():
		return user.IsAdmin, nil
	case agent.OwnerID == user.ID:
		return true, nil
	}
	owner, err := user_model.GetUserByID(ctx, agent.OwnerID)
	if err != nil {
		return false, fmt.Errorf("isAgentOwner: %w", err)
	}
	if owner.IsOrganization() {
		return organization.IsOrganizationOwner(ctx, owner.ID, user.ID)
	}
	return false, nil
}

// ensureBotCanWrite makes the bot a collaborator with write access when it has none yet.
func ensureBotCanWrite(ctx context.Context, repo *repo_model.Repository, bot *user_model.User) error {
	p, err := access_model.GetIndividualUserRepoPermission(ctx, repo, bot)
	if err != nil {
		return fmt.Errorf("ensureBotCanWrite: %w", err)
	}
	if p.CanWrite(unit.TypeCode) {
		return nil
	}
	if err := repo_service.AddOrUpdateCollaborator(ctx, repo, bot, perm.AccessModeWrite); err != nil {
		return fmt.Errorf("ensureBotCanWrite: %w", err)
	}
	return nil
}
