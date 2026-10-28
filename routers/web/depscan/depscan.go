// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package depscan serves the Security tab of repositories: dependency vulnerability alerts and the dependency inventory
package depscan

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	agents_model "gitea.dev/models/agents"
	"gitea.dev/models/db"
	depscan_model "gitea.dev/models/depscan"
	issues_model "gitea.dev/models/issues"
	access_model "gitea.dev/models/perm/access"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unit"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/templates"
	"gitea.dev/modules/util"
	"gitea.dev/modules/web"
	"gitea.dev/modules/web/routing"
	agents_router "gitea.dev/routers/web/agents"
	shared_user "gitea.dev/routers/web/shared/user"
	agents_service "gitea.dev/services/agents"
	"gitea.dev/services/context"
	depscan_service "gitea.dev/services/depscan"

	"xorm.io/builder"
)

const (
	tplAlerts       templates.TplName = "repo/security/alerts"
	tplAlert        templates.TplName = "repo/security/alert"
	tplDependencies templates.TplName = "repo/security/dependencies"
	tplSetting      templates.TplName = "repo/settings/dependency_scanning"
	tplPullReview   templates.TplName = "repo/security/pull_review"
	tplOrgAlerts    templates.TplName = "org/security"
)

func reqCodeWriter(ctx *context.Context) {
	if !ctx.Repo.Permission.CanWrite(unit.TypeCode) {
		ctx.HTTPError(http.StatusForbidden)
	}
}

// RegisterRoutes registers the Security tab and the repository setting
func RegisterRoutes(m *web.Router, reqSignIn func(*context.Context)) {
	m.Combo("/{username}/{reponame}/settings/dependency-scanning", reqSignIn, context.RepoAssignment, context.RequireRepoAdmin(), mustEnable).
		Get(Setting).Post(SettingPost)
	m.Post("/{username}/{reponame}/settings/dependency-scanning/update", reqSignIn, context.RepoAssignment, context.RequireRepoAdmin(), mustEnable, UpdateNow)
	m.Post("/{username}/{reponame}/settings/dependency-scanning/refused/remove", reqSignIn, context.RepoAssignment, context.RequireRepoAdmin(), mustEnable, RemoveRefused)

	m.Group("/{username}/{reponame}/security", func() {
		m.Get("", Alerts)
		m.Get("/dependencies", Dependencies)
		m.Get("/live", routing.MarkLongPolling(), Live)
		m.Post("/scan", Scan)
		m.Group("/alerts/{id}", func() {
			m.Get("", Alert)
			m.Post("/dismiss", Dismiss)
			m.Post("/reopen", Reopen)
			m.Post("/fix", Fix)
		})
	}, reqSignIn, context.RepoAssignment, reqCodeWriter, mustEnable)

	m.Get("/{username}/-/security", reqSignIn, context.UserAssignmentWeb(), context.OrgAssignment(context.OrgAssignmentOptions{RequireMember: true}), mustEnable, OrgAlerts)

	m.Get("/{username}/{reponame}/security/pulls/{index}", reqSignIn, context.RepoAssignment, reqCodeWriter, mustEnable, PullReview)
}

// PullReview shows the vulnerable dependencies a pull request adds
func PullReview(ctx *context.Context) {
	pr, err := issues_model.GetPullRequestByIndex(ctx, ctx.Repo.Repository.ID, ctx.PathParamInt64("index"))
	if err != nil {
		if errors.Is(err, util.ErrNotExist) || issues_model.IsErrPullRequestNotExist(err) {
			ctx.NotFound(nil)
		} else {
			ctx.ServerError("GetPullRequestByIndex", err)
		}
		return
	}
	review, err := depscan_model.GetPullReview(ctx, pr.ID)
	if err != nil {
		if errors.Is(err, util.ErrNotExist) {
			ctx.NotFound(nil)
		} else {
			ctx.ServerError("GetPullReview", err)
		}
		return
	}
	ctx.Data["Title"] = ctx.Tr("depscan.review.title")
	ctx.Data["PageIsPullList"] = true
	ctx.Data["Pull"] = pr
	ctx.Data["Review"] = review
	ctx.Data["CanSeeReviewError"] = ctx.Repo.Permission.CanWrite(unit.TypeCode) // the error can name internal hosts
	ctx.HTML(http.StatusOK, tplPullReview)
}

