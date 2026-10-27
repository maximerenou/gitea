// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"errors"
	"net/http"

	agents_model "gitea.dev/models/agents"
	system_model "gitea.dev/models/system"
	"gitea.dev/modules/agents"
	"gitea.dev/modules/json"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/setting/config"
	"gitea.dev/modules/templates"
	"gitea.dev/modules/util"
	"gitea.dev/modules/web"
	agents_service "gitea.dev/services/agents"
	"gitea.dev/services/context"
)

const (
	tplGeneral        templates.TplName = "agents/admin/general"
	tplConnections    templates.TplName = "agents/admin/connections"
	tplConnectionForm templates.TplName = "agents/admin/connection_form"
	tplAgents         templates.TplName = "agents/admin/agents"
	tplAgentForm      templates.TplName = "agents/admin/agent_form"

	adminLink = "/-/admin/agents"
)

// RequireEnabled turns every agents page into a 404 while the feature is off.
func RequireEnabled(ctx *context.Context) {
	if !agents.IsEnabled(ctx) {
		ctx.NotFound(nil)
	}
}

// AdminContext gives the admin navbar the data it needs to show the agents entries.
func AdminContext(ctx *context.Context) {
	ctx.Data["AgentsEnabled"] = agents.IsEnabled(ctx)
}

// RegisterAdminRoutes adds the site administration pages; "General" stays reachable while disabled so it can be enabled.
func RegisterAdminRoutes(m *web.Router) {
	m.Group(adminLink, func() {
		m.Get("", General)
		m.Post("", web.Bind[*SettingsForm](), GeneralPost)

		m.Group("", func() {
			m.Group("/connections", func() {
				m.Get("", Connections)
				m.Combo("/new").Get(NewConnection).Post(web.Bind[*ConnectionForm](), NewConnectionPost)
				m.Combo("/{id}").Get(EditConnection).Post(web.Bind[*ConnectionForm](), EditConnectionPost)
				m.Post("/{id}/delete", DeleteConnection)
			})
			m.Group("/list", func() {
				m.Get("", Agents)
				m.Combo("/new").Get(NewAgent).Post(web.Bind[*AgentForm](), NewAgentPost)
				m.Combo("/{id}").Get(EditAgent).Post(web.Bind[*AgentForm](), EditAgentPost)
				m.Post("/{id}/delete", DeleteAgent)
			})
		}, RequireEnabled)
	})
}

func prepareAdminPage(ctx *context.Context, titleKey, pageFlag string) {
	ctx.Data["Title"] = ctx.Tr(titleKey)
	ctx.Data["PageIsAdminAgents"] = true
	ctx.Data[pageFlag] = true
}

func General(ctx *context.Context) {
	prepareAdminPage(ctx, "agents.general", "PageIsAdminAgentsGeneral")
	s := agents.GetSettings(ctx)
	ctx.Data["Settings"] = s
	ctx.Data["HelperSHA256"] = agents.FormatHelperSums(s.HelperSHA256)
	ctx.HTML(http.StatusOK, tplGeneral)
}

