// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"context"
	"slices"

	"gitea.dev/models/db"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/timeutil"
)

// RepoSetting is how a repository's administrators open it to agents, repositories without one follow [agents] ENABLE_BY_DEFAULT
type RepoSetting struct {
	ID      int64
	RepoID  int64 `xorm:"UNIQUE NOT NULL"`
	Enabled bool  `xorm:"NOT NULL DEFAULT false"`
	// AllowedAgentIDs restricts the agents usable in the repository, empty allows all
	AllowedAgentIDs []int64 `xorm:"TEXT JSON"`
	// ReviewAgentID reviews the new pull requests, with the API key and the permissions of ReviewUserID who chose it
	ReviewAgentID int64 `xorm:"NOT NULL DEFAULT 0"`
	ReviewUserID  int64 `xorm:"NOT NULL DEFAULT 0"`
	// ReviewEveryone also reviews the pull requests of people who cannot write to the repository
	ReviewEveryone bool `xorm:"NOT NULL DEFAULT false"`

	Updated timeutil.TimeStamp `xorm:"updated"`
}

func (*RepoSetting) TableName() string {
	return "agent_repo_setting"
}

func init() {
	db.RegisterModel(new(RepoSetting))
}

// GetRepoSetting returns the agents setting of a repository, the instance default when it has none
func GetRepoSetting(ctx context.Context, repoID int64) (*RepoSetting, error) {
	rs := &RepoSetting{}
	has, err := db.GetEngine(ctx).Where("repo_id = ?", repoID).Get(rs)
	if err != nil {
		return nil, err
	} else if !has {
		return &RepoSetting{RepoID: repoID, Enabled: setting.Agents.EnableByDefault}, nil
	}
	return rs, nil
}

func (rs *RepoSetting) AllowsAgent(agentID int64) bool {
	return rs.Enabled && (len(rs.AllowedAgentIDs) == 0 || slices.Contains(rs.AllowedAgentIDs, agentID))
}

func SaveRepoSetting(ctx context.Context, rs *RepoSetting) error {
	if rs.ID == 0 {
		return db.Insert(ctx, rs)
	}
	_, err := db.GetEngine(ctx).ID(rs.ID).Cols("enabled", "allowed_agent_ids", "review_agent_id", "review_user_id", "review_everyone").Update(rs)
	return err
}
