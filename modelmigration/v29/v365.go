// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v29

import (
	"context"

	"gitea.dev/modelmigration/base"

	"xorm.io/xorm"
)

func AddAgentSecurityLimits(_ context.Context, x base.EngineMigration) error {
	type ActionRun struct {
		AgentCodeTrust string `xorm:"VARCHAR(16) NOT NULL DEFAULT ''"`
	}
	type AgentRun struct {
		ModelRequests      int64 `xorm:"NOT NULL DEFAULT 0"`
		ModelRequestActive bool  `xorm:"NOT NULL DEFAULT false"`
	}
	_, err := x.SyncWithOptions(xorm.SyncOptions{IgnoreConstrains: true, IgnoreDropIndices: true}, new(ActionRun), new(AgentRun))
	return err
}
