// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	actions_model "gitea.dev/models/actions"
	activities_model "gitea.dev/models/activities"
	agents_model "gitea.dev/models/agents"
	auth_model "gitea.dev/models/auth"
	"gitea.dev/models/db"
	issues_model "gitea.dev/models/issues"
	access_model "gitea.dev/models/perm/access"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unit"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"
	"gitea.dev/modules/json"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/test"
	"gitea.dev/modules/util"
	actions_service "gitea.dev/services/actions"
	notify_service "gitea.dev/services/notify"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	unittest.MainTest(m)
}

func prepare(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	t.Cleanup(test.MockVariableValue(&actions_service.EmitJobsIfReadyByRun, func(int64) error { return nil }))
	t.Cleanup(test.MockVariableValue(&setting.Agents.Enabled, true))
	t.Cleanup(test.MockVariableValue(&setting.Actions.Enabled, true))
	t.Cleanup(test.MockVariableValue(&access_model.AgentDelegateUser, DelegateUser))
	t.Cleanup(test.MockVariableValue(&access_model.AgentAssignable, Assignable))
}

func newAgent(t *testing.T, botName string) *agents_model.Agent {
	agent, err := CreateAgent(t.Context(), AgentOptions{BotName: botName, Name: botName, Provider: agents_model.ProviderOpenAI, Model: "gpt-5-codex"})
	require.NoError(t, err)
	return agent
}

func setKey(t *testing.T, agent *agents_model.Agent, user *user_model.User, key string) {
	_, err := SetUserKey(t.Context(), agent, user, KeyOptions{APIKey: key})
	require.NoError(t, err)
}

func latestRun(t *testing.T, s *agents_model.Session) *agents_model.Run {
	r, err := agents_model.GetLatestRun(t.Context(), s.ID)
	require.NoError(t, err)
	return r
}

// claimTask simulates a runner picking the job of an agent run
func claimTask(t *testing.T, r *agents_model.Run) *actions_model.ActionTask {
	r, err := agents_model.GetRunByID(t.Context(), r.ID)
	require.NoError(t, err)
	jobs, err := actions_model.GetLatestAttemptJobsByRepoAndRunID(t.Context(), r.RepoID, r.ActionRunID)
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	task := &actions_model.ActionTask{JobID: jobs[0].ID, RepoID: r.RepoID, Status: actions_model.StatusRunning}
	task.GenerateAndFillToken()
	require.NoError(t, db.Insert(t.Context(), task))
	return task
}

// startRun claims the run's task like a runner would, which issues its bot token
func startRun(t *testing.T, r *agents_model.Run) (*agents_model.Run, map[string]string) {
	secrets, isAgent, err := agents_model.TaskSecrets(t.Context(), claimTask(t, r))
	require.NoError(t, err)
	require.True(t, isAgent)
	return unittest.AssertExistsAndLoadBean(t, &agents_model.Run{ID: r.ID}), secrets
}

// endRun settles a run and its Actions run, which lets the next queued run start
func endRun(t *testing.T, r *agents_model.Run) {
	require.NoError(t, Finish(t.Context(), r, FinishOptions{Status: "success"}))
	actionRun := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRun{ID: r.ActionRunID})
	actionRun.Status = actions_model.StatusSuccess
	syncActionRunStatus(t.Context(), actionRun)
}

