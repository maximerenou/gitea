// Copyright 2025 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package user

import (
	"testing"

	"gitea.dev/models/unittest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSystemUser(t *testing.T) {
	uid, u, err := GetPossibleUserByID(t.Context(), -1)
	require.NoError(t, err)
	assert.Equal(t, int64(-1), uid)
	assert.Equal(t, "Ghost", u.Name)
	assert.Equal(t, "ghost", u.LowerName)
	assert.True(t, u.IsGhost())

	u = GetSystemUserByName("gHost")
	require.NotNil(t, u)
	assert.Equal(t, "Ghost", u.Name)

	uid, u, err = GetPossibleUserByID(t.Context(), -2)
	require.NoError(t, err)
	assert.Equal(t, int64(-2), uid)
	assert.Equal(t, "gitea-actions", u.Name)
	assert.Equal(t, "gitea-actions", u.LowerName)

	u = GetSystemUserByName("Gitea-actionS")
	require.NotNil(t, u)
	assert.Equal(t, "Gitea Actions", u.FullName)

	uid, u, err = GetPossibleUserByID(t.Context(), 999999)
	require.NoError(t, err)
	assert.Equal(t, int64(-1), uid)
	assert.Equal(t, "Ghost", u.Name)
}

func TestGetDoerPermissionUserCarriesToken(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())

	u, err := GetDoerPermissionUser(t.Context(), 2, NewTokenExtDoerData(CredentialAccessToken, 9, "public-only,read:repository").EncodeToString())
	require.NoError(t, err)
	assert.True(t, IsPublicOnlyDoer(u))
	assert.Equal(t, "access-token:9", GetDoerCredential(u))

	_, err = GetDoerPermissionUser(t.Context(), 2, "access-token:9|bogus")
	assert.Error(t, err)
}

func TestGetDoerPermissionUserCarriesSessionToken(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())

	encoded := NewSessionTokenExtDoerData(7, 3, "write:repository,write:issue").EncodeToString()
	u, err := GetDoerPermissionUser(t.Context(), 2, encoded)
	require.NoError(t, err)
	sessionID, triggerUserID, ok := GetDoerSessionToken(u)
	assert.True(t, ok)
	assert.Equal(t, int64(7), sessionID)
	assert.Equal(t, int64(3), triggerUserID)
	assert.Equal(t, "agent-session:7", GetDoerCredential(u))
	scope, hasScope := GetDoerTokenScope(u) // scope enforcement must stay active for session tokens
	assert.True(t, hasScope)
	assert.EqualValues(t, "write:repository,write:issue", scope)

	// a plain access token is never a session token, and keeps its old two-part encoding
	plain := NewTokenExtDoerData(CredentialAccessToken, 9, "read:repository")
	assert.Equal(t, "access-token:9|read:repository", plain.EncodeToString())
	u, err = GetDoerPermissionUser(t.Context(), 2, plain.EncodeToString())
	require.NoError(t, err)
	_, _, ok = GetDoerSessionToken(u)
	assert.False(t, ok)

	for _, bad := range []string{"agent-session:7|read:repository|7", "agent-session:7|read:repository|0|3", "agent-session:7|read:repository|x|3", "a|b|c|d|e"} {
		_, err = GetDoerPermissionUser(t.Context(), 2, bad)
		assert.Error(t, err, bad)
	}
}
