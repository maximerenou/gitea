// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"

	actions_model "gitea.dev/models/actions"
	"gitea.dev/models/db"
	"gitea.dev/modules/container"
	"gitea.dev/modules/json"
	"gitea.dev/modules/timeutil"
	"gitea.dev/modules/util"

	"xorm.io/builder"
)

type Status string

const (
	StatusQueued    Status = "queued"  // waiting for the session's previous run or the owner's concurrency limit
	StatusWaiting   Status = "waiting" // dispatched to Gitea Actions, waiting for a runner
	StatusRunning   Status = "running"
	StatusSuccess   Status = "success"
	StatusFailure   Status = "failure"
	StatusCancelled Status = "cancelled"
)

// Statuses are the statuses of runs and sessions, in the order they are offered
var Statuses = []Status{StatusQueued, StatusWaiting, StatusRunning, StatusSuccess, StatusFailure, StatusCancelled}

func (s Status) IsValid() bool {
	return slices.Contains(Statuses, s)
}

func (s Status) IsDone() bool {
	return s == StatusSuccess || s == StatusFailure || s == StatusCancelled
}

func (s Status) IsActive() bool {
	return s == StatusWaiting || s == StatusRunning
}

// RunPurpose is what a run does beyond answering its prompt
type RunPurpose string

const (
	RunPurposeMerge  RunPurpose = "merge"
	RunPurposeReview RunPurpose = "review"
)

// Run is one Codex execution of a session, backed by one Gitea Actions run
type Run struct {
	ID            int64
	SessionID     int64  `xorm:"INDEX NOT NULL"`
	RepoID        int64  `xorm:"INDEX NOT NULL"`
	TriggerUserID int64  `xorm:"INDEX NOT NULL"`
	Prompt        string `xorm:"LONGTEXT"`
	Status        Status `xorm:"VARCHAR(16) INDEX NOT NULL"`

	// a merge run merges the target branch into the pull request, then schedules its merge with MergeStyle
	Purpose     RunPurpose `xorm:"VARCHAR(16) NOT NULL DEFAULT ''"`
	MergeStyle  string     `xorm:"VARCHAR(32) NOT NULL DEFAULT ''"`
	MergeReview bool       `xorm:"NOT NULL DEFAULT false"`          // the agent reviews the pull request first, and can block its merge
	MergeHead   string     `xorm:"VARCHAR(64) NOT NULL DEFAULT ''"` // the head the agent reviewed and scheduled the merge of
	// the head before someone else first pushed to the pull request while the agent reviewed it, the review may not cover that push
	ReviewedHead string `xorm:"VARCHAR(64) NOT NULL DEFAULT ''"`
	StartHead    string `xorm:"VARCHAR(64) NOT NULL DEFAULT ''"` // the commit the job checked out, as its driver reports it

	CommentID      int64 `xorm:"NOT NULL DEFAULT 0"` // the comment mentioning the agent
	ReplyCommentID int64 `xorm:"NOT NULL DEFAULT 0"` // the bot comment reporting progress and the answer

	// decided at dispatch from the trigger user's permissions, an empty PushBranch makes the run read-only
	BaseRef        string `xorm:"VARCHAR(255) NOT NULL DEFAULT ''"`
	PushBranch     string `xorm:"VARCHAR(255) NOT NULL DEFAULT ''"`
	PullTarget     string `xorm:"VARCHAR(255) NOT NULL DEFAULT ''"` // base branch of the pull request the session creates
	ReadOnlyReason string `xorm:"TEXT"`

	ActionRunID        int64 `xorm:"INDEX NOT NULL DEFAULT 0"`
	ActionTaskID       int64 `xorm:"NOT NULL DEFAULT 0"`       // the only task allowed to receive the configuration's secret
	AccessTokenID      int64 `xorm:"INDEX NOT NULL DEFAULT 0"` // the bot's token the task pushes with, deleted when the run ends
	ModelRequests      int64 `xorm:"NOT NULL DEFAULT 0"`
	ModelRequestActive bool  `xorm:"NOT NULL DEFAULT false"`
	// the job calls the model through Gitea with this token, Gitea adds the trigger user's API key, which never enters the job
	ModelTokenHash string `xorm:"VARCHAR(64) NOT NULL DEFAULT ''"`

	// token usage reported directly by the model provider
	InputTokens       int64 `xorm:"NOT NULL DEFAULT 0"`
	CachedInputTokens int64 `xorm:"NOT NULL DEFAULT 0"`
	OutputTokens      int64 `xorm:"NOT NULL DEFAULT 0"`

	Progress string `xorm:"TEXT"` // the latest Codex todo list as Markdown
	Summary  string `xorm:"LONGTEXT"`
	Error    string `xorm:"TEXT"`

	Started timeutil.TimeStamp
	Stopped timeutil.TimeStamp
	Created timeutil.TimeStamp `xorm:"created"`
	Updated timeutil.TimeStamp `xorm:"updated"`
}