func mustEnable(ctx *context.Context) {
	if !depscan_service.IsEnabled() {
		ctx.NotFound(nil)
	}
}

func securityLink(ctx *context.Context) string {
	return ctx.Repo.RepoLink + "/security"
}

func prepare(ctx *context.Context) *depscan_model.RepoSetting {
	ctx.Data["Title"] = ctx.Tr("depscan.title")
	ctx.Data["PageIsSecurity"] = true
	rs, err := depscan_model.GetRepoSetting(ctx, ctx.Repo.Repository.ID)
	if err != nil {
		ctx.ServerError("GetRepoSetting", err)
		return nil
	}
	ctx.Data["ScanSetting"] = rs
	ctx.Data["CanAdmin"] = ctx.Repo.Permission.IsAdmin()
	counts, err := depscan_model.CountOpenAlerts(ctx, builder.Eq{"repo_id": ctx.Repo.Repository.ID})
	if err != nil {
		ctx.ServerError("CountOpenAlerts", err)
		return nil
	}
	ctx.Data["OpenCounts"] = counts
	ctx.Data["Severities"] = depscan_model.Severities
	return rs
}

func Alerts(ctx *context.Context) {
	if prepare(ctx) == nil {
		return
	}
	state := depscan_model.AlertState(ctx.FormString("state", string(depscan_model.AlertOpen)))
	if !state.IsValid() {
		state = depscan_model.AlertOpen
	}
	severity := depscan_model.Severity(ctx.FormString("severity"))
	page := max(ctx.FormInt("page"), 1)
	alerts, total, err := db.FindAndCount[depscan_model.Alert](ctx, depscan_model.FindAlertsOptions{
		ListOptions: db.ListOptions{Page: page, PageSize: 30},
		RepoID:      ctx.Repo.Repository.ID,
		State:       state,
		Severity:    severity,
	})
	if err != nil {
		ctx.ServerError("FindAlerts", err)
		return
	}
	ctx.Data["Alerts"] = alerts
	ctx.Data["State"] = state
	ctx.Data["Severity"] = severity
	ctx.Data["Page"] = context.NewPagerBuilder(ctx).TotalCount(total).PerPageLimit(30).CurPage(page).Build()
	ctx.HTML(http.StatusOK, tplAlerts)
}

func Dependencies(ctx *context.Context) {
	if prepare(ctx) == nil {
		return
	}
	page := max(ctx.FormInt("page"), 1)
	keyword := ctx.FormTrim("q")
	deps, total, err := db.FindAndCount[depscan_model.Dependency](ctx, depscan_model.FindDependenciesOptions{
		ListOptions: db.ListOptions{Page: page, PageSize: 50},
		RepoID:      ctx.Repo.Repository.ID,
		Keyword:     keyword,
	})
	if err != nil {
		ctx.ServerError("FindDependencies", err)
		return
	}
	ctx.Data["PageIsSecurityDependencies"] = true
	ctx.Data["Dependencies"] = deps
	ctx.Data["Keyword"] = keyword
	ctx.Data["Page"] = context.NewPagerBuilder(ctx).TotalCount(total).PerPageLimit(50).CurPage(page).Build()
	ctx.HTML(http.StatusOK, tplDependencies)
}

func getAlert(ctx *context.Context) *depscan_model.Alert {
	a, err := depscan_model.GetRepoAlert(ctx, ctx.Repo.Repository.ID, ctx.PathParamInt64("id"))
	if err != nil {
		if errors.Is(err, util.ErrNotExist) {
			ctx.NotFound(nil)
		} else {
			ctx.ServerError("GetRepoAlert", err)
		}
		return nil
	}
	return a
}

func Alert(ctx *context.Context) {
	if prepare(ctx) == nil {
		return
	}
	a := getAlert(ctx)
	if a == nil {
		return
	}
	ctx.Data["Title"] = a.Title()
	ctx.Data["Alert"] = a
	if a.DismissedBy > 0 {
		_, dismisser, err := user_model.GetPossibleUserByID(ctx, a.DismissedBy)
		if err != nil {
			ctx.ServerError("GetPossibleUserByID", err)
			return
		}
		ctx.Data["DismissedBy"] = dismisser
	}
	if a.State == depscan_model.AlertOpen && agents_service.IsRepoUsable(ctx, ctx.Repo.Repository) {
		agents, err := usableAgents(ctx)
		if err != nil {
			ctx.ServerError("usableAgents", err)
			return
		}
		ctx.Data["Agents"] = agents
	}
	ctx.HTML(http.StatusOK, tplAlert)
}