func TestRunsUseTheTriggerUsersKey(t *testing.T) {
	prepare(t)
	ctx := t.Context()
	alice := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	bob := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 4})
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	codex := newAgent(t, "codex")
	assert.True(t, codex.Bot.IsTypeBot())
	assert.Empty(t, codex.Bot.Passwd, "a bot can only authenticate with the token of a running session")

	_, err := StartTaskSession(ctx, alice, repo, StartOptions{Agent: codex, Prompt: "do it"})
	require.ErrorIs(t, err, ErrNoUserKey)

	setKey(t, codex, alice, "sk-alice")
	setKey(t, codex, bob, "sk-bob")
	s, err := StartTaskSession(ctx, alice, repo, StartOptions{Agent: codex, Prompt: "Add a README"})
	require.NoError(t, err)
	r := latestRun(t, s)
	assert.Equal(t, agents_model.StatusWaiting, r.Status)

	task := claimTask(t, r)
	// a runner may pick the job before the agent run stores its Actions run, the link must not depend on it
	_, err = unittest.GetXORMEngine().Exec("UPDATE agent_run SET action_run_id = 0 WHERE id = ?", r.ID)
	require.NoError(t, err)
	secrets, isAgent, err := agents_model.TaskSecrets(ctx, task)
	require.NoError(t, err)
	assert.True(t, isAgent)
	_, err = unittest.GetXORMEngine().Exec("UPDATE agent_run SET action_run_id = ? WHERE id = ?", r.ActionRunID, r.ID)
	require.NoError(t, err)
	gitToken := secrets[agents_model.GitTokenSecretName]
	assert.NotEmpty(t, gitToken)
	modelToken := secrets[agents_model.APIKeySecretName]
	assert.Equal(t, map[string]string{"GITEA_TOKEN": task.Token, "GITHUB_TOKEN": task.Token, agents_model.APIKeySecretName: modelToken, agents_model.GitTokenSecretName: gitToken}, secrets)
	assert.NotContains(t, modelToken, "sk-alice", "the key never enters the job, the setup steps run in its container")
	modelRun, err := agents_model.GetActiveRunByModelToken(ctx, modelToken)
	require.NoError(t, err)
	assert.Equal(t, r.ID, modelRun.ID)
	_, err = agents_model.GetActiveRunByModelToken(ctx, modelToken+"x")
	require.Error(t, err)
	r = unittest.AssertExistsAndLoadBean(t, &agents_model.Run{ID: r.ID})
	token := unittest.AssertExistsAndLoadBean(t, &auth_model.AccessToken{ID: r.AccessTokenID})
	assert.Equal(t, codex.BotID, token.UID)
	assert.Equal(t, auth_model.AccessTokenScopeWriteRepository, token.Scope)

	rerun := claimTask(t, r)
	secrets, _, err = agents_model.TaskSecrets(ctx, rerun)
	require.NoError(t, err)
	assert.NotContains(t, secrets, agents_model.APIKeySecretName, "a rerun must not replay the session on someone's key")
	assert.NotContains(t, secrets, agents_model.GitTokenSecretName)

	other := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionTask{ID: 47})
	_, isAgent, err = agents_model.TaskSecrets(ctx, other)
	require.NoError(t, err)
	assert.False(t, isAgent)

	endRun(t, r)
	unittest.AssertNotExistsBean(t, &auth_model.AccessToken{ID: token.ID}) // the token dies with the run
	_, err = agents_model.GetActiveRunByModelToken(ctx, modelToken)
	require.Error(t, err, "so does the model token")
	unittest.AssertExistsAndLoadBean(t, &activities_model.Notification{UserID: alice.ID, Source: activities_model.NotificationSourceAgentSession, SubjectLink: fmt.Sprintf("agents/%d", s.ID)})

	t.Run("AnotherUserTakesOverWithTheirOwnKey", func(t *testing.T) {
		_, err := SendInstruction(ctx, s, bob, "and add a license")
		require.NoError(t, err)
		bobRun := latestRun(t, s)
		assert.Equal(t, bob.ID, bobRun.TriggerUserID)
		_, secrets := startRun(t, bobRun)
		modelRun, err := agents_model.GetActiveRunByModelToken(ctx, secrets[agents_model.APIKeySecretName])
		require.NoError(t, err)
		assert.Equal(t, bob.ID, modelRun.TriggerUserID)
	})
}

func TestBotsBorrowTheTriggerUsersPermission(t *testing.T) {
	prepare(t)
	ctx := t.Context()
	alice := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	reader := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 5})
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	codex := newAgent(t, "codex")
	setKey(t, codex, alice, "sk-alice")
	setKey(t, codex, reader, "sk-reader")

	private := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 2})
	perm, err := access_model.GetDoerRepoPermission(ctx, private, codex.Bot)
	require.NoError(t, err)
	assert.False(t, perm.CanRead(unit.TypeCode), "outside of a run an agent has no access")

	pull := unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{ID: 3})
	HandleMention(ctx, alice, pull, 0, "@codex explain")
	s := unittest.AssertExistsAndLoadBean(t, &agents_model.Session{AgentID: codex.ID, IssueID: pull.ID})
	require.NoError(t, s.LoadAttributes(ctx))
	r, _ := startRun(t, latestRun(t, s))
	perm, err = access_model.GetDoerRepoPermission(ctx, repo, actingBot(s, r))
	require.NoError(t, err)
	assert.True(t, perm.CanWrite(unit.TypeCode))
	assert.False(t, perm.IsAdmin(), "alice administers the repository, her agent does not")
	perm, err = access_model.GetDoerRepoPermission(ctx, private, actingBot(s, r))
	require.NoError(t, err)
	assert.False(t, perm.CanRead(unit.TypeCode), "a run only borrows its trigger user's access to its own repository")
	endRun(t, r)

	_, err = SendInstruction(ctx, s, reader, "and you?")
	require.NoError(t, err)
	r, _ = startRun(t, latestRun(t, s))
	assert.Equal(t, reader.ID, r.TriggerUserID)
	assert.Empty(t, r.PushBranch, "a reader never gets push access through an agent")
	perm, err = access_model.GetDoerRepoPermission(ctx, repo, actingBot(s, r))
	require.NoError(t, err)
	assert.True(t, perm.CanRead(unit.TypeCode))
	assert.False(t, perm.CanWrite(unit.TypeCode))
}

func TestPlanRunPermissions(t *testing.T) {
	prepare(t)
	ctx := t.Context()
	alice := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	reader := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 5})
	codex := newAgent(t, "codex")
	pull := unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{ID: 3})
	s := &agents_model.Session{RepoID: 1, CreatorID: alice.ID, AgentID: codex.ID, Kind: agents_model.SessionKindPull, IssueID: pull.ID, BaseBranch: "master", Status: agents_model.StatusQueued}
	require.NoError(t, agents_model.InsertSession(ctx, s))
	require.NoError(t, s.LoadAttributes(ctx))

	plan, err := planRun(ctx, s, alice)
	require.NoError(t, err)
	assert.Equal(t, pushPlan{baseRef: "branch2", pushBranch: "branch2"}, plan, "a writer pushes to the pull request branch")

	plan, err = planRun(ctx, s, reader)
	require.NoError(t, err)
	assert.Empty(t, plan.pushBranch, "the same session is read-only for a reader")
	assert.NotEmpty(t, plan.readOnly)

	t.Run("ForkPullIsReadOnly", func(t *testing.T) {
		owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 12})
		fork := unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{ID: 8})
		s := &agents_model.Session{RepoID: fork.RepoID, CreatorID: owner.ID, AgentID: codex.ID, Kind: agents_model.SessionKindPull, IssueID: fork.ID, BaseBranch: "master", Status: agents_model.StatusQueued}
		require.NoError(t, agents_model.InsertSession(ctx, s))
		require.NoError(t, s.LoadAttributes(ctx))
		plan, err := planRun(ctx, s, owner)
		require.NoError(t, err)
		assert.Equal(t, "refs/pull/1/head", plan.baseRef)
		assert.Empty(t, plan.pushBranch, "the fork's commits never reach the base repository")
	})

	t.Run("TheActionsTokenNeverWrites", func(t *testing.T) {
		content, err := buildWorkflow(nil)
		require.NoError(t, err)
		assert.Contains(t, string(content), "contents: read")
	})
}

