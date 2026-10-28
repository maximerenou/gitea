// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"

	agents_model "gitea.dev/models/agents"
	audit_model "gitea.dev/models/audit"
	"gitea.dev/models/db"
	repo_model "gitea.dev/models/repo"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/optional"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/structs"
	"gitea.dev/modules/timeutil"
	"gitea.dev/modules/util"
	"gitea.dev/services/audit"
	user_service "gitea.dev/services/user"
)

type AgentOptions struct {
	BotName            string // only used on creation
	Name               string
	Provider           agents_model.Provider
	BaseURL            string
	Model              string
	AllowModelOverride bool
	ReasoningEffort    string
	MCPServers         string
	IsDisabled         bool
}

func validateAgentOptions(opts *AgentOptions) error {
	opts.Name = strings.TrimSpace(opts.Name)
	opts.Model = strings.TrimSpace(opts.Model)
	if opts.Name == "" || len(opts.Name) > 255 {
		return util.NewInvalidArgumentErrorf("the name is required")
	}
	if opts.Model == "" || len(opts.Model) > 255 {
		return util.NewInvalidArgumentErrorf("the model is required")
	}
	if !opts.Provider.IsValid() {
		return util.NewInvalidArgumentErrorf("unsupported provider %q", opts.Provider)
	}
	if !agents_model.IsValidReasoningEffort(opts.ReasoningEffort) {
		return util.NewInvalidArgumentErrorf("unsupported reasoning effort %q", opts.ReasoningEffort)
	}
	if _, err := ParseMCPServers(opts.MCPServers); err != nil {
		return err
	}
	baseURL, err := NormalizeBaseURL(opts.Provider, opts.BaseURL)
	if err != nil {
		return err
	}
	opts.BaseURL = baseURL
	return nil
}

// CreateAgent creates an agent and its bot account, the bot has no access of its own, it borrows the permissions of whoever triggers it
func CreateAgent(ctx context.Context, opts AgentOptions) (*agents_model.Agent, error) {
	if err := validateAgentOptions(&opts); err != nil {
		return nil, err
	}
	opts.BotName = strings.TrimSpace(opts.BotName)
	agent := &agents_model.Agent{
		Name:               opts.Name,
		Provider:           opts.Provider,
		BaseURL:            opts.BaseURL,
		Model:              opts.Model,
		AllowModelOverride: opts.AllowModelOverride,
		ReasoningEffort:    opts.ReasoningEffort,
		MCPServers:         opts.MCPServers,
		IsDisabled:         opts.IsDisabled,
	}
	err := db.WithTx(ctx, func(ctx context.Context) error {
		bot := &user_model.User{
			Name:        opts.BotName,
			FullName:    opts.Name,
			Email:       fmt.Sprintf("%s@%s", opts.BotName, cmp.Or(setting.Service.NoReplyAddress, "noreply.localhost")),
			Type:        user_model.UserTypeBot,
			Description: "Codex agent, it works with the permissions and the API key of whoever mentions it",
		}
		visibility := structs.VisibleTypePublic
		maxRepos := 0
		if err := user_model.AdminCreateUser(ctx, bot, &user_model.Meta{}, &user_model.CreateUserOverwriteOptions{
			IsActive:                optional.Some(true),
			IsRestricted:            optional.Some(true),
			KeepEmailPrivate:        optional.Some(true),
			AllowCreateOrganization: optional.Some(false),
			Visibility:              &visibility,
			MaxRepoCreation:         &maxRepos,
		}); err != nil {
			return err
		}
		agent.BotID, agent.Bot = bot.ID, bot
		return agents_model.InsertAgent(ctx, agent)
	})
	if err != nil {
		return nil, err
	}
	audit.Record(ctx, audit_model.SystemAgentAdd, nil, "agent", agent.Bot.Name)
	return agent, nil
}

func UpdateAgent(ctx context.Context, agent *agents_model.Agent, opts AgentOptions) error {
	if err := validateAgentOptions(&opts); err != nil {
		return err
	}
	agent.Name, agent.Provider, agent.BaseURL, agent.Model = opts.Name, opts.Provider, opts.BaseURL, opts.Model
	agent.AllowModelOverride, agent.ReasoningEffort, agent.IsDisabled = opts.AllowModelOverride, opts.ReasoningEffort, opts.IsDisabled
	agent.MCPServers = opts.MCPServers
	if err := agent.LoadBot(ctx); err != nil {
		return err
	}
	if agent.Bot.FullName != opts.Name {
		agent.Bot.FullName = opts.Name
		if err := user_model.UpdateUserCols(ctx, agent.Bot, "full_name"); err != nil {
			return err
		}
	}
	if err := agents_model.UpdateAgent(ctx, agent, "name", "provider", "base_url", "model", "allow_model_override", "reasoning_effort", "mcp_servers", "is_disabled"); err != nil {
		return err
	}
	audit.Record(ctx, audit_model.SystemAgentUpdate, nil, "agent", agent.Bot.Name)
	return nil
}

