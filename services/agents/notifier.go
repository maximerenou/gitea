// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"context"
	"errors"

	actions_model "gitea.dev/models/actions"
	agents_model "gitea.dev/models/agents"
	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/agents"
	"gitea.dev/modules/graceful"
	"gitea.dev/modules/log"
	"gitea.dev/modules/queue"
	"gitea.dev/modules/util"
	notify_service "gitea.dev/services/notify"
)

// QueueItem is one request to start an agent; the notifier only enqueues, the work happens in the queue worker.
type QueueItem struct {
	AgentID   int64        `json:"agent_id"`
	IssueID   int64        `json:"issue_id"`
	DoerID    int64        `json:"doer_id"`
	Event     agents.Event `json:"event"`
	CommentID int64        `json:"comment_id,omitempty"`
}

var requestQueue *queue.WorkerPoolQueue[*QueueItem]

// Init registers the notifier and creates the queue. Registering costs nothing while agents are disabled: every
// handler returns before doing any work.
func Init(ctx context.Context) error {
	requestQueue = queue.CreateSimpleQueue(graceful.GetManager().ShutdownContext(), "agents_requests", handleQueueItems)
	if requestQueue == nil {
		return errors.New("unable to create the agents_requests queue")
	}
	go graceful.GetManager().RunWithCancel(requestQueue)
	notify_service.RegisterNotifier(&notifier{})
	return nil
}

// FlushQueue waits until the queued requests are processed; for tests.
func FlushQueue(ctx context.Context) error {
	return requestQueue.FlushWithContext(ctx, 0)
}

func handleQueueItems(items ...*QueueItem) (unhandled []*QueueItem) {
	ctx := graceful.GetManager().ShutdownContext()
	for _, item := range items {
		if err := ProcessQueueItem(ctx, item); err != nil && !errors.Is(err, ErrNotAllowed) {
			log.Error("agents: processing %+v: %v", *item, err)
		}
	}
	return nil // a failed request is logged, never retried: the person can ask again
}

// ProcessQueueItem loads what a request refers to and starts the run if it is allowed.
func ProcessQueueItem(ctx context.Context, item *QueueItem) error {
	agent, err := agents_model.GetAgentByID(ctx, item.AgentID)
	if err != nil {
		return err
	}
	issue, err := issues_model.GetIssueByID(ctx, item.IssueID)
	if err != nil {
		return err
	}
	if err := issue.LoadRepo(ctx); err != nil {
		return err
	}
	doer, err := user_model.GetUserByID(ctx, item.DoerID)
	if err != nil {
		return err
	}
	_, err = StartRun(ctx, StartRunOptions{Agent: agent, Repo: issue.Repo, Issue: issue, Doer: doer, Event: item.Event, CommentID: item.CommentID})
	return err
}

type notifier struct {
	notify_service.NullNotifier
}

func (*notifier) PullRequestReviewRequest(ctx context.Context, doer *user_model.User, issue *issues_model.Issue, reviewer *user_model.User, isRequest bool, _ *issues_model.Comment) {
	if !isRequest || doer == nil || reviewer == nil || !reviewer.IsTypeBot() || !agents.IsEnabled(ctx) {
		return
	}
	agent, err := agents_model.GetAgentByBotUserID(ctx, reviewer.ID)
	if err != nil {
		if !errors.Is(err, util.ErrNotExist) {
			log.Error("agents: GetAgentByBotUserID: %v", err)
		}
		return
	}
	if err := requestQueue.Push(&QueueItem{AgentID: agent.ID, IssueID: issue.ID, DoerID: doer.ID, Event: agents.EventReviewRequested}); err != nil {
		log.Error("agents: enqueue: %v", err)
	}
}

// WorkflowRunStatusUpdate ends the agent run and its tokens when the Actions run ends without having reported a result.
func (*notifier) WorkflowRunStatusUpdate(ctx context.Context, _ *repo_model.Repository, _ *user_model.User, run *actions_model.ActionRun) {
	if !run.Status.IsDone() || !agents.IsEnabled(ctx) {
		return
	}
	agentRun, err := agents_model.GetRunByActionRunID(ctx, run.ID)
	if err != nil {
		if !errors.Is(err, util.ErrNotExist) {
			log.Error("agents: GetRunByActionRunID: %v", err)
		}
		return
	}
	if err := agents_model.RevokeSessionTokensOfRun(ctx, run.ID); err != nil {
		log.Error("agents: RevokeSessionTokensOfRun: %v", err)
	}
	if agentRun.Status.IsTerminal() {
		return
	}
	status, msg := agents_model.RunFailed, "the job ended without reporting a result"
	if run.Status == actions_model.StatusCancelled {
		status, msg = agents_model.RunCancelled, ""
	}
	if _, err := agents_model.FinishRun(ctx, agentRun.ID, status, "", msg, "", ""); err != nil {
		log.Error("agents: FinishRun: %v", err)
	}
}
