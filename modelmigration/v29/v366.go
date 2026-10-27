// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v29

import (
	"context"

	"gitea.dev/modelmigration/base"
	"gitea.dev/modules/timeutil"
)

func AddAgentSessionToken(_ context.Context, x base.EngineMigration) error {
	type AgentSessionToken struct {
		ID            int64              `xorm:"pk autoincr"`
		TokenHash     string             `xorm:"VARCHAR(64) UNIQUE NOT NULL"`
		BotUserID     int64              `xorm:"INDEX NOT NULL"`
		TriggerUserID int64              `xorm:"NOT NULL"`
		SessionID     int64              `xorm:"INDEX NOT NULL"`
		ActionRunID   int64              `xorm:"INDEX NOT NULL"`
		Scope         string             `xorm:"TEXT NOT NULL"`
		ExpiredUnix   timeutil.TimeStamp `xorm:"NOT NULL"`
		RevokedUnix   timeutil.TimeStamp `xorm:"NOT NULL DEFAULT 0"`
		CreatedUnix   timeutil.TimeStamp `xorm:"created NOT NULL"`
	}

	return x.Sync(&AgentSessionToken{})
}
