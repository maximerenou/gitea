// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"gitea.dev/models/unit"
	"gitea.dev/modules/web"
	"gitea.dev/modules/web/routing"
	"gitea.dev/services/context"
)

// RegisterUserSettingsRoutes is called inside the "/user/settings" group
func RegisterUserSettingsRoutes(m *web.Router) {
	m.Group("/agents", func() {
		m.Get("", Settings)
		m.Post("/{id}", SettingsSaveKey)
		m.Post("/{id}/test", SettingsTestKey)
		m.Post("/{id}/delete", SettingsDeleteKey)
	}, MustEnable)
}

// RegisterAdminRoutes is called inside the "/-/admin" group
func RegisterAdminRoutes(m *web.Router) {
	m.Group("/agents", func() {
		m.Get("", AdminList)
		m.Post("", AdminCreate)
		m.Get("/{id}", AdminEdit)
		m.Post("/{id}", AdminUpdate)
		m.Post("/{id}/delete", AdminDelete)
	}, MustEnable)
}

// RegisterRoutes registers the repository Agents tab and the user's sessions page
func RegisterRoutes(m *web.Router, reqSignIn func(*context.Context)) {
	m.Get("/user/agents", reqSignIn, MustEnable, Mine)

	m.Post("/{username}/{reponame}/pulls/{index}/agent-merge", reqSignIn, context.RepoAssignment, context.RequireUnitReader(unit.TypePullRequests), MustEnable, AgentMerge)
	m.Post("/{username}/{reponame}/pulls/{index}/agent-review", reqSignIn, context.RepoAssignment, context.RequireUnitReader(unit.TypePullRequests), MustEnable, AgentReview)

	m.Combo("/{username}/{reponame}/settings/agents", reqSignIn, context.RepoAssignment, context.RequireRepoAdmin(), MustEnable).
		Get(RepoSetting).Post(RepoSettingPost)

	m.Group("/{username}/{reponame}/agents", func() {
		m.Get("", List)
		m.Get("/live", routing.MarkLongPolling(), LiveList)
		m.Post("/new", New)
		m.Group("/{id}", func() {
			m.Get("", View)
			m.Get("/events", Events)
			m.Get("/live", routing.MarkLongPolling(), Live)
			m.Post("/instruct", Instruct)
			m.Post("/cancel", Cancel)
			m.Post("/resume", Resume)
			m.Post("/runs/{run}/retry", Retry)
		}, context.RequireUnitWriter(unit.TypeCode))
	}, reqSignIn, context.RepoAssignment, context.RequireUnitReader(unit.TypeCode), MustEnable)
}
