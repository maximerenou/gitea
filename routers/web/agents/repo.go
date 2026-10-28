// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	actions_model "gitea.dev/models/actions"
	agents_model "gitea.dev/models/agents"
	"gitea.dev/models/db"
	"gitea.dev/models/unit"
	"gitea.dev/modules/git"
	"gitea.dev/modules/templates"
	"gitea.dev/modules/util"
	agents_service "gitea.dev/services/agents"
	"gitea.dev/services/context"
)

const (
	tplRepoAgents templates.TplName = "repo/agents/list"
	tplRepoAgent  templates.TplName = "repo/agents/view"
)

func repoAgentsLink(ctx *context.Context) string {
	return ctx.Repo.RepoLink + "/agents"
}

func prepareRepo(ctx *context.Context) {
	ctx.Data["Title"] = ctx.Tr("agents.title")
	ctx.Data["PageIsAgents"] = true
	ctx.Data["AgentsUsable"] = agents_service.IsRepoUsable(ctx, ctx.Repo.Repository)
	if rs, err := agents_model.GetRepoSetting(ctx, ctx.Repo.Repository.ID); err == nil {
		ctx.Data["AgentsDisabledInRepo"] = !rs.Enabled
	}
}

// sessionFilter is an option of a filter of the session list, its link keeps the other filters
type sessionFilter struct {
	Label    string
	Link     string
	Selected bool
}

func filterLink(ctx *context.Context, key, value string) string {
	query := ctx.Req.URL.Query()
	query.Del("page")
	if value == "" {
		query.Del(key)
	} else {
		query.Set(key, value)
	}
	return "?" + query.Encode()
}

func filterOptions(ctx *context.Context, key, current, allLabel string, values, labels []string) []sessionFilter {
	filters := []sessionFilter{{Label: allLabel, Link: filterLink(ctx, key, ""), Selected: current == ""}}
	for i, value := range values {
		filters = append(filters, sessionFilter{Label: labels[i], Link: filterLink(ctx, key, value), Selected: current == value})
	}
	return filters
}

// prepareSessionFilters reads the filters of the session list and offers their options
func prepareSessionFilters(ctx *context.Context, opts *agents_model.FindSessionsOptions) error {
	if status := agents_model.Status(ctx.FormString("status")); status.IsValid() {
		opts.Status = status
	}
	if kind := agents_model.SessionKind(ctx.FormString("kind")); kind.IsValid() {
		opts.Kind = kind
	}
	opts.AgentID = ctx.FormInt64("agent")
	if ctx.Doer != nil && ctx.FormBool("mine") {
		opts.CreatorID = ctx.Doer.ID
	}

	var values, labels []string
	for _, status := range agents_model.Statuses {
		values, labels = append(values, string(status)), append(labels, ctx.Locale.TrString("agents.status."+string(status)))
	}
	ctx.Data["StatusFilters"] = filterOptions(ctx, "status", string(opts.Status), ctx.Locale.TrString("agents.sessions.filter.any_status"), values, labels)
	values, labels = nil, nil
	for _, kind := range agents_model.SessionKinds {
		values, labels = append(values, string(kind)), append(labels, ctx.Locale.TrString("agents.kind."+string(kind)))
	}
	ctx.Data["KindFilters"] = filterOptions(ctx, "kind", string(opts.Kind), ctx.Locale.TrString("agents.sessions.filter.any_kind"), values, labels)
	agents, err := agents_model.FindAgents(ctx, false)
	if err != nil {
		return err
	}
	if err := agents_model.LoadAgentBots(ctx, agents); err != nil {
		return err
	}
	values, labels = nil, nil
	for _, agent := range agents {
		values, labels = append(values, strconv.FormatInt(agent.ID, 10)), append(labels, "@"+agent.MentionName())
	}
	ctx.Data["AgentFilters"] = filterOptions(ctx, "agent", util.Iif(opts.AgentID > 0, strconv.FormatInt(opts.AgentID, 10), ""), ctx.Locale.TrString("agents.sessions.filter.any_agent"), values, labels)
	if ctx.Doer != nil {
		ctx.Data["MineFilter"] = sessionFilter{Link: filterLink(ctx, "mine", util.Iif(opts.CreatorID > 0, "", "1")), Selected: opts.CreatorID > 0}
	}
	ctx.Data["IsFiltered"] = opts.Status != "" || opts.Kind != "" || opts.AgentID > 0 || opts.CreatorID > 0
	return nil
}

