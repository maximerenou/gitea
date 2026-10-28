// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"context"
	"errors"
	"fmt"
	"strings"

	agents_model "gitea.dev/models/agents"
	issues_model "gitea.dev/models/issues"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/agents"
	"gitea.dev/modules/git"
	"gitea.dev/modules/json"
	"gitea.dev/modules/util"
	issue_service "gitea.dev/services/issue"
	pull_service "gitea.dev/services/pull"
)

// ErrResultAlreadySubmitted is returned when the run has ended or its result is already being applied.
var ErrResultAlreadySubmitted = util.NewAlreadyExistErrorf("the result of this run was already submitted")

// SubmitResult validates a result received from the runner, applies what the agent's action allows as the bot, and
// ends the run. It returns the notes recorded on the run (what was ignored or could not be applied).
// Only the first final result of a run counts.
func SubmitResult(ctx context.Context, run *agents_model.Run, raw []byte) ([]string, error) {
	res, notes, err := agents.ParseResult(raw)
	if err != nil {
		if _, finishErr := agents_model.FinishRun(ctx, run.ID, agents_model.RunFailed, "", "invalid result: "+err.Error(), "", ""); finishErr != nil {
			return nil, finishErr
		}
		return nil, err
	}
	if claimed, err := agents_model.ClaimRunResult(ctx, run.ID); err != nil {
		return nil, fmt.Errorf("SubmitResult: %w", err)
	} else if !claimed {
		return nil, ErrResultAlreadySubmitted
	}
	agent, err := agents_model.GetAgentByID(ctx, run.AgentID)
	if err != nil {
		return nil, fmt.Errorf("SubmitResult: %w", err)
	}
	applicable, ignored := res.ForAction(agent.Action)
	notes = append(notes, ignored...)

	applyNotes, applyErr := applyResult(ctx, run, agent, applicable)
	notes = append(notes, applyNotes...)

	status := agents_model.RunSuccess
	switch {
	case applicable.Status == agents.ResultFailed:
		status = agents_model.RunFailed
	case applicable.Status == agents.ResultNeedsHuman:
		status = agents_model.RunNeedsHuman
	}
	errMsg := ""
	if applyErr != nil {
		status, errMsg = agents_model.RunFailed, applyErr.Error()
	}
	stored, err := json.Marshal(applicable)
	if err != nil {
		return nil, err
	}
	if _, err := agents_model.FinishRun(ctx, run.ID, status, applicable.Summary, errMsg, string(stored), strings.Join(notes, "\n")); err != nil {
		return nil, err
	}
	return notes, applyErr
}

// applyResult carries out what the result asks, with the bot's identity. Anything it cannot do is reported as a note.
func applyResult(ctx context.Context, run *agents_model.Run, agent *agents_model.Agent, res *agents.Result) (notes []string, err error) {
	bot, err := user_model.GetUserByID(ctx, agent.BotUserID)
	if err != nil {
		return nil, fmt.Errorf("applyResult: %w", err)
	}
	if run.IssueID == 0 {
		if res.Comment != "" || res.Review != nil {
			notes = append(notes, "no issue or pull request to answer on: comment and review were not posted")
		}
		return notes, nil
	}
	issue, err := issues_model.GetIssueByID(ctx, run.IssueID)
	if err != nil {
		return nil, fmt.Errorf("applyResult: %w", err)
	}
	if err := issue.LoadRepo(ctx); err != nil {
		return nil, fmt.Errorf("applyResult: %w", err)
	}

	if res.Review != nil {
		reviewNotes, err := submitReview(ctx, bot, issue, res)
		notes = append(notes, reviewNotes...)
		if err != nil {
			return notes, err
		}
	}
	if strings.TrimSpace(res.Comment) != "" {
		if _, err := issue_service.CreateIssueComment(ctx, bot, issue.Repo, issue, res.Comment, nil); err != nil {
			return notes, fmt.Errorf("applyResult: comment: %w", err)
		}
	}
	for _, unsupported := range []struct {
		name string
		set  bool
	}{
		{"pr", res.PR != nil}, {"resolved_threads", len(res.ResolvedThreads) > 0}, {"subtasks", len(res.Subtasks) > 0},
		{"findings", len(res.Findings) > 0}, {"plan", res.Plan != nil},
	} {
		if unsupported.set {
			notes = append(notes, fmt.Sprintf("%q is not applied by this version yet", unsupported.name))
		}
	}
	return notes, nil
}

func submitReview(ctx context.Context, bot *user_model.User, issue *issues_model.Issue, res *agents.Result) (notes []string, err error) {
	if !issue.IsPull {
		return []string{"review ignored: the subject is not a pull request"}, nil
	}
	if err := issue.LoadPullRequest(ctx); err != nil {
		return nil, fmt.Errorf("submitReview: %w", err)
	}
	pr := issue.PullRequest
	gitRepo, closer, err := git.RepositoryFromContextOrOpen(ctx, issue.Repo)
	if err != nil {
		return nil, fmt.Errorf("submitReview: %w", err)
	}
	defer closer.Close()
	commitID, err := gitRepo.GetRefCommitID(ctx, pr.GetGitHeadRefName())
	if err != nil {
		return nil, fmt.Errorf("submitReview: %w", err)
	}

	rv := res.Review
	body := rv.Body
	reviewType := issues_model.ReviewTypeComment
	switch rv.Verdict {
	case agents.VerdictApprove, agents.VerdictRequestChanges:
		if issue.IsPoster(bot.ID) { // a bot cannot judge its own pull request, like any user
			notes = append(notes, "the verdict was posted as a comment: the agent opened this pull request")
		} else if rv.Verdict == agents.VerdictApprove {
			reviewType = issues_model.ReviewTypeApprove
		} else {
			reviewType = issues_model.ReviewTypeReject
			if strings.TrimSpace(body) == "" {
				body = res.Summary
			}
			if strings.TrimSpace(body) == "" {
				body = "Changes requested."
			}
		}
	}

	posted := 0
	for i, c := range rv.Comments {
		if _, err := pull_service.CreateCodeComment(ctx, bot, gitRepo, issue, c.Line, c.Body, c.Path, true, 0, commitID, nil); err != nil {
			notes = append(notes, fmt.Sprintf("review comment %d (%s:%d) was not posted: %v", i+1, c.Path, c.Line, err))
			continue
		}
		posted++
	}
	if strings.TrimSpace(body) == "" && posted == 0 && reviewType == issues_model.ReviewTypeComment {
		body = res.Summary
	}
	if _, _, err := pull_service.SubmitReview(ctx, bot, gitRepo, issue, reviewType, body, commitID, nil); err != nil {
		switch {
		case errors.Is(err, pull_service.ErrSubmitReviewOnClosedPR):
			return append(notes, "review not posted: the pull request is closed"), nil
		case issues_model.IsContentEmptyErr(err):
			return append(notes, "review not posted: it had no body and no comment"), nil
		}
		return notes, fmt.Errorf("submitReview: %w", err)
	}
	return notes, nil
}
