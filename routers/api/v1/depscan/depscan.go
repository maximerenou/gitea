// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package depscan serves the API v1 endpoints of dependency scanning
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
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/json"
	api "gitea.dev/modules/structs"
	"gitea.dev/modules/util"
	"gitea.dev/modules/web"
	"gitea.dev/routers/api/v1/utils"
	agents_service "gitea.dev/services/agents"
	"gitea.dev/services/context"
	depscan_service "gitea.dev/services/depscan"
)

// RegisterRepoRoutes is called inside the API v1 "/repos/{username}/{reponame}" group, with the permissions of the web UI:
// writing the code for the alerts and the dependencies, administering the repository for the updates
func RegisterRepoRoutes(m *web.Router, reqToken, reqCodeWriter, reqAdmin func(*context.APIContext)) {
	m.Get("/dependency-alerts", reqCodeWriter, mustEnable, ListAlerts)
	m.Patch("/dependency-alerts/{id}", reqToken, reqCodeWriter, mustEnable, EditAlert)
	m.Get("/dependencies", reqCodeWriter, mustEnable, ListDependencies)
	m.Group("/dependency-updates", func() {
		m.Combo("").Get(GetUpdates).Put(reqToken, EditUpdates)
		m.Post("/run", reqToken, RunUpdates)
	}, reqAdmin, mustEnable, mustEnableAgents)
}

func mustEnable(ctx *context.APIContext) {
	if !depscan_service.IsEnabled() {
		ctx.APIErrorNotFound()
	}
}

func mustEnableAgents(ctx *context.APIContext) {
	if !agents_service.IsEnabled() {
		ctx.APIErrorNotFound()
	}
}

func decode(ctx *context.APIContext, v any) bool {
	if err := json.NewDecoder(ctx.Req.Body).Decode(v); err != nil {
		ctx.APIError(http.StatusUnprocessableEntity, "invalid JSON body")
		return false
	}
	return true
}

func toAPIAlert(ctx *context.APIContext, a *depscan_model.Alert) *api.DependencyAlert {
	return &api.DependencyAlert{
		ID: a.ID, State: string(a.State), Severity: string(a.Severity), Score: a.Score,
		VulnID: a.VulnID, Aliases: a.Aliases, Summary: a.Summary,
		Ecosystem: a.Ecosystem, Package: a.Package, Version: a.Version, Manifest: a.Manifest,
		FixedVersions: a.FixedVersions, DismissedReason: a.DismissedReason,
		HTMLURL: fmt.Sprintf("%s/security/alerts/%d", ctx.Repo.Repository.HTMLURL(ctx), a.ID),
		Created: a.Created.AsTime(),
	}
}

// ListAlerts lists the dependency alerts of a repository
func ListAlerts(ctx *context.APIContext) {
	// swagger:operation GET /repos/{owner}/{repo}/dependency-alerts repository repoListDependencyAlerts
	// ---
	// summary: List the vulnerability alerts on the dependencies of a repository
	// produces:
	// - application/json
	// parameters:
	// - name: owner
	//   in: path
	//   description: owner of the repo
	//   type: string
	//   required: true
	// - name: repo
	//   in: path
	//   description: name of the repo
	//   type: string
	//   required: true
	// - name: state
	//   in: query
	//   description: open, fixed or dismissed, all when empty
	//   type: string
	// - name: severity
	//   in: query
	//   description: critical, high, moderate, low or unknown, all when empty
	//   type: string
	// - name: page
	//   in: query
	//   description: page number of results to return (1-based)
	//   type: integer
	// - name: limit
	//   in: query
	//   description: page size of results
	//   type: integer
	// responses:
	//   "200":
	//     "$ref": "#/responses/DependencyAlertList"
	//   default:
	//     "$ref": "#/responses/error"
	state := depscan_model.AlertState(ctx.FormString("state"))
	if state != "" && !state.IsValid() {
		ctx.APIErrorAuto(util.NewInvalidArgumentErrorf("invalid state %q", state))
		return
	}
	if !depscan_service.IsRepoScanned(ctx, ctx.Repo.Repository) {
		ctx.SetTotalCountHeader(0) // the alerts of a repository no longer scanned are stale
		ctx.JSON(http.StatusOK, []*api.DependencyAlert{})
		return
	}
	alerts, total, err := db.FindAndCount[depscan_model.Alert](ctx, depscan_model.FindAlertsOptions{
		ListOptions: utils.GetListOptions(ctx),
		RepoID:      ctx.Repo.Repository.ID,
		State:       state,
		Severity:    depscan_model.Severity(ctx.FormString("severity")),
	})
	if err != nil {
		ctx.APIErrorInternal(err)
		return
	}
	result := make([]*api.DependencyAlert, 0, len(alerts))
	for _, a := range alerts {
		result = append(result, toAPIAlert(ctx, a))
	}
	ctx.SetTotalCountHeader(total)
	ctx.JSON(http.StatusOK, result)
}

