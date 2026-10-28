// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"context"
	"fmt"
	"strings"

	agents_model "gitea.dev/models/agents"
	issues_model "gitea.dev/models/issues"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/log"
	issue_service "gitea.dev/services/issue"
)

func sessionURL(ctx context.Context, s *agents_model.Session) string {
	return fmt.Sprintf("%s/agents/%d", s.Repo.HTMLURL(ctx), s.ID)
}

func triggerName(ctx context.Context, r *agents_model.Run) string {
	_, u, err := user_model.GetPossibleUserByID(ctx, r.TriggerUserID)
	if err != nil {
		return ""
	}
	return u.Name
}

func replyFooter(ctx context.Context, s *agents_model.Session) string {
	name := "Codex"
	if s.Agent != nil {
		name = s.Agent.Name
	}
	return fmt.Sprintf("\n\n<sub>%s · [agent session #%d](%s)</sub>", name, s.ID, sessionURL(ctx, s))
}

func replyWorking(ctx context.Context, r *agents_model.Run) string {
	var b strings.Builder
	fmt.Fprintf(&b, "@%s 🔄 Working on it…", triggerName(ctx, r))
	if r.Progress != "" {
		b.WriteString("\n\n" + r.Progress)
	}
	return b.String()
}

func replyFinal(ctx context.Context, s *agents_model.Session, r *agents_model.Run) string {
	var b strings.Builder
	fmt.Fprintf(&b, "@%s ", triggerName(ctx, r))
	switch r.Status {
	case agents_model.StatusSuccess:
		summary := strings.TrimSpace(r.Summary)
		if r.MergeReview {
			summary = presentVerdict(summary)
		}
		if summary == "" {
			summary = "Done."
		}
		b.WriteString(summary)
	case agents_model.StatusCancelled:
		b.WriteString("⏹️ The run was cancelled.")
	default:
		fmt.Fprintf(&b, "❌ The run failed: %s", r.Error)
	}
	if pr := FindSessionPull(ctx, s, r); pr != nil && pr.IssueID != s.IssueID {
		fmt.Fprintf(&b, "\n\nChanges: %s", pr.Issue.HTMLURL(ctx))
	}
	if r.ReadOnlyReason != "" {
		fmt.Fprintf(&b, "\n\n> [!NOTE]\n> No changes were pushed: %s.", r.ReadOnlyReason)
	}
	return b.String()
}

// upsertReply creates or edits the bot comment of a run on the session's conversation
func upsertReply(ctx context.Context, s *agents_model.Session, r *agents_model.Run, content string) error {
	if s.Agent == nil || s.Agent.Bot == nil || s.Issue == nil {
		return nil
	}
	content += replyFooter(ctx, s)
	bot := s.Agent.Bot
	if r.ReplyCommentID == 0 {
		c, err := issue_service.CreateIssueComment(ctx, bot, s.Repo, s.Issue, content, nil)
		if err != nil {
			return err
		}
		r.ReplyCommentID = c.ID
		return agents_model.UpdateRun(ctx, r, "reply_comment_id")
	}
	c, err := issues_model.GetCommentByID(ctx, r.ReplyCommentID)
	if err != nil {
		return err
	}
	if c.Content == content {
		return nil
	}
	oldContent := c.Content
	c.Content = content
	return issue_service.UpdateComment(ctx, c, c.ContentVersion, bot, oldContent)
}

// pullDescription is the description of the pull requests a session maintains
func pullDescription(ctx context.Context, s *agents_model.Session, r *agents_model.Run) string {
	var b strings.Builder
	fmt.Fprintf(&b, "> [!NOTE]\n> Maintained by the Codex agent session [#%d](%s)", s.ID, sessionURL(ctx, s))
	if s.Agent != nil && s.Agent.Bot != nil {
		fmt.Fprintf(&b, " of `%s`", s.Agent.Bot.Name)
	}
	b.WriteString(", it is updated while the agent works.\n\n## Task\n\n")
	b.WriteString(fenced(sessionTask(ctx, s)))
	if s.Kind != agents_model.SessionKindTask && s.Issue != nil {
		fmt.Fprintf(&b, "\n\nRequested in #%d", s.Issue.Index)
	}
	if r.Progress != "" {
		b.WriteString("\n\n## Progress\n\n" + r.Progress)
	}
	if summary := strings.TrimSpace(r.Summary); summary != "" {
		b.WriteString("\n\n## Summary\n\n" + summary)
	}
	return b.String()
}

func sessionTask(ctx context.Context, s *agents_model.Session) string {
	runs, err := agents_model.FindFirstRuns(ctx, s.ID, 1)
	if err != nil || len(runs) == 0 {
		if err != nil {
			log.Error("agents: first run of session %d: %v", s.ID, err)
		}
		return s.Title
	}
	return runs[0].Prompt
}

// fenced keeps user text inert, mentions in it must not trigger agents again from the owner's pull request
func fenced(s string) string {
	fence := "~~~~"
	for strings.Contains(s, fence) {
		fence += "~"
	}
	return fence + "text\n" + strings.TrimSpace(s) + "\n" + fence
}
