// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"context"

	"gitea.dev/models/db"
	"gitea.dev/modules/timeutil"
	"gitea.dev/modules/util"
)

// Session is one agent working on one subject: an issue or pull request (IssueID), or the repository as a whole (IssueID 0).
type Session struct {
	ID             int64              `xorm:"pk autoincr"`
	AgentID        int64              `xorm:"INDEX NOT NULL"`
	RepoID         int64              `xorm:"INDEX NOT NULL"`
	IssueID        int64              `xorm:"INDEX NOT NULL DEFAULT 0"`
	TriggerUserID  int64              `xorm:"NOT NULL DEFAULT 0"`
	ConversationID string             `xorm:"VARCHAR(255) NOT NULL DEFAULT ''"` // the agent CLI's own conversation id, used to resume
	ChainDepth     int                `xorm:"NOT NULL DEFAULT 0"`
	CreatedUnix    timeutil.TimeStamp `xorm:"created NOT NULL"`
	UpdatedUnix    timeutil.TimeStamp `xorm:"updated NOT NULL"`
}

func init() {
	db.RegisterModel(new(Session))
}

func (*Session) TableName() string {
	return "agent_session"
}

var ErrSessionNotFound = util.NewNotExistErrorf("agent session does not exist")

func CreateSession(ctx context.Context, s *Session) error {
	if s.AgentID <= 0 || s.RepoID <= 0 {
		return util.NewInvalidArgumentErrorf("a session needs an agent and a repository")
	}
	return db.Insert(ctx, s)
}

func GetSessionByID(ctx context.Context, id int64) (*Session, error) {
	s, exist, err := db.GetByID[Session](ctx, id)
	if err != nil {
		return nil, err
	} else if !exist {
		return nil, ErrSessionNotFound
	}
	return s, nil
}

// GetSessionBySubject returns the latest session of an agent on an issue or pull request, which later requests resume.
func GetSessionBySubject(ctx context.Context, agentID, issueID int64) (*Session, error) {
	s := &Session{}
	has, err := db.GetEngine(ctx).Where("agent_id = ? AND issue_id = ?", agentID, issueID).OrderBy("id DESC").Get(s)
	if err != nil {
		return nil, err
	} else if !has {
		return nil, ErrSessionNotFound
	}
	return s, nil
}

func SetSessionConversation(ctx context.Context, sessionID int64, conversationID string) error {
	_, err := db.GetEngine(ctx).ID(sessionID).Cols("conversation_id").Update(&Session{ConversationID: util.TruncateRunes(conversationID, 255)})
	return err
}