// DeleteAgent cancels the agent's pending work, deletes the users' keys for it and its bot, sessions stay as history
func DeleteAgent(ctx context.Context, agent *agents_model.Agent) error {
	sessions, err := db.Find[agents_model.Session](ctx, agents_model.FindSessionsOptions{AgentID: agent.ID})
	if err != nil {
		return err
	}
	for _, s := range sessions {
		if err := CancelSession(ctx, s); err != nil {
			return err
		}
	}
	if err := agents_model.DeleteAgent(ctx, agent.ID); err != nil {
		return err
	}
	if err := agent.LoadBot(ctx); err == nil {
		audit.Record(ctx, audit_model.SystemAgentRemove, nil, "agent", agent.Bot.Name)
	}
	bot, err := user_model.GetUserByID(ctx, agent.BotID)
	if user_model.IsErrUserNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	return user_service.DeleteUser(ctx, bot, false)
}

type KeyOptions struct {
	APIKey string // empty keeps the stored key
	Model  string
}

// SetUserKey stores a user's own API key for an agent
func SetUserKey(ctx context.Context, agent *agents_model.Agent, user *user_model.User, opts KeyOptions) (*agents_model.UserKey, error) {
	opts.APIKey, opts.Model = strings.TrimSpace(opts.APIKey), strings.TrimSpace(opts.Model)
	if len(opts.Model) > 255 {
		return nil, util.NewInvalidArgumentErrorf("the model name is too long")
	}
	if !agent.AllowModelOverride {
		opts.Model = ""
	}
	key, err := agents_model.GetUserKey(ctx, agent.ID, user.ID)
	if errors.Is(err, util.ErrNotExist) {
		if opts.APIKey == "" {
			return nil, util.NewInvalidArgumentErrorf("the API key is required")
		}
		key = &agents_model.UserKey{AgentID: agent.ID, UserID: user.ID, Model: opts.Model}
		if err := key.SetAPIKey(opts.APIKey); err != nil {
			return nil, err
		}
		if err := agents_model.InsertUserKey(ctx, key); err != nil {
			return nil, err
		}
		audit.Record(ctx, audit_model.UserAgentKeySet, user, "agent", agent.MentionName())
		return key, nil
	} else if err != nil {
		return nil, err
	}
	key.Model = opts.Model
	cols := []string{"model"}
	if opts.APIKey != "" {
		if err := key.SetAPIKey(opts.APIKey); err != nil {
			return nil, err
		}
		cols = append(cols, "encrypted_api_key")
		audit.Record(ctx, audit_model.UserAgentKeySet, user, "agent", agent.MentionName())
	}
	return key, agents_model.UpdateUserKey(ctx, key, cols...)
}

// TestUserKey checks that the agent's endpoint streams the Responses API with the user's key and model
func TestUserKey(ctx context.Context, agent *agents_model.Agent, key *agents_model.UserKey) (CheckResult, error) {
	apiKey, err := key.APIKey()
	if err != nil {
		return CheckResult{}, err
	}
	result := CheckConnection(ctx, agent.BaseURL, key.EffectiveModel(agent), apiKey)
	key.LastCheckUnix = timeutil.TimeStampNow()
	key.LastCheckError = ""
	if !result.OK {
		key.LastCheckError = result.Message
	}
	return result, agents_model.UpdateUserKey(ctx, key, "last_check_unix", "last_check_error")
}

// DeleteUserKey removes a user's key and cancels the runs they queued with it, their active runs fail without it
func DeleteUserKey(ctx context.Context, agent *agents_model.Agent, user *user_model.User) error {
	if err := agents_model.DeleteUserKey(ctx, agent.ID, user.ID); err != nil {
		return err
	}
	audit.Record(ctx, audit_model.UserAgentKeyRemove, user, "agent", agent.MentionName())
	return cancelRuns(ctx, agents_model.FindRunsOptions{AgentID: agent.ID, TriggerUserID: user.ID, Statuses: []agents_model.Status{agents_model.StatusQueued}}, true)
}

// SaveRepoSetting changes which agents may work in a repository
func SaveRepoSetting(ctx context.Context, repo *repo_model.Repository, rs *agents_model.RepoSetting) error {
	if err := agents_model.SaveRepoSetting(ctx, rs); err != nil {
		return err
	}
	allowed := "all"
	if len(rs.AllowedAgentIDs) > 0 {
		allowed = fmt.Sprint(rs.AllowedAgentIDs)
	}
	audit.Record(ctx, audit_model.RepositoryAgentSettings, repo, "enabled", rs.Enabled, "agents", allowed, "review_agent", rs.ReviewAgentID, "review_everyone", rs.ReviewEveryone)
	return nil
}