func TestMentions(t *testing.T) {
	prepare(t)
	ctx := t.Context()
	alice := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	bob := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 4})
	codex := newAgent(t, "codex")
	setKey(t, codex, alice, "sk-alice")
	pull := unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{ID: 3})

	HandleMention(ctx, bob, pull, 0, "@codex use someone's key")
	unittest.AssertNotExistsBean(t, &agents_model.Session{AgentID: codex.ID})
	noKey := unittest.AssertExistsAndLoadBean(t, &issues_model.Comment{IssueID: pull.ID, PosterID: codex.BotID})
	assert.Contains(t, noKey.Content, "your own API key")

	HandleMention(ctx, codex.Bot, pull, 0, "@codex loop")
	unittest.AssertNotExistsBean(t, &agents_model.Session{AgentID: codex.ID})

	HandleMention(ctx, alice, pull, 0, "@codex explain this change")
	s := unittest.AssertExistsAndLoadBean(t, &agents_model.Session{AgentID: codex.ID, IssueID: pull.ID})
	assert.Equal(t, agents_model.SessionKindPull, s.Kind)
	unittest.AssertExistsAndLoadBean(t, &issues_model.Reaction{IssueID: pull.ID, UserID: codex.BotID, Type: "eyes"})

	setKey(t, codex, bob, "sk-bob")
	HandleMention(ctx, bob, pull, 0, "@codex now fix the tests")
	unittest.AssertCount(t, &agents_model.Session{AgentID: codex.ID}, 1)
	runs, err := db.Find[agents_model.Run](ctx, agents_model.FindRunsOptions{SessionID: s.ID})
	require.NoError(t, err)
	require.Len(t, runs, 2, "one conversation per agent and pull request, shared by its users")
	assert.Equal(t, alice.ID, runs[0].TriggerUserID)
	assert.Equal(t, bob.ID, runs[1].TriggerUserID)
	assert.Equal(t, agents_model.StatusWaiting, runs[0].Status)
	assert.Equal(t, agents_model.StatusQueued, runs[1].Status, "concurrent requests on a session are queued")

	t.Run("AgentsTakeTurnsOnABranch", func(t *testing.T) {
		claude := newAgent(t, "claude")
		setKey(t, claude, alice, "sk-alice-2")
		HandleMention(ctx, alice, pull, 0, "@claude review it too")
		other := unittest.AssertExistsAndLoadBean(t, &agents_model.Session{AgentID: claude.ID, IssueID: pull.ID})
		assert.Equal(t, agents_model.StatusQueued, latestRun(t, other).Status, "codex is still working on branch2")
	})

	t.Run("NextRunStartsWhenThePreviousEnds", func(t *testing.T) {
		require.NoError(t, Finish(ctx, runs[0], FinishOptions{Status: "success", Summary: "It renames a file."}))
		actionRun := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRun{ID: runs[0].ActionRunID})
		actionRun.Status = actions_model.StatusSuccess
		syncActionRunStatus(ctx, actionRun)
		next := unittest.AssertExistsAndLoadBean(t, &agents_model.Run{ID: runs[1].ID})
		assert.Equal(t, agents_model.StatusWaiting, next.Status)
		done := unittest.AssertExistsAndLoadBean(t, &agents_model.Run{ID: runs[0].ID})
		reply := unittest.AssertExistsAndLoadBean(t, &issues_model.Comment{ID: done.ReplyCommentID})
		assert.Equal(t, codex.BotID, reply.PosterID)
		assert.Contains(t, reply.Content, "@user2 It renames a file.")
	})
}

func TestPromptSkipsDraftReviews(t *testing.T) {
	prepare(t)
	ctx := t.Context()
	alice := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	codex := newAgent(t, "codex")
	setKey(t, codex, alice, "sk-alice")
	pull := unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{ID: 3})
	for reviewType, content := range map[issues_model.ReviewType]string{issues_model.ReviewTypePending: "draft of user4", issues_model.ReviewTypeComment: "submitted by user4"} {
		review := &issues_model.Review{Type: reviewType, ReviewerID: 4, IssueID: pull.ID}
		require.NoError(t, db.Insert(ctx, review))
		require.NoError(t, db.Insert(ctx, &issues_model.Comment{Type: issues_model.CommentTypeCode, PosterID: 4, IssueID: pull.ID, ReviewID: review.ID, TreePath: "README.md", Line: 1, Content: content}))
	}

	HandleMention(ctx, alice, pull, 0, "@codex explain this change")
	s := unittest.AssertExistsAndLoadBean(t, &agents_model.Session{AgentID: codex.ID})
	runCtx, err := GetRunContext(ctx, latestRun(t, s))
	require.NoError(t, err)
	assert.Contains(t, runCtx.Prompt, "submitted by user4")
	assert.NotContains(t, runCtx.Prompt, "draft of user4", "a pending review is only visible to its author")
}

