// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"net/http"
	"slices"
	"strconv"

	agents_model "gitea.dev/models/agents"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/templates"
	agents_service "gitea.dev/services/agents"
	"gitea.dev/services/context"
)

const tplRepoSettingAgents templates.TplName = "repo/settings/agents"

// RepoSetting lets the repository's administrators open it to agents and choose which ones
func RepoSetting(ctx *context.Context) {
	ctx.Data["Title"] = ctx.Tr("agents.title")
	ctx.Data["PageIsSettingsAgents"] = true
	rs, err := agents_model.GetRepoSetting(ctx, ctx.Repo.Repository.ID)
	if err != nil {
		ctx.ServerError("GetRepoSetting", err)
		return
	}
	agents, err := agents_model.FindAgents(ctx, true)
	if err != nil {
		ctx.ServerError("FindAgents", err)
		return
	}
	ctx.Data["RepoSetting"] = rs
	ctx.Data["Agents"] = agents
	ctx.Data["AllowedAgentIDs"] = rs.AllowedAgentIDs
	if rs.ReviewAgentID > 0 {
		if reviewer, err := user_model.GetUserByID(ctx, rs.ReviewUserID); err == nil {
			ctx.Data["Reviewer"] = reviewer
		}
		problem, err := agents_service.AutoReviewProblem(ctx, ctx.Repo.Repository, rs)
		if err != nil {
			ctx.ServerError("AutoReviewProblem", err)
			return
		}
		if problem != "" {
			ctx.Data["ReviewProblem"] = ctx.Tr("agents.repo_settings.review_problem." + problem)
		}
	}
	ctx.HTML(http.StatusOK, tplRepoSettingAgents)
}

func RepoSettingPost(ctx *context.Context) {
	rs, err := agents_model.GetRepoSetting(ctx, ctx.Repo.Repository.ID)
	if err != nil {
		ctx.ServerError("GetRepoSetting", err)
		return
	}
	agents, err := agents_model.FindAgents(ctx, true)
	if err != nil {
		ctx.ServerError("FindAgents", err)
		return
	}
	rs.Enabled = ctx.FormBool("enabled")
	rs.AllowedAgentIDs = nil
	if ctx.FormString("agents") == "selected" {
		selected := ctx.FormStrings("agent_ids")
		rs.AllowedAgentIDs = []int64{}
		for _, agent := range agents {
			if slices.Contains(selected, strconv.FormatInt(agent.ID, 10)) {
				rs.AllowedAgentIDs = append(rs.AllowedAgentIDs, agent.ID)
			}
		}
		if len(rs.AllowedAgentIDs) == 0 { // an empty selection would allow every agent
			ctx.JSONError(ctx.Tr("agents.repo_settings.select_agents"))
			return
		}
	}
	reviewAgentID, reviewEveryone := ctx.FormInt64("review_agent_id"), ctx.FormBool("review_everyone")
	// whoever widens what the reviews spend takes them over with their own key
	if reviewAgentID != rs.ReviewAgentID || (reviewAgentID > 0 && (ctx.FormBool("review_with_my_key") || reviewEveryone != rs.ReviewEveryone)) {
		rs.ReviewAgentID, rs.ReviewUserID = 0, 0
		if reviewAgentID > 0 {
			i := slices.IndexFunc(agents, func(a *agents_model.Agent) bool { return a.ID == reviewAgentID })
			if i < 0 {
				ctx.JSONError(ctx.Tr("agents.repo_settings.review_unavailable"))
				return
			}
			agent := agents[i]
			if _, err := agents_model.GetUserKey(ctx, agent.ID, ctx.Doer.ID); err != nil {
				ctx.JSONError(ctx.Tr("agents.session.need_key", settingsLink(), agent.MentionName()))
				return
			}
			rs.ReviewAgentID, rs.ReviewUserID = agent.ID, ctx.Doer.ID
		}
	}
	rs.ReviewEveryone = reviewEveryone
	if err := agents_service.SaveRepoSetting(ctx, ctx.Repo.Repository, rs); err != nil {
		ctx.ServerError("SaveRepoSetting", err)
		return
	}
	ctx.Flash.Success(ctx.Tr("agents.repo_settings.saved"))
	ctx.JSONRedirect(ctx.Repo.RepoLink + "/settings/agents")
}
