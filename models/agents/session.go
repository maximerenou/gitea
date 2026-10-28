// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"gitea.dev/models/db"
	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unit"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/timeutil"
	"gitea.dev/modules/util"

	"xorm.io/builder"
)

type SessionKind string

const (
	SessionKindTask  SessionKind = "task"  // started from the repository Agents tab
	SessionKindIssue SessionKind = "issue" // started by a mention in an issue
	SessionKindPull  SessionKind = "pull"  // started by a mention in a pull request
)

// Session is the Codex conversation of an agent on an issue, a pull request or a task, it keeps one branch and pull request across its runs.
// Anyone with a key for the agent can continue it, each run uses the key and the permissions of the user who triggered it.
type Session struct {
	ID        int64
	RepoID    int64       `xorm:"INDEX NOT NULL"`
	CreatorID int64       `xorm:"INDEX NOT NULL DEFAULT 0"`
	AgentID   int64       `xorm:"INDEX NOT NULL DEFAULT 0"`
	Kind      SessionKind `xorm:"VARCHAR(16) NOT NULL"`
	Title     string      `xorm:"VARCHAR(255) NOT NULL"`
	Status    Status      `xorm:"VARCHAR(16) INDEX NOT NULL"`

	// IssueID is the issue or pull request holding the conversation, task sessions set it once their pull request exists
	IssueID    int64  `xorm:"INDEX NOT NULL DEFAULT 0"`
	BaseBranch string `xorm:"VARCHAR(255) NOT NULL DEFAULT ''"`
	// Branch is where the session pushes, its pull request is maintained by the session when OwnsPull is set
	Branch   string `xorm:"VARCHAR(255) NOT NULL DEFAULT ''"`
	OwnsPull bool   `xorm:"NOT NULL DEFAULT false"`

	ReasoningEffort string `xorm:"VARCHAR(16) NOT NULL DEFAULT ''"` // overrides the agent's
	// the agent fixes the CI failures of its pull request on its own a limited number of times, once per failing commit
	CIFixSHA      string `xorm:"VARCHAR(64) NOT NULL DEFAULT ''"`
	CIFixAttempts int    `xorm:"NOT NULL DEFAULT 0"`
	CodexThreadID string `xorm:"VARCHAR(255) NOT NULL DEFAULT ''"`
	HasState      bool   `xorm:"NOT NULL DEFAULT false"`

	Repo    *repo_model.Repository `xorm:"-"`
	Creator *user_model.User       `xorm:"-"`
	Agent   *Agent                 `xorm:"-"`
	Issue   *issues_model.Issue    `xorm:"-"`

	Created timeutil.TimeStamp `xorm:"created"`
	Updated timeutil.TimeStamp `xorm:"updated"`
}

// SessionKinds are the kinds of sessions, in the order they are offered
var SessionKinds = []SessionKind{SessionKindTask, SessionKindIssue, SessionKindPull}

func (k SessionKind) IsValid() bool {
	return slices.Contains(SessionKinds, k)
}

func (*Session) TableName() string {
	return "agent_session"
}

func (s *Session) Link() string {
	if s.Repo == nil {
		return ""
	}
	return fmt.Sprintf("%s/agents/%d", s.Repo.Link(), s.ID)
}

func (s *Session) LoadAttributes(ctx context.Context) (err error) {
	if s.Repo == nil {
		if s.Repo, err = repo_model.GetRepositoryByID(ctx, s.RepoID); err != nil {
			return err
		}
	}
	if s.Creator == nil {
		if _, s.Creator, err = user_model.GetPossibleUserByID(ctx, s.CreatorID); err != nil {
			return err
		}
	}
	if s.Agent == nil {
		s.Agent, err = GetAgentByID(ctx, s.AgentID)
		if err != nil && !errors.Is(err, util.ErrNotExist) {
			return err
		}
		if s.Agent != nil {
			if err = s.Agent.LoadBot(ctx); err != nil {
				return err
			}
		}
	}
	if s.Issue == nil && s.IssueID > 0 {
		if s.Issue, err = issues_model.GetIssueByID(ctx, s.IssueID); err != nil {
			return err
		}
		s.Issue.Repo = s.Repo
	}
	return nil
}