func TestResumeContext(t *testing.T) {
	prepare(t)
	ctx := t.Context()
	alice := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	codex := newAgent(t, "codex")
	setKey(t, codex, alice, "sk-alice")
	pull := unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{ID: 3})

	HandleMention(ctx, alice, pull, 0, "@codex explain this change")
	s := unittest.AssertExistsAndLoadBean(t, &agents_model.Session{AgentID: codex.ID})
	r := latestRun(t, s)

	runCtx, err := GetRunContext(ctx, r)
	require.NoError(t, err)
	assert.False(t, runCtx.Resume)
	assert.Contains(t, runCtx.Prompt, "You are Codex")
	assert.Contains(t, runCtx.Prompt, "## Request from @user2")
	assert.NotContains(t, runCtx.Prompt, "sk-alice")
	assert.Equal(t, "gpt-5-codex", runCtx.Model)

	require.NoError(t, AppendEvents(ctx, r, []string{`{"type":"thread.started","thread_id":"thread-1"}`, `not json`, `{"type":"thread.started","thread_id":"-cmodel=x"}`}))
	unittest.AssertCount(t, &agents_model.Event{RunID: r.ID}, 2)
	s = unittest.AssertExistsAndLoadBean(t, &agents_model.Session{ID: s.ID})
	assert.Equal(t, "thread-1", s.CodexThreadID)
	assert.Equal(t, agents_model.StatusRunning, unittest.AssertExistsAndLoadBean(t, &agents_model.Run{ID: r.ID}).Status)

	s.HasState = true
	require.NoError(t, agents_model.UpdateSession(ctx, s, "has_state"))
	runCtx, err = GetRunContext(ctx, r)
	require.NoError(t, err)
	assert.True(t, runCtx.Resume)
	assert.Equal(t, "thread-1", runCtx.ThreadID)
	assert.NotContains(t, runCtx.Prompt, "You are Codex", "a resumed conversation already has the instructions")
	assert.Contains(t, runCtx.FreshPrompt, "You are Codex")

	t.Run("UsersPickTheirModelOnlyWhenAllowed", func(t *testing.T) {
		require.NoError(t, UpdateAgent(ctx, codex, AgentOptions{Name: "codex", Provider: agents_model.ProviderOpenAI, Model: "gpt-5-codex", AllowModelOverride: true}))
		_, err := SetUserKey(ctx, codex, alice, KeyOptions{Model: "gpt-5-mini"})
		require.NoError(t, err)
		runCtx, err := GetRunContext(ctx, r)
		require.NoError(t, err)
		assert.Equal(t, "gpt-5-mini", runCtx.Model)

		require.NoError(t, UpdateAgent(ctx, codex, AgentOptions{Name: "codex", Provider: agents_model.ProviderOpenAI, Model: "gpt-5-codex"}))
		runCtx, err = GetRunContext(ctx, r)
		require.NoError(t, err)
		assert.Equal(t, "gpt-5-codex", runCtx.Model)
	})
}

