// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	agents_model "gitea.dev/models/agents"
	"gitea.dev/models/db"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/json"
	"gitea.dev/modules/setting"
	api "gitea.dev/modules/structs"
	"gitea.dev/modules/test"
	actions_service "gitea.dev/services/actions"
	agents_service "gitea.dev/services/agents"
	"gitea.dev/services/context"
	"gitea.dev/services/contexttest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	unittest.MainTest(m)
}

func TestSessionsAPI(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	defer test.MockVariableValue(&setting.Agents.Enabled, true)()
	defer test.MockVariableValue(&actions_service.EmitJobsIfReadyByRun, func(int64) error { return nil })()
	codex, err := agents_service.CreateAgent(t.Context(), agents_service.AgentOptions{BotName: "codex", Name: "Codex", Provider: agents_model.ProviderOpenAI, Model: "gpt-5-codex"})
	require.NoError(t, err)
	alice := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	_, err = agents_service.SetUserKey(t.Context(), codex, alice, agents_service.KeyOptions{APIKey: "sk-alice"})
	require.NoError(t, err)

	ctx, resp := contexttest.MockAPIContext(t, "GET /api/v1/agents")
	contexttest.LoadUser(t, ctx, 2)
	ListAgents(ctx)
	var agents []*api.Agent
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &agents))
	assert.Equal(t, []*api.Agent{{ID: codex.ID, Name: "Codex", MentionName: "codex", Model: "gpt-5-codex", HasKey: true}}, agents)

	create := func(userID int64, body string) (int, string) {
		ctx, resp := contexttest.MockAPIContext(t, "POST /api/v1/repos/user2/repo1/agents/sessions")
		contexttest.LoadUser(t, ctx, userID)
		contexttest.LoadRepo(t, ctx, 1)
		ctx.Req.Body = io.NopCloser(strings.NewReader(body))
		CreateSession(ctx)
		return resp.Code, resp.Body.String()
	}
	code, body := create(4, `{"agent": "codex", "prompt": "Add a README"}`)
	assert.Equal(t, http.StatusBadRequest, code, "bob has no key: %s", body)

	code, body = create(2, `{"agent": "codex", "prompt": "Add a README"}`)
	require.Equal(t, http.StatusCreated, code, body)
	var session api.AgentSession
	require.NoError(t, json.Unmarshal([]byte(body), &session))
	assert.Equal(t, "codex", session.Agent)
	assert.Equal(t, "task", session.Kind)
	assert.Equal(t, "user2", session.Creator.UserName)

	call := func(userID int64, method, path string, handler func(*context.APIContext), params map[string]string, body string) (int, string) {
		ctx, resp := contexttest.MockAPIContext(t, method+" /api/v1/repos/user2/repo1/"+path)
		contexttest.LoadUser(t, ctx, userID)
		contexttest.LoadRepo(t, ctx, 1)
		for k, v := range params {
			ctx.SetPathParam(k, v)
		}
		ctx.Req.Body = io.NopCloser(strings.NewReader(body))
		handler(ctx)
		return resp.Code, resp.Body.String()
	}
	id := strconv.FormatInt(session.ID, 10)
	s, err := agents_model.GetSessionByID(t.Context(), session.ID)
	require.NoError(t, err)
	require.NoError(t, agents_service.CancelSession(t.Context(), s))
	code, body = call(4, "POST", "agents/sessions/"+id+"/resume", ResumeSession, map[string]string{"id": id}, "")
	assert.NotEqual(t, http.StatusCreated, code, "only who was interrupted resumes: %s", body)
	code, body = call(2, "POST", "agents/sessions/"+id+"/resume", ResumeSession, map[string]string{"id": id}, "")
	require.Equal(t, http.StatusCreated, code, body)
	require.NoError(t, agents_service.CancelSession(t.Context(), s))
	runs, err := db.Find[agents_model.Run](t.Context(), agents_model.FindRunsOptions{SessionID: s.ID})
	require.NoError(t, err)
	runID := strconv.FormatInt(runs[0].ID, 10)
	code, body = call(2, "POST", "agents/sessions/"+id+"/runs/"+runID+"/retry", RetryRun, map[string]string{"id": id, "run": runID}, "")
	require.Equal(t, http.StatusCreated, code, body)
	var run api.AgentRun
	require.NoError(t, json.Unmarshal([]byte(body), &run))
	assert.Equal(t, "Add a README", run.Prompt)

	retried, err := agents_model.GetRunByID(t.Context(), run.ID)
	require.NoError(t, err)
	require.NoError(t, agents_service.AppendEvents(t.Context(), retried, []string{`{"type":"turn.started"}`}))
	code, body = call(2, "GET", "agents/sessions/"+id+"/events", ListEvents, map[string]string{"id": id}, "")
	require.Equal(t, http.StatusOK, code, body)
	var events []*api.AgentEvent
	require.NoError(t, json.Unmarshal([]byte(body), &events))
	require.Len(t, events, 1)
	assert.Equal(t, "turn.started", events[0].Type)

	code, body = call(2, "POST", "pulls/3/agent-review", AgentReview, map[string]string{"index": "3"}, `{"agent": "codex"}`)
	require.Equal(t, http.StatusCreated, code, body)
	require.NoError(t, json.Unmarshal([]byte(body), &session))
	assert.Equal(t, "pull", session.Kind)
	assert.EqualValues(t, 3, session.IssueNumber)

	list := func(userID int64, query string) []*api.AgentSession {
		code, body := call(userID, "GET", "agents/sessions?"+query, ListSessions, nil, "")
		require.Equal(t, http.StatusOK, code, body)
		var sessions []*api.AgentSession
		require.NoError(t, json.Unmarshal([]byte(body), &sessions))
		return sessions
	}
	assert.Len(t, list(2, ""), 2)
	pulls := list(2, "kind=pull&agent=codex")
	require.Len(t, pulls, 1)
	assert.Equal(t, session.ID, pulls[0].ID)
	assert.Len(t, list(2, "created_by_me=true"), 2)
	assert.Empty(t, list(4, "created_by_me=true"))
	assert.Empty(t, list(2, "status=success"))
}

func TestDenyAgentBots(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	codex, err := agents_service.CreateAgent(t.Context(), agents_service.AgentOptions{BotName: "codex", Name: "Codex", Provider: agents_model.ProviderOpenAI, Model: "gpt-5-codex"})
	require.NoError(t, err)
	require.NoError(t, codex.LoadBot(t.Context()))

	ctx, resp := contexttest.MockAPIContext(t, "DELETE /api/v1/repos/user2/repo1/branches/master")
	ctx.Doer = codex.Bot
	DenyAgentBots(ctx)
	assert.Equal(t, http.StatusForbidden, resp.Code, "an agent run's token only works with git")

	ctx, resp = contexttest.MockAPIContext(t, "GET /api/v1/user")
	contexttest.LoadUser(t, ctx, 2)
	DenyAgentBots(ctx)
	assert.False(t, ctx.Written())
	assert.Equal(t, http.StatusOK, resp.Code)
}