func GetSessionByID(ctx context.Context, id int64) (*Session, error) {
	s, exist, err := db.GetByID[Session](ctx, id)
	if err != nil {
		return nil, err
	} else if !exist {
		return nil, util.NewNotExistErrorf("agent session %d does not exist", id)
	}
	return s, nil
}

func GetRepoSession(ctx context.Context, repoID, id int64) (*Session, error) {
	s, err := GetSessionByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if s.RepoID != repoID {
		return nil, util.NewNotExistErrorf("agent session %d does not exist", id)
	}
	return s, nil
}

// GetConversationSession finds the session of an agent on an issue or pull request
func GetConversationSession(ctx context.Context, agentID, issueID int64) (*Session, error) {
	s := &Session{}
	has, err := db.GetEngine(ctx).Where("agent_id = ? AND issue_id = ?", agentID, issueID).OrderBy("id").Get(s)
	if err != nil {
		return nil, err
	} else if !has {
		return nil, util.NewNotExistErrorf("no session of agent %d on issue %d", agentID, issueID)
	}
	return s, nil
}

type FindSessionsOptions struct {
	db.ListOptions
	RepoID        int64
	AgentID       int64
	CreatorID     int64
	Kind          SessionKind
	Status        Status
	ParticipantID int64            // sessions the user triggered a run in
	ReadableBy    *user_model.User // in repositories whose code the user can read, and of issues and pull requests they can read
	// the issues or pull requests of the repository the user cannot read, their sessions show their titles
	HideIssues, HidePulls bool
}

// issueSessionsCond keeps the sessions without issue and those of the issues matching cond
func issueSessionsCond(cond builder.Cond) builder.Cond {
	return builder.Eq{"issue_id": 0}.Or(builder.In("issue_id", builder.Select("id").From("issue").Where(cond)))
}

func (opts FindSessionsOptions) ToConds() builder.Cond {
	cond := builder.NewCond()
	if opts.RepoID > 0 {
		cond = cond.And(builder.Eq{"repo_id": opts.RepoID})
	}
	if opts.AgentID > 0 {
		cond = cond.And(builder.Eq{"agent_id": opts.AgentID})
	}
	if opts.CreatorID > 0 {
		cond = cond.And(builder.Eq{"creator_id": opts.CreatorID})
	}
	if opts.Kind != "" {
		cond = cond.And(builder.Eq{"kind": opts.Kind})
	}
	if opts.Status != "" {
		cond = cond.And(builder.Eq{"status": opts.Status})
	}
	if opts.ParticipantID > 0 {
		cond = cond.And(builder.In("id", builder.Select("session_id").From("agent_run").Where(builder.Eq{"trigger_user_id": opts.ParticipantID})))
	}
	if opts.ReadableBy != nil {
		cond = cond.And(builder.In("repo_id", builder.Select("id").From("repository").Where(repo_model.AccessibleRepositoryCondition(opts.ReadableBy, unit.TypeCode))))
		readable := func(unitType unit.Type) builder.Cond {
			return builder.In("repo_id", builder.Select("id").From("repository").Where(repo_model.AccessibleRepositoryCondition(opts.ReadableBy, unitType)))
		}
		cond = cond.And(issueSessionsCond(builder.Eq{"is_pull": false}.And(readable(unit.TypeIssues)).Or(builder.Eq{"is_pull": true}.And(readable(unit.TypePullRequests)))))
	}
	if opts.HideIssues && opts.HidePulls {
		cond = cond.And(builder.Eq{"issue_id": 0})
	} else if opts.HideIssues || opts.HidePulls {
		cond = cond.And(issueSessionsCond(builder.Eq{"is_pull": opts.HideIssues}))
	}
	return cond
}

func (opts FindSessionsOptions) ToOrders() string {
	return "updated DESC, id DESC"
}