func TestPublishKeepsOnePullRequest(t *testing.T) {
	prepare(t)
	ctx := t.Context()
	alice := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	bob := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 4})
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	codex := newAgent(t, "codex")
	setKey(t, codex, alice, "sk-alice")
	setKey(t, codex, bob, "sk-bob")

	s, err := StartTaskSession(ctx, alice, repo, StartOptions{Agent: codex, Prompt: "Improve the docs"})
	require.NoError(t, err)
	r, _ := startRun(t, latestRun(t, s))
	require.Equal(t, s.Branch, r.PushBranch)

	_, err = Publish(ctx, r, PublishOptions{Progress: "- [ ] step"}, false)
	require.NoError(t, err)
	unittest.AssertNotExistsBean(t, &issues_model.PullRequest{HeadBranch: s.Branch}) // nothing was pushed yet

	// the driver pushes its first checkpoint
	branch2, err := git.GetBranchCommitID(ctx, repo, "branch2")
	require.NoError(t, err)
	require.NoError(t, git.UpdateRef(ctx, repo, git.BranchPrefix+s.Branch, branch2))

	pullURL, err := Publish(ctx, r, PublishOptions{Progress: "- [x] step\n- [ ] tests"}, false)
	require.NoError(t, err)
	pr := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{HeadBranch: s.Branch, BaseBranch: "master"})
	require.NoError(t, pr.LoadIssue(ctx))
	assert.Equal(t, fmt.Sprintf("%s/pulls/%d", repo.HTMLURL(), pr.Index), pullURL)
	assert.True(t, issues_model.HasWorkInProgressPrefix(pr.Issue.Title), "the pull request is a draft while the agent works")
	assert.Contains(t, pr.Issue.Content, "- [ ] tests")
	assert.Equal(t, codex.BotID, pr.Issue.PosterID, "the agent opens the pull request under its own identity")

	_, err = Publish(ctx, r, PublishOptions{Progress: "- [x] step\n- [x] tests", Summary: "Docs improved."}, true)
	require.NoError(t, err)
	unittest.AssertCount(t, &issues_model.PullRequest{HeadBranch: s.Branch}, 1)
	issue := unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{ID: pr.IssueID})
	assert.False(t, issues_model.HasWorkInProgressPrefix(issue.Title))
	assert.Contains(t, issue.Content, "Docs improved.")
	assert.Equal(t, pr.IssueID, unittest.AssertExistsAndLoadBean(t, &agents_model.Session{ID: s.ID}).IssueID, "mentions on the pull request continue this session")
	unittest.AssertExistsAndLoadBean(t, &issues_model.Review{IssueID: pr.IssueID, ReviewerID: alice.ID, Type: issues_model.ReviewTypeRequest})
	endRun(t, r)
	_, err = Publish(ctx, r, PublishOptions{Progress: "late"}, false)
	require.ErrorIs(t, err, util.ErrInvalidArgument, "a finished run publishes nothing")

	t.Run("ParticipantsReviewWithoutMentioning", func(t *testing.T) {
		comment := &issues_model.Comment{ID: 1}
		(&notifier{}).PullRequestReview(ctx, pr, &issues_model.Review{IssueID: pr.IssueID, ReviewerID: bob.ID, Reviewer: bob, Type: issues_model.ReviewTypeReject, Content: "spend her tokens"}, comment, nil)
		unittest.AssertCount(t, &agents_model.Run{SessionID: s.ID}, 1) // bob never talked to the agent here

		(&notifier{}).PullRequestReview(ctx, pr, &issues_model.Review{IssueID: pr.IssueID, ReviewerID: alice.ID, Reviewer: alice, Type: issues_model.ReviewTypeReject, Content: "Rename the helper"}, comment, nil)
		latest := latestRun(t, s)
		assert.Equal(t, "Rename the helper", latest.Prompt)
		assert.Equal(t, alice.ID, latest.TriggerUserID)
	})

	t.Run("ClosingThePullRequestStopsTheSession", func(t *testing.T) {
		latest := latestRun(t, s)
		require.False(t, latest.Status.IsDone())
		(&notifier{}).IssueChangeStatus(ctx, alice, "", issue, nil, true)
		assert.Equal(t, agents_model.StatusCancelled, latestRun(t, s).Status)

		issue.IsClosed = true
		require.NoError(t, issues_model.UpdateIssueCols(ctx, issue, "is_closed"))
		next, err := SendInstruction(ctx, s, alice, "one more thing")
		require.NoError(t, err)
		next = unittest.AssertExistsAndLoadBean(t, &agents_model.Run{ID: next.ID})
		assert.False(t, next.CanPush(), "a push would open another pull request")
	})
}

func TestAssignIssue(t *testing.T) {
	prepare(t)
	ctx := t.Context()
	alice := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	bob := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 4})
	codex := newAgent(t, "codex")
	setKey(t, codex, alice, "sk-alice")
	issue := unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{ID: 1})
	require.NoError(t, issue.LoadRepo(ctx))

	ok, err := access_model.CanBeAssigned(ctx, codex.Bot, issue.Repo)
	require.NoError(t, err)
	assert.True(t, ok, "agents can be assigned wherever sessions can run")

	(&notifier{}).IssueChangeAssignee(ctx, bob, issue, codex.Bot, false, nil)
	unittest.AssertNotExistsBean(t, &agents_model.Session{AgentID: codex.ID}) // bob has no key

	(&notifier{}).IssueChangeAssignee(ctx, alice, issue, codex.Bot, false, nil)
	s := unittest.AssertExistsAndLoadBean(t, &agents_model.Session{AgentID: codex.ID, IssueID: issue.ID})
	assert.Equal(t, agents_model.SessionKindIssue, s.Kind)
	assert.Equal(t, "agents/codex/issue-1", s.Branch)
	assert.Equal(t, alice.ID, latestRun(t, s).TriggerUserID)

	(&notifier{}).IssueChangeAssignee(ctx, alice, issue, codex.Bot, true, nil)
	assert.Equal(t, agents_model.StatusCancelled, latestRun(t, s).Status, "unassigning the agent stops it")
}

func TestCheckAgentPush(t *testing.T) {
	prepare(t)
	ctx := t.Context()
	alice := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	codex := newAgent(t, "codex")
	setKey(t, codex, alice, "sk-alice")
	s, err := StartTaskSession(ctx, alice, repo, StartOptions{Agent: codex, Prompt: "task"})
	require.NoError(t, err)
	task := claimTask(t, latestRun(t, s))
	_, _, err = agents_model.TaskSecrets(ctx, task)
	require.NoError(t, err)
	r := latestRun(t, s)

	bot := actingBot(s, r)
	assert.NoError(t, CheckAgentPush(ctx, bot, repo.ID, git.RefNameFromBranch(s.Branch), false))
	assert.Error(t, CheckAgentPush(ctx, bot, repo.ID, git.RefNameFromBranch(s.Branch), true))
	assert.Error(t, CheckAgentPush(ctx, bot, repo.ID, git.RefNameFromBranch("master"), false))
	assert.Error(t, CheckAgentPush(ctx, bot, repo.ID, git.RefNameFromTag("v1"), false))
	assert.Error(t, CheckAgentPush(ctx, bot, 2, git.RefNameFromBranch(s.Branch), false), "only in the run's repository")
	assert.Error(t, CheckAgentPush(ctx, codex.Bot, repo.ID, git.RefNameFromBranch(s.Branch), false), "a bot pushes only with a run token")
	assert.Error(t, CheckAgentPush(ctx, user_model.NewActionsUserWithTaskID(task.ID), repo.ID, git.RefNameFromBranch(s.Branch), false), "the Actions token of an agent job never pushes")
	assert.NoError(t, CheckAgentPush(ctx, user_model.NewActionsUserWithTaskID(47), repo.ID, git.RefNameFromBranch("master"), false))
	assert.NoError(t, CheckAgentPush(ctx, alice, repo.ID, git.RefNameFromBranch("master"), false))

	require.NoError(t, Finish(ctx, r, FinishOptions{Status: "success"}))
	assert.Error(t, CheckAgentPush(ctx, bot, repo.ID, git.RefNameFromBranch(s.Branch), false), "the token is useless once the run ended")
}

