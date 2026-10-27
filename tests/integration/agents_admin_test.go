// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package integration

import (
	"net/http"
	"net/url"
	"strconv"
	"testing"

	agents_model "gitea.dev/models/agents"
	"gitea.dev/models/db"
	repo_model "gitea.dev/models/repo"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/agents"
	"gitea.dev/modules/test"
	"gitea.dev/tests"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func agentsSettingsForm(enabled bool) map[string]string {
	s := agents.DefaultSettings()
	form := map[string]string{
		"runner_label": s.RunnerLabel, "job_image": s.JobImage, "checkout_action": s.CheckoutAction,
		"runs_per_issue_per_hour": "200", "max_agent_round_trips": "20", "auto_retries": "0", "failure_memory_hours": "1",
		"queue_ttl_hours": "24", "ci_repairs_per_commit": "3", "blocked_reminder_hours": "168", "blocked_expiry_hours": "0",
	}
	if enabled {
		form["enabled"] = "on"
	}
	return form
}

func TestAgentsDisabledByDefault(t *testing.T) {
	defer tests.PrepareTestEnv(t)()
	admin := loginUser(t, "user1")

	assert.False(t, agents.IsEnabled(t.Context()))
	// General stays reachable so the feature can be switched on, everything else is hidden
	resp := admin.MakeRequest(t, NewRequest(t, "GET", "/-/admin/agents"), http.StatusOK)
	assert.True(t, test.IsNormalPageCompleted(resp.Body.String()))
	for _, link := range []string{"/connections", "/connections/new", "/list", "/list/new", "/list/1", "/connections/1"} {
		admin.MakeRequest(t, NewRequest(t, "GET", "/-/admin/agents"+link), http.StatusNotFound)
	}
	admin.MakeRequest(t, NewRequestWithValues(t, "POST", "/-/admin/agents/list/1/delete", nil), http.StatusNotFound)

	// a regular user never reaches any of the pages
	user := loginUser(t, "user2")
	user.MakeRequest(t, NewRequest(t, "GET", "/-/admin/agents"), http.StatusForbidden)
}

func TestAgentsAdminSettings(t *testing.T) {
	defer tests.PrepareTestEnv(t)()
	admin := loginUser(t, "user1")

	bad := agentsSettingsForm(true)
	bad["blocked_expiry_hours"] = "5" // neither 0 nor within 24-8760
	admin.MakeRequest(t, NewRequestWithValues(t, "POST", "/-/admin/agents", bad), http.StatusUnprocessableEntity)
	assert.False(t, agents.IsEnabled(t.Context()))

	admin.MakeRequest(t, NewRequestWithValues(t, "POST", "/-/admin/agents", agentsSettingsForm(true)), http.StatusSeeOther)
	assert.True(t, agents.IsEnabled(t.Context()))
	admin.MakeRequest(t, NewRequest(t, "GET", "/-/admin/agents/connections"), http.StatusOK)
	admin.MakeRequest(t, NewRequest(t, "GET", "/-/admin/agents/list"), http.StatusOK)

	admin.MakeRequest(t, NewRequestWithValues(t, "POST", "/-/admin/agents", agentsSettingsForm(false)), http.StatusSeeOther)
	assert.False(t, agents.IsEnabled(t.Context()))
	admin.MakeRequest(t, NewRequest(t, "GET", "/-/admin/agents/list"), http.StatusNotFound)
}

func TestAgentsAdminAgentLifecycle(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, _ *url.URL) { // the git hooks of the agent repository need the internal server
		admin := loginUser(t, "user1")
		admin.MakeRequest(t, NewRequestWithValues(t, "POST", "/-/admin/agents", agentsSettingsForm(true)), http.StatusSeeOther)

		admin.MakeRequest(t, NewRequestWithValues(t, "POST", "/-/admin/agents/connections/new", map[string]string{
			"name": "local", "provider": "ollama", "base_url": "http://ollama:11434", "token": "s3cret", "cli": "codex", "access_mode": "none",
		}), http.StatusSeeOther)
		conns, err := db.Find[agents_model.Connection](t.Context(), agents_model.FindConnectionsOptions{ListOptions: db.ListOptionsAll})
		require.NoError(t, err)
		require.Len(t, conns, 1)
		assert.NotContains(t, conns[0].Token, "s3cret")
		resp := admin.MakeRequest(t, NewRequest(t, "GET", "/-/admin/agents/connections/"+idStr(conns[0].ID)), http.StatusOK)
		assert.NotContains(t, resp.Body.String(), "s3cret") // the token is write-only

		agentForm := map[string]string{
			"name": "reviewer-bot", "action": "review", "connection_id": idStr(conns[0].ID), "model": "llama3", "events": "all",
		}
		// an action/event pair the action does not accept is refused and creates nothing
		invalid := map[string]string{"name": "bad-bot", "action": "review", "connection_id": idStr(conns[0].ID), "model": "m", "events": "replied"}
		admin.MakeRequest(t, NewRequestWithValues(t, "POST", "/-/admin/agents/list/new", invalid), http.StatusUnprocessableEntity)
		unittestNoUser(t, "bad-bot")

		admin.MakeRequest(t, NewRequestWithValues(t, "POST", "/-/admin/agents/list/new", agentForm), http.StatusSeeOther)
		bot, err := user_model.GetUserByName(t.Context(), "reviewer-bot")
		require.NoError(t, err)
		assert.True(t, bot.IsTypeBot())
		assert.True(t, bot.KeepEmailPrivate)
		assert.False(t, bot.AllowCreateOrganization)

		repo, err := repo_model.GetRepositoryByName(t.Context(), bot.ID, ".agent")
		require.NoError(t, err)
		assert.True(t, repo.IsPrivate)
		assert.False(t, repo.IsEmpty)
		for _, f := range []string{"system-prompt.md", "settings.yml", "mcp.json"} {
			resp := MakeRequest(t, NewRequest(t, "GET", "/api/v1/repos/reviewer-bot/.agent/raw/"+f).AddTokenAuth(getUserToken(t, "user1", "read:repository")), http.StatusOK)
			assert.NotEmpty(t, resp.Body.String(), f)
		}

		agent, err := agents_model.GetAgentByBotUserID(t.Context(), bot.ID)
		require.NoError(t, err)
		assert.Equal(t, agents.ActionReview, agent.Action)

		// a name that is already a user is refused and leaves no extra bot behind
		dup := map[string]string{"name": "user2", "action": "review", "connection_id": idStr(conns[0].ID), "model": "m", "events": "all"}
		admin.MakeRequest(t, NewRequestWithValues(t, "POST", "/-/admin/agents/list/new", dup), http.StatusUnprocessableEntity)

		// the connection cannot be removed while in use
		admin.MakeRequest(t, NewRequestWithValues(t, "POST", "/-/admin/agents/connections/"+idStr(conns[0].ID)+"/delete", nil), http.StatusOK)
		_, err = agents_model.GetConnectionByID(t.Context(), conns[0].ID)
		require.NoError(t, err)

		admin.MakeRequest(t, NewRequestWithValues(t, "POST", "/-/admin/agents/list/"+idStr(agent.ID)+"/delete", nil), http.StatusOK)
		unittestNoUser(t, "reviewer-bot")
		_, err = agents_model.GetAgentByID(t.Context(), agent.ID)
		assert.ErrorIs(t, err, agents_model.ErrAgentNotFound)
		_, err = repo_model.GetRepositoryByID(t.Context(), repo.ID)
		assert.Error(t, err) // the configuration repository went with the bot
	})
}

func unittestNoUser(t *testing.T, name string) {
	_, err := user_model.GetUserByName(t.Context(), name)
	assert.True(t, user_model.IsErrUserNotExist(err), name)
}

func idStr(id int64) string {
	return strconv.FormatInt(id, 10)
}