// List shows the sessions of the repository and the form to start one
func List(ctx *context.Context) {
	prepareRepo(ctx)
	page := max(ctx.FormInt("page"), 1)
	opts := agents_model.FindSessionsOptions{
		ListOptions: db.ListOptions{Page: page, PageSize: 20},
		RepoID:      ctx.Repo.Repository.ID,
		HideIssues:  !ctx.Repo.Permission.CanRead(unit.TypeIssues),
		HidePulls:   !ctx.Repo.Permission.CanRead(unit.TypePullRequests),
	}
	if err := prepareSessionFilters(ctx, &opts); err != nil {
		ctx.ServerError("prepareSessionFilters", err)
		return
	}
	sessions, total, err := db.FindAndCount[agents_model.Session](ctx, opts)
	if err != nil {
		ctx.ServerError("FindSessions", err)
		return
	}
	for _, s := range sessions {
		s.Repo = ctx.Repo.Repository
		if err := s.LoadAttributes(ctx); err != nil {
			ctx.ServerError("LoadAttributes", err)
			return
		}
	}
	ctx.Data["Sessions"] = sessions

	if ctx.Doer != nil && ctx.Repo.Permission.CanWrite(unit.TypeCode) {
		agents, err := usableAgents(ctx, ctx.Repo.Repository)
		if err != nil {
			ctx.ServerError("usableAgents", err)
			return
		}
		ctx.Data["Agents"] = agents
		ctx.Data["ReasoningEfforts"] = agents_model.ReasoningEfforts
		ctx.Data["CanStartSession"] = true
	}
	ctx.Data["CanViewSession"] = ctx.Repo.Permission.CanWrite(unit.TypeCode)
	ctx.Data["SettingsLink"] = settingsLink()

	ctx.Data["Page"] = context.NewPagerBuilder(ctx).TotalCount(total).PerPageLimit(20).CurPage(page).Build()
	ctx.HTML(http.StatusOK, tplRepoAgents)
}

func jsonServiceError(ctx *context.Context, err error) {
	switch {
	case errors.Is(err, util.ErrInvalidArgument), errors.Is(err, util.ErrPermissionDenied), errors.Is(err, util.ErrNotExist):
		ctx.JSONError(err.Error())
	default:
		ctx.ServerError("agents", err)
	}
}

func New(ctx *context.Context) {
	agent, err := agents_model.GetAgentByID(ctx, ctx.FormInt64("agent_id"))
	if err != nil {
		jsonServiceError(ctx, err)
		return
	}
	s, err := agents_service.StartTaskSession(ctx, ctx.Doer, ctx.Repo.Repository, agents_service.StartOptions{
		Agent:           agent,
		Prompt:          ctx.FormString("prompt"),
		BaseBranch:      ctx.FormString("base_branch"),
		ReasoningEffort: ctx.FormString("reasoning_effort"),
	})
	if err != nil {
		jsonServiceError(ctx, err)
		return
	}
	ctx.JSONRedirect(fmt.Sprintf("%s/%d", repoAgentsLink(ctx), s.ID))
}

func getSession(ctx *context.Context) *agents_model.Session {
	s, err := agents_model.GetRepoSession(ctx, ctx.Repo.Repository.ID, ctx.PathParamInt64("id"))
	if err != nil {
		if errors.Is(err, util.ErrNotExist) {
			ctx.NotFound(nil)
		} else {
			ctx.ServerError("GetRepoSession", err)
		}
		return nil
	}
	s.Repo = ctx.Repo.Repository
	if err := s.LoadAttributes(ctx); err != nil {
		ctx.ServerError("LoadAttributes", err)
		return nil
	}
	if s.Issue != nil && !ctx.Repo.Permission.CanReadIssuesOrPulls(s.Issue.IsPull) { // its prompts quote the discussion
		ctx.NotFound(nil)
		return nil
	}
	return s
}

// canCancel lets code writers stop any run, and participants stop the runs they share, cancelling never spends anyone's key
func canCancel(ctx *context.Context, s *agents_model.Session) bool {
	return agents_service.CanCancel(ctx, s, ctx.Doer, ctx.Repo.Permission)
}

// canInstruct tells whether the doer can continue the session, their own key and permissions are used
func canInstruct(ctx *context.Context, s *agents_model.Session) bool {
	if ctx.Doer == nil || s.Agent == nil || s.Agent.IsDisabled {
		return false
	}
	_, err := agents_model.GetUserKey(ctx, s.AgentID, ctx.Doer.ID)
	return err == nil
}