// EditAlert dismisses or reopens a dependency alert
func EditAlert(ctx *context.APIContext) {
	// swagger:operation PATCH /repos/{owner}/{repo}/dependency-alerts/{id} repository repoEditDependencyAlert
	// ---
	// summary: Dismiss a dependency alert with a reason, or reopen a dismissed one
	// consumes:
	// - application/json
	// produces:
	// - application/json
	// parameters:
	// - name: owner
	//   in: path
	//   description: owner of the repo
	//   type: string
	//   required: true
	// - name: repo
	//   in: path
	//   description: name of the repo
	//   type: string
	//   required: true
	// - name: id
	//   in: path
	//   description: id of the alert
	//   type: integer
	//   format: int64
	//   required: true
	// - name: body
	//   in: body
	//   schema:
	//     "$ref": "#/definitions/EditDependencyAlertOption"
	// responses:
	//   "200":
	//     "$ref": "#/responses/DependencyAlert"
	//   default:
	//     "$ref": "#/responses/error"
	a, err := depscan_model.GetRepoAlert(ctx, ctx.Repo.Repository.ID, ctx.PathParamInt64("id"))
	if errors.Is(err, util.ErrNotExist) {
		ctx.APIErrorNotFound()
		return
	} else if err != nil {
		ctx.APIErrorInternal(err)
		return
	}
	var form api.EditDependencyAlertOption
	if !decode(ctx, &form) {
		return
	}
	switch depscan_model.AlertState(form.State) {
	case depscan_model.AlertDismissed:
		err = depscan_service.DismissAlert(ctx, ctx.Doer, ctx.Repo.Repository, a, form.Reason)
	case depscan_model.AlertOpen:
		err = depscan_service.ReopenAlert(ctx, ctx.Repo.Repository, a)
	default:
		err = util.NewInvalidArgumentErrorf("the state must be dismissed or open")
	}
	if err != nil {
		ctx.APIErrorAuto(err)
		return
	}
	ctx.JSON(http.StatusOK, toAPIAlert(ctx, a))
}

// ListDependencies lists the dependencies of a repository's default branch
func ListDependencies(ctx *context.APIContext) {
	// swagger:operation GET /repos/{owner}/{repo}/dependencies repository repoListDependencies
	// ---
	// summary: List the package versions the manifests and lockfiles of the default branch pin, as of the last scan
	// produces:
	// - application/json
	// parameters:
	// - name: owner
	//   in: path
	//   description: owner of the repo
	//   type: string
	//   required: true
	// - name: repo
	//   in: path
	//   description: name of the repo
	//   type: string
	//   required: true
	// - name: q
	//   in: query
	//   description: part of the package names, ignoring case
	//   type: string
	// - name: page
	//   in: query
	//   description: page number of results to return (1-based)
	//   type: integer
	// - name: limit
	//   in: query
	//   description: page size of results
	//   type: integer
	// responses:
	//   "200":
	//     "$ref": "#/responses/DependencyList"
	//   default:
	//     "$ref": "#/responses/error"
	deps, total, err := db.FindAndCount[depscan_model.Dependency](ctx, depscan_model.FindDependenciesOptions{
		ListOptions: utils.GetListOptions(ctx),
		RepoID:      ctx.Repo.Repository.ID,
		Keyword:     ctx.FormTrim("q"),
	})
	if err != nil {
		ctx.APIErrorInternal(err)
		return
	}
	result := make([]*api.Dependency, 0, len(deps))
	for _, d := range deps {
		result = append(result, &api.Dependency{Ecosystem: d.Ecosystem, Name: d.Name, Version: d.Version, Manifest: d.Manifest})
	}
	ctx.SetTotalCountHeader(total)
	ctx.JSON(http.StatusOK, result)
}

