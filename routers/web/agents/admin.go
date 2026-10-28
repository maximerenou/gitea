// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	agents_model "gitea.dev/models/agents"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/templates"
	"gitea.dev/modules/timeutil"
	"gitea.dev/modules/util"
	agents_service "gitea.dev/services/agents"
	"gitea.dev/services/context"
)

const (
	tplAdminAgents    templates.TplName = "admin/agents/list"
	tplAdminAgentEdit templates.TplName = "admin/agents/edit"
)

func adminLink() string {
	return setting.AppSubURL + "/-/admin/agents"
}

func prepareAdmin(ctx *context.Context) {
	ctx.Data["Title"] = ctx.Tr("agents.admin.title")
	ctx.Data["PageIsAdminAgents"] = true
	ctx.Data["Providers"] = []agents_model.Provider{agents_model.ProviderOpenAI, agents_model.ProviderCustom}
	ctx.Data["OpenAIBaseURL"] = agents_model.OpenAIBaseURL
	ctx.Data["ReasoningEfforts"] = agents_model.ReasoningEfforts
}

func AdminList(ctx *context.Context) {
	prepareAdmin(ctx)
	agents, err := agents_model.FindAgents(ctx, false)
	if err != nil {
		ctx.ServerError("FindAgents", err)
		return
	}
	ctx.Data["Agents"] = agents
	usage, err := agents_model.FindUsage(ctx, usageSince(), 0)
	if err != nil {
		ctx.ServerError("FindUsage", err)
		return
	}
	userIDs := make([]int64, 0, len(usage))
	for _, u := range usage {
		userIDs = append(userIDs, u.UserID)
	}
	users, err := user_model.GetUsersMapByIDs(ctx, userIDs)
	if err != nil {
		ctx.ServerError("GetUsersMapByIDs", err)
		return
	}
	agentNames := make(map[int64]string, len(agents))
	for _, agent := range agents {
		agentNames[agent.ID] = agent.MentionName()
	}
	ctx.Data["Usage"] = usage
	ctx.Data["UsageUsers"] = users
	ctx.Data["UsageAgents"] = agentNames
	ctx.HTML(http.StatusOK, tplAdminAgents)
}

func agentOptionsFromForm(ctx *context.Context) agents_service.AgentOptions {
	return agents_service.AgentOptions{
		BotName:            ctx.FormString("bot_name"),
		Name:               ctx.FormString("name"),
		Provider:           agents_model.Provider(ctx.FormString("provider")),
		BaseURL:            ctx.FormString("base_url"),
		Model:              ctx.FormString("model"),
		AllowModelOverride: ctx.FormBool("allow_model_override"),
		ReasoningEffort:    ctx.FormString("reasoning_effort"),
		MCPServers:         ctx.FormString("mcp_servers"),
		IsDisabled:         ctx.FormBool("is_disabled"),
	}
}

func AdminCreate(ctx *context.Context) {
	if _, err := agents_service.CreateAgent(ctx, agentOptionsFromForm(ctx)); err != nil {
		jsonError(ctx, err)
		return
	}
	ctx.Flash.Success(ctx.Tr("agents.admin.created"))
	ctx.JSONRedirect(adminLink())
}

func adminAgent(ctx *context.Context) *agents_model.Agent {
	agent, err := agents_model.GetAgentByID(ctx, ctx.PathParamInt64("id"))
	if err == nil {
		err = agent.LoadBot(ctx)
	}
	if err != nil {
		jsonError(ctx, err)
		return nil
	}
	return agent
}

func AdminEdit(ctx *context.Context) {
	agent, err := agents_model.GetAgentByID(ctx, ctx.PathParamInt64("id"))
	if err == nil {
		err = agent.LoadBot(ctx)
	}
	if errors.Is(err, util.ErrNotExist) {
		ctx.NotFound(nil)
		return
	} else if err != nil {
		ctx.ServerError("GetAgentByID", err)
		return
	}
	prepareAdmin(ctx)
	ctx.Data["Agent"] = agent
	ctx.HTML(http.StatusOK, tplAdminAgentEdit)
}

func AdminUpdate(ctx *context.Context) {
	agent := adminAgent(ctx)
	if agent == nil {
		return
	}
	if err := agents_service.UpdateAgent(ctx, agent, agentOptionsFromForm(ctx)); err != nil {
		jsonError(ctx, err)
		return
	}
	ctx.Flash.Success(ctx.Tr("agents.admin.updated"))
	ctx.JSONRedirect(fmt.Sprintf("%s/%d", adminLink(), agent.ID))
}

func AdminDelete(ctx *context.Context) {
	agent := adminAgent(ctx)
	if agent == nil {
		return
	}
	if err := agents_service.DeleteAgent(ctx, agent); err != nil {
		jsonError(ctx, err)
		return
	}
	ctx.Flash.Success(ctx.Tr("agents.admin.deleted"))
	ctx.JSONRedirect(adminLink())
}

const usageDays = 30

func usageSince() timeutil.TimeStamp {
	return timeutil.TimeStamp(time.Now().AddDate(0, 0, -usageDays).Unix())
}