func GeneralPost(ctx *context.Context) {
	form := web.GetForm[*SettingsForm](ctx)
	sums, err := agents.ParseHelperSums(form.HelperSHA256)
	s := agents.Settings{
		Enabled:              form.Enabled,
		RunnerLabel:          form.RunnerLabel,
		RunsPerIssuePerHour:  form.RunsPerIssuePerHour,
		MaxAgentRoundTrips:   form.MaxAgentRoundTrips,
		AutoRetries:          form.AutoRetries,
		FailureMemoryHours:   form.FailureMemoryHours,
		QueueTTLHours:        form.QueueTTLHours,
		CheckoutAction:       form.CheckoutAction,
		CIRepairsPerCommit:   form.CIRepairsPerCommit,
		BlockedReminderHours: form.BlockedReminderHours,
		BlockedExpiryHours:   form.BlockedExpiryHours,
		JobImage:             form.JobImage,
		HelperDownloadURL:    form.HelperDownloadURL,
		HelperSHA256:         sums,
	}
	if err == nil {
		err = s.Validate()
	}
	if err != nil {
		prepareAdminPage(ctx, "agents.general", "PageIsAdminAgentsGeneral")
		ctx.Data["Settings"] = s
		ctx.Data["HelperSHA256"] = form.HelperSHA256
		ctx.Flash.Error(err.Error(), true)
		ctx.HTML(http.StatusUnprocessableEntity, tplGeneral)
		return
	}
	value, err := json.Marshal(s)
	if err != nil {
		ctx.ServerError("Marshal", err)
		return
	}
	if err := system_model.SetSettings(ctx, map[string]string{agents.SettingsKey(): string(value)}); err != nil {
		ctx.ServerError("SetSettings", err)
		return
	}
	config.GetDynGetter().InvalidateCache()
	ctx.Flash.Success(ctx.Tr("agents.settings_saved"))
	ctx.Redirect(setting.AppSubURL + adminLink)
}

func Connections(ctx *context.Context) {
	prepareAdminPage(ctx, "agents.connections", "PageIsAdminAgentsConnections")
	conns, err := findConnections(ctx)
	if err != nil {
		ctx.ServerError("FindConnections", err)
		return
	}
	ctx.Data["Connections"] = conns
	ctx.HTML(http.StatusOK, tplConnections)
}

func prepareConnectionForm(ctx *context.Context) {
	prepareAdminPage(ctx, "agents.connection", "PageIsAdminAgentsConnections")
	ctx.Data["Providers"] = []agents_model.Provider{agents_model.ProviderAnthropic, agents_model.ProviderOpenAICompatible, agents_model.ProviderOllama}
	ctx.Data["CLIs"] = []string{agents_model.CLICodex}
	ctx.Data["AccessModes"] = []agents_model.ConnectionAccessMode{agents_model.ConnectionAccessNone, agents_model.ConnectionAccessAll}
}

func NewConnection(ctx *context.Context) {
	prepareConnectionForm(ctx)
	ctx.Data["Connection"] = &agents_model.Connection{CLI: agents_model.CLICodex, AccessMode: agents_model.ConnectionAccessNone}
	ctx.HTML(http.StatusOK, tplConnectionForm)
}

func connectionFromForm(c *agents_model.Connection, form *ConnectionForm) {
	c.Name, c.Provider, c.BaseURL, c.CLI = form.Name, agents_model.Provider(form.Provider), form.BaseURL, form.CLI
	c.AccessMode = agents_model.ConnectionAccessMode(form.AccessMode)
	if c.AccessMode == "" {
		c.AccessMode = agents_model.ConnectionAccessNone
	}
}

func NewConnectionPost(ctx *context.Context) {
	form := web.GetForm[*ConnectionForm](ctx)
	c := &agents_model.Connection{}
	connectionFromForm(c, form)
	err := c.SetToken(form.Token)
	if err == nil {
		err = agents_model.CreateConnection(ctx, c)
	}
	if err != nil {
		renderConnectionError(ctx, c, err)
		return
	}
	ctx.Flash.Success(ctx.Tr("agents.connection_saved"))
	ctx.Redirect(setting.AppSubURL + adminLink + "/connections")
}

func loadConnection(ctx *context.Context) *agents_model.Connection {
	c, err := agents_model.GetConnectionByID(ctx, ctx.PathParamInt64("id"))
	if err == nil && !c.IsGlobal() {
		err = agents_model.ErrConnectionNotFound // only global connections are managed here
	}
	if err != nil {
		if errors.Is(err, util.ErrNotExist) {
			ctx.NotFound(err)
		} else {
			ctx.ServerError("GetConnectionByID", err)
		}
		return nil
	}
	return c
}

func EditConnection(ctx *context.Context) {
	c := loadConnection(ctx)
	if c == nil {
		return
	}
	prepareConnectionForm(ctx)
	ctx.Data["Connection"] = c
	ctx.HTML(http.StatusOK, tplConnectionForm)
}

