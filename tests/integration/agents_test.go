// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package integration

import (
	"fmt"
	"net/http"
	"strconv"
	"testing"

	agents_model "gitea.dev/models/agents"
	auth_model "gitea.dev/models/auth"
	access_model "gitea.dev/models/perm/access"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/test"
	agents_service "gitea.dev/services/agents"
	"gitea.dev/tests"

	"github.com/stretchr/testify/require"
)

func TestAgentsAccess(t *testing.T) {
	defer tests.PrepareTestEnv(t)()
	defer test.MockVariableValue(&setting.Agents.Enabled, true)()
	defer test.MockVariableValue(&setting.Actions.Enabled, true)()
	defer test.MockVariableValue(&access_model.AgentDelegateUser, agents_service.DelegateUser)()
	ctx := t.Context()
	alice := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	bob := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 4})
	codex, err := agents_service.CreateAgent(ctx, agents_service.AgentOptions{BotName: "codex", Name: "Codex", Provider: agents_model.ProviderOpenAI, Model: "gpt-5-codex"})
	require.NoError(t, err)
	for _, u := range []*user_model.User{alice, bob} {
		_, err := agents_service.SetUserKey(ctx, codex, u, agents_service.KeyOptions{APIKey: "sk-" + u.Name})
		require.NoError(t, err)
	}
	newSession := func(repoID int64) *agents_model.Session {
		s := &agents_model.Session{RepoID: repoID, CreatorID: alice.ID, AgentID: codex.ID, Kind: agents_model.SessionKindTask, Title: "task", BaseBranch: "master", Status: agents_model.StatusSuccess}
		require.NoError(t, agents_model.InsertSession(ctx, s))
		return s
	}
	public := newSession(1)
	private := newSession(2)
	require.True(t, unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 2}).IsPrivate)
	bobSession := loginUser(t, "user4")

	t.Run("ReadersCannotStartSessions", func(t *testing.T) {
		req := NewRequestWithValues(t, "POST", "/user2/repo1/agents/new", map[string]string{"agent_id": strconv.FormatInt(codex.ID, 10), "prompt": "rewrite everything"})
		bobSession.MakeRequest(t, req, http.StatusBadRequest)
		unittest.AssertNotExistsBean(t, &agents_model.Session{CreatorID: bob.ID})
	})

	t.Run("SessionsNeedSignInAndWrite", func(t *testing.T) {
		resp := MakeRequest(t, NewRequest(t, "GET", "/user2/repo1/agents"), http.StatusSeeOther)
		require.Contains(t, resp.Header().Get("Location"), "/user/login")
		MakeRequest(t, NewRequest(t, "GET", fmt.Sprintf("/user2/repo1/agents/%d", public.ID)), http.StatusSeeOther)
		bobSession.MakeRequest(t, NewRequest(t, "GET", fmt.Sprintf("/user2/repo1/agents/%d", public.ID)), http.StatusNotFound)
	})

	t.Run("PrivateSessionsStayPrivate", func(t *testing.T) {
		for _, link := range []string{
			fmt.Sprintf("/user2/repo2/agents/%d", private.ID),
			fmt.Sprintf("/user2/repo2/agents/%d/events", private.ID),
			fmt.Sprintf("/user2/repo1/agents/%d", private.ID), // a session is only reachable through its own repository
		} {
			bobSession.MakeRequest(t, NewRequest(t, "GET", link), http.StatusNotFound)
		}
		req := NewRequestWithValues(t, "POST", fmt.Sprintf("/user2/repo2/agents/%d/instruct", private.ID), map[string]string{"prompt": "leak it"})
		bobSession.MakeRequest(t, req, http.StatusNotFound)

		token := getTokenForLoggedInUser(t, bobSession, auth_model.AccessTokenScopeReadRepository)
		MakeRequest(t, NewRequest(t, "GET", fmt.Sprintf("/api/v1/repos/user2/repo2/agents/sessions/%d", private.ID)).AddTokenAuth(token), http.StatusNotFound)
		MakeRequest(t, NewRequest(t, "GET", fmt.Sprintf("/api/v1/repos/user2/repo1/agents/sessions/%d", private.ID)).AddTokenAuth(token), http.StatusNotFound)
		unittest.AssertNotExistsBean(t, &agents_model.Run{SessionID: private.ID})
	})

	t.Run("KeysBelongToTheirUser", func(t *testing.T) {
		bobSession.MakeRequest(t, NewRequest(t, "POST", fmt.Sprintf("/user/settings/agents/%d/delete", codex.ID)), http.StatusOK)
		_, err := agents_model.GetUserKey(ctx, codex.ID, bob.ID)
		require.Error(t, err)
		_, err = agents_model.GetUserKey(ctx, codex.ID, alice.ID)
		require.NoError(t, err, "deleting a key never touches another user's")

		req := NewRequestWithValues(t, "POST", fmt.Sprintf("/user2/repo1/agents/%d/instruct", public.ID), map[string]string{"prompt": "go on"})
		bobSession.MakeRequest(t, req, http.StatusNotFound)
		unittest.AssertNotExistsBean(t, &agents_model.Run{SessionID: public.ID}) // never on the creator's key

		token := getTokenForLoggedInUser(t, bobSession, auth_model.AccessTokenScopeWriteRepository)
		req = NewRequestWithJSON(t, "POST", "/api/v1/repos/user2/repo1/pulls/3/agent-review", map[string]string{"agent": "codex"}).AddTokenAuth(token)
		MakeRequest(t, req, http.StatusBadRequest)
		unittest.AssertNotExistsBean(t, &agents_model.Session{AgentID: codex.ID, IssueID: 3})
	})

	t.Run("PagesRender", func(t *testing.T) {
		aliceSession := loginUser(t, "user2")
		for _, link := range []string{"/user2/repo1/agents?status=success&kind=task&mine=1", "/user/settings/agents", "/user2/repo1/settings/agents", "/user2/repo1/pulls/3"} {
			aliceSession.MakeRequest(t, NewRequest(t, "GET", link), http.StatusOK)
		}
	})
}
