// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"net/http"

	agents_model "gitea.dev/models/agents"
	"gitea.dev/models/db"
	"gitea.dev/modules/templates"
	"gitea.dev/services/context"
)

const tplMine templates.TplName = "user/agents"

// Mine lists the sessions the doer took part in across repositories
func Mine(ctx *context.Context) {
	ctx.Data["Title"] = ctx.Tr("agents.mine.title")
	page := max(ctx.FormInt("page"), 1)
	sessions, total, err := db.FindAndCount[agents_model.Session](ctx, agents_model.FindSessionsOptions{
		ListOptions:   db.ListOptions{Page: page, PageSize: 20},
		ParticipantID: ctx.Doer.ID,
		ReadableBy:    ctx.Doer, // a repository the doer lost access to keeps its sessions hidden
	})
	if err != nil {
		ctx.ServerError("FindSessions", err)
		return
	}
	for _, s := range sessions {
		if err := s.LoadAttributes(ctx); err != nil {
			ctx.ServerError("LoadAttributes", err)
			return
		}
	}
	ctx.Data["Sessions"] = sessions
	ctx.Data["SettingsLink"] = settingsLink()
	ctx.Data["Page"] = context.NewPagerBuilder(ctx).TotalCount(total).PerPageLimit(20).CurPage(page).Build()
	ctx.HTML(http.StatusOK, tplMine)
}