func TestCancelSession(t *testing.T) {
	prepare(t)
	ctx := t.Context()
	alice := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	codex := newAgent(t, "codex")
	setKey(t, codex, alice, "sk-alice")
	s, err := StartTaskSession(ctx, alice, repo, StartOptions{Agent: codex, Prompt: "task"})
	require.NoError(t, err)
	_, err = SendInstruction(ctx, s, alice, "and then this")
	require.NoError(t, err)

	require.NoError(t, CancelSession(ctx, s))
	runs, err := db.Find[agents_model.Run](ctx, agents_model.FindRunsOptions{SessionID: s.ID})
	require.NoError(t, err)
	require.Len(t, runs, 2)
	for _, r := range runs {
		assert.Equal(t, agents_model.StatusCancelled, r.Status)
		assert.Empty(t, r.Error)
	}
	assert.True(t, unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRun{ID: runs[0].ActionRunID}).Status.IsDone())
	assert.Equal(t, agents_model.StatusCancelled, unittest.AssertExistsAndLoadBean(t, &agents_model.Session{ID: s.ID}).Status)

	user4 := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 4})
	setKey(t, codex, user4, "sk-4")
	_, err = ResumeSession(ctx, s, user4)
	assert.Error(t, err, "an interrupted request is only resumed by who made it")
	_, err = RetryRun(ctx, s, user4, runs[0].ID)
	assert.Error(t, err, "a prompt is only retried by who wrote it")
	_, err = ResumeSession(ctx, s, alice)
	assert.NoError(t, err)
	require.NoError(t, CancelSession(ctx, s))
	_, err = RetryRun(ctx, s, alice, runs[0].ID)
	assert.NoError(t, err)
}

// statusNotifier forwards the Actions run status updates to the agents like the registered notifier, only while a test enables it
type statusNotifier struct {
	notify_service.NullNotifier
}

var (
	registerStatusNotifier sync.Once
	statusNotifierEnabled  atomic.Bool
)

func (statusNotifier) WorkflowRunStatusUpdate(ctx context.Context, repo *repo_model.Repository, doer *user_model.User, run *actions_model.ActionRun) {
	if statusNotifierEnabled.Load() {
		(&notifier{}).WorkflowRunStatusUpdate(ctx, repo, doer, run)
	}
}

func TestCancelSessionWithStatusNotifier(t *testing.T) {
	prepare(t)
	registerStatusNotifier.Do(func() { notify_service.RegisterNotifier(&statusNotifier{}) })
	statusNotifierEnabled.Store(true)
	defer statusNotifierEnabled.Store(false)
	alice := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	codex := newAgent(t, "codex")
	setKey(t, codex, alice, "sk-alice")
	s, err := StartTaskSession(t.Context(), alice, repo, StartOptions{Agent: codex, Prompt: "task"})
	require.NoError(t, err)
	r := latestRun(t, s)
	require.Equal(t, agents_model.StatusWaiting, r.Status)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	require.NoError(t, CancelSession(ctx, s))
	require.NoError(t, ctx.Err(), "cancelling the Actions run dispatches again, it must not wait for the lock held by the cancellation")
	assert.Equal(t, agents_model.StatusCancelled, unittest.AssertExistsAndLoadBean(t, &agents_model.Run{ID: r.ID}).Status)
	assert.True(t, unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRun{ID: r.ActionRunID}).Status.IsDone())
}