func (*Run) TableName() string {
	return "agent_run"
}

func (r *Run) CanPush() bool {
	return r.PushBranch != ""
}

func GetRunByID(ctx context.Context, id int64) (*Run, error) {
	r, exist, err := db.GetByID[Run](ctx, id)
	if err != nil {
		return nil, err
	} else if !exist {
		return nil, util.NewNotExistErrorf("agent run %d does not exist", id)
	}
	return r, nil
}

// GetRepoRunByActionRunID returns the agent run an Actions run was linked to, not for granting access, see GetRunOfActionRun
func GetRepoRunByActionRunID(ctx context.Context, repoID, actionRunID int64) (*Run, error) {
	r := &Run{}
	has, err := db.GetEngine(ctx).Where("repo_id = ? AND action_run_id = ?", repoID, actionRunID).Get(r)
	if err != nil {
		return nil, err
	} else if !has {
		return nil, util.NewNotExistErrorf("action run %d has no agent run", actionRunID)
	}
	return r, nil
}

// ExistingActionRuns returns which Actions runs of the agent runs still exist, users can delete finished ones
func ExistingActionRuns(ctx context.Context, repoID int64, runs []*Run) (container.Set[int64], error) {
	ids := make([]int64, 0, len(runs))
	for _, r := range runs {
		if r.ActionRunID > 0 {
			ids = append(ids, r.ActionRunID)
		}
	}
	existing := container.Set[int64]{}
	if len(ids) == 0 {
		return existing, nil
	}
	var found []*actions_model.ActionRun
	if err := db.GetEngine(ctx).Where("repo_id = ?", repoID).In("id", ids).Cols("id").Find(&found); err != nil {
		return nil, err
	}
	for _, run := range found {
		existing.Add(run.ID)
	}
	return existing, nil
}

// EventPayload is the event payload of the Actions run of an agent run, it links them from the run's insertion,
// so a runner can never pick an agent job before Gitea knows it is one
func EventPayload(r *Run) string {
	return fmt.Sprintf(`{"gitea_agent_run":%d}`, r.ID)
}

// GetRunOfActionRun returns the agent run an Actions run executes, util.ErrNotExist for other Actions runs
func GetRunOfActionRun(ctx context.Context, actionRun *actions_model.ActionRun) (*Run, error) {
	var payload struct {
		AgentRunID int64 `json:"gitea_agent_run"`
	}
	if actionRun.WorkflowID != WorkflowID || json.Unmarshal([]byte(actionRun.EventPayload), &payload) != nil || payload.AgentRunID == 0 {
		return nil, util.NewNotExistErrorf("action run %d is not an agent run", actionRun.ID)
	}
	r, err := GetRunByID(ctx, payload.AgentRunID)
	if err != nil {
		return nil, err
	}
	if r.RepoID != actionRun.RepoID || (r.ActionRunID != 0 && r.ActionRunID != actionRun.ID) {
		return nil, util.NewPermissionDeniedErrorf("action run %d does not belong to agent run %d", actionRun.ID, r.ID)
	}
	return r, nil
}

