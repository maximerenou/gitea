// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	actions_model "gitea.dev/models/actions"
	agents_model "gitea.dev/models/agents"
	issues_model "gitea.dev/models/issues"
	access_model "gitea.dev/models/perm/access"
	repo_model "gitea.dev/models/repo"
	system_model "gitea.dev/models/system"
	"gitea.dev/models/unit"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/globallock"
	"gitea.dev/modules/graceful"
	"gitea.dev/modules/log"
	"gitea.dev/modules/references"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/storage"
	"gitea.dev/modules/util"
	issue_service "gitea.dev/services/issue"
	notify_service "gitea.dev/services/notify"
)

type notifier struct {
	notify_service.NullNotifier
}

var _ notify_service.Notifier = &notifier{}

func (n *notifier) CreateIssueComment(ctx context.Context, doer *user_model.User, repo *repo_model.Repository, issue *issues_model.Issue, comment *issues_model.Comment, _ []*user_model.User) {
	HandleMention(ctx, doer, issue, comment.ID, comment.Content)
}

func (n *notifier) NewIssue(ctx context.Context, issue *issues_model.Issue, _ []*user_model.User) {
	if err := issue.LoadPoster(ctx); err != nil {
		log.Error("agents: load poster of issue %d: %v", issue.ID, err)
		return
	}
	HandleMention(ctx, issue.Poster, issue, 0, issue.Content)
}

// DeleteIssue deletes the agent sessions of a deleted issue, their runs cannot report anywhere anymore
func (n *notifier) DeleteIssue(ctx context.Context, _ *user_model.User, issue *issues_model.Issue) {
	if err := cancelRuns(ctx, agents_model.FindRunsOptions{IssueID: issue.ID, Statuses: pendingStatuses}, false); err != nil {
		log.Error("agents: cancel the runs of issue %d: %v", issue.ID, err)
	}
	withState, err := agents_model.DeleteIssueSessions(ctx, issue.ID)
	if err != nil {
		log.Error("agents: delete the sessions of issue %d: %v", issue.ID, err)
		return
	}
	for _, sessionID := range withState {
		system_model.RemoveStorageWithNotice(ctx, storage.Actions, "Delete agent conversation", agents_model.StatePath(sessionID))
	}
	notifyRepo(issue.RepoID)
}

// isSessionPull reports whether an agent session opened the pull request from its agents/<bot>/ branch
func isSessionPull(pr *issues_model.PullRequest) bool {
	return pr.HeadRepoID == pr.BaseRepoID && strings.HasPrefix(pr.HeadBranch, "agents/")
}

func (n *notifier) NewPullRequest(ctx context.Context, pr *issues_model.PullRequest, _ []*user_model.User) {
	autoReview(ctx, pr)
	if isSessionPull(pr) { // its description quotes the request
		return
	}
	if err := pr.LoadIssue(ctx); err != nil {
		log.Error("agents: load issue of pull request %d: %v", pr.ID, err)
		return
	}
	if err := pr.Issue.LoadPoster(ctx); err != nil {
		log.Error("agents: load poster of pull request %d: %v", pr.ID, err)
		return
	}
	HandleMention(ctx, pr.Issue.Poster, pr.Issue, 0, pr.Issue.Content)
}

// IssueChangeTitle renames the conversations on the issue, and reviews a pull request once it is no longer a work in progress
func (n *notifier) IssueChangeTitle(ctx context.Context, _ *user_model.User, issue *issues_model.Issue, oldTitle string) {
	if err := agents_model.UpdateIssueSessionsTitle(ctx, issue.ID, util.EllipsisDisplayString(issue.Title, 255)); err != nil {
		log.Error("agents: rename the sessions of issue %d: %v", issue.ID, err)
	} else {
		notifyRepo(issue.RepoID)
	}
	if !issue.IsPull || !issues_model.HasWorkInProgressPrefix(oldTitle) || issues_model.HasWorkInProgressPrefix(issue.Title) {
		return
	}
	if err := issue.LoadPullRequest(ctx); err != nil {
		log.Error("agents: load pull request of issue %d: %v", issue.ID, err)
		return
	}
	autoReview(ctx, issue.PullRequest)
}

// PullRequestReview handles a review with its comments on lines as one request, mentioning the agent in any of them starts a single run
func (n *notifier) PullRequestReview(ctx context.Context, pr *issues_model.PullRequest, review *issues_model.Review, comment *issues_model.Comment, _ []*user_model.User) {
	if comment == nil || pr.LoadIssue(ctx) != nil || review.LoadReviewer(ctx) != nil {
		return
	}
	content := reviewContent(review)
	if HandleMention(ctx, review.Reviewer, pr.Issue, comment.ID, content) || review.Type == issues_model.ReviewTypeApprove {
		return
	}
	if err := handleSessionPullReview(ctx, review.Reviewer, pr, comment, content); err != nil {
		log.Warn("agents: review of pull request %d: %v", pr.ID, err)
	}
}