func TestEndpointPolicy(t *testing.T) {
	prepare(t)
	for _, u := range []string{"http://127.0.0.1:8080/v1", "https://10.0.0.1/v1", "https://user:pass@example.com/v1", "ftp://example.com", "https://example.com/v1?x=1"} {
		_, err := NormalizeBaseURL(agents_model.ProviderCustom, u)
		assert.Error(t, err, u)
	}
	got, err := NormalizeBaseURL(agents_model.ProviderOpenAI, "")
	require.NoError(t, err)
	assert.Equal(t, agents_model.OpenAIBaseURL, got)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/responses", r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer good-key" {
			http.Error(w, `{"error":"bad key good-key"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\"}\n\n")
	}))
	defer server.Close()

	result := CheckConnection(t.Context(), server.URL+"/v1", "m", "good-key")
	assert.False(t, result.OK, "loopback endpoints need an explicit allow entry")

	defer test.MockVariableValue(&setting.Agents.AllowedHostList, "loopback")()
	assert.True(t, CheckConnection(t.Context(), server.URL+"/v1", "m", "good-key").OK)
	result = CheckConnection(t.Context(), server.URL+"/v1", "m", "wrong-key")
	assert.False(t, result.OK)
	assert.NotContains(t, result.Message, "wrong-key")
}

func TestToEventView(t *testing.T) {
	view := func(payload string) *EventView {
		return ToEventView(&agents_model.Event{ID: 1, RunID: 2, Payload: payload})
	}
	cmd := view(`{"type":"item.completed","item":{"id":"i1","type":"command_execution","command":"go test","aggregated_output":"ok","exit_code":1,"status":"failed"}}`)
	assert.Equal(t, &EventView{ID: 1, RunID: 2, Key: "run-2-i1", Kind: "command", Title: "go test", Body: "ok", Status: "exit 1"}, cmd)
	plan := view(`{"type":"item.updated","item":{"id":"i2","type":"todo_list","items":[{"text":"a","completed":true}]}}`)
	assert.Equal(t, []TodoView{{Text: "a", Completed: true}}, plan.Todos)
	files := view(`{"type":"item.completed","item":{"id":"i3","type":"file_change","changes":[{"path":"a.go","kind":"update"}],"diff":"+x"}}`)
	assert.True(t, files.Diff)
	assert.Equal(t, "+x", files.Body)
	assert.Nil(t, view(`{"type":"unknown"}`))
	_, err := json.Marshal(cmd)
	assert.NoError(t, err)
	assert.True(t, strings.HasPrefix(view(`{"type":"thread.started","thread_id":"x"}`).Key, "event-"))
}

func TestProxyModel(t *testing.T) {
	prepare(t)
	ctx := t.Context()
	var got *http.Request
	var gotBody map[string]any
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, gotBody = r, nil
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {}\n\n"))
	}))
	defer provider.Close()
	defer test.MockVariableValue(&setting.Agents.AllowedHostList, "loopback")()
	alice := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	codex, err := CreateAgent(ctx, AgentOptions{BotName: "codex", Name: "codex", Provider: agents_model.ProviderCustom, BaseURL: provider.URL + "/v1", Model: "m"})
	require.NoError(t, err)
	setKey(t, codex, alice, "sk-alice")
	s, err := StartTaskSession(ctx, alice, repo, StartOptions{Agent: codex, Prompt: "a"})
	require.NoError(t, err)
	r, _ := startRun(t, latestRun(t, s))

	header := http.Header{"Content-Type": {"application/json"}, "Authorization": {"Bearer model-token"}, "Cookie": {"c"}}
	post := func(path, body string) error {
		resp, err := ProxyModel(ctx, r, http.MethodPost, path, header, strings.NewReader(body))
		if err == nil {
			resp.Body.Close()
		}
		return err
	}
	codexBody := `{"model":"gpt-other","tools":[{"type":"function","name":"shell"},{"type":"custom","name":"apply_patch"},{"type":"namespace","tools":[{"type":"function"}]}],` +
		`"input":[{"type":"message","content":[{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}]}`
	require.NoError(t, post("responses", codexBody))
	assert.Equal(t, "/v1/responses", got.URL.Path)
	assert.Equal(t, "Bearer sk-alice", got.Header.Get("Authorization"))
	assert.Empty(t, got.Header.Get("Cookie"))
	assert.Equal(t, "m", gotBody["model"], "the run's model, whatever the job asks")
	assert.Len(t, gotBody["tools"], 3)
	assert.EqualValues(t, setting.Agents.MaxModelOutputTokens, gotBody["max_output_tokens"])
	assert.Equal(t, false, gotBody["store"])
	assert.Equal(t, false, gotBody["background"])
	require.NoError(t, post("responses", `{"max_output_tokens":1000000,"store":true,"background":true,"conversation":"c","previous_response_id":"r"}`))
	assert.EqualValues(t, setting.Agents.MaxModelOutputTokens, gotBody["max_output_tokens"])
	assert.Equal(t, false, gotBody["store"])
	assert.Equal(t, false, gotBody["background"])
	assert.NotContains(t, gotBody, "conversation")
	assert.NotContains(t, gotBody, "previous_response_id")
	require.NoError(t, post("responses", `{"max_output_tokens":100}`))
	assert.EqualValues(t, 100, gotBody["max_output_tokens"])
	require.NoError(t, post("responses/compact", `{"model":"gpt-other","input":[],"store":true,"max_output_tokens":1000000}`))
	assert.Equal(t, map[string]any{"model": "m", "input": []any{}}, gotBody)
	assert.Equal(t, "m", gotBody["model"])

	require.NoError(t, post("responses", `{"tools":[{"type":"web_search"},{"type":"mcp","server_url":"https://example.com"},`+
		`{"type":"namespace","tools":[{"type":"code_interpreter"},{"type":"function"}]}],"tool_choice":{"type":"web_search"}}`))
	assert.Equal(t, []any{map[string]any{"type": "namespace", "tools": []any{map[string]any{"type": "function"}}}}, gotBody["tools"], "hosted tools are dropped")
	assert.Equal(t, "auto", gotBody["tool_choice"])

	for _, body := range []string{
		`not json`,
		`null`,
		`{} {}`,
		`{"tools":"file_search"}`,
		`{"input":[{"type":"message","content":[{"type":"input_image","image_url":"https://example.com/a.png"}]}]}`,
		`{"input":[{"type":"function_call_output","output":[{"type":"input_file","file_url":"HTTP://example.com/a.pdf"}]}]}`,
	} {
		assert.ErrorIs(t, post("responses", body), util.ErrInvalidArgument, body)
	}

	refused := func(method, path string) {
		resp, err := ProxyModel(ctx, r, method, path, header, nil)
		if err == nil {
			resp.Body.Close()
		}
		assert.ErrorIs(t, err, util.ErrPermissionDenied, path)
	}
	for _, path := range []string{"../admin", "files", "fine_tuning/jobs", "responses/../files"} {
		refused(http.MethodPost, path)
	}
	refused(http.MethodGet, "responses")
}

func TestWorkflowsKept(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@a", "GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@a")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
		return strings.TrimSpace(string(out))
	}
	commit := func(path, content string) string {
		if content == "" {
			run("rm", "-q", path)
		} else {
			require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(dir, path)), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(dir, path), []byte(content), 0o644))
			run("add", ".")
		}
		run("commit", "-qm", path)
		return run("rev-parse", "HEAD")
	}
	run("init", "-q", "-b", "main")
	commit(".gitea/workflows/ci.yml", "on: push")
	base := commit(".gitea/workflows/lint.yml", "on: push")
	target := commit(".gitea/workflows/lint.yml", "on: pull_request")
	run("checkout", "-q", "-b", "session", base)
	old := commit(".gitea/workflows/ci.yml", "on: [push, pull_request]")
	code := commit("main.go", "package main")
	run("merge", "-q", "--no-edit", "main")
	merge := run("rev-parse", "HEAD")
	changed := commit(".gitea/workflows/ci.yml", "on: workflow_dispatch")
	run("reset", "-q", "--hard", merge)
	added := commit(".github/workflows/new.yml", "on: push")
	run("reset", "-q", "--hard", merge)
	deleted := commit(".gitea/workflows/lint.yml", "")

	gitRepo, err := git.OpenRepositoryLocal(t.Context(), dir)
	require.NoError(t, err)
	defer gitRepo.Close()
	blobs := func(ref string) map[string]string {
		b, err := workflowBlobs(t.Context(), gitRepo, nil, []string{".gitea/workflows", ".github/workflows"}, ref)
		require.NoError(t, err)
		return b
	}
	assert.Len(t, blobs(base), 2)
	assert.Equal(t, blobs(old), blobs(code), "other files do not count")
	sides := []map[string]string{blobs(old), blobs(target)}
	assert.True(t, workflowsKept(blobs(code), sides))
	assert.True(t, workflowsKept(blobs(merge), sides), "each side changed a different workflow")
	assert.False(t, workflowsKept(blobs(changed), sides))
	assert.False(t, workflowsKept(blobs(added), sides))
	assert.False(t, workflowsKept(blobs(deleted), sides))
	assert.True(t, workflowsKept(blobs(deleted), append(sides, map[string]string{})), "missing on a side counts as kept")
	assert.False(t, workflowsKept(blobs(code), nil))
}

func TestProxyModelLimits(t *testing.T) {
	prepare(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		assert.NoError(t, http.NewResponseController(w).Flush())
		<-req.Context().Done()
	}))
	defer provider.Close()
	defer test.MockVariableValue(&setting.Agents.AllowedHostList, "loopback")()
	defer test.MockVariableValue(&setting.Agents.MaxModelRequestsPerRun, 2)()
	alice := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	agent, err := CreateAgent(ctx, AgentOptions{BotName: "limited", Name: "limited", Provider: agents_model.ProviderCustom, BaseURL: provider.URL, Model: "m"})
	require.NoError(t, err)
	setKey(t, agent, alice, "test-key")
	s, err := StartTaskSession(ctx, alice, repo, StartOptions{Agent: agent, Prompt: "a"})
	require.NoError(t, err)
	r, _ := startRun(t, latestRun(t, s))
	request := func() (*http.Response, error) {
		return ProxyModel(ctx, r, http.MethodPost, "responses", http.Header{}, strings.NewReader(`{"input":"x"}`))
	}
	refused := func() error {
		resp, err := request()
		if resp != nil {
			_ = resp.Body.Close()
		}
		return err
	}
	first, err := request()
	require.NoError(t, err)
	defer first.Body.Close()
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			err := refused()
			assert.ErrorIs(t, err, util.ErrPermissionDenied)
		})
	}
	wg.Wait()
	require.NoError(t, first.Body.Close())
	assert.EqualValues(t, 1, unittest.AssertExistsAndLoadBean(t, &agents_model.Run{ID: r.ID}).ModelRequests)
	second, err := request()
	require.NoError(t, err)
	defer second.Body.Close()
	require.NoError(t, second.Body.Close())
	err = refused()
	require.ErrorIs(t, err, util.ErrPermissionDenied, "closing a response does not reset the budget")
	defer test.MockVariableValue(&setting.Agents.MaxModelRequestsPerRun, 3)()
	third, err := request()
	require.NoError(t, err)
	defer third.Body.Close()
	cancelled, err := agents_model.UpdateRunStatus(ctx, r, agents_model.StatusCancelled)
	require.NoError(t, err)
	require.True(t, cancelled)
	_, err = io.ReadAll(third.Body)
	require.Error(t, err, "in-flight requests stop when another process cancels the run")
	require.NoError(t, third.Body.Close())
	err = refused()
	assert.ErrorIs(t, err, util.ErrPermissionDenied)
}