// usableAgents returns the agents the doer has a key for and the repository allows
func usableAgents(ctx *context.Context) ([]*agents_model.Agent, error) {
	agents, err := agents_model.FindAgents(ctx, true)
	if err != nil {
		return nil, err
	}
	keys, err := agents_model.FindUserKeys(ctx, ctx.Doer.ID)
	if err != nil {
		return nil, err
	}
	usable := make([]*agents_model.Agent, 0, len(agents))
	for _, agent := range agents {
		if keys[agent.ID] != nil && agent.Bot != nil && agents_service.IsAgentAllowed(ctx, ctx.Repo.Repository, agent) {
			usable = append(usable, agent)
		}
	}
	return usable, nil
}

func alertLink(ctx *context.Context, a *depscan_model.Alert) string {
	return fmt.Sprintf("%s/alerts/%d", securityLink(ctx), a.ID)
}

func jsonServiceError(ctx *context.Context, err error) {
	if errors.Is(err, util.ErrInvalidArgument) || errors.Is(err, util.ErrPermissionDenied) || errors.Is(err, util.ErrNotExist) {
		ctx.JSONError(err.Error())
		return
	}
	ctx.ServerError("depscan", err)
}

func Dismiss(ctx *context.Context) {
	a := getAlert(ctx)
	if a == nil {
		return
	}
	if err := depscan_service.DismissAlert(ctx, ctx.Doer, ctx.Repo.Repository, a, ctx.FormString("reason")); err != nil {
		jsonServiceError(ctx, err)
		return
	}
	ctx.JSONRedirect(alertLink(ctx, a))
}

func Reopen(ctx *context.Context) {
	a := getAlert(ctx)
	if a == nil {
		return
	}
	if err := depscan_service.ReopenAlert(ctx, ctx.Repo.Repository, a); err != nil {
		jsonServiceError(ctx, err)
		return
	}
	ctx.JSONRedirect(alertLink(ctx, a))
}

func Fix(ctx *context.Context) {
	a := getAlert(ctx)
	if a == nil {
		return
	}
	agent, err := agents_model.GetAgentByID(ctx, ctx.FormInt64("agent_id"))
	if err == nil {
		err = agent.LoadBot(ctx)
	}
	if err != nil {
		jsonServiceError(ctx, err)
		return
	}
	s, err := depscan_service.FixWithAgent(ctx, ctx.Doer, ctx.Repo.Repository, a, agent)
	if err != nil {
		jsonServiceError(ctx, err)
		return
	}
	ctx.JSONRedirect(fmt.Sprintf("%s/agents/%d", ctx.Repo.RepoLink, s.ID))
}

func Scan(ctx *context.Context) {
	if !depscan_service.IsRepoScanned(ctx, ctx.Repo.Repository) {
		ctx.JSONError(ctx.Tr("depscan.disabled"))
		return
	}
	depscan_service.QueueScan(ctx.Repo.Repository)
	ctx.Flash.Success(ctx.Tr("depscan.scan_queued"))
	ctx.JSONRedirect(securityLink(ctx))
}