// GetRunOfTask returns the agent run a task executes, only for the first task of the run so a rerun never gets the secrets
func GetRunOfTask(ctx context.Context, task *actions_model.ActionTask) (*Run, error) {
	if err := task.LoadAttributes(ctx); err != nil {
		return nil, err
	}
	r, err := GetRunOfActionRun(ctx, task.Job.Run)
	if err != nil {
		return nil, err
	}
	if r.ActionTaskID != task.ID {
		return nil, util.NewPermissionDeniedErrorf("task %d is not the task of agent run %d", task.ID, r.ID)
	}
	return r, nil
}

// ClaimRunTask binds the first task picked for the run's job, later tasks of the same run are refused
func ClaimRunTask(ctx context.Context, r *Run, taskID int64) (bool, error) {
	n, err := db.GetEngine(ctx).Table("agent_run").Where("id = ? AND action_task_id = 0", r.ID).
		Update(map[string]any{"action_task_id": taskID})
	if err != nil {
		return false, err
	}
	if n == 1 {
		r.ActionTaskID = taskID
		return true, nil
	}
	return r.ActionTaskID == taskID, nil
}

type FindRunsOptions struct {
	db.ListOptions
	SessionID     int64
	IssueID       int64 // runs of the sessions conversing on the issue
	AgentID       int64
	TriggerUserID int64
	Statuses      []Status
}

func (opts FindRunsOptions) ToConds() builder.Cond {
	cond := builder.NewCond()
	if opts.SessionID > 0 {
		cond = cond.And(builder.Eq{"session_id": opts.SessionID})
	}
	if opts.IssueID > 0 {
		cond = cond.And(issueRunsCond(opts.IssueID))
	}
	if opts.AgentID > 0 {
		cond = cond.And(builder.In("session_id", builder.Select("id").From("agent_session").Where(builder.Eq{"agent_id": opts.AgentID})))
	}
	if opts.TriggerUserID > 0 {
		cond = cond.And(builder.Eq{"trigger_user_id": opts.TriggerUserID})
	}
	if len(opts.Statuses) > 0 {
		cond = cond.And(builder.In("status", opts.Statuses))
	}
	return cond
}

func (opts FindRunsOptions) ToOrders() string {
	return "id"
}

func InsertRun(ctx context.Context, r *Run) error {
	return db.Insert(ctx, r)
}

func UpdateRun(ctx context.Context, r *Run, cols ...string) error {
	_, err := db.GetEngine(ctx).ID(r.ID).Cols(cols...).Update(r)
	return err
}

// UpdateRunStatus only moves runs that are not done yet, the first final status wins
func UpdateRunStatus(ctx context.Context, r *Run, status Status, cols ...string) (bool, error) {
	r.Status = status
	if status == StatusRunning && r.Started == 0 {
		r.Started = timeutil.TimeStampNow()
		cols = append(cols, "started")
	}
	if status.IsDone() {
		r.Stopped = timeutil.TimeStampNow()
		cols = append(cols, "stopped")
	}
	n, err := db.GetEngine(ctx).ID(r.ID).
		NotIn("status", StatusSuccess, StatusFailure, StatusCancelled).
		Cols(append(cols, "status")...).Update(r)
	return n == 1, err
}

// FindQueuedSessionIDs lists sessions with queued runs and no active run, oldest first
func FindQueuedSessionIDs(ctx context.Context) ([]int64, error) {
	ids := make([]int64, 0, 8)
	active := builder.Select("session_id").From("agent_run").Where(builder.In("status", StatusWaiting, StatusRunning))
	err := db.GetEngine(ctx).Table("agent_run").
		Where(builder.Eq{"status": StatusQueued}.And(builder.NotIn("session_id", active))).
		GroupBy("session_id").Select("session_id").OrderBy("MIN(id)").Find(&ids)
	return ids, err
}

