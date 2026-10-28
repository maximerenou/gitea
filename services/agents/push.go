// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"

	actions_model "gitea.dev/models/actions"
	agents_model "gitea.dev/models/agents"
	issues_model "gitea.dev/models/issues"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"
	"gitea.dev/modules/git/gitcmd"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/util"
)

// CheckAgentPush lets an agent bot push only with the token of an active run, to the single branch decided at its dispatch
func CheckAgentPush(ctx context.Context, doer *user_model.User, repoID int64, ref git.RefName, isDelete bool) error {
	if taskID, ok := user_model.GetActionsUserTaskID(doer); ok {
		return checkAgentTaskPush(ctx, taskID)
	}
	if doer == nil || !doer.IsTypeBot() {
		return nil
	}
	if _, err := agents_model.GetAgentByBotID(ctx, doer.ID); errors.Is(err, util.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	denied := util.NewPermissionDeniedErrorf("agent @%s can only push to the branch of its running session", doer.Name)
	r, err := runOfCredential(ctx, doer)
	if errors.Is(err, util.ErrNotExist) {
		return denied
	} else if err != nil {
		return err
	}
	if r.RepoID != repoID || !r.CanPush() || isDelete || !ref.IsBranch() || ref.BranchName() != r.PushBranch {
		return denied
	}
	return nil
}

// CheckAgentWorkflows refuses agent pushes that change the repository's workflows: they would run with its secrets,
// and the agent reads text anyone can write. Each workflow file must be kept from the branch or merged from the base.
func CheckAgentWorkflows(ctx context.Context, doer *user_model.User, gitRepo *git.Repository, env []string, oldCommitID, newCommitID string) error {
	dirs := slices.Concat(setting.Actions.WorkflowDirs, setting.Actions.ScopedWorkflowDirs)
	if len(dirs) == 0 || doer == nil || !doer.IsTypeBot() {
		return nil
	}
	r, err := runOfCredential(ctx, doer)
	if errors.Is(err, util.ErrNotExist) {
		return nil // CheckAgentPush already refused it
	} else if err != nil {
		return err
	}
	refs := []string{r.BaseRef, r.PullTarget}
	if !git.IsEmptyCommitID(oldCommitID) {
		refs = append(refs, oldCommitID)
	}
	if s, err := agents_model.GetSessionByID(ctx, r.SessionID); err != nil {
		return err
	} else if s.BaseBranch != "" {
		refs = append(refs, s.BaseBranch)
	} else if s.IssueID > 0 {
		if pr, err := issues_model.GetPullRequestByIssueID(ctx, s.IssueID); err == nil {
			refs = append(refs, pr.BaseBranch)
		}
	}
	pushed, err := workflowBlobs(ctx, gitRepo, env, dirs, newCommitID)
	if err != nil {
		return err
	}
	var sides []map[string]string
	for _, ref := range refs {
		if blobs, err := workflowBlobs(ctx, gitRepo, env, dirs, ref); err == nil {
			sides = append(sides, blobs)
		}
	}
	if !workflowsKept(pushed, sides) {
		return util.NewPermissionDeniedErrorf("agents cannot change the repository's workflows, a person has to push such changes")
	}
	return nil
}

// workflowsKept reports whether each workflow file, added, changed or deleted, matches one of the sides,
// so a merge of sides that changed different workflows passes
func workflowsKept(pushed map[string]string, sides []map[string]string) bool {
	paths := maps.Clone(pushed)
	for _, side := range sides {
		maps.Copy(paths, side)
	}
	for p := range paths {
		if !slices.ContainsFunc(sides, func(side map[string]string) bool { return side[p] == pushed[p] }) {
			return false
		}
	}
	return true
}

// workflowBlobs maps the files of a commit's workflow directories to their mode and blob, env reaches the objects of a push being received
func workflowBlobs(ctx context.Context, gitRepo *git.Repository, env, dirs []string, ref string) (map[string]string, error) {
	if ref == "" {
		return nil, util.NewNotExistErrorf("no ref")
	}
	stdout, _, err := gitcmd.NewCommand("ls-tree", "-r", "-z").AddDynamicArguments(ref).AddDashesAndList(dirs...).
		WithEnv(env).WithRepo(gitRepo).RunStdString(ctx)
	if err != nil {
		return nil, err
	}
	blobs := map[string]string{}
	for entry := range strings.SplitSeq(stdout, "\x00") {
		info, p, ok := strings.Cut(entry, "\t")
		if !ok {
			continue
		}
		mode, rest, _ := strings.Cut(info, " ")
		_, object, _ := strings.Cut(rest, " ")
		blobs[p] = mode + " " + object
	}
	return blobs, nil
}

// checkAgentTaskPush denies pushes with the Actions token of an agent job, agents push as their bot
func checkAgentTaskPush(ctx context.Context, taskID int64) error {
	task, err := actions_model.GetTaskByID(ctx, taskID)
	if err != nil {
		return err
	}
	if err := task.LoadAttributes(ctx); err != nil {
		return err
	}
	if _, err := agents_model.GetRunOfActionRun(ctx, task.Job.Run); errors.Is(err, util.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return util.NewPermissionDeniedErrorf("the Actions token of an agent job cannot push")
}
