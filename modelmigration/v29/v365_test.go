// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v29

import (
	"testing"

	"gitea.dev/modelmigration/migrationtest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAddAgentSecurityLimits(t *testing.T) {
	type ActionRun struct {
		ID int64 `xorm:"pk"`
	}
	type AgentRun struct {
		ID int64 `xorm:"pk"`
	}
	x, cleanup := migrationtest.PrepareTestEnv(t, 0, new(ActionRun), new(AgentRun))
	defer cleanup()
	if x == nil || t.Failed() {
		return
	}
	_, err := x.Insert(&ActionRun{ID: 1}, &AgentRun{ID: 1})
	require.NoError(t, err)
	require.NoError(t, AddAgentSecurityLimits(t.Context(), x))
	var action struct {
		AgentCodeTrust string
	}
	var agent struct {
		ModelRequests      int64
		ModelRequestActive bool
	}
	has, err := x.Table("action_run").Where("id = 1").Get(&action)
	require.NoError(t, err)
	require.True(t, has)
	assert.Empty(t, action.AgentCodeTrust)
	has, err = x.Table("agent_run").Where("id = 1").Get(&agent)
	require.NoError(t, err)
	require.True(t, has)
	assert.Zero(t, agent.ModelRequests)
	assert.False(t, agent.ModelRequestActive)
}
