// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package access

import (
	"context"

	"gitea.dev/models/perm"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unit"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/log"
)

// agentConfigRepoName is the bot-owned repository holding the agent configuration; a run may read it, never write it.
const agentConfigRepoName = ".agent"

// getAgentSessionRepoPermission is the permission of an agent session token on a repository: for every unit the
// lowest of what the bot may do and what the person who triggered the run may do. It starts from no access and
// never carries an admin or owner mode, so owner-only shortcuts cannot apply to a session.
// Anything it cannot resolve fails closed.
func getAgentSessionRepoPermission(ctx context.Context, repo *repo_model.Repository, bot *user_model.User, triggerUserID int64) (Permission, error) {
	if triggerUserID <= 0 {
		return PermissionNoAccess(), nil
	}
	trigger, err := user_model.GetValidDoerUserByID(ctx, triggerUserID)
	if err != nil {
		log.Warn("agent session of bot %d: triggering user %d is not usable: %v", bot.ID, triggerUserID, err)
		return PermissionNoAccess(), nil
	}

	// both sides are computed as plain users, without the session credential
	plainBot := *bot
	plainBot.ExtDoerData = nil
	botPerm, err := GetIndividualUserRepoPermission(ctx, repo, &plainBot)
	if err != nil {
		return PermissionNoAccess(), err
	}
	triggerPerm, err := GetIndividualUserRepoPermission(ctx, repo, trigger)
	if err != nil {
		return PermissionNoAccess(), err
	}

	isConfigRepo := repo.OwnerID == bot.ID && repo.LowerName == agentConfigRepoName
	res := Permission{AccessMode: perm.AccessModeNone, units: repo.Units, unitsMode: map[unit.Type]perm.AccessMode{}}
	for _, u := range repo.Units {
		mode := min(botPerm.UnitAccessMode(u.Type), triggerPerm.UnitAccessMode(u.Type))
		if isConfigRepo {
			mode = min(mode, perm.AccessModeRead)
		}
		res.unitsMode[u.Type] = mode
	}
	return res, nil
}
