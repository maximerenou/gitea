// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	agents_model "gitea.dev/models/agents"
	issues_model "gitea.dev/models/issues"
	access_model "gitea.dev/models/perm/access"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unit"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"
	"gitea.dev/modules/json"
	"gitea.dev/modules/log"
	"gitea.dev/modules/timeutil"
	"gitea.dev/modules/util"
	pull_service "gitea.dev/services/pull"
)

const maxReviewComments = 50

var maxAutoReviewsPerDay int64 = 50 // bounds what the automatic reviews of a repository spend of its reviewer's key

// StartAgentReview asks an agent to review a pull request, it answers with a pull request review commenting the changed lines
func StartAgentReview(ctx context.Context, doer *user_model.User, agent *agents_model.Agent, pr *issues_model.PullRequest) (*agents_model.Session, error) {
	if err := pr.LoadIssue(ctx); err != nil {
		return nil, err
	}
	if pr.HasMerged || pr.Issue.IsClosed {
		return nil, util.NewInvalidArgumentErrorf("the pull request is closed")
	}
	if err := agent.LoadBot(ctx); err != nil {
		return nil, err
	}
	return startConversationRun(ctx, doer, agent, pr.Issue, 0, reviewPrompt(pr), func(r *agents_model.Run) {
		r.Purpose = agents_model.RunPurposeReview
	})
}

func reviewPrompt(pr *issues_model.PullRequest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Review this pull request: `git diff origin/%s...HEAD` shows its changes. ", pr.BaseBranch)
	b.WriteString("Read the code they touch to understand them, and look for bugs, security issues, missing tests and code that is hard to maintain. ")
	b.WriteString("Only report real problems, with a suggestion to fix each; skip what linters and formatters already enforce.\n\n")
	b.WriteString("Finish with a short overall assessment, followed by a fenced code block with the language `review-comments` holding a JSON array of your comments on lines: ")
	b.WriteString("`[{\"path\": \"path/of/file\", \"line\": 12, \"body\": \"Markdown comment\"}]`, where `line` is a line of the new version of the file among the changed ones. ")
	b.WriteString("Use an empty array when you have no comment on lines.\n")
	return b.String()
}

var reviewCommentsPattern = regexp.MustCompile("(?s)```review-comments[ \t]*\n(.*?)\n```")

type reviewComment struct {
	Path string `json:"path"`
	Line int64  `json:"line"`
	Body string `json:"body"`
}

// parseReview splits the agent's answer into the body of the review and its comments on lines
func parseReview(summary string) (string, []reviewComment) {
	matches := reviewCommentsPattern.FindAllStringSubmatchIndex(summary, -1)
	if len(matches) == 0 {
		return strings.TrimSpace(summary), nil
	}
	last := matches[len(matches)-1]
	var comments []reviewComment
	if err := json.Unmarshal([]byte(summary[last[2]:last[3]]), &comments); err != nil {
		return strings.TrimSpace(summary), nil
	}
	body := strings.TrimSpace(summary[:last[0]] + summary[last[1]:])
	return body, comments
}

