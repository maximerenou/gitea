// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package integration

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"

	actions_model "gitea.dev/models/actions"
	agents_model "gitea.dev/models/agents"
	auth_model "gitea.dev/models/auth"
	"gitea.dev/models/db"
	"gitea.dev/models/perm"
	repo_model "gitea.dev/models/repo"
	system_model "gitea.dev/models/system"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/agents"
	"gitea.dev/modules/git/gitcmd"
	"gitea.dev/modules/json"
	"gitea.dev/modules/optional"
	"gitea.dev/modules/setting/config"
	api "gitea.dev/modules/structs"

	agents_service "gitea.dev/services/agents"
	repo_service "gitea.dev/services/repository"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setAgentsEnabled(t testing.TB, enabled bool) {
	s := agents.DefaultSettings()
	s.Enabled = enabled
	v, err := json.Marshal(s)
	require.NoError(t, err)
	require.NoError(t, system_model.SetSettings(t.Context(), map[string]string{agents.SettingsKey(): string(v)}))
	config.GetDynGetter().InvalidateCache()
}

type agentSessionFixture struct {
	bot   *user_model.User
	agent *agents_model.Agent
	run   *actions_model.ActionRun
}

// newAgentFixture enables agents and creates an agent whose bot can write to user2/repo1 and the private user2/repo2.
func newAgentFixture(t *testing.T) *agentSessionFixture {
	ctx := t.Context()
	setAgentsEnabled(t, true)
	conn := &agents_model.Connection{Name: "sec-conn", Provider: agents_model.ProviderOllama, BaseURL: "http://ollama:11434", CLI: agents_model.CLICodex}
	require.NoError(t, conn.SetToken("model-provider-secret"))
	require.NoError(t, agents_model.CreateConnection(ctx, conn))
	agent, err := agents_service.CreateAgent(ctx, agents_service.CreateAgentOptions{
		Name: "sec-bot", ConnectionID: conn.ID, Model: "m", Action: agents.ActionReview, Events: []agents.Event{agents.EventAll},
	})
	require.NoError(t, err)
	bot, err := user_model.GetUserByID(ctx, agent.BotUserID)
	require.NoError(t, err)
	for _, id := range []int64{1, 2} {
		repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: id})
		require.NoError(t, repo_service.AddOrUpdateCollaborator(ctx, repo, bot, perm.AccessModeWrite))
	}
	run := &actions_model.ActionRun{RepoID: 1, OwnerID: 2, Index: 9100, WorkflowID: "agent.yml", Status: actions_model.StatusRunning, TriggerUserID: 2}
	require.NoError(t, db.Insert(ctx, run))
	return &agentSessionFixture{bot: bot, agent: agent, run: run}
}

func (f *agentSessionFixture) token(t testing.TB, triggerUserID, sessionID int64) string {
	token, _, err := agents_model.CreateSessionToken(t.Context(), agents_model.CreateSessionTokenOptions{
		BotUserID: f.bot.ID, TriggerUserID: triggerUserID, SessionID: sessionID, ActionRunID: f.run.ID,
	})
	require.NoError(t, err)
	return token
}

func TestAgentSessionTokenAPI(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, _ *url.URL) { // the git hooks of the agent repository need the internal server
		f := newAgentFixture(t)
		issueURL := "/api/v1/repos/user2/repo1/issues"
		repoURL := "/api/v1/repos/user2/repo1"
		newIssue := &api.CreateIssueOption{Title: "from an agent", Body: "body"}

		// creating a label needs write access, unlike opening an issue which any reader may do on a public repository
		label := &api.CreateLabelOption{Name: "from-agent", Color: "#00aabb"}
		labelURL := "/api/v1/repos/user2/repo1/labels"

		t.Run("TriggerCanWrite", func(t *testing.T) {
			token := f.token(t, 2, 1)
			MakeRequest(t, NewRequest(t, "GET", repoURL).AddTokenAuth(token), http.StatusOK)
			resp := MakeRequest(t, NewRequestWithJSON(t, "POST", issueURL, newIssue).AddTokenAuth(token), http.StatusCreated)
			assert.Equal(t, f.bot.Name, DecodeJSON(t, resp, &api.Issue{}).Poster.UserName) // it acts as the bot
			MakeRequest(t, NewRequestWithJSON(t, "POST", labelURL, label).AddTokenAuth(token), http.StatusCreated)
		})

		t.Run("TriggerCannotWriteSoSessionCannot", func(t *testing.T) {
			token := f.token(t, 5, 2) // user5 only reads the public repository, although the bot has write access
			MakeRequest(t, NewRequest(t, "GET", repoURL).AddTokenAuth(token), http.StatusOK)
			MakeRequest(t, NewRequestWithJSON(t, "POST", labelURL, &api.CreateLabelOption{Name: "denied", Color: "#00aabb"}).AddTokenAuth(token), http.StatusForbidden)
			// the private repository is invisible to the person, so it is to the session
			MakeRequest(t, NewRequest(t, "GET", "/api/v1/repos/user2/repo2").AddTokenAuth(token), http.StatusNotFound)
		})

		t.Run("AdministrationAndOrganizationsAreOutOfReach", func(t *testing.T) {
			token := f.token(t, 1, 3) // even when the triggering user is a site administrator
			MakeRequest(t, NewRequest(t, "GET", "/api/v1/admin/users").AddTokenAuth(token), http.StatusForbidden)
			MakeRequest(t, NewRequestWithJSON(t, "POST", "/api/v1/admin/users", &api.CreateUserOption{Username: "evil", Email: "evil@example.com", Password: "password123!"}).AddTokenAuth(token), http.StatusForbidden)
			MakeRequest(t, NewRequest(t, "GET", "/api/v1/orgs/org3/members").AddTokenAuth(token), http.StatusForbidden)
			MakeRequest(t, NewRequest(t, "GET", "/api/v1/user/settings").AddTokenAuth(token), http.StatusForbidden)
			unittest.AssertNotExistsBean(t, &user_model.User{LowerName: "evil"})
		})

		t.Run("RepositorySearchIsLimitedToWhatBothCanSee", func(t *testing.T) {
			names := func(token string) []string {
				resp := MakeRequest(t, NewRequest(t, "GET", "/api/v1/repos/search?q=repo&limit=50").AddTokenAuth(token), http.StatusOK)
				var res api.SearchResults
				DecodeJSON(t, resp, &res)
				var out []string
				for _, r := range res.Data {
					out = append(out, r.FullName)
				}
				return out
			}
			assert.Contains(t, names(f.token(t, 2, 4)), "user2/repo2")    // the bot and the owner both see it
			assert.NotContains(t, names(f.token(t, 5, 5)), "user2/repo2") // the bot does, the triggering user does not
		})
	})
}

