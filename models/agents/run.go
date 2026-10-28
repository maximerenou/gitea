// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"context"
	"slices"

	"gitea.dev/models/db"
	"gitea.dev/modules/timeutil"
	"gitea.dev/modules/util"

	"xorm.io/builder"
)

type RunStatus string

const (
	RunPending    RunStatus = "pending" // created, the Actions run has not reported anything yet
	RunRunning    RunStatus = "running"
	RunSuccess    RunStatus = "success"
	RunFailed     RunStatus = "failed"
	RunCancelled  RunStatus = "cancelled"
	RunNeedsHuman RunStatus = "needs_human"
)

var terminalRunStatuses = []RunStatus{RunSuccess, RunFailed, RunCancelled, RunNeedsHuman}

func (s RunStatus) IsTerminal() bool {
	return slices.Contains(terminalRunStatuses, s)
}

const MaxProgressLength = 64 * 1024

// Run is one execution of a session on an Actions runner.
type Run struct {
	ID               int64              `xorm:"pk autoincr"`
	SessionID        int64              `xorm:"INDEX NOT NULL"`
	AgentID          int64              `xorm:"INDEX NOT NULL"`
	RepoID           int64              `xorm:"INDEX NOT NULL"`
	IssueID          int64              `xorm:"INDEX NOT NULL DEFAULT 0"`
	ActionRunID      int64              `xorm:"INDEX NOT NULL DEFAULT 0"`
	Status           RunStatus          `xorm:"VARCHAR(16) INDEX NOT NULL"`
	TriggerEvent     string             `xorm:"VARCHAR(32) NOT NULL DEFAULT ''"`
	TriggerUserID    int64              `xorm:"NOT NULL DEFAULT 0"` // 0 for runs without a person, which get no session token
	TriggerCommentID int64              `xorm:"NOT NULL DEFAULT 0"`
	ConfigCommitSHA  string             `xorm:"VARCHAR(64) NOT NULL DEFAULT ''"` // commit of the agent's .agent repository used for this run
	Summary          string             `xorm:"TEXT"`
	Error            string             `xorm:"TEXT"`
	Result           string             `xorm:"LONGTEXT"` // the validated result.json that was received
	Notes            string             `xorm:"TEXT"`     // what the service ignored or could not apply
	Progress         string             `xorm:"LONGTEXT"`
	CreatedUnix      timeutil.TimeStamp `xorm:"created NOT NULL"`
	UpdatedUnix      timeutil.TimeStamp `xorm:"updated NOT NULL"`
	EndedUnix        timeutil.TimeStamp `xorm:"NOT NULL DEFAULT 0"`
}

func init() {
	db.RegisterModel(new(Run))
}

func (*Run) TableName() string {
	return "agent_run"
}

var ErrRunNotFound = util.NewNotExistErrorf("agent run does not exist")

func CreateRun(ctx context.Context, r *Run) error {
	if r.SessionID <= 0 || r.AgentID <= 0 || r.RepoID <= 0 {
		return util.NewInvalidArgumentErrorf("a run needs a session, an agent and a repository")
	}
	if r.Status == "" {
		r.Status = RunPending
	}
	return db.Insert(ctx, r)
}

func GetRunByID(ctx context.Context, id int64) (*Run, error) {
	r, exist, err := db.GetByID[Run](ctx, id)
	if err != nil {
		return nil, err
	} else if !exist {
		return nil, ErrRunNotFound
	}
	return r, nil
}

func GetRunByActionRunID(ctx context.Context, actionRunID int64) (*Run, error) {
	r := &Run{}
	has, err := db.GetEngine(ctx).Where("action_run_id = ?", actionRunID).Get(r)
	if err != nil {
		return nil, err
	} else if !has {
		return nil, ErrRunNotFound
	}
	return r, nil
}

// GetActiveRunOfSession returns the run of the session that has not ended, if any.
func GetActiveRunOfSession(ctx context.Context, sessionID int64) (*Run, error) {
	r := &Run{}
	has, err := db.GetEngine(ctx).Where(builder.Eq{"session_id": sessionID}.And(builder.NotIn("status", terminalRunStatuses))).OrderBy("id DESC").Get(r)
	if err != nil {
		return nil, err
	} else if !has {
		return nil, ErrRunNotFound
	}
	return r, nil
}

func ListRunsOfIssue(ctx context.Context, issueID int64) ([]*Run, error) {
	runs := make([]*Run, 0, 4)
	return runs, db.GetEngine(ctx).Where("issue_id = ?", issueID).OrderBy("id").Find(&runs)
}

// SetRunActionRun links the Actions run created for it.
func SetRunActionRun(ctx context.Context, runID, actionRunID int64) error {
	_, err := db.GetEngine(ctx).ID(runID).Cols("action_run_id").Update(&Run{ActionRunID: actionRunID})
	return err
}

// MarkRunRunning moves a pending run to running; any other state is left alone.
func MarkRunRunning(ctx context.Context, runID int64) error {
	_, err := db.GetEngine(ctx).Where("id = ? AND status = ?", runID, RunPending).Cols("status").Update(&Run{Status: RunRunning})
	return err
}

// FinishRun records the end of a run exactly once: it only applies while the run has not reached a final status,
// so a late result, a second call or a cancellation racing with a result cannot overwrite what was decided first.
// It reports whether this call was the one that ended the run.
func FinishRun(ctx context.Context, runID int64, status RunStatus, summary, errMsg, result, notes string) (bool, error) {
	if !status.IsTerminal() {
		return false, util.NewInvalidArgumentErrorf("%q is not a final run status", status)
	}
	affected, err := db.GetEngine(ctx).
		Where(builder.Eq{"id": runID}.And(builder.NotIn("status", terminalRunStatuses))).
		Cols("status", "summary", "error", "result", "notes", "ended_unix").
		Update(&Run{
			Status:    status,
			Summary:   util.TruncateRunes(summary, 4096),
			Error:     util.TruncateRunes(errMsg, 4096),
			Result:    result,
			Notes:     util.TruncateRunes(notes, 4096),
			EndedUnix: timeutil.TimeStampNow(),
		})
	return affected == 1, err
}

// AppendProgress keeps the most recent MaxProgressLength bytes of the run output.
func AppendProgress(ctx context.Context, runID int64, text string) error {
	r, err := GetRunByID(ctx, runID)
	if err != nil {
		return err
	}
	all := r.Progress + text
	if len(all) > MaxProgressLength {
		all = all[len(all)-MaxProgressLength:]
	}
	_, err = db.GetEngine(ctx).ID(runID).Cols("progress").Update(&Run{Progress: all})
	return err
}
