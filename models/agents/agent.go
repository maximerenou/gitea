// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"context"
	"fmt"
	"slices"

	"gitea.dev/models/db"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/timeutil"
	"gitea.dev/modules/util"

	"xorm.io/builder"
)

// Provider is a model provider Codex CLI can talk to, all of them use the OpenAI Responses API wire protocol
type Provider string

const (
	ProviderOpenAI Provider = "openai"
	ProviderCustom Provider = "custom"
)

const OpenAIBaseURL = "https://api.openai.com/v1"

func (p Provider) IsValid() bool {
	return p == ProviderOpenAI || p == ProviderCustom
}

// ReasoningEfforts are the values of Codex's model_reasoning_effort, empty keeps the model default
var ReasoningEfforts = []string{"minimal", "low", "medium", "high"}

func IsValidReasoningEffort(effort string) bool {
	return effort == "" || slices.Contains(ReasoningEfforts, effort)
}

// Agent is a mentionable bot defined by the site administrator, users bring their own API key to use it
type Agent struct {
	ID                 int64
	BotID              int64    `xorm:"UNIQUE NOT NULL"`
	Name               string   `xorm:"VARCHAR(255) NOT NULL"`
	Provider           Provider `xorm:"VARCHAR(32) NOT NULL"`
	BaseURL            string   `xorm:"VARCHAR(1024) NOT NULL"`
	Model              string   `xorm:"VARCHAR(255) NOT NULL"`
	AllowModelOverride bool     `xorm:"NOT NULL DEFAULT false"`
	ReasoningEffort    string   `xorm:"VARCHAR(16) NOT NULL DEFAULT ''"`
	MCPServers         string   `xorm:"TEXT"` // YAML, in the format of the mcp_servers section of the repositories' setup file
	IsDisabled         bool     `xorm:"NOT NULL DEFAULT false"`

	Bot *user_model.User `xorm:"-"`

	Created timeutil.TimeStamp `xorm:"created"`
	Updated timeutil.TimeStamp `xorm:"updated"`
}

func (*Agent) TableName() string {
	return "agent"
}

func init() {
	db.RegisterModel(new(Agent))
	db.RegisterModel(new(UserKey))
	db.RegisterModel(new(Session))
	db.RegisterModel(new(Run))
	db.RegisterModel(new(Event))
}

func (a *Agent) LoadBot(ctx context.Context) (err error) {
	if a.Bot == nil {
		a.Bot, err = user_model.GetUserByID(ctx, a.BotID)
	}
	return err
}

// MentionName is the bot account name used to mention the agent
func (a *Agent) MentionName() string {
	if a.Bot == nil {
		return ""
	}
	return a.Bot.Name
}

func GetAgentByID(ctx context.Context, id int64) (*Agent, error) {
	a, exist, err := db.GetByID[Agent](ctx, id)
	if err != nil {
		return nil, err
	} else if !exist {
		return nil, util.NewNotExistErrorf("agent %d does not exist", id)
	}
	return a, nil
}

func GetAgentByBotID(ctx context.Context, botID int64) (*Agent, error) {
	a := &Agent{}
	has, err := db.GetEngine(ctx).Where("bot_id = ?", botID).Get(a)
	if err != nil {
		return nil, err
	} else if !has {
		return nil, util.NewNotExistErrorf("no agent for bot %d", botID)
	}
	return a, nil
}

// FindAgents lists the agents with their bots, the enabled ones only when onlyEnabled is set
func FindAgents(ctx context.Context, onlyEnabled bool) ([]*Agent, error) {
	agents := make([]*Agent, 0, 4)
	sess := db.GetEngine(ctx).OrderBy("id")
	if onlyEnabled {
		sess = sess.Where("is_disabled = ?", false)
	}
	if err := sess.Find(&agents); err != nil {
		return nil, err
	}
	return agents, LoadAgentBots(ctx, agents)
}

func LoadAgentBots(ctx context.Context, agents []*Agent) error {
	ids := make([]int64, 0, len(agents))
	for _, a := range agents {
		ids = append(ids, a.BotID)
	}
	users, err := user_model.GetUsersMapByIDs(ctx, ids)
	if err != nil {
		return err
	}
	for _, a := range agents {
		a.Bot = users[a.BotID]
	}
	return nil
}

func InsertAgent(ctx context.Context, a *Agent) error {
	return db.Insert(ctx, a)
}

func UpdateAgent(ctx context.Context, a *Agent, cols ...string) error {
	_, err := db.GetEngine(ctx).ID(a.ID).Cols(cols...).Update(a)
	return err
}

func DeleteAgent(ctx context.Context, id int64) error {
	return db.WithTx(ctx, func(ctx context.Context) error {
		if _, err := db.GetEngine(ctx).Where("agent_id = ?", id).Delete(new(UserKey)); err != nil {
			return err
		}
		_, err := db.GetEngine(ctx).ID(id).Delete(new(Agent))
		return err
	})
}

// GetEnabledAgentsByBotNames maps lower-cased bot names to their enabled agent, used to resolve mentions
func GetEnabledAgentsByBotNames(ctx context.Context, names []string) (map[string]*Agent, error) {
	result := make(map[string]*Agent, len(names))
	if len(names) == 0 {
		return result, nil
	}
	bots := make([]*user_model.User, 0, len(names))
	if err := db.GetEngine(ctx).Where(builder.In("lower_name", names).And(builder.Eq{"type": user_model.UserTypeBot})).Find(&bots); err != nil {
		return nil, fmt.Errorf("GetEnabledAgentsByBotNames: %w", err)
	}
	if len(bots) == 0 {
		return result, nil
	}
	byBotID := make(map[int64]*user_model.User, len(bots))
	botIDs := make([]int64, 0, len(bots))
	for _, bot := range bots {
		byBotID[bot.ID] = bot
		botIDs = append(botIDs, bot.ID)
	}
	agents := make([]*Agent, 0, len(bots))
	if err := db.GetEngine(ctx).In("bot_id", botIDs).And("is_disabled = ?", false).Find(&agents); err != nil {
		return nil, fmt.Errorf("GetEnabledAgentsByBotNames: %w", err)
	}
	for _, a := range agents {
		a.Bot = byBotID[a.BotID]
		result[a.Bot.LowerName] = a
	}
	return result, nil
}