// submitAgentReview publishes the answer of a review run as a review of the pull request,
// the comments on lines that cannot be placed go to the review's body
func submitAgentReview(ctx context.Context, s *agents_model.Session, r *agents_model.Run) error {
	if err := s.Issue.LoadPullRequest(ctx); err != nil {
		return err
	}
	pr := s.Issue.PullRequest
	if err := pr.LoadBaseRepo(ctx); err != nil {
		return err
	}
	gitRepo, err := git.OpenRepository(ctx, pr.BaseRepo)
	if err != nil {
		return err
	}
	defer gitRepo.Close()
	var headCommitID string
	if r.ReviewedHead != "" { // someone pushed during the review, which covers the commit the job checked out
		headCommitID = cmp.Or(r.StartHead, r.ReviewedHead)
	} else {
		if headCommitID, err = gitRepo.GetRefCommitID(ctx, pr.GetGitHeadRefName()); err != nil {
			return err
		}
	}
	changed, err := gitRepo.GetFilesChangedBetween(ctx, pr.MergeBase, headCommitID)
	if err != nil {
		return err
	}

	body, comments := parseReview(r.Summary)
	if body == "" {
		body = "No problem found."
	}
	var unplaced []string
	for i, c := range comments {
		c.Body = strings.TrimSpace(c.Body)
		if c.Body == "" || i >= maxReviewComments {
			continue
		}
		if c.Line > 0 && slices.Contains(changed, c.Path) {
			if _, err := pull_service.CreateCodeComment(ctx, s.Agent.Bot, gitRepo, s.Issue, c.Line, c.Body, c.Path, true, 0, headCommitID, nil); err == nil {
				continue
			}
		}
		unplaced = append(unplaced, fmt.Sprintf("**`%s` line %d**\n\n%s", c.Path, c.Line, c.Body))
	}
	if len(unplaced) > 0 {
		body += "\n\n---\n\n" + strings.Join(unplaced, "\n\n")
	}
	_, _, err = pull_service.SubmitReview(ctx, s.Agent.Bot, gitRepo, s.Issue, issues_model.ReviewTypeComment, body+replyFooter(ctx, s), headCommitID, nil)
	return err
}

// autoReview starts the review a repository's administrators set up for its new pull requests
func autoReview(ctx context.Context, pr *issues_model.PullRequest) {
	if !IsEnabled() || pr.IsWorkInProgress(ctx) || isSessionPull(pr) {
		return
	}
	if err := pr.LoadIssue(ctx); err != nil {
		log.Error("agents: load issue of pull request %d: %v", pr.ID, err)
		return
	}
	if err := pr.Issue.LoadRepo(ctx); err != nil {
		log.Error("agents: load repository of pull request %d: %v", pr.ID, err)
		return
	}
	rs, err := agents_model.GetRepoSetting(ctx, pr.Issue.RepoID)
	if err != nil || rs.ReviewAgentID == 0 {
		return
	}
	if !rs.ReviewEveryone {
		if err := pr.Issue.LoadPoster(ctx); err != nil {
			return
		}
		perm, err := access_model.GetDoerRepoPermission(ctx, pr.Issue.Repo, pr.Issue.Poster)
		if err != nil || !perm.CanWrite(unit.TypeCode) {
			return
		}
	}
	reviewer, agent, problem, err := reviewSetup(ctx, pr.Issue.Repo, rs)
	if err != nil {
		log.Error("agents: automatic reviews of %s: %v", pr.Issue.Repo.FullName(), err)
		return
	} else if problem != "" {
		log.Warn("agents: automatic reviews of %s are paused: %s", pr.Issue.Repo.FullName(), problem)
		return
	}
	if reviewed, err := isHeadReviewed(ctx, agent, pr); err != nil || reviewed {
		return
	}
	if reached, err := autoReviewLimitReached(ctx, pr.Issue.RepoID, reviewer.ID); err != nil || reached {
		if reached {
			log.Info("agents: %s reached its %d reviews a day, pull request %d is not reviewed", pr.Issue.Repo.FullName(), maxAutoReviewsPerDay, pr.ID)
		}
		return
	}
	if _, err := StartAgentReview(ctx, reviewer, agent, pr); err != nil {
		log.Warn("agents: review of pull request %d: %v", pr.ID, err)
	}
}

// Problems keeping the automatic reviews of a repository from starting, the repository settings show them
const (
	ReviewProblemReviewerGone    = "reviewer_gone"
	ReviewProblemNotAdmin        = "not_admin"
	ReviewProblemAgentGone       = "agent_gone"
	ReviewProblemAgentDisabled   = "agent_disabled"
	ReviewProblemAgentNotAllowed = "agent_not_allowed"
	ReviewProblemRepoUnusable    = "repo_unusable"
	ReviewProblemNoKey           = "no_key"
	ReviewProblemLimit           = "limit"
)

