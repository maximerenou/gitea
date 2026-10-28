// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package integration

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	actions_model "gitea.dev/models/actions"
	agents_model "gitea.dev/models/agents"
	"gitea.dev/models/db"
	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/agents"
	api "gitea.dev/modules/structs"
	agents_service "gitea.dev/services/agents"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startedTask mimics what happens when a runner picks the job up: the agent gets its session token as its only secret.
func startedTask(t *testing.T, actionRunID int64, runRepoID int64) (token string) {
	actionRun, err := actions_model.GetRunByRepoAndID(t.Context(), runRepoID, actionRunID)
	require.NoError(t, err)
	secrets := map[string]string{"GITEA_TOKEN": "task-token", "DEPLOY_KEY": "repository-secret"}
	task := &actions_model.ActionTask{Job: &actions_model.ActionRunJob{RunID: actionRun.ID, Run: actionRun}}
	require.NoError(t, agents_model.RestrictTaskSecrets(t.Context(), task, secrets))
	require.Len(t, secrets, 1)
	return secrets[agents_model.SessionTokenSecretName]
}

func TestAgentReviewRunEndToEnd(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, _ *url.URL) {
		f := newAgentFixture(t)
		ctx := t.Context()
		user2 := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
		repo1 := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
		session := loginUser(t, "user2")
		ownerToken := getUserToken(t, "user2", "write:repository", "write:issue")

		// a pull request whose title tries to break out of everything
		hostileTitle := `Fix "x": ${{ secrets.AGENT_SESSION_TOKEN }} $(touch /tmp/pwned) - evil: 1`
		testCreateFileInBranch(t, user2, repo1, createFileInBranchOptions{OldBranch: "master", NewBranch: "agent-pr"}, map[string]string{"new.txt": "hello wrold\n"})
		testPullCreate(t, session, "user2", "repo1", false, "master", "agent-pr", hostileTitle)
		pr := unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{RepoID: repo1.ID, Title: hostileTitle, IsPull: true})
		require.NoError(t, pr.LoadPullRequest(ctx))

		requestReview := func() {
			MakeRequest(t, NewRequestWithJSON(t, "POST", fmt.Sprintf("/api/v1/repos/user2/repo1/pulls/%d/requested_reviewers", pr.Index),
				&api.PullReviewRequestOptions{Reviewers: []string{f.bot.Name}}).AddTokenAuth(ownerToken), http.StatusCreated)
			require.NoError(t, agents_service.FlushQueue(ctx))
		}
		startRun := func() { // the same request the notifier queues, without going through the API again
			require.NoError(t, agents_service.ProcessQueueItem(ctx, &agents_service.QueueItem{AgentID: f.agent.ID, IssueID: pr.ID, DoerID: 2, Event: agents.EventReviewRequested}))
		}
		runsOfPR := func() []*agents_model.Run {
			runs, err := agents_model.ListRunsOfIssue(ctx, pr.ID)
			require.NoError(t, err)
			return runs
		}

		requestReview()
		runs := runsOfPR()
		require.Len(t, runs, 1)
		run := runs[0]

		t.Run("RunIsCreatedWithoutAnyFileInTheRepository", func(t *testing.T) {
			assert.Equal(t, agents_model.RunPending, run.Status)
			assert.Equal(t, int64(2), run.TriggerUserID)
			assert.Equal(t, string(agents.EventReviewRequested), run.TriggerEvent)
			assert.NotEmpty(t, run.ConfigCommitSHA)
			require.NotZero(t, run.ActionRunID)

			actionRun, err := actions_model.GetRunByRepoAndID(ctx, repo1.ID, run.ActionRunID)
			require.NoError(t, err)
			assert.Equal(t, fmt.Sprintf("agent-%d.yml", run.ID), actionRun.WorkflowID)
			assert.Equal(t, int64(2), actionRun.TriggerUserID)
			assert.Contains(t, actionRun.Title, hostileTitle) // shown as plain text only

			jobs, err := actions_model.GetLatestAttemptJobsByRun(ctx, actionRun)
			require.NoError(t, err)
			require.Len(t, jobs, 1)
			payload := string(jobs[0].WorkflowPayload)
			assert.Contains(t, payload, "agent-run prepare")
			assert.NotContains(t, payload, "pwned") // the title never reaches the workflow
			assert.NotContains(t, payload, "evil")
			assert.Equal(t, []string{"ubuntu-latest"}, jobs[0].RunsOn)

			MakeRequest(t, NewRequest(t, "GET", "/api/v1/repos/user2/repo1/contents/.gitea/workflows?ref=master").AddTokenAuth(ownerToken), http.StatusNotFound)
		})

		token := startedTask(t, run.ActionRunID, repo1.ID)
		contextURL := fmt.Sprintf("/api/v1/agents/runs/%d/context", run.ID)

		t.Run("ContextIsOnlyForTheRunItself", func(t *testing.T) {
			resp := MakeRequest(t, NewRequest(t, "GET", contextURL).AddTokenAuth(token), http.StatusOK)
			rc := DecodeJSON(t, resp, &api.AgentRunContext{})
			assert.Equal(t, "review", rc.Action)
			assert.Equal(t, f.bot.Name, rc.Agent.Name)
			assert.Equal(t, "model-provider-secret", rc.Agent.Token)
			assert.Equal(t, fmt.Sprintf("agent/%d/work", rc.SessionID), rc.Branch)
			assert.Equal(t, pr.PullRequest.GetGitHeadRefName(), rc.CheckoutRef)
			require.NotNil(t, rc.Subject)
			assert.Equal(t, "pull_request", rc.Subject.Kind)
			assert.Equal(t, hostileTitle, rc.Subject.Title) // delivered as data, unchanged
			assert.Contains(t, rc.SystemPrompt, "untrusted")
			fence := strings.Index(rc.Prompt, "<<<UNTRUSTED CONTENT")
			require.Positive(t, fence)
			assert.Greater(t, strings.Index(rc.Prompt, hostileTitle), fence) // only ever inside the fenced block
			assert.NotContains(t, rc.Prompt[:fence], "pwned")
			assert.Contains(t, rc.ResultSchema, `"const": "v1"`)

			// nobody else gets the model token or the pull request content
			MakeRequest(t, NewRequest(t, "GET", contextURL), http.StatusUnauthorized)
			MakeRequest(t, NewRequest(t, "GET", contextURL).AddTokenAuth(ownerToken), http.StatusForbidden)
			MakeRequest(t, NewRequest(t, "GET", contextURL).AddTokenAuth(f.token(t, 2, run.SessionID+1000)), http.StatusNotFound) // another session
			MakeRequest(t, NewRequest(t, "GET", "/api/v1/agents/runs/999999/context").AddTokenAuth(token), http.StatusNotFound)
		})

		t.Run("ProgressAndStatus", func(t *testing.T) {
			MakeRequest(t, NewRequestWithJSON(t, "POST", fmt.Sprintf("/api/v1/agents/runs/%d/progress", run.ID), &api.AgentRunProgressOption{Text: "reading new.txt\n"}).AddTokenAuth(token), http.StatusNoContent)
			got, err := agents_model.GetRunByID(ctx, run.ID)
			require.NoError(t, err)
			assert.Equal(t, agents_model.RunRunning, got.Status)
			assert.Contains(t, got.Progress, "reading new.txt")
			resp := MakeRequest(t, NewRequest(t, "GET", fmt.Sprintf("/api/v1/agents/runs/%d/status", run.ID)).AddTokenAuth(token), http.StatusOK)
			st := DecodeJSON(t, resp, &api.AgentRunStatus{})
			assert.False(t, st.Cancelled)
		})

		t.Run("ResultIsAppliedAsTheBotOnce", func(t *testing.T) {
			result := `{"version":"v1","status":"done","summary":"typo found","comment":"Review finished.",
				"review":{"verdict":"request_changes","body":"Please fix the typo.","comments":[{"path":"new.txt","line":1,"body":"wrold -> world"},{"path":"nope.txt","line":9,"body":"lost"}]},
				"findings":[{"severity":"low","file":"new.txt","explanation":"not for a review agent"}],"surprise":true}`
			resultURL := fmt.Sprintf("/api/v1/agents/runs/%d/result", run.ID)
			resp := MakeRequest(t, NewRequestWithBody(t, "POST", resultURL, strings.NewReader(result)).AddTokenAuth(token), http.StatusOK)
			res := DecodeJSON(t, resp, &api.AgentRunResultResponse{})
			assert.Equal(t, "success", res.Status)
			notes := strings.Join(res.Notes, "\n")
			assert.Contains(t, notes, `unknown field "surprise"`)
			assert.Contains(t, notes, `"findings" is not applied by a review agent`)
			assert.Contains(t, notes, "nope.txt:9") // a comment on a line that is not in the diff is reported, not fatal

			review := unittest.AssertExistsAndLoadBean(t, &issues_model.Review{IssueID: pr.ID, ReviewerID: f.bot.ID, Type: issues_model.ReviewTypeReject})
			assert.Equal(t, "Please fix the typo.", review.Content)
			unittest.AssertExistsAndLoadBean(t, &issues_model.Comment{IssueID: pr.ID, PosterID: f.bot.ID, Content: "wrold -> world", ReviewID: review.ID})
			unittest.AssertExistsAndLoadBean(t, &issues_model.Comment{IssueID: pr.ID, PosterID: f.bot.ID, Content: "Review finished."})
			unittest.AssertNotExistsBean(t, &issues_model.Comment{IssueID: pr.ID, Content: "lost"})

			got, err := agents_model.GetRunByID(ctx, run.ID)
			require.NoError(t, err)
			assert.Equal(t, agents_model.RunSuccess, got.Status)
			assert.Equal(t, "typo found", got.Summary)

			// the result counts once, and the run is closed for everything else
			MakeRequest(t, NewRequestWithBody(t, "POST", resultURL, strings.NewReader(result)).AddTokenAuth(token), http.StatusConflict)
			MakeRequest(t, NewRequest(t, "GET", contextURL).AddTokenAuth(token), http.StatusConflict)
			assert.Equal(t, 1, unittestCount(t, &issues_model.Review{IssueID: pr.ID, ReviewerID: f.bot.ID, Type: issues_model.ReviewTypeReject}))
		})

		t.Run("SessionCardOnThePullRequest", func(t *testing.T) {
			pageURL := fmt.Sprintf("/user2/repo1/pulls/%d", pr.Index)
			page := session.MakeRequest(t, NewRequest(t, "GET", pageURL), http.StatusOK).Body.String()
			assert.Contains(t, page, "agent-session-card")
			assert.Contains(t, page, f.bot.Name)
			assert.Contains(t, page, "typo found")
			assert.Contains(t, page, "reading new.txt") // the writer sees the output
			assert.Contains(t, page, fmt.Sprintf("/user2/repo1/actions/runs/%d", unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRun{ID: run.ActionRunID}).Index))
			assert.NotContains(t, page, "pwned")

			anonymous := MakeRequest(t, NewRequest(t, "GET", pageURL), http.StatusOK).Body.String()
			assert.Contains(t, anonymous, "typo found")
			assert.NotContains(t, anonymous, "reading new.txt") // but a reader does not
		})

		t.Run("InvalidResultFailsTheRun", func(t *testing.T) {
			startRun()
			runs := runsOfPR()
			require.Len(t, runs, 2)
			next := runs[1]
			nextToken := startedTask(t, next.ActionRunID, repo1.ID)
			resultURL := fmt.Sprintf("/api/v1/agents/runs/%d/result", next.ID)
			MakeRequest(t, NewRequestWithBody(t, "POST", resultURL, strings.NewReader(`{"version":"v9"}`)).AddTokenAuth(nextToken), http.StatusUnprocessableEntity)
			got, err := agents_model.GetRunByID(ctx, next.ID)
			require.NoError(t, err)
			assert.Equal(t, agents_model.RunFailed, got.Status)
			assert.Contains(t, got.Error, "invalid result")
		})

		t.Run("CancelStopsTheActionsRun", func(t *testing.T) {
			startRun()
			runs := runsOfPR()
			require.Len(t, runs, 3)
			next := runs[2]
			nextToken := startedTask(t, next.ActionRunID, repo1.ID)
			require.NoError(t, agents_service.CancelRun(ctx, next))
			require.NoError(t, agents_service.CancelRun(ctx, next)) // idempotent
			resp := MakeRequest(t, NewRequest(t, "GET", fmt.Sprintf("/api/v1/agents/runs/%d/status", next.ID)).AddTokenAuth(nextToken), http.StatusUnauthorized)
			_ = resp // the session token died with the Actions run: the helper sees a refusal and stops
			actionRun, err := actions_model.GetRunByRepoAndID(ctx, repo1.ID, next.ActionRunID)
			require.NoError(t, err)
			assert.Equal(t, actions_model.StatusCancelled, actionRun.Status)
			got, err := agents_model.GetRunByID(ctx, next.ID)
			require.NoError(t, err)
			assert.Equal(t, agents_model.RunCancelled, got.Status)
		})

		t.Run("NothingStartsWithoutTheRightToCommand", func(t *testing.T) {
			before := len(runsOfPR())
			item := func(doerID int64, event agents.Event) *agents_service.QueueItem {
				return &agents_service.QueueItem{AgentID: f.agent.ID, IssueID: pr.ID, DoerID: doerID, Event: event}
			}
			// user5 can read the public repository but not write to it
			assert.ErrorIs(t, agents_service.ProcessQueueItem(ctx, item(5, agents.EventReviewRequested)), agents_service.ErrNotAllowed)
			// the agent does not react to this event
			assert.NoError(t, agents_service.ProcessQueueItem(ctx, item(2, agents.EventPRCreated)))
			assert.Equal(t, before+1, len(runsOfPR())) // pr_created is accepted by a review agent with "all events"

			f.agent.Events = []agents.Event{agents.EventMentioned}
			require.NoError(t, agents_model.UpdateAgent(ctx, f.agent))
			assert.ErrorIs(t, agents_service.ProcessQueueItem(ctx, item(2, agents.EventReviewRequested)), agents_service.ErrNotAllowed)
			f.agent.Events = []agents.Event{agents.EventAll}

			// "owner only" on a global agent means a site administrator
			f.agent.AccessMode = agents_model.AccessOwner
			require.NoError(t, agents_model.UpdateAgent(ctx, f.agent))
			assert.ErrorIs(t, agents_service.ProcessQueueItem(ctx, item(2, agents.EventReviewRequested)), agents_service.ErrNotAllowed)
			assert.NoError(t, agents_service.ProcessQueueItem(ctx, item(1, agents.EventReviewRequested)))
			f.agent.AccessMode = agents_model.AccessWriters
			require.NoError(t, agents_model.UpdateAgent(ctx, f.agent))

			// the bot never commands itself
			assert.ErrorIs(t, agents_service.ProcessQueueItem(ctx, item(f.bot.ID, agents.EventReviewRequested)), agents_service.ErrNotAllowed)

			// nothing at all while agents are disabled, not even a queued request
			count := len(runsOfPR())
			setAgentsEnabled(t, false)
			MakeRequest(t, NewRequestWithJSON(t, "DELETE", fmt.Sprintf("/api/v1/repos/user2/repo1/pulls/%d/requested_reviewers", pr.Index),
				&api.PullReviewRequestOptions{Reviewers: []string{f.bot.Name}}).AddTokenAuth(ownerToken), http.StatusNoContent)
			requestReview()
			assert.Len(t, runsOfPR(), count)
			assert.ErrorIs(t, agents_service.ProcessQueueItem(ctx, item(2, agents.EventReviewRequested)), agents_service.ErrNotAllowed)
			disabledPage := session.MakeRequest(t, NewRequest(t, "GET", fmt.Sprintf("/user2/repo1/pulls/%d", pr.Index)), http.StatusOK).Body.String()
			assert.NotContains(t, disabledPage, "agent-session-card") // the page is the original one while disabled
			setAgentsEnabled(t, true)
		})
	})
}

func unittestCount(t *testing.T, bean any) int {
	n, err := db_count(t, bean)
	require.NoError(t, err)
	return int(n)
}

func db_count(t *testing.T, bean any) (int64, error) {
	return db.GetEngine(t.Context()).Count(bean)
}
