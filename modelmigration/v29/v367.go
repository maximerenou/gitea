// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v29

import (
	"context"

	"gitea.dev/modelmigration/base"
	"gitea.dev/modules/timeutil"
)

func AddAgentSessionAndRun(_ context.Context, x base.EngineMigration) error {
	type AgentSession struct {
		ID             int64              `xorm:"pk autoincr"`
		AgentID        int64              `xorm:"INDEX NOT NULL"`
		RepoID         int64              `xorm:"INDEX NOT NULL"`
		IssueID        int64              `xorm:"INDEX NOT NULL DEFAULT 0"`
		TriggerUserID  int64              `xorm:"NOT NULL DEFAULT 0"`
		ConversationID string             `xorm:"VARCHAR(255) NOT NULL DEFAULT ''"`
		ChainDepth     int                `xorm:"NOT NULL DEFAULT 0"`
		CreatedUnix    timeutil.TimeStamp `xorm:"created NOT NULL"`
		UpdatedUnix    timeutil.TimeStamp `xorm:"updated NOT NULL"`
	}

	type AgentRun struct {
		ID               int64              `xorm:"pk autoincr"`
		SessionID        int64              `xorm:"INDEX NOT NULL"`
		AgentID          int64              `xorm:"INDEX NOT NULL"`
		RepoID           int64              `xorm:"INDEX NOT NULL"`
		IssueID          int64              `xorm:"INDEX NOT NULL DEFAULT 0"`
		ActionRunID      int64              `xorm:"INDEX NOT NULL DEFAULT 0"`
		Status           string             `xorm:"VARCHAR(16) INDEX NOT NULL"`
		TriggerEvent     string             `xorm:"VARCHAR(32) NOT NULL DEFAULT ''"`
		TriggerUserID    int64              `xorm:"NOT NULL DEFAULT 0"`
		TriggerCommentID int64              `xorm:"NOT NULL DEFAULT 0"`
		ConfigCommitSHA  string             `xorm:"VARCHAR(64) NOT NULL DEFAULT ''"`
		Summary          string             `xorm:"TEXT"`
		Error            string             `xorm:"TEXT"`
		Result           string             `xorm:"LONGTEXT"`
		Notes            string             `xorm:"TEXT"`
		Progress         string             `xorm:"LONGTEXT"`
		CreatedUnix      timeutil.TimeStamp `xorm:"created NOT NULL"`
		UpdatedUnix      timeutil.TimeStamp `xorm:"updated NOT NULL"`
		EndedUnix        timeutil.TimeStamp `xorm:"NOT NULL DEFAULT 0"`
	}

	return x.Sync(&AgentSession{}, &AgentRun{})
}
