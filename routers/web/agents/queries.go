// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"context"

	agents_model "gitea.dev/models/agents"
	"gitea.dev/models/db"
)

func findConnections(ctx context.Context) ([]*agents_model.Connection, error) {
	return db.Find[agents_model.Connection](ctx, agents_model.FindConnectionsOptions{ListOptions: db.ListOptionsAll})
}

func findAgents(ctx context.Context) ([]*agents_model.Agent, error) {
	return db.Find[agents_model.Agent](ctx, agents_model.FindAgentsOptions{ListOptions: db.ListOptionsAll, OnlyGlobal: true})
}
