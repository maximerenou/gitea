// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package agents serves the Codex agent pages: the site agents, the users' keys and the repository Agents tab
package agents

import (
	"errors"
	"net/http"

	agents_model "gitea.dev/models/agents"
	"gitea.dev/models/db"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/templates"
	"gitea.dev/modules/util"
	agents_service "gitea.dev/services/agents"
	"gitea.dev/services/context"
)

const tplSettingsAgents templates.TplName = "user/settings/agents"

func settingsLink() string {
	return setting.AppSubURL + "/user/settings/agents"
}

// MustEnable hides every agent page when the feature is disabled
func MustEnable(ctx *context.Context) {
	if !agents_service.IsEnabled() {
		ctx.NotFound(nil)
	}
}

func jsonError(ctx *context.Context, err error) {
	switch {
	case user_model.IsErrUserAlreadyExist(err):
		ctx.JSONError(ctx.Tr("agents.admin.name_taken"))
	case db.IsErrNameReserved(err), db.IsErrNamePatternNotAllowed(err), db.IsErrNameCharsNotAllowed(err):
		ctx.JSONError(ctx.Tr("agents.admin.name_invalid"))
	case errors.Is(err, util.ErrInvalidArgument), errors.Is(err, util.ErrPermissionDenied):
		ctx.JSONError(err.Error())
	case errors.Is(err, util.ErrNotExist):
		ctx.JSONErrorNotFound()
	default:
		ctx.ServerError("agents", err)
	}
}

// AgentKeyView is an agent as a user sees it in their settings
type AgentKeyView struct {
	Agent *agents_model.Agent
	Key   *agents_model.UserKey
	Usage *agents_model.Usage
}

// Settings lists the site agents and the doer's keys for them
func Settings(ctx *context.Context) {
	ctx.Data["Title"] = ctx.Tr("agents.settings.title")
	ctx.Data["PageIsSettingsAgents"] = true
	agents, err := agents_model.FindAgents(ctx, false)
	if err != nil {
		ctx.ServerError("FindAgents", err)
		return
	}
	keys, err := agents_model.FindUserKeys(ctx, ctx.Doer.ID)
	if err != nil {
		ctx.ServerError("FindUserKeys", err)
		return
	}
	usage, err := agents_model.FindUsage(ctx, usageSince(), ctx.Doer.ID)
	if err != nil {
		ctx.ServerError("FindUsage", err)
		return
	}
	views := make([]*AgentKeyView, 0, len(agents))
	for _, agent := range agents {
		if agent.IsDisabled && keys[agent.ID] == nil { // a disabled agent is only listed to remove the key kept for it
			continue
		}
		view := &AgentKeyView{Agent: agent, Key: keys[agent.ID]}
		for _, u := range usage {
			if u.AgentID == agent.ID {
				view.Usage = u
			}
		}
		views = append(views, view)
	}
	ctx.Data["Agents"] = views
	ctx.HTML(http.StatusOK, tplSettingsAgents)
}

func enabledAgent(ctx *context.Context) *agents_model.Agent {
	agent, err := agents_model.GetAgentByID(ctx, ctx.PathParamInt64("id"))
	if err == nil && agent.IsDisabled {
		err = util.NewNotExistErrorf("agent %d is disabled", agent.ID)
	}
	if err != nil {
		jsonError(ctx, err)
		return nil
	}
	return agent
}

func SettingsSaveKey(ctx *context.Context) {
	agent := enabledAgent(ctx)
	if agent == nil {
		return
	}
	if _, err := agents_service.SetUserKey(ctx, agent, ctx.Doer, agents_service.KeyOptions{
		APIKey: ctx.FormString("api_key"),
		Model:  ctx.FormString("model"),
	}); err != nil {
		jsonError(ctx, err)
		return
	}
	ctx.Flash.Success(ctx.Tr("agents.settings.key_saved"))
	ctx.JSONRedirect(settingsLink())
}

func SettingsTestKey(ctx *context.Context) {
	agent := enabledAgent(ctx)
	if agent == nil {
		return
	}
	key, err := agents_model.GetUserKey(ctx, agent.ID, ctx.Doer.ID)
	if err != nil {
		jsonError(ctx, err)
		return
	}
	result, err := agents_service.TestUserKey(ctx, agent, key)
	if err != nil {
		jsonError(ctx, err)
		return
	}
	if result.OK {
		ctx.Flash.Success(ctx.Tr("agents.settings.test_success", result.Message))
	} else {
		ctx.Flash.Error(ctx.Tr("agents.settings.test_failure", result.Message))
	}
	ctx.JSONRedirect(settingsLink())
}

func SettingsDeleteKey(ctx *context.Context) {
	agent, err := agents_model.GetAgentByID(ctx, ctx.PathParamInt64("id"))
	if err == nil {
		err = agent.LoadBot(ctx)
	}
	if err == nil {
		err = agents_service.DeleteUserKey(ctx, agent, ctx.Doer)
	}
	if err != nil {
		jsonError(ctx, err)
		return
	}
	ctx.Flash.Success(ctx.Tr("agents.settings.key_deleted"))
	ctx.JSONRedirect(settingsLink())
}