func Setting(ctx *context.Context) {
	ctx.Data["Title"] = ctx.Tr("depscan.title")
	ctx.Data["PageIsSettingsDependencyScanning"] = true
	rs, err := depscan_model.GetRepoSetting(ctx, ctx.Repo.Repository.ID)
	if err != nil {
		ctx.ServerError("GetRepoSetting", err)
		return
	}
	ctx.Data["ScanSetting"] = rs
	ctx.Data["Thresholds"] = append(slices.Clone(depscan_model.Severities), depscan_model.SeverityNone)
	ctx.Data["NotifySeverity"] = util.IfZero(rs.AlertNotifySeverity, depscan_model.SeverityCritical)
	ctx.Data["ReviewFailSeverity"] = util.IfZero(rs.ReviewFailSeverity, depscan_model.SeverityUnknown)
	if agents_service.IsEnabled() {
		agents, err := agents_model.FindAgents(ctx, true)
		if err != nil {
			ctx.ServerError("FindAgents", err)
			return
		}
		// a disabled agent stays listed while it is set, so saving the form does not turn the updates off
		if rs.UpdatesAgentID > 0 && !slices.ContainsFunc(agents, func(a *agents_model.Agent) bool { return a.ID == rs.UpdatesAgentID }) {
			if agent, err := agents_model.GetAgentByID(ctx, rs.UpdatesAgentID); err == nil && agent.LoadBot(ctx) == nil {
				agents = append(agents, agent)
			}
		}
		ctx.Data["UpdateAgents"] = agents
		ctx.Data["UpdatesIntervals"] = depscan_model.UpdatesIntervals
		ctx.Data["UpdatesScopes"] = depscan_model.UpdatesScopes
		ctx.Data["UpdatesScope"] = util.Iif(rs.UpdatesSecurityOnly(), depscan_model.UpdatesScopeSecurity, depscan_model.UpdatesScopeAll)
		if rs.UpdatesAgentID > 0 {
			if updater, err := user_model.GetUserByID(ctx, rs.UpdatesUserID); err == nil {
				ctx.Data["Updater"] = updater
			}
		}
	}
	ctx.HTML(http.StatusOK, tplSetting)
}

// formLines returns the non-empty lines of a textarea
func formLines(ctx *context.Context, name string) []string {
	var values []string
	for line := range strings.SplitSeq(ctx.FormString(name), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			values = append(values, line)
		}
	}
	return values
}

func SettingPost(ctx *context.Context) {
	if agents_service.IsEnabled() {
		opts := depscan_service.UpdateOptions{
			Interval: depscan_model.UpdatesInterval(ctx.FormString("updates_interval")),
			Major:    ctx.FormBool("updates_major"),
			Scope:    depscan_model.UpdatesScope(ctx.FormString("updates_scope")),
			OnAlert:  ctx.FormBool("updates_on_alert"),
			TakeOver: ctx.FormBool("updates_with_my_key"),
		}
		opts.Ignore = formLines(ctx, "updates_ignore")
		if agentID := ctx.FormInt64("updates_agent_id"); agentID > 0 {
			agent, err := agents_model.GetAgentByID(ctx, agentID)
			if err == nil {
				err = agent.LoadBot(ctx)
			}
			if err != nil {
				jsonServiceError(ctx, err)
				return
			}
			opts.Agent = agent
		}
		err := depscan_service.SaveUpdateSettings(ctx, ctx.Doer, ctx.Repo.Repository, opts)
		if errors.Is(err, agents_service.ErrNoUserKey) {
			ctx.JSONError(ctx.Tr("agents.session.need_key", setting.AppSubURL+"/user/settings/agents", opts.Agent.MentionName()))
			return
		} else if err != nil {
			jsonServiceError(ctx, err)
			return
		}
	}
	err := depscan_service.SaveRepoSetting(ctx, ctx.Repo.Repository, depscan_service.ScanOptions{
		Enabled:            ctx.FormBool("enabled"),
		NotifySeverity:     depscan_model.Severity(ctx.FormString("notify_severity")),
		ReviewFailSeverity: depscan_model.Severity(ctx.FormString("review_fail_severity")),
		ReviewAllowList:    formLines(ctx, "review_allow_list"),
	})
	if err != nil {
		jsonServiceError(ctx, err)
		return
	}
	ctx.Flash.Success(ctx.Tr("depscan.settings.saved"))
	ctx.JSONRedirect(ctx.Repo.RepoLink + "/settings/dependency-scanning")
}

// UpdateNow updates the dependencies at once with the repository's agent, the doer takes the updates over with their key
func UpdateNow(ctx *context.Context) {
	started, err := depscan_service.UpdateNow(ctx, ctx.Doer, ctx.Repo.Repository)
	if errors.Is(err, agents_service.ErrNoUserKey) {
		var mention string
		if rs, err := depscan_model.GetRepoSetting(ctx, ctx.Repo.Repository.ID); err == nil {
			if agent, err := agents_model.GetAgentByID(ctx, rs.UpdatesAgentID); err == nil && agent.LoadBot(ctx) == nil {
				mention = agent.MentionName()
			}
		}
		ctx.JSONError(ctx.Tr("agents.session.need_key", setting.AppSubURL+"/user/settings/agents", mention))
		return
	} else if err != nil {
		jsonServiceError(ctx, err)
		return
	}
	if started == 0 {
		ctx.Flash.Info(ctx.Tr("depscan.updates.busy"))
	} else {
		ctx.Flash.Success(ctx.Tr("depscan.updates.started"))
	}
	ctx.JSONRedirect(ctx.Repo.RepoLink + "/agents")
}

