// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	actions_model "gitea.dev/models/actions"
	agents_model "gitea.dev/models/agents"
	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/agents"
	"gitea.dev/modules/git"
	"gitea.dev/modules/json"
	"gitea.dev/modules/log"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/util"
	actions_service "gitea.dev/services/actions"
)

// ErrNotAllowed is returned when a run must not start; callers must not tell the requester why.
var ErrNotAllowed = util.NewPermissionDeniedErrorf("the agent cannot be started for this request")

type StartRunOptions struct {
	Agent     *agents_model.Agent
	Repo      *repo_model.Repository
	Issue     *issues_model.Issue // the issue or pull request, nil for a run on the repository itself
	Doer      *user_model.User    // the person who triggered the run
	Event     agents.Event
	CommentID int64 // the comment that asked for the run, if any
}

// SessionBranch is the only branch family a session may push to.
func SessionBranch(sessionID int64) string {
	return "agent/" + strconv.FormatInt(sessionID, 10) + "/work"
}

// StartRun checks that the request is allowed, then creates the session and run records and the Actions run that
// executes them. The Actions run is built from a generated workflow and no file is written in the repository.
func StartRun(ctx context.Context, opts StartRunOptions) (*agents_model.Run, error) {
	settings := agents.GetSettings(ctx)
	if !settings.Enabled {
		return nil, ErrNotAllowed
	}
	if !setting.Actions.Enabled {
		return nil, errors.New("StartRun: agents need Gitea Actions, which is disabled in app.ini")
	}
	agent, repo, doer := opts.Agent, opts.Repo, opts.Doer
	if !agents.Accepts(agent.Action, agent.Events, opts.Event) {
		return nil, ErrNotAllowed
	}
	if ok, err := CanCommand(ctx, agent, repo, doer); err != nil {
		return nil, err
	} else if !ok {
		return nil, ErrNotAllowed
	}
	bot, err := user_model.GetUserByID(ctx, agent.BotUserID)
	if err != nil {
		return nil, fmt.Errorf("StartRun: bot: %w", err)
	}
	if err := ensureBotCanWrite(ctx, repo, bot); err != nil {
		return nil, err
	}

	var issueID int64
	if opts.Issue != nil {
		issueID = opts.Issue.ID
	}
	session, err := agents_model.GetSessionBySubject(ctx, agent.ID, issueID)
	if errors.Is(err, util.ErrNotExist) || issueID == 0 { // a run on the repository itself always opens a new session
		session = &agents_model.Session{AgentID: agent.ID, RepoID: repo.ID, IssueID: issueID, TriggerUserID: doer.ID}
		err = agents_model.CreateSession(ctx, session)
	}
	if err != nil {
		return nil, fmt.Errorf("StartRun: session: %w", err)
	}

	configSHA, err := configCommitSHA(ctx, bot)
	if err != nil {
		return nil, fmt.Errorf("StartRun: %w", err)
	}
	run := &agents_model.Run{
		SessionID: session.ID, AgentID: agent.ID, RepoID: repo.ID, IssueID: issueID,
		TriggerEvent: string(opts.Event), TriggerUserID: doer.ID, TriggerCommentID: opts.CommentID, ConfigCommitSHA: configSHA,
	}
	if err := agents_model.CreateRun(ctx, run); err != nil {
		return nil, fmt.Errorf("StartRun: %w", err)
	}

	actionRun, err := createActionsRun(ctx, settings, run, agent, repo, doer, opts.Issue)
	if err != nil {
		if _, finishErr := agents_model.FinishRun(ctx, run.ID, agents_model.RunFailed, "", "could not create the Actions run: "+err.Error(), "", ""); finishErr != nil {
			log.Error("StartRun: FinishRun: %v", finishErr)
		}
		return nil, fmt.Errorf("StartRun: %w", err)
	}
	if err := agents_model.SetRunActionRun(ctx, run.ID, actionRun.ID); err != nil {
		return nil, fmt.Errorf("StartRun: %w", err)
	}
	run.ActionRunID = actionRun.ID
	return run, nil
}

func configCommitSHA(ctx context.Context, bot *user_model.User) (string, error) {
	cfgRepo, err := repo_model.GetRepositoryByName(ctx, bot.ID, ConfigRepoName)
	if err != nil {
		return "", fmt.Errorf("configuration repository of %s: %w", bot.Name, err)
	}
	return git.GetBranchCommitID(ctx, cfgRepo, cfgRepo.DefaultBranch)
}

func createActionsRun(ctx context.Context, settings agents.Settings, run *agents_model.Run, agent *agents_model.Agent, repo *repo_model.Repository, doer *user_model.User, issue *issues_model.Issue) (*actions_model.ActionRun, error) {
	content, err := BuildWorkflow(run.ID, settings)
	if err != nil {
		return nil, err
	}
	headSHA, err := git.GetBranchCommitID(ctx, repo, repo.DefaultBranch)
	if err != nil {
		return nil, fmt.Errorf("default branch of %s: %w", repo.FullName(), err)
	}
	title := agent.Name
	if issue != nil {
		title += ": " + issue.Title // plain data for display only, it never reaches the workflow
	}
	payload, err := json.Marshal(map[string]int64{"agent_run_id": run.ID})
	if err != nil {
		return nil, err
	}
	// The event is a workflow dispatch, which creates no commit status. Nothing here is read from git later: the
	// workflow is stored with the run, so reruns and the "workflow file" link of the run page need no file either.
	actionRun := &actions_model.ActionRun{
		Title:             title,
		RepoID:            repo.ID,
		Repo:              repo,
		OwnerID:           repo.OwnerID,
		WorkflowID:        WorkflowIDPrefix + strconv.FormatInt(run.ID, 10) + ".yml",
		TriggerUserID:     doer.ID,
		TriggerUser:       doer,
		Ref:               string(git.RefNameFromBranch(repo.DefaultBranch)),
		CommitSHA:         headSHA,
		Event:             "workflow_dispatch",
		TriggerEvent:      "workflow_dispatch",
		EventPayload:      string(payload),
		Status:            actions_model.StatusWaiting,
		WorkflowRepoID:    repo.ID,
		WorkflowCommitSHA: headSHA,
	}
	if err := actions_service.PrepareRunAndInsert(ctx, content, actionRun, nil); err != nil {
		return nil, err
	}
	return actionRun, nil
}
