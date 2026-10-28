// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package integration

import (
	"net/http"
	"strconv"
	"testing"

	agents_model "gitea.dev/models/agents"
	auth_model "gitea.dev/models/auth"
	"gitea.dev/models/db"
	depscan_model "gitea.dev/models/depscan"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/setting"
	api "gitea.dev/modules/structs"
	"gitea.dev/modules/test"
	agents_service "gitea.dev/services/agents"
	"gitea.dev/tests"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAPIDependencyScanning(t *testing.T) {
	defer tests.PrepareTestEnv(t)()
	defer test.MockVariableValue(&setting.DependencyScanning.Enabled, true)()
	defer test.MockVariableValue(&setting.Agents.Enabled, true)()
	defer test.MockVariableValue(&setting.Actions.Enabled, true)()
	ctx := t.Context()
	require.NoError(t, depscan_model.SaveRepoSetting(ctx, &depscan_model.RepoSetting{RepoID: 1, Enabled: true}))
	alert := &depscan_model.Alert{RepoID: 1, VulnID: "GHSA-1", Ecosystem: "npm", Package: "lodash", Manifest: "package-lock.json", Version: "4.17.15", State: depscan_model.AlertOpen, Severity: depscan_model.SeverityHigh}
	require.NoError(t, db.Insert(ctx, alert))
	require.NoError(t, depscan_model.ReplaceDependencies(ctx, 1, []*depscan_model.Dependency{{RepoID: 1, Manifest: "package-lock.json", Ecosystem: "npm", Name: "Lodash", Version: "4.17.15"}}))

	writer := getTokenForLoggedInUser(t, loginUser(t, "user40"), auth_model.AccessTokenScopeWriteRepository) // writes the code of user2/repo1
	admin := getTokenForLoggedInUser(t, loginUser(t, "user2"), auth_model.AccessTokenScopeWriteRepository)

	t.Run("Dependencies", func(t *testing.T) {
		resp := MakeRequest(t, NewRequest(t, "GET", "/api/v1/repos/user2/repo1/dependencies?q=lodash").AddTokenAuth(writer), http.StatusOK)
		deps := DecodeJSON(t, resp, []*api.Dependency{})
		require.Len(t, deps, 1)
		assert.Equal(t, api.Dependency{Ecosystem: "npm", Name: "Lodash", Version: "4.17.15", Manifest: "package-lock.json"}, *deps[0])
	})

	t.Run("DismissAndReopen", func(t *testing.T) {
		link := "/api/v1/repos/user2/repo1/dependency-alerts/" + strconv.FormatInt(alert.ID, 10)
		MakeRequest(t, NewRequestWithJSON(t, "PATCH", link, &api.EditDependencyAlertOption{State: "dismissed"}).AddTokenAuth(writer), http.StatusBadRequest)
		resp := MakeRequest(t, NewRequestWithJSON(t, "PATCH", link, &api.EditDependencyAlertOption{State: "dismissed", Reason: "not reachable"}).AddTokenAuth(writer), http.StatusOK)
		assert.Equal(t, "dismissed", DecodeJSON(t, resp, &api.DependencyAlert{}).State)
		assert.Equal(t, "not reachable", unittest.AssertExistsAndLoadBean(t, &depscan_model.Alert{ID: alert.ID}).DismissedReason)
		resp = MakeRequest(t, NewRequestWithJSON(t, "PATCH", link, &api.EditDependencyAlertOption{State: "open"}).AddTokenAuth(writer), http.StatusOK)
		assert.Equal(t, "open", DecodeJSON(t, resp, &api.DependencyAlert{}).State)
	})

	t.Run("AlertsOfUnscannedRepositoriesAreStale", func(t *testing.T) {
		resp := MakeRequest(t, NewRequest(t, "GET", "/api/v1/repos/user2/repo1/dependency-alerts").AddTokenAuth(writer), http.StatusOK)
		assert.Len(t, DecodeJSON(t, resp, []*api.DependencyAlert{}), 1)
		_, err := db.GetEngine(ctx).Where("repo_id = ?", 1).Cols("enabled").Update(&depscan_model.RepoSetting{Enabled: false})
		require.NoError(t, err)
		defer func() {
			_, err := db.GetEngine(ctx).Where("repo_id = ?", 1).Cols("enabled").Update(&depscan_model.RepoSetting{Enabled: true})
			require.NoError(t, err)
		}()
		resp = MakeRequest(t, NewRequest(t, "GET", "/api/v1/repos/user2/repo1/dependency-alerts").AddTokenAuth(writer), http.StatusOK)
		assert.Empty(t, DecodeJSON(t, resp, []*api.DependencyAlert{}))
	})

	t.Run("UpdatesNeedAnAdministrator", func(t *testing.T) {
		MakeRequest(t, NewRequest(t, "GET", "/api/v1/repos/user2/repo1/dependency-updates").AddTokenAuth(writer), http.StatusForbidden)
		MakeRequest(t, NewRequest(t, "POST", "/api/v1/repos/user2/repo1/dependency-updates/run").AddTokenAuth(writer), http.StatusForbidden)

		codex, err := agents_service.CreateAgent(ctx, agents_service.AgentOptions{BotName: "codex", Name: "Codex", Provider: agents_model.ProviderOpenAI, Model: "m"})
		require.NoError(t, err)
		agent, scope := "codex", "security"
		edit := NewRequestWithJSON(t, "PUT", "/api/v1/repos/user2/repo1/dependency-updates", &api.DependencyUpdatesOption{Agent: &agent, Scope: &scope, Ignore: []string{"react@no-major"}})
		MakeRequest(t, edit.AddTokenAuth(admin), http.StatusUnprocessableEntity) // the updates run with the key of who sets them up
		_, err = agents_service.SetUserKey(ctx, codex, unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2}), agents_service.KeyOptions{APIKey: "sk-user2"})
		require.NoError(t, err)
		edit = NewRequestWithJSON(t, "PUT", "/api/v1/repos/user2/repo1/dependency-updates", &api.DependencyUpdatesOption{Agent: &agent, Scope: &scope, Ignore: []string{"react@no-major"}})
		MakeRequest(t, edit.AddTokenAuth(admin), http.StatusNoContent)

		resp := MakeRequest(t, NewRequest(t, "GET", "/api/v1/repos/user2/repo1/dependency-updates").AddTokenAuth(admin), http.StatusOK)
		updates := DecodeJSON(t, resp, &api.DependencyUpdates{})
		assert.Equal(t, "codex", updates.Agent)
		assert.Equal(t, "user2", updates.Updater)
		assert.Equal(t, "weekly", updates.Interval)
		assert.Equal(t, "security", updates.Scope)
		assert.Equal(t, []string{"react@no-major"}, updates.Ignore)
	})

	t.Run("SettingsPage", func(t *testing.T) {
		refused := &depscan_model.RepoSetting{UpdatesRefused: []*depscan_model.RefusedVersion{{Ecosystem: "npm", Package: "lodash", Version: "5.0.0"}}, UpdatesLastError: "no key"}
		_, err := db.GetEngine(ctx).Where("repo_id = ?", 1).Cols("updates_refused", "updates_last_error").Update(refused)
		require.NoError(t, err)
		resp := loginUser(t, "user2").MakeRequest(t, NewRequest(t, "GET", "/user2/repo1/settings/dependency-scanning"), http.StatusOK)
		doc := NewHTMLParser(t, resp.Body)
		assert.Equal(t, "lodash 5.0.0", doc.Find(".item code").Text())
		assert.Equal(t, "security", doc.Find("#updates_scope option[selected]").AttrOr("value", ""))
		assert.Contains(t, doc.Find(".error.message").Text(), "no key")

		loginUser(t, "user2").MakeRequest(t, NewRequest(t, "GET", "/org3/-/security"), http.StatusOK) // owners list the alerts with a subquery
	})
}