func GetFirstQueuedRun(ctx context.Context, sessionID int64) (*Run, error) {
	r := &Run{}
	has, err := db.GetEngine(ctx).Where("session_id = ? AND status = ?", sessionID, StatusQueued).OrderBy("id").Get(r)
	if err != nil || !has {
		return nil, err
	}
	return r, nil
}

// CountRepoRunsSince counts the runs of a purpose a user triggered in a repository since a time
func CountRepoRunsSince(ctx context.Context, repoID, userID int64, purpose RunPurpose, since timeutil.TimeStamp) (int64, error) {
	return db.GetEngine(ctx).Where(builder.Eq{"repo_id": repoID, "trigger_user_id": userID, "purpose": purpose}.And(builder.Gte{"created": since})).Count(new(Run))
}

func CountActiveRunsByUser(ctx context.Context, userID int64) (int64, error) {
	return db.GetEngine(ctx).Where(builder.Eq{"trigger_user_id": userID}.And(builder.In("status", StatusWaiting, StatusRunning))).Count(new(Run))
}

func HasActiveRun(ctx context.Context, sessionID int64) (bool, error) {
	return db.GetEngine(ctx).Where(builder.Eq{"session_id": sessionID}.And(builder.In("status", StatusWaiting, StatusRunning))).Exist(new(Run))
}

func GetLatestRun(ctx context.Context, sessionID int64) (*Run, error) {
	r := &Run{}
	has, err := db.GetEngine(ctx).Where("session_id = ?", sessionID).OrderBy("id DESC").Get(r)
	if err != nil || !has {
		return nil, err
	}
	return r, nil
}

func issueRunsCond(issueID int64) builder.Cond {
	return builder.In("session_id", builder.Select("id").From("agent_session").Where(builder.Eq{"issue_id": issueID}))
}

// FindReviewingRuns returns the runs of an issue that a push to its pull request concerns:
// the running reviews and reviewed merges, and the merges scheduled after a review
func FindReviewingRuns(ctx context.Context, issueID int64) ([]*Run, error) {
	running := builder.Eq{"status": StatusRunning}.And(builder.Eq{"purpose": RunPurposeReview}.Or(builder.Eq{"purpose": RunPurposeMerge, "merge_review": true}))
	runs := make([]*Run, 0, 2)
	return runs, db.GetEngine(ctx).Where(issueRunsCond(issueID).And(running.Or(builder.Neq{"merge_head": ""}))).OrderBy("id").Find(&runs)
}

// ClearMergeHeads forgets the merges scheduled by the earlier runs of an issue, a new one replaces them
func ClearMergeHeads(ctx context.Context, issueID, exceptRunID int64) error {
	_, err := db.GetEngine(ctx).Where(issueRunsCond(issueID).And(builder.Neq{"id": exceptRunID}).And(builder.Neq{"merge_head": ""})).
		Cols("merge_head").Update(&Run{})
	return err
}

func FindFirstRuns(ctx context.Context, sessionID int64, limit int) ([]*Run, error) {
	runs := make([]*Run, 0, limit)
	return runs, db.GetEngine(ctx).Where("session_id = ?", sessionID).OrderBy("id").Limit(limit).Find(&runs)
}

// GetPreviousRun returns the run of the session created before runID
func GetPreviousRun(ctx context.Context, sessionID, runID int64) (*Run, error) {
	r := &Run{}
	has, err := db.GetEngine(ctx).Where("session_id = ? AND id < ?", sessionID, runID).OrderBy("id DESC").Get(r)
	if err != nil || !has {
		return nil, err
	}
	return r, nil
}

// GetActiveRunByAccessToken returns the running agent run a bot token was issued for
func GetActiveRunByAccessToken(ctx context.Context, tokenID int64) (*Run, error) {
	r := &Run{}
	has, err := db.GetEngine(ctx).Where(builder.Eq{"access_token_id": tokenID}.And(builder.In("status", StatusWaiting, StatusRunning))).Get(r)
	if err != nil {
		return nil, err
	} else if !has {
		return nil, util.NewNotExistErrorf("no active agent run for token %d", tokenID)
	}
	return r, nil
}

