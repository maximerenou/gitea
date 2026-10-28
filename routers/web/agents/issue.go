// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"errors"
	"fmt"
	"strings"

	agents_model "gitea.dev/models/agents"
	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unit"
	"gitea.dev/modules/log"
	agents_service "gitea.dev/services/agents"
	"gitea.dev/services/context"
)

// PrepareIssueView lists the agent sessions working on an issue or pull request, for its sidebar
func PrepareIssueView(ctx *context.Context, issue *issues_model.Issue) {
	if !agents_service.IsEnabled() {
		return
	}
	headBranch := ""
	if issue.IsPull && issue.PullRequest != nil && issue.PullRequest.HeadRepoID == issue.RepoID {
		headBranch = issue.PullRequest.HeadBranch
	}
	sessions, err := agents_model.FindIssueSessions(ctx, issue.RepoID, issue.ID, headBranch)
	if err != nil {
		log.Error("agents: sessions of issue %d: %v", issue.ID, err)
		return
	}
	for _, s := range sessions {
		s.Repo = ctx.Repo.Repository
		if err := s.LoadAttributes(ctx); err != nil {
			log.Error("agents: load session %d: %v", s.ID, err)
			return
		}
	}
	ctx.Data["AgentSessions"] = sessions
	var liveURLs []string
	for _, s := range sessions {
		if !s.Status.IsDone() {
			liveURLs = append(liveURLs, s.Link()+"/live")
		}
	}
	ctx.Data["AgentLiveURLs"] = strings.Join(liveURLs, " ")
	if ctx.Repo.Permission.CanRead(unit.TypeCode) && agents_service.IsRepoUsable(ctx, ctx.Repo.Repository) {
		ctx.Data["AgentRepoLiveURL"] = repoAgentsLink(ctx) + "/live" // tells when a session starts here
	}
	if issue.IsPull && issue.PullRequest != nil {
		prepareAgentMerge(ctx, issue.PullRequest)
		prepareAgentReview(ctx, issue.PullRequest)
	}
}

// prepareAgentReview offers to review an open pull request with the agents the user has a key for
func prepareAgentReview(ctx *context.Context, pr *issues_model.PullRequest) {
	if pr.HasMerged || pr.Issue == nil || pr.Issue.IsClosed {
		return
	}
	agents, err := usableAgents(ctx, ctx.Repo.Repository)
	if err != nil || len(agents) == 0 {
		return
	}
	ctx.Data["AgentReviewAgents"] = agents
	ctx.Data["AgentReviewLink"] = fmt.Sprintf("%s/pulls/%d/agent-review", ctx.Repo.RepoLink, pr.Index)
}

// AgentReview asks an agent to review the pull request with the doer's key
func AgentReview(ctx *context.Context) {
	issue, err := issues_model.GetIssueByIndex(ctx, ctx.Repo.Repository.ID, ctx.PathParamInt64("index"))
	if err != nil || !issue.IsPull {
		ctx.NotFound(nil)
		return
	}
	if err := issue.LoadPullRequest(ctx); err != nil {
		ctx.ServerError("LoadPullRequest", err)
		return
	}
	agent, err := agents_model.GetAgentByID(ctx, ctx.FormInt64("agent_id"))
	if err != nil {
		jsonServiceError(ctx, err)
		return
	}
	_, err = agents_service.StartAgentReview(ctx, ctx.Doer, agent, issue.PullRequest)
	if errors.Is(err, agents_service.ErrNoUserKey) {
		ctx.JSONError(ctx.Tr("agents.session.need_key", settingsLink(), agent.MentionName()))
		return
	} else if err != nil {
		jsonServiceError(ctx, err)
		return
	}
	ctx.Flash.Success(ctx.Tr("agents.review.started", agent.MentionName()))
	ctx.JSONRedirect(issue.Link())
}

var mergeStyleLabels = []struct {
	Style repo_model.MergeStyle
	Label string
}{
	{repo_model.MergeStyleMerge, "repo.pulls.merge_pull_request"},
	{repo_model.MergeStyleRebase, "repo.pulls.rebase_merge_pull_request"},
	{repo_model.MergeStyleRebaseMerge, "repo.pulls.rebase_merge_commit_pull_request"},
	{repo_model.MergeStyleSquash, "repo.pulls.squash_merge_pull_request"},
	{repo_model.MergeStyleFastForwardOnly, "repo.pulls.fast_forward_only_merge_pull_request"},
}

// prepareAgentMerge offers to merge the pull request with an agent to the users who can merge it
func prepareAgentMerge(ctx *context.Context, pr *issues_model.PullRequest) {
	if ok, err := agents_service.CanAgentMerge(ctx, ctx.Doer, pr); err != nil || !ok {
		return
	}
	agents, err := usableAgents(ctx, ctx.Repo.Repository)
	if err != nil || len(agents) == 0 {
		return
	}
	cfg := ctx.Repo.Repository.MustGetUnit(ctx, unit.TypePullRequests).PullRequestsConfig()
	styles := make([]map[string]any, 0, len(mergeStyleLabels))
	for _, s := range mergeStyleLabels {
		if cfg.IsMergeStyleAllowed(s.Style) {
			styles = append(styles, map[string]any{"Style": s.Style, "Label": s.Label, "Default": s.Style == cfg.DefaultMergeStyle})
		}
	}
	if len(styles) == 0 {
		return
	}
	ctx.Data["AgentMergeAgents"] = agents
	ctx.Data["AgentMergeStyles"] = styles
	ctx.Data["AgentMergeLink"] = fmt.Sprintf("%s/pulls/%d/agent-merge", ctx.Repo.RepoLink, pr.Index)
}

// AgentMerge asks an agent to bring the pull request up to date and to merge it once its checks pass
func AgentMerge(ctx *context.Context) {
	issue, err := issues_model.GetIssueByIndex(ctx, ctx.Repo.Repository.ID, ctx.PathParamInt64("index"))
	if err != nil || !issue.IsPull {
		ctx.NotFound(nil)
		return
	}
	if err := issue.LoadPullRequest(ctx); err != nil {
		ctx.ServerError("LoadPullRequest", err)
		return
	}
	agent, err := agents_model.GetAgentByID(ctx, ctx.FormInt64("agent_id"))
	if err != nil {
		jsonServiceError(ctx, err)
		return
	}
	_, err = agents_service.StartAgentMerge(ctx, ctx.Doer, issue.PullRequest, agents_service.MergeOptions{
		Agent:  agent,
		Style:  repo_model.MergeStyle(ctx.FormString("merge_style")),
		Review: ctx.FormBool("review"),
	})
	if errors.Is(err, agents_service.ErrNoUserKey) {
		ctx.JSONError(ctx.Tr("agents.session.need_key", settingsLink(), agent.MentionName()))
		return
	} else if err != nil {
		jsonServiceError(ctx, err)
		return
	}
	ctx.Flash.Success(ctx.Tr("agents.merge.started", agent.MentionName()))
	ctx.JSONRedirect(issue.Link())
}