// reviewSetup returns the reviewer and the agent of the automatic reviews of a repository, or the problem keeping them from starting
func reviewSetup(ctx context.Context, repo *repo_model.Repository, rs *agents_model.RepoSetting) (reviewer *user_model.User, agent *agents_model.Agent, problem string, err error) {
	reviewer, err = user_model.GetUserByID(ctx, rs.ReviewUserID)
	if user_model.IsErrUserNotExist(err) {
		return nil, nil, ReviewProblemReviewerGone, nil
	} else if err != nil {
		return nil, nil, "", err
	}
	if !access_model.IsUserRepoAdmin(ctx, repo, reviewer) { // only an administrator could set up reviews with their key
		return nil, nil, ReviewProblemNotAdmin, nil
	}
	agent, err = agents_model.GetAgentByID(ctx, rs.ReviewAgentID)
	if errors.Is(err, util.ErrNotExist) {
		return nil, nil, ReviewProblemAgentGone, nil
	} else if err != nil {
		return nil, nil, "", err
	}
	if agent.IsDisabled {
		return nil, nil, ReviewProblemAgentDisabled, nil
	}
	if rs.Enabled && !IsRepoUsable(ctx, repo) { // agents are on for the repository but it cannot run them, e.g. Actions are off
		return nil, nil, ReviewProblemRepoUnusable, nil
	}
	if !IsAgentAllowed(ctx, repo, agent) {
		return nil, nil, ReviewProblemAgentNotAllowed, nil
	}
	if _, err := agents_model.GetUserKey(ctx, agent.ID, reviewer.ID); errors.Is(err, util.ErrNotExist) {
		return nil, nil, ReviewProblemNoKey, nil
	} else if err != nil {
		return nil, nil, "", err
	}
	return reviewer, agent, "", nil
}

// AutoReviewProblem tells why the automatic reviews a repository set up do not start, empty when they do or when none is set up
func AutoReviewProblem(ctx context.Context, repo *repo_model.Repository, rs *agents_model.RepoSetting) (string, error) {
	if rs.ReviewAgentID == 0 || !IsEnabled() {
		return "", nil
	}
	_, _, problem, err := reviewSetup(ctx, repo, rs)
	if problem != "" || err != nil {
		return problem, err
	}
	if reached, err := autoReviewLimitReached(ctx, repo.ID, rs.ReviewUserID); err != nil || reached {
		return util.Iif(reached, ReviewProblemLimit, ""), err
	}
	return "", nil
}

// autoReviewLimitReached reports whether the reviewer's reviews of the repository in the last day reached the limit of automatic ones
func autoReviewLimitReached(ctx context.Context, repoID, reviewerID int64) (bool, error) {
	count, err := agents_model.CountRepoRunsSince(ctx, repoID, reviewerID, agents_model.RunPurposeReview, timeutil.TimeStampNow().AddDuration(-24*time.Hour))
	return count >= maxAutoReviewsPerDay, err
}

// isHeadReviewed reports whether the agent already reviews or reviewed the head of a pull request
func isHeadReviewed(ctx context.Context, agent *agents_model.Agent, pr *issues_model.PullRequest) (bool, error) {
	s, err := agents_model.GetConversationSession(ctx, agent.ID, pr.IssueID)
	if errors.Is(err, util.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if latest, err := agents_model.GetLatestRun(ctx, s.ID); err != nil {
		return false, err
	} else if latest != nil && latest.Purpose == agents_model.RunPurposeReview && !latest.Status.IsDone() {
		return true, nil
	}
	gitRepo, err := git.OpenRepository(ctx, pr.Issue.Repo)
	if err != nil {
		return false, err
	}
	defer gitRepo.Close()
	head, err := gitRepo.GetRefCommitID(ctx, pr.GetGitHeadRefName())
	if err != nil {
		return false, err
	}
	reviews, err := issues_model.FindReviews(ctx, issues_model.FindReviewOptions{IssueID: pr.IssueID, ReviewerID: agent.BotID})
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(reviews, func(r *issues_model.Review) bool { return r.CommitID == head }), nil
}
