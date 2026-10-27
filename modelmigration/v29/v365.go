// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v29

import (
	"context"

	"gitea.dev/modelmigration/base"
	"gitea.dev/modules/timeutil"
)

func AddAgentTables(_ context.Context, x base.EngineMigration) error {
	type AgentConnection struct {
		ID          int64              `xorm:"pk autoincr"`
		OwnerID     int64              `xorm:"UNIQUE(owner_name) NOT NULL DEFAULT 0"`
		Name        string             `xorm:"UNIQUE(owner_name) VARCHAR(255) NOT NULL"`
		Provider    string             `xorm:"VARCHAR(32) NOT NULL"`
		BaseURL     string             `xorm:"VARCHAR(2048) NOT NULL"`
		Token       string             `xorm:"TEXT"`
		CLI         string             `xorm:"VARCHAR(32) NOT NULL"`
		AccessMode  string             `xorm:"VARCHAR(16) NOT NULL DEFAULT 'none'"`
		AccessList  string             `xorm:"JSON TEXT"`
		CreatedUnix timeutil.TimeStamp `xorm:"created NOT NULL"`
		UpdatedUnix timeutil.TimeStamp `xorm:"updated NOT NULL"`
	}

	type Agent struct {
		ID             int64              `xorm:"pk autoincr"`
		OwnerID        int64              `xorm:"INDEX NOT NULL DEFAULT 0"`
		BotUserID      int64              `xorm:"UNIQUE NOT NULL"`
		Name           string             `xorm:"VARCHAR(40) NOT NULL"`
		ConnectionID   int64              `xorm:"INDEX NOT NULL"`
		Model          string             `xorm:"VARCHAR(255) NOT NULL"`
		Description    string             `xorm:"TEXT"`
		Language       string             `xorm:"VARCHAR(32) NOT NULL DEFAULT ''"`
		Action         string             `xorm:"VARCHAR(32) NOT NULL"`
		Events         string             `xorm:"JSON TEXT"`
		ActionSettings string             `xorm:"JSON TEXT"`
		AccessMode     string             `xorm:"VARCHAR(16) NOT NULL DEFAULT 'writers'"`
		AccessList     string             `xorm:"JSON TEXT"`
		CreatedUnix    timeutil.TimeStamp `xorm:"created NOT NULL"`
		UpdatedUnix    timeutil.TimeStamp `xorm:"updated NOT NULL"`
	}

	return x.Sync(&AgentConnection{}, &Agent{})
}