// GetUpdates returns how an agent updates the dependencies of a repository
func GetUpdates(ctx *context.APIContext) {
	// swagger:operation GET /repos/{owner}/{repo}/dependency-updates repository repoGetDependencyUpdates
	// ---
	// summary: Get how an agent updates the dependencies of a repository
	// produces:
	// - application/json
	// parameters:
	// - name: owner
	//   in: path
	//   description: owner of the repo
	//   type: string
	//   required: true
	// - name: repo
	//   in: path
	//   description: name of the repo
	//   type: string
	//   required: true
	// responses:
	//   "200":
	//     "$ref": "#/responses/DependencyUpdates"
	//   default:
	//     "$ref": "#/responses/error"
	rs, err := depscan_model.GetRepoSetting(ctx, ctx.Repo.Repository.ID)
	if err != nil {
		ctx.APIErrorInternal(err)
		return
	}
	result := &api.DependencyUpdates{
		Interval: string(util.IfZero(rs.UpdatesInterval, depscan_model.UpdatesWeekly)),
		Major:    rs.UpdatesMajor,
		Ignore:   append([]string{}, rs.UpdatesIgnore...),
		Scope:    string(util.Iif(rs.UpdatesSecurityOnly(), depscan_model.UpdatesScopeSecurity, depscan_model.UpdatesScopeAll)),
		OnAlert:  rs.UpdatesOnAlert,
		Refused:  make([]*api.RefusedDependencyVersion, 0, len(rs.UpdatesRefused)),
	}
	for _, r := range rs.UpdatesRefused {
		result.Refused = append(result.Refused, &api.RefusedDependencyVersion{Ecosystem: r.Ecosystem, Package: r.Package, Version: r.Version})
	}
	if rs.UpdatesAgentID > 0 {
		if agent, err := agents_model.GetAgentByID(ctx, rs.UpdatesAgentID); err == nil && agent.LoadBot(ctx) == nil {
			result.Agent = agent.MentionName()
		}
		if _, updater, err := user_model.GetPossibleUserByID(ctx, rs.UpdatesUserID); err == nil {
			result.Updater = updater.Name
		}
		result.LastError = rs.UpdatesLastError
		if rs.UpdatesLastUnix > 0 {
			lastRun := rs.UpdatesLastUnix.AsTime()
			result.LastRun = &lastRun
		}
	}
	ctx.JSON(http.StatusOK, result)
}

