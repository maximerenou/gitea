// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	agents_model "gitea.dev/models/agents"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/agents"
	"gitea.dev/modules/log"
	"gitea.dev/modules/optional"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/util"
	repo_service "gitea.dev/services/repository"
	files_service "gitea.dev/services/repository/files"
	user_service "gitea.dev/services/user"
)

// ConfigRepoName is the private repository, owned by the bot, that holds the agent's versioned configuration.
const ConfigRepoName = ".agent"

type CreateAgentOptions struct {
	OwnerID      int64 // 0 for a global agent
	Name         string
	ConnectionID int64
	Model        string
	Description  string
	Language     string
	Action       agents.Action
	Events       []agents.Event
	Settings     map[string]any
	AccessMode   agents_model.AccessMode
	AccessList   *agents_model.AccessEntries
}

// CreateAgent creates the bot account, the agent record and the bot's private ".agent" repository.
// Nothing is left behind if a step fails.
func CreateAgent(ctx context.Context, opts CreateAgentOptions) (_ *agents_model.Agent, err error) {
	opts.Name = strings.TrimSpace(opts.Name)
	agent := &agents_model.Agent{
		OwnerID:        opts.OwnerID,
		Name:           opts.Name,
		ConnectionID:   opts.ConnectionID,
		Model:          opts.Model,
		Description:    opts.Description,
		Language:       opts.Language,
		Action:         opts.Action,
		Events:         opts.Events,
		ActionSettings: opts.Settings,
		AccessMode:     opts.AccessMode,
		AccessList:     opts.AccessList,
	}
	if agent.AccessMode == "" {
		agent.AccessMode = agents_model.AccessWriters
	}
	if err := agent.Validate(); err != nil {
		return nil, err
	}
	conn, err := agents_model.GetConnectionByID(ctx, opts.ConnectionID)
	if err != nil {
		return nil, fmt.Errorf("CreateAgent: connection %d: %w", opts.ConnectionID, err)
	}
	if !conn.IsGlobal() && conn.OwnerID != opts.OwnerID {
		return nil, util.NewInvalidArgumentErrorf("connection is not available to this owner")
	}
	if err := user_model.IsUsableUsername(opts.Name); err != nil {
		return nil, err
	}

	bot := &user_model.User{
		Name:     opts.Name,
		FullName: opts.Name,
		Email:    fmt.Sprintf("%s@%s", strings.ToLower(opts.Name), setting.Service.NoReplyAddress),
		Type:     user_model.UserTypeBot,
	}
	if err := user_model.CreateUser(ctx, bot, &user_model.Meta{}, &user_model.CreateUserOverwriteOptions{
		IsActive:                optional.Some(true),
		KeepEmailPrivate:        optional.Some(true),
		AllowCreateOrganization: optional.Some(false),
	}); err != nil {
		return nil, fmt.Errorf("CreateAgent: create bot: %w", err)
	}
	defer func() {
		if err != nil {
			if delErr := user_service.DeleteUser(context.WithoutCancel(ctx), bot, true); delErr != nil {
				log.Error("CreateAgent: cleanup of bot %q failed: %v", bot.Name, delErr)
			}
		}
	}()

	agent.BotUserID = bot.ID
	if err = agents_model.CreateAgent(ctx, agent); err != nil {
		return nil, fmt.Errorf("CreateAgent: %w", err)
	}
	if err = initConfigRepo(ctx, bot, agent.Action); err != nil {
		return nil, fmt.Errorf("CreateAgent: %w", err)
	}
	return agent, nil
}

func initConfigRepo(ctx context.Context, bot *user_model.User, action agents.Action) error {
	repo, err := repo_service.CreateRepository(ctx, bot, bot, repo_service.CreateRepoOptions{
		Name:          ConfigRepoName,
		Description:   "Configuration of the " + bot.Name + " agent",
		IsPrivate:     true,
		DefaultBranch: setting.Repository.DefaultBranch,
	})
	if err != nil {
		return fmt.Errorf("initConfigRepo: create repository: %w", err)
	}
	tpl, err := repoTemplateFiles(action)
	if err != nil {
		return err
	}
	treePaths := slices.Sorted(maps.Keys(tpl))
	changes := make([]*files_service.ChangeRepoFile, 0, len(treePaths))
	for _, p := range treePaths {
		changes = append(changes, &files_service.ChangeRepoFile{
			Operation:     "create",
			TreePath:      p,
			ContentReader: bytes.NewReader([]byte(tpl[p])),
		})
	}
	_, err = files_service.ChangeRepoFiles(ctx, repo, bot, &files_service.ChangeRepoFilesOptions{
		Message: "Initialize agent configuration",
		Files:   changes,
	})
	if err != nil {
		return fmt.Errorf("initConfigRepo: commit files: %w", err)
	}
	return nil
}

// DeleteAgent removes the bot with everything it owns (including ".agent"), then the agent record.
func DeleteAgent(ctx context.Context, agent *agents_model.Agent) error {
	bot, err := user_model.GetUserByID(ctx, agent.BotUserID)
	if err != nil && !user_model.IsErrUserNotExist(err) {
		return fmt.Errorf("DeleteAgent: %w", err)
	}
	if bot != nil && bot.Type == user_model.UserTypeBot {
		if err := user_service.DeleteUser(ctx, bot, true); err != nil {
			return fmt.Errorf("DeleteAgent: delete bot: %w", err)
		}
	}
	return agents_model.DeleteAgent(ctx, agent.ID)
}
