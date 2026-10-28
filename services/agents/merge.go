// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	agents_model "gitea.dev/models/agents"
	issues_model "gitea.dev/models/issues"
	access_model "gitea.dev/models/perm/access"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unit"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"
	"gitea.dev/modules/log"
	"gitea.dev/modules/util"
	"gitea.dev/services/automerge"
	issue_service "gitea.dev/services/issue"
	pull_service "gitea.dev/services/pull"
)

// MergeOptions are the choices of a user asking an agent to merge a pull request
type MergeOptions struct {
	Agent  *agents_model.Agent
	Style  repo_model.MergeStyle
	Review bool
}

// CanAgentMerge reports whether the user may ask an agent to merge the pull request: the merge is theirs, and so is the push to its head
func CanAgentMerge(ctx context.Context, doer *user_model.User, pr *issues_model.PullRequest) (bool, error) {
	refusal, err := agentMergeRefusal(ctx, doer, pr)
	return refusal == "", err
}

// agentMergeRefusal tells why the user cannot ask an agent to merge the pull request, empty when they can
func agentMergeRefusal(ctx context.Context, doer *user_model.User, pr *issues_model.PullRequest) (string, error) {
	if doer == nil {
		return "you cannot merge this pull request", nil
	}
	if pr.HasMerged {
		return "the pull request is merged", nil
	}
	if err := pr.LoadIssue(ctx); err != nil {
		return "", err
	}
	if err := pr.LoadBaseRepo(ctx); err != nil {
		return "", err
	}
	if pr.Issue.IsClosed || !IsRepoUsable(ctx, pr.BaseRepo) {
		return "agents cannot merge this pull request", nil
	}
	if pr.HeadRepoID != pr.BaseRepoID { // the agent would have to push the merge to the fork
		return "pull requests from forks cannot be merged with an agent", nil
	}
	perm, err := access_model.GetDoerRepoPermission(ctx, pr.BaseRepo, doer)
	if err != nil {
		return "", err
	}
	if ok, err := pull_service.IsUserAllowedToMerge(ctx, pr, perm, doer); err != nil {
		return "", err
	} else if !ok {
		return "you cannot merge this pull request", nil
	}
	if !canPushBranch(ctx, pr.BaseRepo, doer, perm, pr.HeadBranch) {
		return "you cannot push to the branch of this pull request", nil
	}
	return "", nil
}

// StartAgentMerge asks an agent to merge the target branch into a pull request, resolving the conflicts, and to fix what the merge broke.
// Once the run succeeds, and the agent approved when asked to review, the pull request is scheduled to merge when its checks pass.
func StartAgentMerge(ctx context.Context, doer *user_model.User, pr *issues_model.PullRequest, opts MergeOptions) (*agents_model.Session, error) {
	if refusal, err := agentMergeRefusal(ctx, doer, pr); err != nil {
		return nil, err
	} else if refusal != "" {
		return nil, util.NewPermissionDeniedErrorf("%s", refusal)
	}
	if !pr.BaseRepo.MustGetUnit(ctx, unit.TypePullRequests).PullRequestsConfig().IsMergeStyleAllowed(opts.Style) {
		return nil, util.NewInvalidArgumentErrorf("the merge style %q is not allowed in this repository", opts.Style)
	}
	if err := opts.Agent.LoadBot(ctx); err != nil {
		return nil, err
	}
	return startConversationRun(ctx, doer, opts.Agent, pr.Issue, 0, mergePrompt(pr, opts.Review), func(r *agents_model.Run) {
		r.Purpose, r.MergeStyle, r.MergeReview = agents_model.RunPurposeMerge, string(opts.Style), opts.Review
	})
}

func mergePrompt(pr *issues_model.PullRequest, review bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Merge the target branch `%s` into this pull request so it can be merged.\n\n", pr.BaseBranch)
	b.WriteString("The job already started merging it in your working copy: run `git status` to see the conflicts left, if any. ")
	b.WriteString("Resolve every conflict by keeping the intent of both sides and remove all the conflict markers. Do not commit nor abort the merge, the job commits it for you. ")
	b.WriteString("Then run the checks of the project that the merge could break, and fix what it broke.\n")
	if review {
		fmt.Fprintf(&b, "\nFinally, review the whole pull request against `%s` as a careful reviewer would before merging it. ", pr.BaseBranch)
		b.WriteString("End your last message with a line that is exactly `MERGE-VERDICT: approve`, or `MERGE-VERDICT: block` followed by the problems that must be fixed before merging. ")
		b.WriteString("Only block for real defects, not for style.\n")
	}
	return b.String()
}

var mergeVerdictPattern = regexp.MustCompile(`(?m)^[ \t]*MERGE-VERDICT:[ \t]*(approve|block)\b[ \t:.—–-]*(.*)$`)

// mergeVerdict returns the last verdict of a review, an absent verdict blocks the merge
func mergeVerdict(summary string) string {
	matches := mergeVerdictPattern.FindAllStringSubmatch(summary, -1)
	if len(matches) == 0 {
		return "block"
	}
	return matches[len(matches)-1][1]
}

// presentVerdict words the verdict lines Gitea reads for the participants of the pull request
func presentVerdict(summary string) string {
	return mergeVerdictPattern.ReplaceAllStringFunc(summary, func(line string) string {
		m := mergeVerdictPattern.FindStringSubmatch(line)
		if m[1] == "approve" {
			return strings.TrimSpace("✅ My review approves the merge. " + m[2])
		}
		if m[2] == "" {
			return "⛔ My review blocks the merge."
		}
		return "⛔ My review blocks the merge: " + m[2]
	})
}