// EditUpdates changes how an agent updates the dependencies of a repository
func EditUpdates(ctx *context.APIContext) {
	// swagger:operation PUT /repos/{owner}/{repo}/dependency-updates repository repoEditDependencyUpdates
	// ---
	// summary: Change how an agent updates the dependencies of a repository
	// description: Whoever changes the updates becomes their updater, the agent then runs with their API key and permissions.
	// consumes:
	// - application/json
	// parameters:
	// - name: owner
	//   in: path
	//   description: owner of the repo
	//   type: string
	//   required: true
	// - name: repo
	//   in: path
	//   description: name of the repo
	//   type: string
	//   required: true
	// - name: body
	//   in: body
	//   schema:
	//     "$ref": "#/definitions/DependencyUpdatesOption"
	// responses:
	//   "204":
	//     "$ref": "#/responses/empty"
	//   default:
	//     "$ref": "#/responses/error"
	var form api.DependencyUpdatesOption
	if !decode(ctx, &form) {
		return
	}
	rs, err := depscan_model.GetRepoSetting(ctx, ctx.Repo.Repository.ID)
	if err != nil {
		ctx.APIErrorInternal(err)
		return
	}
	opts := depscan_service.UpdateOptions{
		Interval: rs.UpdatesInterval, Major: rs.UpdatesMajor, Ignore: rs.UpdatesIgnore,
		Scope: rs.UpdatesScope, OnAlert: rs.UpdatesOnAlert, TakeOver: form.TakeOver,
	}
	if rs.UpdatesAgentID > 0 {
		if opts.Agent, err = agents_model.GetAgentByID(ctx, rs.UpdatesAgentID); err == nil {
			err = opts.Agent.LoadBot(ctx)
		}
		if err != nil {
			ctx.APIErrorInternal(err)
			return
		}
	}
	if form.Agent != nil {
		opts.Agent = nil
		if name := strings.ToLower(strings.TrimPrefix(*form.Agent, "@")); name != "" {
			agents, err := agents_model.GetEnabledAgentsByBotNames(ctx, []string{name})
			if err != nil {
				ctx.APIErrorInternal(err)
				return
			}
			if opts.Agent = agents[name]; opts.Agent == nil {
				ctx.APIErrorNotFound("agent not found")
				return
			}
		}
	}
	if form.Interval != nil {
		opts.Interval = depscan_model.UpdatesInterval(*form.Interval)
		if !slices.Contains(depscan_model.UpdatesIntervals, opts.Interval) {
			ctx.APIErrorAuto(util.NewInvalidArgumentErrorf("the interval must be daily, weekly or monthly"))
			return
		}
	}
	if form.Scope != nil {
		opts.Scope = depscan_model.UpdatesScope(*form.Scope)
		if !slices.Contains(depscan_model.UpdatesScopes, opts.Scope) {
			ctx.APIErrorAuto(util.NewInvalidArgumentErrorf("the scope must be all or security"))
			return
		}
	}
	if form.Major != nil {
		opts.Major = *form.Major
	}
	if form.OnAlert != nil {
		opts.OnAlert = *form.OnAlert
	}
	if form.Ignore != nil {
		opts.Ignore = nil
		for _, entry := range form.Ignore {
			if entry = strings.TrimSpace(entry); entry != "" {
				opts.Ignore = append(opts.Ignore, entry)
			}
		}
	}
	err = depscan_service.SaveUpdateSettings(ctx, ctx.Doer, ctx.Repo.Repository, opts)
	if errors.Is(err, agents_service.ErrNoUserKey) {
		ctx.APIError(http.StatusUnprocessableEntity, fmt.Sprintf("you have no API key for @%s, add one in your settings", opts.Agent.MentionName()))
		return
	} else if err != nil {
		ctx.APIErrorAuto(err)
		return
	}
	for _, r := range form.AllowRefused {
		err := depscan_service.RemoveRefusedVersion(ctx, ctx.Repo.Repository, depscan_model.RefusedVersion{Ecosystem: r.Ecosystem, Package: r.Package, Version: r.Version})
		if err != nil {
			ctx.APIErrorAuto(err)
			return
		}
	}
	ctx.Status(http.StatusNoContent)
}

// RunUpdates starts the dependency updates at once
func RunUpdates(ctx *context.APIContext) {
	// swagger:operation POST /repos/{owner}/{repo}/dependency-updates/run repository repoRunDependencyUpdates
	// ---
	// summary: Start the dependency updates at once
	// description: The doer takes the updates over, the agent then runs with their API key and permissions.
	// produces:
	// - application/json
	// parameters:
	// - name: owner
	//   in: path
	//   description: owner of the repo
	//   type: string
	//   required: true
	// - name: repo
	//   in: path
	//   description: name of the repo
	//   type: string
	//   required: true
	// responses:
	//   "202":
	//     "$ref": "#/responses/DependencyUpdatesRun"
	//   default:
	//     "$ref": "#/responses/error"
	started, err := depscan_service.UpdateNow(ctx, ctx.Doer, ctx.Repo.Repository)
	if errors.Is(err, agents_service.ErrNoUserKey) {
		ctx.APIError(http.StatusUnprocessableEntity, "you have no API key for the update agent, add one in your settings")
		return
	} else if err != nil {
		ctx.APIErrorAuto(err)
		return
	}
	ctx.JSON(http.StatusAccepted, &api.DependencyUpdatesRun{Started: started})
}
