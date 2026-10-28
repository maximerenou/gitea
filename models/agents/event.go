// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"context"

	"gitea.dev/models/db"
	"gitea.dev/modules/timeutil"

	"xorm.io/builder"
)

// Event is a Codex CLI JSONL event of a run, kept as received so the UI can replay a session
type Event struct {
	ID        int64
	SessionID int64  `xorm:"INDEX NOT NULL"`
	RunID     int64  `xorm:"INDEX NOT NULL"`
	Type      string `xorm:"VARCHAR(64) NOT NULL"`
	ItemID    string `xorm:"VARCHAR(255) NOT NULL DEFAULT ''"`
	Payload   string `xorm:"LONGTEXT"`

	Created timeutil.TimeStamp `xorm:"created"`
}

func (*Event) TableName() string {
	return "agent_event"
}

func InsertEvents(ctx context.Context, events []*Event) error {
	if len(events) == 0 {
		return nil
	}
	return db.Insert(ctx, events)
}

func CountRunEvents(ctx context.Context, runID int64) (int64, error) {
	return db.GetEngine(ctx).Where("run_id = ?", runID).Count(new(Event))
}

// FindSessionEvents returns the events after afterID, oldest first
func FindSessionEvents(ctx context.Context, sessionID, afterID int64, limit int) ([]*Event, error) {
	events := make([]*Event, 0, limit)
	return events, db.GetEngine(ctx).Where("session_id = ? AND id > ?", sessionID, afterID).OrderBy("id").Limit(limit).Find(&events)
}

type FindEventsOptions struct {
	db.ListOptions
	SessionID int64
}

func (opts FindEventsOptions) ToConds() builder.Cond {
	return builder.Eq{"session_id": opts.SessionID}
}

func (opts FindEventsOptions) ToOrders() string {
	return "id"
}

// DeleteEventsOfRunsStoppedBefore removes the logs of runs finished before a time, the runs themselves stay as history
func DeleteEventsOfRunsStoppedBefore(ctx context.Context, before timeutil.TimeStamp) (int64, error) {
	runs := builder.Select("id").From("agent_run").Where(builder.Lt{"stopped": before}.And(builder.Gt{"stopped": 0}))
	return db.GetEngine(ctx).Where(builder.In("run_id", runs)).Delete(new(Event))
}
