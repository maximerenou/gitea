// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"testing"

	"gitea.dev/models/db"
	"gitea.dev/models/unittest"
	"gitea.dev/modules/agents"
	"gitea.dev/modules/util"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newConnection(t *testing.T, ownerID int64, name string) *Connection {
	c := &Connection{OwnerID: ownerID, Name: name, Provider: ProviderOllama, BaseURL: "http://ollama:11434", CLI: CLICodex}
	require.NoError(t, c.SetToken("s3cret"))
	require.NoError(t, CreateConnection(t.Context(), c))
	return c
}

func TestConnection(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := t.Context()

	c := newConnection(t, 0, "local")
	assert.True(t, c.HasToken())
	assert.NotContains(t, c.Token, "s3cret") // stored encrypted
	plain, err := c.DecryptToken()
	require.NoError(t, err)
	assert.Equal(t, "s3cret", plain)

	err = CreateConnection(ctx, &Connection{Name: "local", Provider: ProviderOllama, BaseURL: "x", CLI: CLICodex})
	assert.ErrorIs(t, err, util.ErrAlreadyExist)
	assert.ErrorIs(t, CreateConnection(ctx, &Connection{Name: "bad", Provider: "nope", BaseURL: "x", CLI: CLICodex}), util.ErrInvalidArgument)
	// a local connection cannot be shared
	assert.ErrorIs(t, CreateConnection(ctx, &Connection{OwnerID: 5, Name: "x", Provider: ProviderOllama, BaseURL: "x", CLI: CLICodex, AccessMode: ConnectionAccessAll}), util.ErrInvalidArgument)

	// updating without updateToken keeps the stored token (write-only)
	got, err := GetConnectionByID(ctx, c.ID)
	require.NoError(t, err)
	got.Token = ""
	got.BaseURL = "http://other:11434"
	require.NoError(t, UpdateConnection(ctx, got, false))
	got, err = GetConnectionByID(ctx, c.ID)
	require.NoError(t, err)
	assert.True(t, got.HasToken())
	assert.Equal(t, "http://other:11434", got.BaseURL)

	list, err := db_find(t, 0)
	require.NoError(t, err)
	assert.Len(t, list, 1)
}

func TestAgent(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := t.Context()
	c := newConnection(t, 0, "local")

	a := &Agent{BotUserID: 1001, Name: "reviewer", ConnectionID: c.ID, Model: "m", Action: agents.ActionReview, Events: []agents.Event{agents.EventAll}}
	require.NoError(t, CreateAgent(ctx, a))
	assert.Equal(t, AccessWriters, a.AccessMode)
	assert.True(t, a.IsGlobal())

	bad := *a
	bad.ID, bad.BotUserID, bad.Events = 0, 1002, []agents.Event{agents.EventReplied}
	assert.ErrorIs(t, CreateAgent(ctx, &bad), util.ErrInvalidArgument) // action/event pair refused
	bad.Events = nil
	assert.ErrorIs(t, CreateAgent(ctx, &bad), util.ErrInvalidArgument)
	bad.Events, bad.ConnectionID = []agents.Event{agents.EventAll}, 99999
	assert.ErrorIs(t, CreateAgent(ctx, &bad), util.ErrNotExist)

	// the action cannot be changed by an update
	a.Action = agents.ActionAnalyze
	a.Events = []agents.Event{agents.EventMentioned}
	a.Description = "changed"
	require.NoError(t, UpdateAgent(ctx, a))
	got, err := GetAgentByID(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, agents.ActionReview, got.Action)
	assert.Equal(t, "changed", got.Description)
	assert.Equal(t, []agents.Event{agents.EventMentioned}, got.Events)

	ok, err := IsAgentBot(ctx, 1001)
	require.NoError(t, err)
	assert.True(t, ok)
	ok, _ = IsAgentBot(ctx, 1)
	assert.False(t, ok)

	assert.ErrorIs(t, DeleteConnection(ctx, c.ID), util.ErrInvalidArgument) // still used
	require.NoError(t, DeleteAgent(ctx, a.ID))
	require.NoError(t, DeleteConnection(ctx, c.ID))
	_, err = GetAgentByID(ctx, a.ID)
	assert.ErrorIs(t, err, util.ErrNotExist)
}

func db_find(t *testing.T, ownerID int64) ([]*Connection, error) {
	return db.Find[Connection](t.Context(), FindConnectionsOptions{OwnerID: ownerID})
}
