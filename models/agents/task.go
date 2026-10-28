// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	actions_model "gitea.dev/models/actions"
	auth_model "gitea.dev/models/auth"
	"gitea.dev/models/db"
	git_model "gitea.dev/models/git"
	issues_model "gitea.dev/models/issues"
	user_model "gitea.dev/models/user"
	actions_module "gitea.dev/modules/actions"
	"gitea.dev/modules/git"
	"gitea.dev/modules/util"

	"xorm.io/builder"
)

const (
	// WorkflowID names the generated workflow of agent runs in Gitea Actions
	WorkflowID = "gitea-agent-session.yml"
	// APIKeySecretName is the job secret Codex authenticates to the model with: a model token of the run, never the trigger user's key
	APIKeySecretName = "GITEA_AGENT_API_KEY"
	// GitTokenSecretName is the job secret holding the bot's token for this run, the agent pushes under its own identity
	GitTokenSecretName = "GITEA_AGENT_GIT_TOKEN"
)

// TaskSecrets returns the only secrets an agent task gets: its Actions token, a model token and the bot's run token.
// The model token lets the job call the model through Gitea with the trigger user's key while the run lasts, the key itself never enters the job:
// the repository's setup steps run in the same container. ok is false for tasks that are not agent runs.
// Reruns of an agent run get no model token, so nobody can replay a session on its owner's credentials.
func TaskSecrets(ctx context.Context, task *actions_model.ActionTask) (secrets map[string]string, ok bool, err error) {
	if err := task.LoadAttributes(ctx); err != nil {
		return nil, false, err
	}
	r, err := GetRunOfActionRun(ctx, task.Job.Run)
	if errors.Is(err, util.ErrNotExist) {
		if task.Job.Run.WorkflowID == WorkflowID && task.Job.Run.EventPayload == "{}" {
			// an agent run dispatched before agent runs were linked from their insertion, never hand it the repository's secrets
			return map[string]string{"GITHUB_TOKEN": task.Token, "GITEA_TOKEN": task.Token}, true, nil
		}
		return nil, false, nil
	} else if err != nil {
		return nil, true, err
	}

	secrets = map[string]string{"GITHUB_TOKEN": task.Token, "GITEA_TOKEN": task.Token}
	claimed, err := ClaimRunTask(ctx, r, task.ID)
	if err != nil || !claimed || r.Status.IsDone() {
		return secrets, true, err
	}
	s, err := GetSessionByID(ctx, r.SessionID)
	if err != nil {
		return nil, true, err
	}
	agent, err := GetAgentByID(ctx, s.AgentID)
	if errors.Is(err, util.ErrNotExist) {
		return secrets, true, nil
	} else if err != nil {
		return nil, true, err
	}
	userKey, err := GetUserKey(ctx, agent.ID, r.TriggerUserID)
	if errors.Is(err, util.ErrNotExist) {
		return secrets, true, nil
	} else if err != nil {
		return nil, true, err
	}
	if _, err := userKey.APIKey(); err != nil {
		return nil, true, err
	}
	modelSecret := util.CryptoRandomString(40)
	sum := sha256.Sum256([]byte(modelSecret))
	r.ModelTokenHash = hex.EncodeToString(sum[:])
	if err := UpdateRun(ctx, r, "model_token_hash"); err != nil {
		return nil, true, err
	}
	gitToken, err := issueRunToken(ctx, r, agent)
	if err != nil {
		return nil, true, err
	}
	secrets[APIKeySecretName] = fmt.Sprintf("%d.%s", r.ID, modelSecret)
	secrets[GitTokenSecretName] = gitToken
	return secrets, true, nil
}

// RunsAgentCode reports whether an Actions run executes code an agent bot pushed, its push or the head of its pull request:
// prompt injection can make the agent change a script CI runs, so like fork pull requests the run gets no secrets
func RunsAgentCode(ctx context.Context, run *actions_model.ActionRun) (bool, error) {
	if run.AgentCodeTrust != "" {
		return run.AgentCodeTrust != "trusted", nil
	}
	if run.TriggerEvent == actions_module.GithubEventPullRequestTarget { // runs the base's code
		return false, nil
	}
	if isAgent, err := isAgentBot(ctx, run.TriggerUserID); isAgent || err != nil {
		return isAgent, err
	}
	index, ok := git.RefName(run.Ref).PullIndex()
	if !ok {
		return false, nil
	}
	pr, err := issues_model.GetPullRequestByIndex(ctx, run.RepoID, index)
	if errors.Is(err, util.ErrNotExist) {
		return true, nil
	} else if err != nil {
		return false, err
	}
	if pr.HeadRepoID != run.RepoID { // agents never push to forks
		return false, nil
	}
	branch, err := git_model.GetBranchExisting(ctx, pr.HeadRepoID, pr.HeadBranch)
	if errors.Is(err, util.ErrNotExist) {
		return true, nil
	} else if err != nil {
		return false, err
	}
	if run.ID != 0 || run.CommitSHA == "" || branch.CommitID != run.CommitSHA {
		return true, nil // legacy runs and moved heads have no immutable trust decision
	}
	return isAgentBot(ctx, branch.PusherID)
}