func EditConnectionPost(ctx *context.Context) {
	c := loadConnection(ctx)
	if c == nil {
		return
	}
	form := web.GetForm[*ConnectionForm](ctx)
	connectionFromForm(c, form)
	updateToken := form.ClearToken || form.Token != ""
	var err error
	if updateToken {
		err = c.SetToken(form.Token)
	}
	if err == nil {
		err = agents_model.UpdateConnection(ctx, c, updateToken)
	}
	if err != nil {
		renderConnectionError(ctx, c, err)
		return
	}
	ctx.Flash.Success(ctx.Tr("agents.connection_saved"))
	ctx.Redirect(setting.AppSubURL + adminLink + "/connections")
}

func renderConnectionError(ctx *context.Context, c *agents_model.Connection, err error) {
	prepareConnectionForm(ctx)
	ctx.Data["Connection"] = c
	ctx.Flash.Error(err.Error(), true)
	ctx.HTML(http.StatusUnprocessableEntity, tplConnectionForm)
}

func DeleteConnection(ctx *context.Context) {
	c := loadConnection(ctx)
	if c == nil {
		return
	}
	if err := agents_model.DeleteConnection(ctx, c.ID); err != nil {
		ctx.Flash.Error(err.Error())
	} else {
		ctx.Flash.Success(ctx.Tr("agents.connection_deleted"))
	}
	ctx.JSONRedirect(setting.AppSubURL + adminLink + "/connections")
}

type agentRow struct {
	*agents_model.Agent
	Role           string
	ConnectionName string
}

func Agents(ctx *context.Context) {
	prepareAdminPage(ctx, "agents.agents", "PageIsAdminAgentsList")
	list, err := findAgents(ctx)
	if err != nil {
		ctx.ServerError("FindAgents", err)
		return
	}
	conns, err := findConnections(ctx)
	if err != nil {
		ctx.ServerError("FindConnections", err)
		return
	}
	names := map[int64]string{}
	for _, c := range conns {
		names[c.ID] = c.Name
	}
	rows := make([]agentRow, 0, len(list))
	for _, a := range list {
		rows = append(rows, agentRow{Agent: a, Role: a.Action.Role(), ConnectionName: names[a.ConnectionID]})
	}
	ctx.Data["Agents"] = rows
	ctx.HTML(http.StatusOK, tplAgents)
}

func prepareAgentForm(ctx *context.Context, agent *agents_model.Agent) bool {
	prepareAdminPage(ctx, "agents.agent", "PageIsAdminAgentsList")
	conns, err := findConnections(ctx)
	if err != nil {
		ctx.ServerError("FindConnections", err)
		return false
	}
	ctx.Data["Connections"] = conns
	ctx.Data["Agent"] = agent
	ctx.Data["IsNew"] = agent.ID == 0
	ctx.Data["Actions"] = agents.Actions()
	ctx.Data["AccessModes"] = []agents_model.AccessMode{agents_model.AccessWriters, agents_model.AccessOwner}
	// events of the fixed action when editing; every event otherwise (the server refuses a bad pair)
	events := agent.Action.AcceptedEvents()
	if agent.ID == 0 {
		events = allEvents()
	}
	ctx.Data["EventChoices"] = events
	selected := map[agents.Event]bool{}
	for _, e := range agent.Events {
		selected[e] = true
	}
	ctx.Data["SelectedEvents"] = selected
	return true
}

func allEvents() []agents.Event {
	return []agents.Event{
		agents.EventAll, agents.EventIssueCreated, agents.EventAssigned, agents.EventMentioned, agents.EventReplied,
		agents.EventPRCreated, agents.EventReviewRequested, agents.EventReviewSubmitted, agents.EventReviewComment,
		agents.EventNewFinding, agents.EventRequestedOnFinding, agents.EventConfirmedFinding,
		agents.EventDefaultBranchCIFailed, agents.EventScheduledScan, agents.EventPushDefaultBranch,
	}
}