func InsertSession(ctx context.Context, s *Session) error {
	return db.Insert(ctx, s)
}

func UpdateSession(ctx context.Context, s *Session, cols ...string) error {
	_, err := db.GetEngine(ctx).ID(s.ID).Cols(cols...).Update(s)
	return err
}

// FindIssueSessions returns the sessions conversing on an issue or pushing to a pull request branch
func FindIssueSessions(ctx context.Context, repoID, issueID int64, headBranch string) ([]*Session, error) {
	cond := builder.Eq{"issue_id": issueID}.Or(builder.Eq{"branch": headBranch}.And(builder.Neq{"branch": ""}))
	sessions := make([]*Session, 0, 2)
	return sessions, db.GetEngine(ctx).Where(builder.Eq{"repo_id": repoID}.And(cond)).OrderBy("id").Find(&sessions)
}

// GetSessionByBranch returns the session pushing to a branch of a repository
func GetSessionByBranch(ctx context.Context, repoID int64, branch string) (*Session, error) {
	s := &Session{}
	has, err := db.GetEngine(ctx).Where("repo_id = ? AND branch = ?", repoID, branch).Get(s)
	if err != nil {
		return nil, err
	} else if !has || branch == "" {
		return nil, util.NewNotExistErrorf("no agent session pushes to %q", branch)
	}
	return s, nil
}

// EffectiveReasoningEffort is the reasoning effort of the session's runs, empty keeps the model default
func (s *Session) EffectiveReasoningEffort() string {
	if s.ReasoningEffort != "" || s.Agent == nil {
		return s.ReasoningEffort
	}
	return s.Agent.ReasoningEffort
}

// FindIdleSessionsWithState returns the sessions keeping a Codex state that were not updated since a time
func FindIdleSessionsWithState(ctx context.Context, before timeutil.TimeStamp) ([]*Session, error) {
	sessions := make([]*Session, 0, 8)
	return sessions, db.GetEngine(ctx).Where(builder.Eq{"has_state": true}.And(builder.Lt{"updated": before}).And(builder.NotIn("status", StatusQueued, StatusWaiting, StatusRunning))).Find(&sessions)
}

// FindPullOwningSessions returns the sessions maintaining a pull request in a repository
func FindPullOwningSessions(ctx context.Context, repoID int64) ([]*Session, error) {
	sessions := make([]*Session, 0, 4)
	return sessions, db.GetEngine(ctx).Where("repo_id = ? AND owns_pull = ? AND branch <> ''", repoID, true).Find(&sessions)
}

// FindMergingSessions returns the sessions of a repository whose agent scheduled the merge of a pull request of the repository,
// they maintain its branch until it merges
func FindMergingSessions(ctx context.Context, repoID int64) ([]*Session, error) {
	merged := builder.Select("session_id").From("agent_run").Where(builder.Eq{"purpose": RunPurposeMerge, "status": StatusSuccess})
	scheduled := builder.Select("pull_request.issue_id").From("pull_request").
		Join("INNER", "pull_auto_merge", "pull_auto_merge.pull_id = pull_request.id").
		Where(builder.Eq{"pull_request.base_repo_id": repoID, "pull_request.head_repo_id": repoID, "pull_request.has_merged": false})
	sessions := make([]*Session, 0, 2)
	return sessions, db.GetEngine(ctx).Where(builder.Eq{"repo_id": repoID, "kind": SessionKindPull}.
		And(builder.In("id", merged)).And(builder.In("issue_id", scheduled))).Find(&sessions)
}

// UpdateIssueSessionsTitle renames the conversations on an issue after it, development sessions keep the title of their task
func UpdateIssueSessionsTitle(ctx context.Context, issueID int64, title string) error {
	_, err := db.GetEngine(ctx).Where(builder.Eq{"issue_id": issueID}.And(builder.Neq{"kind": SessionKindTask})).
		Cols("title").NoAutoTime().Update(&Session{Title: title})
	return err
}