// finishAgentMerge schedules the merge of a pull request once its merge run succeeded, with the permissions of the user who asked for it
func finishAgentMerge(ctx context.Context, s *agents_model.Session, r *agents_model.Run) {
	if r.Purpose != agents_model.RunPurposeMerge || r.Status != agents_model.StatusSuccess || s.Issue == nil || s.Agent == nil {
		return
	}
	pr, err := issues_model.GetPullRequestByIssueID(ctx, s.IssueID)
	if err != nil {
		log.Error("agents: pull request of session %d: %v", s.ID, err)
		return
	}
	trigger, err := user_model.GetUserByID(ctx, r.TriggerUserID)
	if err != nil {
		log.Error("agents: trigger user of run %d: %v", r.ID, err)
		return
	}
	note := func(format string, args ...any) {
		content := fmt.Sprintf("@%s ", trigger.Name) + fmt.Sprintf(format, args...)
		if _, err := issue_service.CreateIssueComment(ctx, s.Agent.Bot, s.Repo, s.Issue, content, nil); err != nil {
			log.Warn("agents: comment on pull request %d: %v", pr.ID, err)
		}
	}
	if r.MergeReview && mergeVerdict(r.Summary) != "approve" {
		note("I did not schedule the merge: my review found problems to fix first, see above.")
		return
	}
	if r.MergeReview && r.ReviewedHead != "" {
		note("I did not schedule the merge: commits were pushed to this pull request while I reviewed it.")
		return
	}
	if pr.HeadRepoID != pr.BaseRepoID || r.PushBranch != pr.HeadBranch {
		note("I could not update the branch of this pull request, so I did not schedule its merge.")
		return
	}
	if ok, err := CanAgentMerge(ctx, trigger, pr); err != nil || !ok {
		note("I did not schedule the merge: you can no longer merge this pull request.")
		return
	}
	gitRepo, err := git.OpenRepository(ctx, pr.BaseRepo)
	if err != nil {
		log.Error("agents: open %s: %v", pr.BaseRepo.FullName(), err)
		return
	}
	defer gitRepo.Close()
	style := repo_model.MergeStyle(r.MergeStyle)
	message, body, err := pull_service.GetDefaultMergeMessage(ctx, gitRepo, pr, style)
	if err != nil {
		log.Error("agents: merge message of pull request %d: %v", pr.ID, err)
		return
	}
	if body != "" {
		message += "\n\n" + body
	}
	if r.MergeReview {
		// the branch itself, the pull request's ref follows it asynchronously; the run's token is revoked, nobody else pushed meanwhile
		if r.MergeHead, err = gitRepo.GetBranchCommitID(ctx, pr.HeadBranch); err != nil {
			log.Error("agents: head of pull request %d: %v", pr.ID, err)
			return
		}
		if err := agents_model.UpdateRun(ctx, r, "merge_head"); err != nil {
			log.Error("agents: update run %d: %v", r.ID, err)
			return
		}
	}
	if err := agents_model.ClearMergeHeads(ctx, s.IssueID, r.ID); err != nil {
		log.Error("agents: clear the earlier merges of pull request %d: %v", pr.ID, err)
	}
	deleteBranch := pr.BaseRepo.MustGetUnit(ctx, unit.TypePullRequests).PullRequestsConfig().DefaultDeleteBranchAfterMerge
	if _, err := automerge.ScheduleAutoMerge(ctx, trigger, pr, style, message, deleteBranch); err != nil {
		log.Error("agents: schedule the merge of pull request %d: %v", pr.ID, err)
		note("I could not schedule the merge: %v", err)
	}
}

// PullRequestSynchronized follows the pushes of others to a pull request an agent reviews: a running review records the head it
// started from, and a merge scheduled after a review is cancelled, the review does not cover their commits.
// The agent's own pushes only happen during its run, with the run's token, and are part of what it reviewed.
func (n *notifier) PullRequestSynchronized(ctx context.Context, doer *user_model.User, pr *issues_model.PullRequest, before, _ string) {
	runs, err := agents_model.FindReviewingRuns(ctx, pr.IssueID)
	if err != nil {
		log.Error("agents: reviewing runs of pull request %d: %v", pr.ID, err)
		return
	}
	for _, r := range runs {
		s, err := agents_model.GetSessionByID(ctx, r.SessionID)
		if err == nil {
			err = s.LoadAttributes(ctx)
		}
		if err != nil {
			log.Error("agents: session of run %d: %v", r.ID, err)
			continue
		}
		if s.Agent == nil || (doer != nil && doer.ID == s.Agent.BotID) {
			continue
		}
		if r.Status == agents_model.StatusRunning {
			if r.ReviewedHead == "" && before != "" {
				r.ReviewedHead = before
				if err := agents_model.UpdateRun(ctx, r, "reviewed_head"); err != nil {
					log.Error("agents: update run %d: %v", r.ID, err)
				}
			}
			continue
		}
		r.MergeHead = "" // even when it pushed the scheduled head: its notification came late, the head was not the agent's
		if err := agents_model.UpdateRun(ctx, r, "merge_head"); err != nil {
			log.Error("agents: update run %d: %v", r.ID, err)
			continue
		}
		if err := automerge.RemoveScheduledAutoMerge(ctx, s.Agent.Bot, pr); err != nil {
			log.Error("agents: cancel the merge of pull request %d: %v", pr.ID, err)
		}
	}
}