func codeCommentContent(c *issues_model.Comment) string {
	return fmt.Sprintf("(on `%s` line %d)\n\n%s", c.TreePath, c.UnsignedLine(), c.Content)
}

// reviewContent is the body of a review followed by its comments on lines
func reviewContent(review *issues_model.Review) string {
	var comments []*issues_model.Comment
	for _, lines := range review.CodeComments {
		for _, lineComments := range lines {
			comments = append(comments, lineComments...)
		}
	}
	slices.SortFunc(comments, func(a, b *issues_model.Comment) int { return cmp.Compare(a.ID, b.ID) })
	parts := make([]string, 0, len(comments)+1)
	if body := strings.TrimSpace(review.Content); body != "" {
		parts = append(parts, body)
	}
	for _, c := range comments {
		parts = append(parts, codeCommentContent(c))
	}
	return strings.Join(parts, "\n\n---\n\n")
}

// handleSessionPullReview lets the participants of a session review its pull request like a colleague's, without mentioning the agent.
// Other reviewers mention the agent explicitly, so nobody spends their key by merely reviewing.
func handleSessionPullReview(ctx context.Context, reviewer *user_model.User, pr *issues_model.PullRequest, comment *issues_model.Comment, content string) error {
	if !IsEnabled() || reviewer == nil || !reviewer.IsIndividual() || pr.HeadRepoID != pr.BaseRepoID {
		return nil
	}
	s, err := agents_model.GetSessionByBranch(ctx, pr.BaseRepoID, pr.HeadBranch)
	if errors.Is(err, util.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if !s.OwnsPull || s.IssueID != pr.IssueID {
		return nil
	}
	if participant, err := agents_model.IsParticipant(ctx, s.ID, reviewer.ID); err != nil || !participant {
		return err
	}
	if strings.TrimSpace(content) == "" {
		content = "Address the review comments."
	}
	if _, err := SendInstruction(ctx, s, reviewer, content); err != nil {
		return err
	}
	acknowledge(ctx, s, pr.Issue, comment.ID)
	return nil
}

func (n *notifier) IssueChangeAssignee(ctx context.Context, doer *user_model.User, issue *issues_model.Issue, assignee *user_model.User, removed bool, _ *issues_model.Comment) {
	if !IsEnabled() || assignee == nil || !assignee.IsTypeBot() || doer == nil || !doer.IsIndividual() {
		return
	}
	agent, err := agents_model.GetAgentByBotID(ctx, assignee.ID)
	if err != nil {
		return // not an agent
	}
	if removed {
		stopAgentOnIssue(ctx, agent, issue)
		return
	}
	agent.Bot = assignee
	prompt := fmt.Sprintf("You were assigned %s #%d. Resolve it.", util.Iif(issue.IsPull, "pull request", "issue"), issue.Index)
	if _, err := startConversationRun(ctx, doer, agent, issue, 0, prompt); errors.Is(err, ErrNoUserKey) {
		replyNoKey(ctx, agent, doer, issue)
	} else if err != nil {
		log.Warn("agents: assignment of @%s by %s on issue %d: %v", assignee.Name, doer.Name, issue.ID, err)
	}
}

// stopAgentOnIssue cancels the runs of an agent unassigned from an issue, it was told to stop working on it
func stopAgentOnIssue(ctx context.Context, agent *agents_model.Agent, issue *issues_model.Issue) {
	s, err := agents_model.GetConversationSession(ctx, agent.ID, issue.ID)
	if errors.Is(err, util.ErrNotExist) {
		return
	} else if err != nil {
		log.Error("agents: session of agent %d on issue %d: %v", agent.ID, issue.ID, err)
		return
	}
	if err := CancelSession(ctx, s); err != nil {
		log.Error("agents: cancel session %d: %v", s.ID, err)
	}
}

func (n *notifier) IssueChangeStatus(ctx context.Context, _ *user_model.User, _ string, issue *issues_model.Issue, _ *issues_model.Comment, closed bool) {
	if closed && issue.IsPull {
		stopPullSessions(ctx, issue.RepoID, issue.ID)
	}
}

func (n *notifier) MergePullRequest(ctx context.Context, _ *user_model.User, pr *issues_model.PullRequest) {
	stopPullSessions(ctx, pr.BaseRepoID, pr.IssueID)
}

func (n *notifier) AutoMergePullRequest(ctx context.Context, _ *user_model.User, pr *issues_model.PullRequest) {
	stopPullSessions(ctx, pr.BaseRepoID, pr.IssueID)
}

// stopPullSessions cancels the work of the sessions maintaining a closed or merged pull request, it is done
func stopPullSessions(ctx context.Context, repoID, issueID int64) {
	if !IsEnabled() {
		return
	}
	sessions, err := agents_model.FindIssueSessions(ctx, repoID, issueID, "")
	if err != nil {
		log.Error("agents: sessions of issue %d: %v", issueID, err)
		return
	}
	for _, s := range sessions {
		if !s.OwnsPull || s.IssueID != issueID {
			continue
		}
		if err := CancelSession(ctx, s); err != nil {
			log.Error("agents: cancel session %d: %v", s.ID, err)
		}
	}
}

func (n *notifier) PullRequestCodeComment(ctx context.Context, pr *issues_model.PullRequest, comment *issues_model.Comment, _ []*user_model.User) {
	if comment.ReviewID > 0 { // PullRequestReview handled it with its review
		return
	}
	if pr.LoadIssue(ctx) != nil || comment.LoadPoster(ctx) != nil {
		return
	}
	HandleMention(ctx, comment.Poster, pr.Issue, comment.ID, codeCommentContent(comment))
}

func (n *notifier) WorkflowRunStatusUpdate(ctx context.Context, _ *repo_model.Repository, _ *user_model.User, run *actions_model.ActionRun) {
	syncActionRunStatus(ctx, run)
}

func mentionedNames(content string) []string {
	names := references.FindAllMentionsMarkdown(content)
	for i := range names {
		names[i] = strings.ToLower(names[i])
	}
	return names
}

// UpdateComment handles the agents a comment mentions only since it was edited, the others already got the request
func (n *notifier) UpdateComment(ctx context.Context, doer *user_model.User, c *issues_model.Comment, oldContent string) {
	if c.Type != issues_model.CommentTypeComment && c.Type != issues_model.CommentTypeCode {
		return
	}
	if c.ReviewID > 0 { // the comments of a review are handled when it is submitted, edits of a draft too
		review, err := issues_model.GetReviewByID(ctx, c.ReviewID)
		if err != nil || review.Type == issues_model.ReviewTypePending {
			return
		}
	}
	old := mentionedNames(oldContent)
	var added []string
	for _, name := range mentionedNames(c.Content) {
		if !slices.Contains(old, name) && !slices.Contains(added, name) {
			added = append(added, name)
		}
	}
	if len(added) == 0 || c.LoadIssue(ctx) != nil {
		return
	}
	content := c.Content
	if c.Type == issues_model.CommentTypeCode {
		content = codeCommentContent(c)
	}
	handleMentions(ctx, doer, c.Issue, c.ID, content, added)
}

// HandleMention starts or continues the conversation of every agent the doer mentions, with the doer's own key and permissions.
// It reports whether the doer mentioned any agent.
func HandleMention(ctx context.Context, doer *user_model.User, issue *issues_model.Issue, commentID int64, content string) (handled bool) {
	return handleMentions(ctx, doer, issue, commentID, content, mentionedNames(content))
}

// handleMentions starts the conversations of the agents among names that content mentions
func handleMentions(ctx context.Context, doer *user_model.User, issue *issues_model.Issue, commentID int64, content string, names []string) (handled bool) {
	if !IsEnabled() || doer == nil || !doer.IsIndividual() {
		return false // bots, including agents themselves, never trigger agents
	}
	if len(names) == 0 {
		return false
	}
	agents, err := agents_model.GetEnabledAgentsByBotNames(ctx, names)
	if err != nil {
		log.Error("agents: resolve mentions: %v", err)
		return false
	}
	for _, agent := range agents {
		handled = true
		if slices.ContainsFunc(commandHandlers, func(h CommandHandler) bool { return h(ctx, doer, agent, issue, content) }) {
			continue
		}
		_, err := startConversationRun(ctx, doer, agent, issue, commentID, content)
		if errors.Is(err, ErrNoUserKey) {
			replyNoKey(ctx, agent, doer, issue)
		} else if err != nil {
			log.Warn("agents: mention of @%s by %s on issue %d: %v", agent.Bot.Name, doer.Name, issue.ID, err)
		}
	}
	return handled
}

// CommandHandler handles a comment as a command to the agent rather than a request, and reports whether it did
type CommandHandler func(ctx context.Context, doer *user_model.User, agent *agents_model.Agent, issue *issues_model.Issue, content string) bool

var commandHandlers []CommandHandler

// RegisterCommandHandler lets another service answer some comments mentioning an agent instead of starting a run
func RegisterCommandHandler(h CommandHandler) {
	commandHandlers = append(commandHandlers, h)
}

// replyNoKey tells a user who mentions an agent that runs need their own key, nobody else's is ever used
func replyNoKey(ctx context.Context, agent *agents_model.Agent, doer *user_model.User, issue *issues_model.Issue) {
	if err := issue.LoadRepo(ctx); err != nil {
		return
	}
	content := fmt.Sprintf("@%s I work with your own API key: add one for @%s in your [settings](%s/user/settings/agents) and mention me again.",
		doer.Name, agent.Bot.Name, strings.TrimSuffix(setting.AppURL, "/"))
	if _, err := issue_service.CreateIssueComment(ctx, agent.Bot, issue.Repo, issue, content, nil); err != nil {
		log.Warn("agents: reply to %s on issue %d: %v", doer.Name, issue.ID, err)
	}
}

func startConversationRun(ctx context.Context, doer *user_model.User, agent *agents_model.Agent, issue *issues_model.Issue, commentID int64, content string, setup ...func(*agents_model.Run)) (*agents_model.Session, error) {
	if err := issue.LoadRepo(ctx); err != nil {
		return nil, err
	}
	if err := checkCanUse(ctx, issue.Repo, agent, doer); err != nil {
		return nil, err
	}
	repo := issue.Repo
	perm, err := access_model.GetDoerRepoPermission(ctx, repo, doer)
	if err != nil {
		return nil, err
	}
	if !perm.CanRead(unit.TypeCode) || !perm.CanReadIssuesOrPulls(issue.IsPull) {
		return nil, fmt.Errorf("%s cannot read %s", doer.Name, repo.FullName())
	}

	var s *agents_model.Session
	err = globallock.LockAndDo(ctx, fmt.Sprintf("agents_conversation_%d_%d", agent.ID, issue.ID), func(ctx context.Context) error {
		if s, err = agents_model.GetConversationSession(ctx, agent.ID, issue.ID); !errors.Is(err, util.ErrNotExist) {
			return err
		}
		s = &agents_model.Session{
			RepoID:     repo.ID,
			CreatorID:  doer.ID,
			AgentID:    agent.ID,
			Kind:       agents_model.SessionKindIssue,
			Title:      issue.Title,
			Status:     agents_model.StatusQueued,
			IssueID:    issue.ID,
			BaseBranch: repo.DefaultBranch,
			Branch:     sessionBranch(agent.Bot, fmt.Sprintf("issue-%d", issue.Index)),
		}
		if issue.IsPull {
			if err := issue.LoadPullRequest(ctx); err != nil {
				return err
			}
			s.Kind, s.BaseBranch, s.Branch = agents_model.SessionKindPull, issue.PullRequest.BaseBranch, ""
		}
		return agents_model.InsertSession(ctx, s)
	})
	if err != nil {
		return nil, err
	}
	if _, err = EnqueueRun(ctx, s, doer, content, commentID, setup...); err != nil {
		return nil, err
	}
	s.Agent = agent
	acknowledge(ctx, s, issue, commentID)
	return s, nil
}

// acknowledge reacts with 👀 to the request, the bot answers once it has worked on it
func acknowledge(ctx context.Context, s *agents_model.Session, issue *issues_model.Issue, commentID int64) {
	if s.Agent == nil || s.Agent.Bot == nil {
		return
	}
	var err error
	if commentID > 0 {
		var c *issues_model.Comment
		if c, err = issues_model.GetCommentByID(ctx, commentID); err == nil {
			_, err = issue_service.CreateCommentReaction(ctx, s.Agent.Bot, c, "eyes")
		}
	} else {
		_, err = issue_service.CreateIssueReaction(ctx, s.Agent.Bot, issue, "eyes")
	}
	if err != nil {
		log.Warn("agents: react to issue %d: %v", issue.ID, err)
	}
}

// Init registers the agents hooks and notifier, and dispatches the runs queued before a restart
func Init(ctx context.Context) error {
	if !IsEnabled() {
		return nil
	}
	access_model.AgentDelegateUser = DelegateUser
	access_model.AgentAssignable = Assignable
	notify_service.RegisterNotifier(&notifier{})
	registerCleanupTask()
	go DispatchQueued(graceful.GetManager().ShutdownContext())
	return nil
}