func NewAgent(ctx *context.Context) {
	a := &agents_model.Agent{Action: agents.ActionReview, Events: []agents.Event{agents.EventAll}, AccessMode: agents_model.AccessWriters}
	if prepareAgentForm(ctx, a) {
		ctx.HTML(http.StatusOK, tplAgentForm)
	}
}

func toEvents(in []string) []agents.Event {
	out := make([]agents.Event, 0, len(in))
	for _, e := range in {
		out = append(out, agents.Event(e))
	}
	return out
}

func NewAgentPost(ctx *context.Context) {
	form := web.GetForm[*AgentForm](ctx)
	opts := agents_service.CreateAgentOptions{
		Name:         form.Name,
		ConnectionID: form.ConnectionID,
		Model:        form.Model,
		Description:  form.Description,
		Language:     form.Language,
		Action:       agents.Action(form.Action),
		Events:       toEvents(form.Events),
		AccessMode:   agents_model.AccessMode(form.AccessMode),
	}
	if _, err := agents_service.CreateAgent(ctx, opts); err != nil {
		a := &agents_model.Agent{Name: opts.Name, ConnectionID: opts.ConnectionID, Model: opts.Model, Description: opts.Description,
			Language: opts.Language, Action: opts.Action, Events: opts.Events, AccessMode: opts.AccessMode}
		if prepareAgentForm(ctx, a) {
			ctx.Flash.Error(err.Error(), true)
			ctx.HTML(http.StatusUnprocessableEntity, tplAgentForm)
		}
		return
	}
	ctx.Flash.Success(ctx.Tr("agents.agent_created"))
	ctx.Redirect(setting.AppSubURL + adminLink + "/list")
}

func loadAgent(ctx *context.Context) *agents_model.Agent {
	a, err := agents_model.GetAgentByID(ctx, ctx.PathParamInt64("id"))
	if err == nil && !a.IsGlobal() {
		err = agents_model.ErrAgentNotFound // only global agents are managed here
	}
	if err != nil {
		if errors.Is(err, util.ErrNotExist) {
			ctx.NotFound(err)
		} else {
			ctx.ServerError("GetAgentByID", err)
		}
		return nil
	}
	return a
}

func EditAgent(ctx *context.Context) {
	a := loadAgent(ctx)
	if a != nil && prepareAgentForm(ctx, a) {
		ctx.HTML(http.StatusOK, tplAgentForm)
	}
}

func EditAgentPost(ctx *context.Context) {
	a := loadAgent(ctx)
	if a == nil {
		return
	}
	form := web.GetForm[*AgentForm](ctx)
	a.ConnectionID, a.Model, a.Description, a.Language = form.ConnectionID, form.Model, form.Description, form.Language
	a.Events = toEvents(form.Events)
	a.AccessMode = agents_model.AccessMode(form.AccessMode)
	conn, err := agents_model.GetConnectionByID(ctx, a.ConnectionID)
	if err == nil && !conn.IsGlobal() {
		err = util.NewInvalidArgumentErrorf("connection is not available to this agent")
	}
	if err == nil {
		err = agents_model.UpdateAgent(ctx, a)
	}
	if err != nil {
		if prepareAgentForm(ctx, a) {
			ctx.Flash.Error(err.Error(), true)
			ctx.HTML(http.StatusUnprocessableEntity, tplAgentForm)
		}
		return
	}
	ctx.Flash.Success(ctx.Tr("agents.agent_saved"))
	ctx.Redirect(setting.AppSubURL + adminLink + "/list")
}

func DeleteAgent(ctx *context.Context) {
	a := loadAgent(ctx)
	if a == nil {
		return
	}
	if err := agents_service.DeleteAgent(ctx, a); err != nil {
		ctx.Flash.Error(err.Error())
	} else {
		ctx.Flash.Success(ctx.Tr("agents.agent_deleted"))
	}
	ctx.JSONRedirect(setting.AppSubURL + adminLink + "/list")
}
