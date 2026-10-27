// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package access

import (
	"testing"

	"gitea.dev/models/db"
	perm_model "gitea.dev/models/perm"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unit"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/optional"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newSessionBot(t *testing.T, name string) *user_model.User {
	bot := &user_model.User{Name: name, Email: name + "@example.com", Type: user_model.UserTypeBot}
	require.NoError(t, user_model.CreateUser(t.Context(), bot, &user_model.Meta{}, &user_model.CreateUserOverwriteOptions{IsActive: optional.Some(true)}))
	return bot
}

func addCollaborator(t *testing.T, repo *repo_model.Repository, u *user_model.User, mode perm_model.AccessMode) {
	require.NoError(t, db.Insert(t.Context(), &repo_model.Collaboration{RepoID: repo.ID, UserID: u.ID, Mode: mode}))
	require.NoError(t, RecalculateUserAccess(t.Context(), repo, u.ID))
}

func sessionPerm(t *testing.T, repo *repo_model.Repository, bot *user_model.User, triggerUserID int64) Permission {
	doer := *bot
	doer.ExtDoerData = user_model.NewSessionTokenExtDoerData(1, triggerUserID, "write:repository,write:issue")
	p, err := GetDoerRepoPermission(t.Context(), repo, &doer)
	require.NoError(t, err)
	return p
}

func TestAgentSessionRepoPermission(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := t.Context()
	bot := newSessionBot(t, "sessbot")
	repoPublic := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})  // public, owner user2
	repoPrivate := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 2}) // private, owner user2
	user2 := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	user5 := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 5})
	addCollaborator(t, repoPublic, bot, perm_model.AccessModeWrite)
	addCollaborator(t, repoPrivate, bot, perm_model.AccessModeWrite)

	// preconditions of the scenario: user5 is a stranger who can only read the public repository
	p, err := GetIndividualUserRepoPermission(ctx, repoPrivate, user5)
	require.NoError(t, err)
	require.False(t, p.HasAnyUnitAccessOrPublicAccess())
	p, err = GetIndividualUserRepoPermission(ctx, repoPublic, user5)
	require.NoError(t, err)
	require.False(t, p.CanWrite(unit.TypeCode))

	t.Run("BotAndTriggerBothWrite", func(t *testing.T) {
		p := sessionPerm(t, repoPrivate, bot, user2.ID)
		assert.True(t, p.CanWrite(unit.TypeCode))
		assert.True(t, p.CanWrite(unit.TypeIssues))
		// the trigger owns the repository, the session must not inherit owner or admin mode
		assert.False(t, p.IsOwner())
		assert.False(t, p.IsAdmin())
	})

	t.Run("TriggerCanOnlyRead", func(t *testing.T) {
		p := sessionPerm(t, repoPublic, bot, user5.ID)
		assert.True(t, p.CanRead(unit.TypeCode))
		assert.False(t, p.CanWrite(unit.TypeCode)) // the bot may write, the person may not
		assert.False(t, p.CanWrite(unit.TypeIssues))
	})

	t.Run("TriggerCannotSeeRepository", func(t *testing.T) {
		p := sessionPerm(t, repoPrivate, bot, user5.ID)
		assert.False(t, p.HasAnyUnitAccessOrPublicAccess())
		assert.False(t, p.CanRead(unit.TypeCode))
	})

	t.Run("BotCannotAccessRepository", func(t *testing.T) {
		repo3 := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 3}) // private, owner user3, the bot is not a collaborator
		user3 := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 3})
		p := sessionPerm(t, repo3, bot, user3.ID) // the owner cannot lend the bot access it does not have
		assert.False(t, p.HasAnyUnitAccessOrPublicAccess())
	})

	t.Run("NoUsableTrigger", func(t *testing.T) {
		for _, id := range []int64{0, -1, 999999} {
			p := sessionPerm(t, repoPrivate, bot, id)
			assert.False(t, p.HasAnyUnitAccessOrPublicAccess(), id)
			assert.False(t, p.CanWrite(unit.TypeCode), id)
		}
		require.NoError(t, user_model.UpdateUserCols(ctx, &user_model.User{ID: user2.ID, IsActive: false}, "is_active"))
		p := sessionPerm(t, repoPrivate, bot, user2.ID)
		assert.False(t, p.HasAnyUnitAccessOrPublicAccess())
	})
}

func TestAgentSessionConfigRepoIsReadOnly(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := t.Context()
	bot := newSessionBot(t, "cfgbot")
	user2 := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})

	repo := &repo_model.Repository{OwnerID: bot.ID, OwnerName: bot.Name, Name: ".agent", LowerName: ".agent", IsPrivate: true}
	require.NoError(t, db.Insert(ctx, repo))
	require.NoError(t, db.Insert(ctx, &repo_model.RepoUnit{RepoID: repo.ID, Type: unit.TypeCode}))
	require.NoError(t, db.Insert(ctx, &repo_model.RepoUnit{RepoID: repo.ID, Type: unit.TypeIssues}))
	addCollaborator(t, repo, user2, perm_model.AccessModeWrite) // the person may write, the bot owns it

	p, err := GetIndividualUserRepoPermission(ctx, repo, bot)
	require.NoError(t, err)
	require.True(t, p.IsOwner()) // as a plain user the bot owns its configuration repository

	p = sessionPerm(t, repo, bot, user2.ID)
	assert.True(t, p.CanRead(unit.TypeCode))
	assert.False(t, p.CanWrite(unit.TypeCode))
	assert.False(t, p.CanWrite(unit.TypeIssues))
	assert.False(t, p.IsOwner())

	// another repository of the same bot is not affected by the cap
	other := &repo_model.Repository{OwnerID: bot.ID, OwnerName: bot.Name, Name: "work", LowerName: "work", IsPrivate: true}
	require.NoError(t, db.Insert(ctx, other))
	require.NoError(t, db.Insert(ctx, &repo_model.RepoUnit{RepoID: other.ID, Type: unit.TypeCode}))
	addCollaborator(t, other, user2, perm_model.AccessModeWrite)
	otherPerm := sessionPerm(t, other, bot, user2.ID)
	assert.True(t, otherPerm.CanWrite(unit.TypeCode))
}