func TestAgentSessionTokenLifetime(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, _ *url.URL) { // the git hooks of the agent repository need the internal server
		f := newAgentFixture(t)
		check := func(token string, status int) {
			MakeRequest(t, NewRequest(t, "GET", "/api/v1/repos/user2/repo1").AddTokenAuth(token), status)
		}

		t.Run("RefusedOnceTheRunEnded", func(t *testing.T) {
			token := f.token(t, 2, 1)
			check(token, http.StatusOK)
			_, err := db.GetEngine(t.Context()).Exec("UPDATE action_run SET status = ? WHERE id = ?", actions_model.StatusSuccess, f.run.ID)
			require.NoError(t, err)
			check(token, http.StatusUnauthorized)
			_, err = db.GetEngine(t.Context()).Exec("UPDATE action_run SET status = ? WHERE id = ?", actions_model.StatusRunning, f.run.ID)
			require.NoError(t, err)
		})

		t.Run("RefusedOnceAgentsAreDisabled", func(t *testing.T) {
			token := f.token(t, 2, 2)
			check(token, http.StatusOK)
			setAgentsEnabled(t, false)
			check(token, http.StatusUnauthorized)
			setAgentsEnabled(t, true)
		})

		t.Run("RefusedWhenTheTriggeringUserIsGone", func(t *testing.T) {
			token := f.token(t, 5, 3)
			check(token, http.StatusOK)
			require.NoError(t, user_model.UpdateUserCols(t.Context(), &user_model.User{ID: 5, ProhibitLogin: true}, "prohibit_login"))
			check(token, http.StatusUnauthorized)
		})

		t.Run("UnknownTokensAreRefused", func(t *testing.T) {
			check(agents_model.SessionTokenPrefix+"0123456789012345678901234567890123456789", http.StatusUnauthorized)
		})
	})
}

func TestAgentBotPersonalTokensAreRefused(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, _ *url.URL) { // the git hooks of the agent repository need the internal server
		f := newAgentFixture(t)
		ctx := t.Context()

		pat := &auth_model.AccessToken{UID: f.bot.ID, Name: "permanent", Scope: auth_model.AccessTokenScopeAll}
		require.NoError(t, auth_model.NewAccessToken(ctx, pat))
		MakeRequest(t, NewRequest(t, "GET", "/api/v1/user").AddTokenAuth(pat.Token), http.StatusUnauthorized)
		MakeRequest(t, NewRequest(t, "GET", "/api/v1/user").AddBasicAuth(f.bot.Name), http.StatusUnauthorized)

		// a bot that is not an agent keeps working exactly as before
		plain := &user_model.User{Name: "plain-bot", Email: "plain-bot@example.com", Type: user_model.UserTypeBot}
		require.NoError(t, user_model.CreateUser(ctx, plain, &user_model.Meta{}, &user_model.CreateUserOverwriteOptions{IsActive: optional.Some(true)}))
		plainPAT := &auth_model.AccessToken{UID: plain.ID, Name: "permanent", Scope: auth_model.AccessTokenScopeAll}
		require.NoError(t, auth_model.NewAccessToken(ctx, plainPAT))
		MakeRequest(t, NewRequest(t, "GET", "/api/v1/user").AddTokenAuth(plainPAT.Token), http.StatusOK)
	})
}