func isAgentBot(ctx context.Context, userID int64) (bool, error) {
	if userID <= 0 {
		return false, nil
	}
	_, err := GetAgentByBotID(ctx, userID)
	if errors.Is(err, util.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// issueRunToken creates the bot token of a run, the token only lives while the run is active
func issueRunToken(ctx context.Context, r *Run, agent *Agent) (string, error) {
	bot, err := user_model.GetUserByID(ctx, agent.BotID)
	if err != nil {
		return "", err
	}
	scope := auth_model.AccessTokenScopeReadRepository // a run that cannot push only fetches
	if r.CanPush() {
		scope = auth_model.AccessTokenScopeWriteRepository
	}
	token := &auth_model.AccessToken{UID: bot.ID, Name: fmt.Sprintf("agent-run-%d", r.ID), Scope: scope}
	if err := auth_model.NewAccessToken(ctx, token); err != nil {
		return "", err
	}
	r.AccessTokenID = token.ID
	if err := UpdateRun(ctx, r, "access_token_id"); err != nil {
		return "", err
	}
	return token.Token, nil
}

// RevokeRunToken deletes the bot token of a finished run
func RevokeRunToken(ctx context.Context, r *Run) error {
	if r.AccessTokenID == 0 {
		return nil
	}
	if _, err := db.GetEngine(ctx).ID(r.AccessTokenID).Delete(new(auth_model.AccessToken)); err != nil {
		return err
	}
	r.AccessTokenID, r.ModelTokenHash = 0, ""
	return UpdateRun(ctx, r, "access_token_id", "model_token_hash")
}

// StatePath is where the Codex conversation of a session is kept in the Actions storage between its jobs
func StatePath(sessionID int64) string {
	return fmt.Sprintf("agents/sessions/%d.tar.gz", sessionID)
}

// DeleteRepoSessions deletes the sessions of a repository, their runs, events and tokens, and its agents setting.
// It returns the sessions whose Codex conversation must be removed from the storage once the transaction is committed.
func DeleteRepoSessions(ctx context.Context, repoID int64) ([]int64, error) {
	withState, err := deleteSessions(ctx, builder.Eq{"repo_id": repoID})
	if err != nil {
		return nil, err
	}
	return withState, db.DeleteBeans(ctx, &RepoSetting{RepoID: repoID})
}

// DeleteIssueSessions deletes the sessions of an issue like DeleteRepoSessions
func DeleteIssueSessions(ctx context.Context, issueID int64) ([]int64, error) {
	return deleteSessions(ctx, builder.Eq{"issue_id": issueID})
}

func deleteSessions(ctx context.Context, cond builder.Cond) ([]int64, error) {
	var withState []int64
	if err := db.GetEngine(ctx).Table("agent_session").Where(cond.And(builder.Eq{"has_state": true})).Cols("id").Find(&withState); err != nil {
		return nil, err
	}
	sessions := builder.Select("id").From("agent_session").Where(cond)
	if _, err := db.GetEngine(ctx).Where(builder.In("session_id", sessions)).Delete(new(Event)); err != nil {
		return nil, err
	}
	tokens := builder.Select("access_token_id").From("agent_run").Where(builder.In("session_id", sessions).And(builder.Gt{"access_token_id": 0}))
	if _, err := db.GetEngine(ctx).Where(builder.In("id", tokens)).Delete(new(auth_model.AccessToken)); err != nil {
		return nil, err
	}
	if _, err := db.GetEngine(ctx).Where(builder.In("session_id", sessions)).Delete(new(Run)); err != nil {
		return nil, err
	}
	_, err := db.GetEngine(ctx).Where(cond).Delete(new(Session))
	return withState, err
}
