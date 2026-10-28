// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package access

import (
	"context"
	"errors"
	"maps"

	perm_model "gitea.dev/models/perm"
	repo_model "gitea.dev/models/repo"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/util"
)

// Agent hooks are set when agent sessions are enabled
var (
	// AgentDelegateUser returns the user an agent bot works for in a repository, from the run its credential belongs to.
	// It returns util.ErrNotExist for other bots, and nil for an agent bot outside of a run in that repository, which then has no access at all.
	AgentDelegateUser func(ctx context.Context, bot *user_model.User, repo *repo_model.Repository) (*user_model.User, error)
	// AgentAssignable reports whether a user is an agent that can be assigned in the repository
	AgentAssignable func(ctx context.Context, user *user_model.User, repo *repo_model.Repository) bool
)

// agentRepoPermission gives an agent bot the permission of the user it works for, capped to write
func agentRepoPermission(ctx context.Context, repo *repo_model.Repository, bot *user_model.User) (perm Permission, ok bool, err error) {
	if AgentDelegateUser == nil || bot == nil || !bot.IsTypeBot() {
		return perm, false, nil
	}
	delegate, err := AgentDelegateUser(ctx, bot, repo)
	if errors.Is(err, util.ErrNotExist) || user_model.IsErrUserNotExist(err) {
		return perm, false, nil
	} else if err != nil {
		return perm, false, err
	} else if delegate == nil {
		return PermissionNoAccess(), true, nil
	}
	perm, err = GetIndividualUserRepoPermission(ctx, repo, delegate)
	if err != nil {
		return perm, false, err
	}
	perm.AccessMode = min(perm.AccessMode, perm_model.AccessModeWrite)
	if perm.unitsMode != nil {
		perm.unitsMode = maps.Clone(perm.unitsMode)
		for unitType, mode := range perm.unitsMode {
			perm.unitsMode[unitType] = min(mode, perm_model.AccessModeWrite)
		}
	}
	return perm, true, nil
}