func TestAgentSessionTokenGitPush(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, u *url.URL) {
		f := newAgentFixture(t)
		const sessionID = 42
		sessionBranch := fmt.Sprintf("agent/%d/work", sessionID)

		remote := func(token, repoPath string) *url.URL {
			cu := *u
			cu.Path = repoPath
			cu.User = url.UserPassword("agent", token)
			return &cu
		}
		commit := func(dir, file string) {
			_, _, err := gitcmd.NewCommand("checkout", "-B").AddDynamicArguments(sessionBranch).WithDir(dir).RunStdString(t.Context())
			require.NoError(t, err)
			doGitCheckoutWriteFileCommit(localGitAddCommitOptions{LocalRepoPath: dir, CheckoutBranch: sessionBranch, TreeFilePath: file, TreeFileContent: file})(t)
		}

		token := f.token(t, 2, sessionID)
		dir := t.TempDir()
		doGitClone(dir, remote(token, "/user2/repo1.git"))(t)

		t.Run("SessionBranchIsWritable", func(t *testing.T) {
			commit(dir, "a.txt")
			doGitPushTestRepository(dir, "origin", sessionBranch)(t)
			commit(dir, "b.txt") // a fast-forward on the same branch is fine
			doGitPushTestRepository(dir, "origin", sessionBranch)(t)
		})

		t.Run("OtherRefsAreRefused", func(t *testing.T) {
			doGitPushTestRepositoryFail(dir, "origin", sessionBranch+":refs/heads/agent/43/work")(t) // another session
			doGitPushTestRepositoryFail(dir, "origin", sessionBranch+":refs/heads/feature")(t)       // any other branch
			doGitPushTestRepositoryFail(dir, "origin", sessionBranch+":refs/heads/master")(t)        // the default branch
			doGitPushTestRepositoryFail(dir, "origin", sessionBranch+":refs/heads/agent/4/work")(t)  // prefix of another session id
			_, _, err := gitcmd.NewCommand("tag", "v-agent").WithDir(dir).RunStdString(t.Context())
			require.NoError(t, err)
			doGitPushTestRepositoryFail(dir, "origin", "v-agent")(t)
			doGitPushTestRepositoryFail(dir, "origin", ":"+"master")(t) // deleting a foreign branch
		})

		t.Run("ForcePushIsRefused", func(t *testing.T) {
			_, _, err := gitcmd.NewCommand("reset", "--hard", "HEAD~1").WithDir(dir).RunStdString(t.Context())
			require.NoError(t, err)
			commit(dir, "c.txt") // diverges from what is already on the server
			doGitPushTestRepositoryFail(dir, "--force", "origin", sessionBranch)(t)
			doGitPushTestRepositoryFail(dir, "origin", "+"+sessionBranch)(t)
		})

		t.Run("ReadOnlyTriggerCannotPush", func(t *testing.T) {
			readerDir := t.TempDir()
			readerToken := f.token(t, 5, sessionID)
			doGitClone(readerDir, remote(readerToken, "/user2/repo1.git"))(t)
			// a branch that does not exist yet, so that only the permission can make the push fail (not a non-fast-forward)
			freshBranch := fmt.Sprintf("agent/%d/reader", sessionID)
			_, _, err := gitcmd.NewCommand("checkout", "-B").AddDynamicArguments(freshBranch).WithDir(readerDir).RunStdString(t.Context())
			require.NoError(t, err)
			doGitCheckoutWriteFileCommit(localGitAddCommitOptions{LocalRepoPath: readerDir, CheckoutBranch: freshBranch, TreeFilePath: "r.txt", TreeFileContent: "r"})(t)
			doGitPushTestRepositoryFail(readerDir, "origin", freshBranch)(t)
		})

		t.Run("PrivateRepositoryOfAnotherUserIsHidden", func(t *testing.T) {
			// the bot may write user2/repo2, but the triggering user5 cannot even read it
			doGitCloneFail(remote(f.token(t, 5, sessionID), "/user2/repo2.git"))(t)
		})

		t.Run("ConfigRepositoryIsReadOnly", func(t *testing.T) {
			cfg := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{OwnerID: f.bot.ID, LowerName: ".agent"})
			user2 := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
			require.NoError(t, repo_service.AddOrUpdateCollaborator(t.Context(), cfg, user2, perm.AccessModeWrite))
			cfgDir := t.TempDir()
			cfgURL := remote(f.token(t, 2, sessionID), "/"+f.bot.Name+"/.agent.git")
			require.NoError(t, cloneOnly(t, cfgDir, cfgURL)) // reading the configuration is allowed
			commit(cfgDir, "evil.txt")
			doGitPushTestRepositoryFail(cfgDir, "origin", sessionBranch)(t)
		})
	})
}

func cloneOnly(t *testing.T, dir string, u *url.URL) error {
	_, _, err := gitcmd.NewCommand("clone").AddDynamicArguments(u.String(), dir).RunStdString(t.Context())
	return err
}