// RemoveRefused lets the updates propose a version refused by closing its pull request again
func RemoveRefused(ctx *context.Context) {
	err := depscan_service.RemoveRefusedVersion(ctx, ctx.Repo.Repository, depscan_model.RefusedVersion{
		Ecosystem: ctx.FormString("ecosystem"), Package: ctx.FormString("package"), Version: ctx.FormString("version"),
	})
	if err != nil {
		jsonServiceError(ctx, err)
		return
	}
	ctx.Flash.Success(ctx.Tr("depscan.updates.refused_removed"))
	ctx.JSONRedirect(ctx.Repo.RepoLink + "/settings/dependency-scanning")
}

// OrgAlerts lists the open alerts of the organization's repositories whose code the doer can write, like the Security tab of each
func OrgAlerts(ctx *context.Context) {
	if _, err := shared_user.RenderUserOrgHeader(ctx); err != nil {
		ctx.ServerError("RenderUserOrgHeader", err)
		return
	}
	repoCond, err := orgWritableReposCond(ctx)
	if err != nil {
		ctx.ServerError("orgWritableReposCond", err)
		return
	}
	repoCond = repoCond.And(depscan_model.ScannedReposCond()) // the alerts of repositories no longer scanned are stale

	ctx.Data["Title"] = ctx.Tr("depscan.title")
	ctx.Data["PageIsOrgSecurity"] = true
	ctx.Data["Severities"] = depscan_model.Severities
	severity := depscan_model.Severity(ctx.FormString("severity"))
	ctx.Data["Severity"] = severity
	page := max(ctx.FormInt("page"), 1)
	if ctx.Data["OpenCounts"], err = depscan_model.CountOpenAlerts(ctx, repoCond); err != nil {
		ctx.ServerError("CountOpenAlerts", err)
		return
	}
	alerts, total, err := db.FindAndCount[depscan_model.Alert](ctx, depscan_model.FindAlertsOptions{
		ListOptions: db.ListOptions{Page: page, PageSize: 30},
		RepoCond:    repoCond,
		State:       depscan_model.AlertOpen,
		Severity:    severity,
	})
	if err != nil {
		ctx.ServerError("FindAlerts", err)
		return
	}
	repoIDs := make([]int64, 0, len(alerts))
	for _, a := range alerts {
		repoIDs = append(repoIDs, a.RepoID)
	}
	if ctx.Data["Repos"], err = repo_model.GetRepositoriesMapByIDs(ctx, repoIDs); err != nil {
		ctx.ServerError("GetRepositoriesMapByIDs", err)
		return
	}
	ctx.Data["Alerts"] = alerts
	ctx.Data["Page"] = context.NewPagerBuilder(ctx).TotalCount(total).PerPageLimit(30).CurPage(page).Build()
	ctx.HTML(http.StatusOK, tplOrgAlerts)
}

// orgWritableReposCond selects the organization's repositories whose code the doer can write.
// Owners write all of them, a subquery then avoids listing thousands of IDs.
func orgWritableReposCond(ctx *context.Context) (builder.Cond, error) {
	if ctx.Org.IsOwner {
		return builder.In("repo_id", builder.Select("id").From("repository").Where(builder.Eq{"owner_id": ctx.ContextUser.ID})), nil
	}
	ids, err := repo_model.FindUserCodeAccessibleOwnerRepoIDs(ctx, ctx.ContextUser.ID, ctx.Doer)
	if err != nil {
		return nil, err
	}
	repos, err := repo_model.GetRepositoriesMapByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	writable := make([]int64, 0, len(repos))
	for id, repo := range repos {
		perm, err := access_model.GetDoerRepoPermission(ctx, repo, ctx.Doer)
		if err != nil {
			return nil, err
		}
		if perm.CanWrite(unit.TypeCode) {
			writable = append(writable, id)
		}
	}
	return builder.In("repo_id", writable), nil
}

// Live tells the Security tab when a scan of the repository ended, the tab then reloads
func Live(ctx *context.Context) {
	agents_router.ServeLive(ctx, depscan_service.ScanTopic(ctx.Repo.Repository.ID))
}