func View(ctx *context.Context) {
	prepareRepo(ctx)
	s := getSession(ctx)
	if s == nil {
		return
	}
	runs, err := db.Find[agents_model.Run](ctx, agents_model.FindRunsOptions{SessionID: s.ID})
	if err != nil {
		ctx.ServerError("FindRuns", err)
		return
	}
	ctx.Data["RunUsage"] = agents_service.SessionUsage(runs)
	ctx.Data["Title"] = s.Title
	ctx.Data["Session"] = s
	ctx.Data["Runs"] = runs
	if ctx.Data["ExistingActionRuns"], err = agents_model.ExistingActionRuns(ctx, s.RepoID, runs); err != nil {
		ctx.ServerError("ExistingActionRuns", err)
		return
	}
	ctx.Data["CanInstruct"] = canInstruct(ctx, s)
	ctx.Data["CanCancel"] = canCancel(ctx, s)
	if ctx.Data["CanResume"], err = agents_service.CanResume(ctx, s, ctx.Doer); err != nil {
		ctx.ServerError("CanResume", err)
		return
	}
	ctx.Data["SettingsLink"] = settingsLink()
	ctx.Data["SessionLink"] = fmt.Sprintf("%s/%d", repoAgentsLink(ctx), s.ID)
	if s.Branch != "" && git.IsBranchExist(ctx, ctx.Repo.Repository, s.Branch) {
		ctx.Data["BranchLink"] = ctx.Repo.RepoLink + "/src/branch/" + util.PathEscapeSegments(s.Branch)
	}
	if len(runs) > 0 {
		latest := runs[len(runs)-1]
		ctx.Data["LatestRun"] = latest
		if pr := agents_service.FindSessionPull(ctx, s, latest); pr != nil {
			ctx.Data["PullRequest"] = pr
		}
	}
	ctx.HTML(http.StatusOK, tplRepoAgent)
}

type runStatusView struct {
	ID     int64               `json:"id"`
	Status agents_model.Status `json:"status"`
}

// Events serves the events after the given id, the page polls it while the session is active
func Events(ctx *context.Context) {
	s := getSession(ctx)
	if s == nil {
		return
	}
	events, err := agents_model.FindSessionEvents(ctx, s.ID, ctx.FormInt64("after"), 500)
	if err != nil {
		ctx.ServerError("FindSessionEvents", err)
		return
	}
	views := make([]*agents_service.EventView, 0, len(events))
	lastID := ctx.FormInt64("after")
	for _, e := range events {
		lastID = e.ID
		if view := agents_service.ToEventView(e); view != nil {
			views = append(views, view)
		}
	}
	agents_service.RenderEventViews(ctx, ctx.Repo.Repository, views)
	runs, err := db.Find[agents_model.Run](ctx, agents_model.FindRunsOptions{SessionID: s.ID})
	if err != nil {
		ctx.ServerError("FindRuns", err)
		return
	}
	runViews := make([]runStatusView, 0, len(runs))
	for _, r := range runs {
		runViews = append(runViews, runStatusView{ID: r.ID, Status: r.Status})
	}
	ctx.JSON(http.StatusOK, map[string]any{
		"lastId":   lastID,
		"events":   views,
		"status":   s.Status,
		"active":   !s.Status.IsDone(),
		"complete": len(events) < 500,
		"runs":     runViews,
	})
}

func Instruct(ctx *context.Context) {
	s := getSession(ctx)
	if s == nil {
		return
	}
	if _, err := agents_service.SendInstruction(ctx, s, ctx.Doer, ctx.FormString("prompt")); err != nil {
		jsonServiceError(ctx, err)
		return
	}
	ctx.JSONRedirect(fmt.Sprintf("%s/%d", repoAgentsLink(ctx), s.ID))
}

func Cancel(ctx *context.Context) {
	s := getSession(ctx)
	if s == nil {
		return
	}
	if !canCancel(ctx, s) {
		ctx.JSONError(ctx.Tr("agents.session.cannot_cancel"))
		return
	}
	if err := agents_service.CancelSession(ctx, s); err != nil {
		jsonServiceError(ctx, err)
		return
	}
	ctx.JSONRedirect(fmt.Sprintf("%s/%d", repoAgentsLink(ctx), s.ID))
}

func Resume(ctx *context.Context) {
	s := getSession(ctx)
	if s == nil {
		return
	}
	if _, err := agents_service.ResumeSession(ctx, s, ctx.Doer); err != nil {
		jsonServiceError(ctx, err)
		return
	}
	ctx.JSONRedirect(fmt.Sprintf("%s/%d", repoAgentsLink(ctx), s.ID))
}

func Retry(ctx *context.Context) {
	s := getSession(ctx)
	if s == nil {
		return
	}
	if _, err := agents_service.RetryRun(ctx, s, ctx.Doer, ctx.PathParamInt64("run")); err != nil {
		jsonServiceError(ctx, err)
		return
	}
	ctx.JSONRedirect(fmt.Sprintf("%s/%d", repoAgentsLink(ctx), s.ID))
}

// RedirectWorkflowFile sends the workflow file link of an agent run to its session, the workflow is generated and not in the repository
func RedirectWorkflowFile(ctx *context.Context, actionRun *actions_model.ActionRun) bool {
	if actionRun.WorkflowID != agents_model.WorkflowID {
		return false
	}
	r, err := agents_model.GetRepoRunByActionRunID(ctx, actionRun.RepoID, actionRun.ID)
	if err != nil {
		ctx.Redirect(repoAgentsLink(ctx))
		return true
	}
	ctx.Redirect(fmt.Sprintf("%s/%d", repoAgentsLink(ctx), r.SessionID))
	return true
}