// GetActiveRunByModelToken returns the active run a model token was issued for, the token has the form "<run id>.<secret>"
func GetActiveRunByModelToken(ctx context.Context, token string) (*Run, error) {
	idStr, secret, _ := strings.Cut(token, ".")
	id, _ := strconv.ParseInt(idStr, 10, 64)
	r, err := GetRunByID(ctx, id)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(secret))
	if r.ModelTokenHash == "" || subtle.ConstantTimeCompare([]byte(r.ModelTokenHash), []byte(hex.EncodeToString(sum[:]))) != 1 ||
		(r.Status != StatusWaiting && r.Status != StatusRunning) {
		return nil, util.NewNotExistErrorf("no active agent run for this model token")
	}
	return r, nil
}

// IsParticipant reports whether the user triggered a run of the session
func IsParticipant(ctx context.Context, sessionID, userID int64) (bool, error) {
	return db.GetEngine(ctx).Where("session_id = ? AND trigger_user_id = ?", sessionID, userID).Exist(new(Run))
}

// HasActiveRunOnBranch reports whether a run of any session is working on a branch, only one at a time may push to it
func HasActiveRunOnBranch(ctx context.Context, repoID int64, branch string) (bool, error) {
	return db.GetEngine(ctx).Where(builder.Eq{"repo_id": repoID, "push_branch": branch}.And(builder.In("status", StatusWaiting, StatusRunning))).Exist(new(Run))
}

func AddRunUsage(ctx context.Context, runID, input, cached, output int64) error {
	_, err := db.GetEngine(ctx).Exec("UPDATE agent_run SET input_tokens = input_tokens + ?, cached_input_tokens = cached_input_tokens + ?, output_tokens = output_tokens + ? WHERE id = ?",
		input, cached, output, runID)
	return err
}

// Usage is the consumption of a user with an agent
type Usage struct {
	UserID            int64
	AgentID           int64
	Runs              int64
	InputTokens       int64
	CachedInputTokens int64
	OutputTokens      int64
}

// FindUsage sums the runs started since a time per user and agent, of one user when userID is set
func FindUsage(ctx context.Context, since timeutil.TimeStamp, userID int64) ([]*Usage, error) {
	cond := builder.Gte{"agent_run.created": since}.And(builder.Neq{"agent_run.status": StatusQueued})
	if userID > 0 {
		cond = cond.And(builder.Eq{"agent_run.trigger_user_id": userID})
	}
	usage := make([]*Usage, 0, 8)
	err := db.GetEngine(ctx).Table("agent_run").
		Join("INNER", "agent_session", "agent_session.id = agent_run.session_id").
		Where(cond).
		GroupBy("agent_run.trigger_user_id, agent_session.agent_id").
		Select("agent_run.trigger_user_id AS user_id, agent_session.agent_id AS agent_id, COUNT(*) AS runs, " +
			"SUM(agent_run.input_tokens) AS input_tokens, SUM(agent_run.cached_input_tokens) AS cached_input_tokens, SUM(agent_run.output_tokens) AS output_tokens").
		OrderBy("input_tokens DESC").
		Find(&usage)
	return usage, err
}

// ClaimModelRequest reserves a request before contacting the provider, including failed requests.
func ClaimModelRequest(ctx context.Context, runID int64, limit int) (bool, error) {
	n, err := db.GetEngine(ctx).Exec("UPDATE agent_run SET model_requests = model_requests + 1, model_request_active = ? WHERE id = ? AND model_request_active = ? AND model_requests < ? AND status IN (?, ?)",
		true, runID, false, limit, StatusWaiting, StatusRunning)
	if err != nil {
		return false, err
	}
	count, err := n.RowsAffected()
	return count == 1, err
}

func ReleaseModelRequest(ctx context.Context, runID int64) error {
	_, err := db.GetEngine(ctx).ID(runID).Cols("model_request_active").Update(&Run{ModelRequestActive: false})
	return err
}
