// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	actions_model "gitea.dev/models/actions"
	agents_model "gitea.dev/models/agents"
	git_model "gitea.dev/models/git"
	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	user_model "gitea.dev/models/user"
	actions_module "gitea.dev/modules/actions"
	"gitea.dev/modules/git"
	"gitea.dev/modules/globallock"
	"gitea.dev/modules/log"
	"gitea.dev/modules/repository"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/util"
	issue_service "gitea.dev/services/issue"
)

func (n *notifier) CreateCommitStatus(ctx context.Context, repo *repo_model.Repository, commit *repository.PushCommit, sender *user_model.User, status *git_model.CommitStatus) {
	if !status.State.IsFailure() && !status.State.IsError() {
		return
	}
	if err := handleCIFailure(ctx, repo, commit.Sha1, sender, status); err != nil {
		log.Warn("agents: CI failure on %s@%s: %v", repo.FullName(), commit.Sha1, err)
	}
}

// handleCIFailure asks the agent to fix a failing check of the pull request it maintains, once per commit and a limited number of times in a row,
// with the key and the permissions of the user who triggered its latest run.
// Only Gitea's own checks and the statuses of that user count: anyone else could spend their key and steer the agent with the description.
func handleCIFailure(ctx context.Context, repo *repo_model.Repository, sha string, sender *user_model.User, status *git_model.CommitStatus) error {
	if !IsEnabled() || setting.Agents.MaxCIFixAttempts <= 0 || strings.HasPrefix(status.Context, "Agent session") {
		return nil
	}
	sessions, err := agents_model.FindPullOwningSessions(ctx, repo.ID)
	if err != nil {
		return err
	}
	merging, err := agents_model.FindMergingSessions(ctx, repo.ID)
	if err != nil {
		return err
	}
	sessions = append(sessions, merging...)
	for _, s := range sessions {
		if s.CIFixSHA == sha || s.CIFixAttempts > setting.Agents.MaxCIFixAttempts {
			continue
		}
		branch := s.Branch
		if !s.OwnsPull { // the agent merges the pull request, it maintains its head until then
			pr, err := issues_model.GetPullRequestByIssueID(ctx, s.IssueID)
			if err != nil {
				return err
			}
			branch = pr.HeadBranch
		}
		if head, err := git.GetBranchCommitID(ctx, repo, branch); err != nil || head != sha {
			continue
		}
		err := globallock.LockAndDo(ctx, fmt.Sprintf("agents_session_%d", s.ID), func(ctx context.Context) error {
			if active, err := agents_model.HasActiveRun(ctx, s.ID); err != nil || active {
				return err // the running agent pushes again anyway
			}
			latest, err := agents_model.GetLatestRun(ctx, s.ID)
			if err != nil || latest == nil {
				return err
			}
			if sender == nil || (sender.ID != user_model.ActionsUserID && sender.ID != latest.TriggerUserID) {
				return nil
			}
			trigger, err := user_model.GetUserByID(ctx, latest.TriggerUserID)
			if err != nil {
				return err
			}
			if err := s.LoadAttributes(ctx); err != nil {
				return err
			}
			if s.CIFixAttempts == setting.Agents.MaxCIFixAttempts { // a request of a user resets the count
				s.CIFixSHA = sha
				s.CIFixAttempts++
				if err := agents_model.UpdateSession(ctx, s, "ci_fix_sha", "ci_fix_attempts"); err != nil {
					return err
				}
				return replyCIFixLimit(ctx, s, trigger, status)
			}
			if err := checkCanUse(ctx, s.Repo, s.Agent, trigger); err != nil {
				return err
			}
			s.CIFixSHA = sha
			s.CIFixAttempts++
			if err := agents_model.UpdateSession(ctx, s, "ci_fix_sha", "ci_fix_attempts"); err != nil {
				return err
			}
			prompt := fmt.Sprintf("The CI check `%s` failed on commit %s of the pull request: %s", status.Context, sha, status.Description)
			if status.TargetURL != "" {
				prompt += "\nDetails: " + absoluteURL(status.TargetURL)
			}
			if tail := actionsLogTail(ctx, repo, status.TargetURL); tail != "" {
				prompt += "\n\nEnd of the job log:\n```\n" + tail + "\n```"
			}
			prompt += "\nFind the cause in the repository and its CI configuration, fix it even if your earlier changes did not cause it, and run the relevant checks locally."
			_, err = enqueueRun(ctx, s, trigger, prompt, 0)
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// replyCIFixLimit tells once that the agent stopped fixing the checks of a pull request on its own
func replyCIFixLimit(ctx context.Context, s *agents_model.Session, trigger *user_model.User, status *git_model.CommitStatus) error {
	if s.Agent == nil || s.Agent.Bot == nil || s.Issue == nil {
		return nil
	}
	content := fmt.Sprintf("@%s The CI check `%s` failed again. I already tried to fix the checks of this pull request %d times in a row, so I stopped: mention me to try again.",
		trigger.Name, status.Context, setting.Agents.MaxCIFixAttempts)
	_, err := issue_service.CreateIssueComment(ctx, s.Agent.Bot, s.Repo, s.Issue, content+replyFooter(ctx, s), nil)
	return err
}

func absoluteURL(target string) string {
	if !strings.HasPrefix(target, "/") {
		return target
	}
	return strings.TrimSuffix(setting.AppURL, "/") + strings.TrimPrefix(target, setting.AppSubURL)
}

var actionsJobURL = regexp.MustCompile(`/actions/runs/(\d+)/jobs/(\d+)$`)

const ciLogTailLines = 80

// actionsLogTail returns the end of the log of a failed Gitea Actions job of the repository, Codex cannot read it by itself
func actionsLogTail(ctx context.Context, repo *repo_model.Repository, target string) string {
	if !strings.HasPrefix(absoluteURL(target), repo.HTMLURL(ctx)+"/actions/runs/") {
		return ""
	}
	m := actionsJobURL.FindStringSubmatch(target)
	if m == nil {
		return ""
	}
	runID, _ := strconv.ParseInt(m[1], 10, 64)
	jobID, _ := strconv.ParseInt(m[2], 10, 64)
	job, err := actions_model.GetRunJobByRunAndID(ctx, runID, jobID)
	if err != nil || job.RepoID != repo.ID || job.TaskID == 0 {
		return ""
	}
	task, err := actions_model.GetTaskByID(ctx, job.TaskID)
	if err != nil || task.LogFilename == "" || len(task.LogIndexes) == 0 {
		return ""
	}
	first := max(len(task.LogIndexes)-ciLogTailLines, 0)
	rows, err := actions_module.ReadLogs(ctx, task.LogInStorage, task.LogFilename, task.LogIndexes[first], ciLogTailLines)
	if err != nil {
		log.Warn("agents: read the log of task %d: %v", task.ID, err)
		return ""
	}
	lines := make([]string, 0, len(rows))
	for _, row := range rows {
		lines = append(lines, row.Content)
	}
	return util.EllipsisDisplayString(strings.Join(lines, "\n"), 8*1024)
}
