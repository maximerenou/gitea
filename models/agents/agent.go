// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"context"
	"fmt"
	"slices"

	"gitea.dev/models/db"
	"gitea.dev/modules/agents"
	"gitea.dev/modules/timeutil"
	"gitea.dev/modules/util"

	"xorm.io/builder"
)

const MaxAgentNameLength = 40

type AccessMode string

const (
	AccessWriters AccessMode = "writers" // every user with write access to the repository (default)
	AccessList    AccessMode = "list"    // owner plus an explicit list
	AccessOwner   AccessMode = "owner"   // owner only
)

func (m AccessMode) IsValid() bool {
	return slices.Contains([]AccessMode{AccessWriters, AccessList, AccessOwner}, m)
}

// AccessEntries names who may command an agent in "list" mode.
type AccessEntries struct {
	UserIDs []int64
	TeamIDs []int64
}

// Agent is an LLM acting through a dedicated bot account. OwnerID 0 means a global agent.
// The action is fixed at creation; events and settings stay editable.
type Agent struct {
	ID             int64              `xorm:"pk autoincr"`
	OwnerID        int64              `xorm:"INDEX NOT NULL DEFAULT 0"`
	BotUserID      int64              `xorm:"UNIQUE NOT NULL"`
	Name           string             `xorm:"VARCHAR(40) NOT NULL"` // same as the bot's user name
	ConnectionID   int64              `xorm:"INDEX NOT NULL"`
	Model          string             `xorm:"VARCHAR(255) NOT NULL"`
	Description    string             `xorm:"TEXT"`
	Language       string             `xorm:"VARCHAR(32) NOT NULL DEFAULT ''"` // empty: follow the triggering message, else English
	Action         agents.Action      `xorm:"VARCHAR(32) NOT NULL"`
	Events         []agents.Event     `xorm:"JSON TEXT"`
	ActionSettings map[string]any     `xorm:"JSON TEXT"`
	AccessMode     AccessMode         `xorm:"VARCHAR(16) NOT NULL DEFAULT 'writers'"`
	AccessList     *AccessEntries     `xorm:"JSON TEXT"`
	CreatedUnix    timeutil.TimeStamp `xorm:"created NOT NULL"`
	UpdatedUnix    timeutil.TimeStamp `xorm:"updated NOT NULL"`
}

func init() {
	db.RegisterModel(new(Agent))
}

var ErrAgentNotFound = util.NewNotExistErrorf("agent does not exist")

func (a *Agent) IsGlobal() bool {
	return a.OwnerID == 0
}

// Validate checks everything that does not need the database.
func (a *Agent) Validate() error {
	switch {
	case a.Name == "" || len(a.Name) > MaxAgentNameLength:
		return util.NewInvalidArgumentErrorf("agent name must be 1-%d characters", MaxAgentNameLength)
	case a.Model == "":
		return util.NewInvalidArgumentErrorf("model is required")
	case !a.AccessMode.IsValid():
		return util.NewInvalidArgumentErrorf("unknown access mode %q", a.AccessMode)
	}
	if err := agents.ValidateEvents(a.Action, a.Events); err != nil {
		return util.NewInvalidArgumentErrorf("%v", err)
	}
	return nil
}

func CreateAgent(ctx context.Context, a *Agent) error {
	if a.AccessMode == "" {
		a.AccessMode = AccessWriters
	}
	if err := a.Validate(); err != nil {
		return err
	}
	if _, err := GetConnectionByID(ctx, a.ConnectionID); err != nil {
		return fmt.Errorf("CreateAgent: connection %d: %w", a.ConnectionID, err)
	}
	return db.Insert(ctx, a)
}

func GetAgentByID(ctx context.Context, id int64) (*Agent, error) {
	a, exist, err := db.GetByID[Agent](ctx, id)
	if err != nil {
		return nil, err
	} else if !exist {
		return nil, ErrAgentNotFound
	}
	return a, nil
}

func GetAgentByBotUserID(ctx context.Context, botUserID int64) (*Agent, error) {
	a := &Agent{}
	has, err := db.GetEngine(ctx).Where("bot_user_id = ?", botUserID).Get(a)
	if err != nil {
		return nil, err
	} else if !has {
		return nil, ErrAgentNotFound
	}
	return a, nil
}

// IsAgentBot reports whether the user is the bot of an agent; used to scope bot-specific restrictions to agent bots only.
func IsAgentBot(ctx context.Context, userID int64) (bool, error) {
	return db.GetEngine(ctx).Where("bot_user_id = ?", userID).Exist(new(Agent))
}

// UpdateAgent saves the mutable fields; the action, bot and name never change.
func UpdateAgent(ctx context.Context, a *Agent) error {
	if err := a.Validate(); err != nil {
		return err
	}
	_, err := db.GetEngine(ctx).ID(a.ID).
		Cols("connection_id", "model", "description", "language", "events", "action_settings", "access_mode", "access_list").
		Update(a)
	return err
}

func DeleteAgent(ctx context.Context, id int64) error {
	_, err := db.GetEngine(ctx).ID(id).Delete(new(Agent))
	return err
}

type FindAgentsOptions struct {
	db.ListOptions
	OwnerID    int64
	OnlyGlobal bool // OwnerID 0 is otherwise "no filter"
}

func (opts FindAgentsOptions) ToConds() builder.Cond {
	cond := builder.NewCond()
	if opts.OnlyGlobal {
		cond = cond.And(builder.Eq{"owner_id": 0})
	} else if opts.OwnerID != 0 {
		cond = cond.And(builder.Eq{"owner_id": opts.OwnerID})
	}
	return cond
}

func (opts FindAgentsOptions) ToOrders() string {
	return "name, id"
}
