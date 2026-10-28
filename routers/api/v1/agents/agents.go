// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package agents serves the API v1 endpoints of the Codex agents and their sessions
package agents

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	agents_model "gitea.dev/models/agents"
	"gitea.dev/models/db"
	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unit"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/json"
	api "gitea.dev/modules/structs"
	"gitea.dev/modules/timeutil"
	"gitea.dev/modules/util"
	"gitea.dev/modules/web"
	"gitea.dev/routers/api/v1/utils"
	agents_service "gitea.dev/services/agents"
	"gitea.dev/services/context"
	"gitea.dev/services/convert"
)

// RegisterRepoRoutes is called inside the API v1 "/repos/{username}/{reponame}" group
func RegisterRepoRoutes(m *web.Router, reqToken, reqCodeReader, reqPullsReader func(*context.APIContext)) {
	m.Group("/agents/sessions", func() {
		m.Combo("").Get(ListSessions).Post(reqToken, CreateSession)
		m.Group("/{id}", func() {
			m.Get("", GetSession)
			m.Get("/runs", ListRuns)
			m.Get("/events", ListEvents)
			m.Post("/instructions", reqToken, SendInstruction)
			m.Post("/cancel", reqToken, CancelSession)
			m.Post("/resume", reqToken, ResumeSession)
			m.Post("/runs/{run}/retry", reqToken, RetryRun)
		})
	}, reqCodeReader, mustEnable)
	m.Group("/pulls/{index}", func() {
		m.Post("/agent-review", AgentReview)
		m.Post("/agent-merge", AgentMerge)
	}, reqToken, reqCodeReader, reqPullsReader, mustEnable)
}

// RegisterRoutes is called at the root of API v1
func RegisterRoutes(m *web.Router, reqToken func(*context.APIContext)) {
	m.Get("/agents", reqToken, mustEnable, ListAgents)
}

// DenyAgentBots keeps the token of an agent run to git: it carries its trigger user's write access, and Gitea publishes the pull requests and comments of runs itself
func DenyAgentBots(ctx *context.APIContext) {
	if ctx.Doer == nil || !ctx.Doer.IsTypeBot() {
		return
	}
	if _, err := agents_model.GetAgentByBotID(ctx, ctx.Doer.ID); err == nil {
		ctx.APIError(http.StatusForbidden, "the token of an agent run can only be used with git")
	}
}

func mustEnable(ctx *context.APIContext) {
	if !agents_service.IsEnabled() {
		ctx.APIErrorNotFound()
	}
}

func toTime(t timeutil.TimeStamp) *time.Time {
	if t == 0 {
		return nil
	}
	return new(t.AsTime())
}

func toSession(ctx *context.APIContext, s *agents_model.Session) *api.AgentSession {
	session := &api.AgentSession{
		ID:         s.ID,
		Title:      s.Title,
		Kind:       string(s.Kind),
		Status:     string(s.Status),
		Creator:    convert.ToUser(ctx, s.Creator, ctx.Doer),
		Branch:     s.Branch,
		BaseBranch: s.BaseBranch,
		HTMLURL:    fmt.Sprintf("%s/agents/%d", s.Repo.HTMLURL(ctx), s.ID),
		Created:    s.Created.AsTime(),
		Updated:    s.Updated.AsTime(),
	}
	if s.Agent != nil && s.Agent.Bot != nil {
		session.Agent = s.Agent.Bot.Name
	}
	if s.Issue != nil {
		session.IssueNumber = s.Issue.Index
	}
	return session
}

func getSession(ctx *context.APIContext) *agents_model.Session {
	s, err := agents_model.GetRepoSession(ctx, ctx.Repo.Repository.ID, ctx.PathParamInt64("id"))
	if err == nil {
		s.Repo = ctx.Repo.Repository
		err = s.LoadAttributes(ctx)
	}
	if err != nil {
		ctx.APIErrorAuto(err)
		return nil
	}
	if s.Issue != nil && !ctx.Repo.Permission.CanReadIssuesOrPulls(s.Issue.IsPull) {
		ctx.APIErrorNotFound()
		return nil
	}
	return s
}

func decode(ctx *context.APIContext, v any) bool {
	if err := json.NewDecoder(ctx.Req.Body).Decode(v); err != nil {
		ctx.APIError(http.StatusUnprocessableEntity, "invalid JSON body")
		return false
	}
	return true
}

// ListAgents lists the enabled agents
func ListAgents(ctx *context.APIContext) {
	// swagger:operation GET /agents agents agentList
	// ---
	// summary: List the agents available on the instance
	// produces:
	// - application/json
	// responses:
	//   "200":
	//     "$ref": "#/responses/AgentList"
	//   default:
	//     "$ref": "#/responses/error"
	agents, err := agents_model.FindAgents(ctx, true)
	if err != nil {
		ctx.APIErrorInternal(err)
		return
	}
	keys, err := agents_model.FindUserKeys(ctx, ctx.Doer.ID)
	if err != nil {
		ctx.APIErrorInternal(err)
		return
	}
	result := make([]*api.Agent, 0, len(agents))
	for _, agent := range agents {
		model := agent.Model
		if key := keys[agent.ID]; key != nil {
			model = key.EffectiveModel(agent)
		}
		result = append(result, &api.Agent{ID: agent.ID, Name: agent.Name, MentionName: agent.MentionName(), Model: model, HasKey: keys[agent.ID] != nil})
	}
	ctx.JSON(http.StatusOK, result)
}

// ListSessions lists the agent sessions of a repository
func ListSessions(ctx *context.APIContext) {
	// swagger:operation GET /repos/{owner}/{repo}/agents/sessions repository agentListSessions
	// ---
	// summary: List the agent sessions of a repository
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
	// - name: status
	//   in: query
	//   description: only the sessions with this status
	//   type: string
	//   enum: [queued, waiting, running, success, failure, cancelled]
	// - name: kind
	//   in: query
	//   description: only the sessions of this kind
	//   type: string
	//   enum: [task, issue, pull]
	// - name: agent
	//   in: query
	//   description: only the sessions of the agent with this mention name
	//   type: string
	// - name: created_by_me
	//   in: query
	//   description: only the sessions you created
	//   type: boolean
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
	//     "$ref": "#/responses/AgentSessionList"
	//   default:
	//     "$ref": "#/responses/error"
	opts := agents_model.FindSessionsOptions{
		ListOptions: utils.GetListOptions(ctx),
		RepoID:      ctx.Repo.Repository.ID,
		HideIssues:  !ctx.Repo.Permission.CanRead(unit.TypeIssues),
		HidePulls:   !ctx.Repo.Permission.CanRead(unit.TypePullRequests),
		Status:      agents_model.Status(ctx.FormString("status")),
		Kind:        agents_model.SessionKind(ctx.FormString("kind")),
	}
	if (opts.Status != "" && !opts.Status.IsValid()) || (opts.Kind != "" && !opts.Kind.IsValid()) {
		ctx.APIError(http.StatusUnprocessableEntity, "invalid status or kind")
		return
	}
	if name := ctx.FormString("agent"); name != "" {
		agent := findAgent(ctx, name)
		if agent == nil {
			return
		}
		opts.AgentID = agent.ID
	}
	if ctx.FormBool("created_by_me") {
		if ctx.Doer == nil {
			ctx.APIError(http.StatusUnauthorized, "sign in to list your sessions")
			return
		}
		opts.CreatorID = ctx.Doer.ID
	}
	sessions, total, err := db.FindAndCount[agents_model.Session](ctx, opts)
	if err != nil {
		ctx.APIErrorInternal(err)
		return
	}
	result := make([]*api.AgentSession, 0, len(sessions))
	for _, s := range sessions {
		s.Repo = ctx.Repo.Repository
		if err := s.LoadAttributes(ctx); err != nil {
			ctx.APIErrorInternal(err)
			return
		}
		result = append(result, toSession(ctx, s))
	}
	ctx.SetTotalCountHeader(total)
	ctx.JSON(http.StatusOK, result)
}

// CreateSession starts a development session
func CreateSession(ctx *context.APIContext) {
	// swagger:operation POST /repos/{owner}/{repo}/agents/sessions repository agentCreateSession
	// ---
	// summary: Start an agent development session, it runs with your own API key and permissions
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
	// - name: body
	//   in: body
	//   schema:
	//     "$ref": "#/definitions/CreateAgentSessionOption"
	// responses:
	//   "201":
	//     "$ref": "#/responses/AgentSession"
	//   default:
	//     "$ref": "#/responses/error"
	var form api.CreateAgentSessionOption
	if !decode(ctx, &form) {
		return
	}
	agent := findAgent(ctx, form.Agent)
	if agent == nil {
		return
	}
	s, err := agents_service.StartTaskSession(ctx, ctx.Doer, ctx.Repo.Repository, agents_service.StartOptions{
		Agent:           agent,
		Prompt:          form.Prompt,
		BaseBranch:      form.BaseBranch,
		ReasoningEffort: form.ReasoningEffort,
	})
	if err != nil {
		ctx.APIErrorAuto(err)
		return
	}
	ctx.JSON(http.StatusCreated, toSession(ctx, s))
}

// GetSession returns an agent session
func GetSession(ctx *context.APIContext) {
	// swagger:operation GET /repos/{owner}/{repo}/agents/sessions/{id} repository agentGetSession
	// ---
	// summary: Get an agent session
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
	//   description: id of the session
	//   type: integer
	//   format: int64
	//   required: true
	// responses:
	//   "200":
	//     "$ref": "#/responses/AgentSession"
	//   default:
	//     "$ref": "#/responses/error"
	if s := getSession(ctx); s != nil {
		ctx.JSON(http.StatusOK, toSession(ctx, s))
	}
}

// ListRuns lists the runs of an agent session
func ListRuns(ctx *context.APIContext) {
	// swagger:operation GET /repos/{owner}/{repo}/agents/sessions/{id}/runs repository agentListRuns
	// ---
	// summary: List the runs of an agent session
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
	//   description: id of the session
	//   type: integer
	//   format: int64
	//   required: true
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
	//     "$ref": "#/responses/AgentRunList"
	//   default:
	//     "$ref": "#/responses/error"
	s := getSession(ctx)
	if s == nil {
		return
	}
	runs, total, err := db.FindAndCount[agents_model.Run](ctx, agents_model.FindRunsOptions{ListOptions: utils.GetListOptions(ctx), SessionID: s.ID})
	if err != nil {
		ctx.APIErrorInternal(err)
		return
	}
	result := make([]*api.AgentRun, 0, len(runs))
	for _, r := range runs {
		run, err := toRun(ctx, r)
		if err != nil {
			ctx.APIErrorInternal(err)
			return
		}
		result = append(result, run)
	}
	ctx.SetTotalCountHeader(total)
	ctx.JSON(http.StatusOK, result)
}

func toRun(ctx *context.APIContext, r *agents_model.Run) (*api.AgentRun, error) {
	trigger, err := convertUser(ctx, r.TriggerUserID)
	if err != nil {
		return nil, err
	}
	return &api.AgentRun{
		ID:             r.ID,
		Status:         string(r.Status),
		Prompt:         r.Prompt,
		TriggerUser:    trigger,
		Summary:        r.Summary,
		Error:          r.Error,
		ReadOnlyReason: r.ReadOnlyReason,
		Created:        r.Created.AsTime(),
		Started:        toTime(r.Started),
		Stopped:        toTime(r.Stopped),
	}, nil
}

// SendInstruction queues new instructions on a session
func SendInstruction(ctx *context.APIContext) {
	// swagger:operation POST /repos/{owner}/{repo}/agents/sessions/{id}/instructions repository agentSendInstruction
	// ---
	// summary: Send new instructions to an agent session, they run with your own API key and permissions
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
	//   description: id of the session
	//   type: integer
	//   format: int64
	//   required: true
	// - name: body
	//   in: body
	//   schema:
	//     "$ref": "#/definitions/AgentInstructionOption"
	// responses:
	//   "201":
	//     "$ref": "#/responses/AgentRun"
	//   default:
	//     "$ref": "#/responses/error"
	s := getSession(ctx)
	if s == nil {
		return
	}
	var form api.AgentInstructionOption
	if !decode(ctx, &form) {
		return
	}
	r, err := agents_service.SendInstruction(ctx, s, ctx.Doer, form.Prompt)
	if err != nil {
		ctx.APIErrorAuto(err)
		return
	}
	run, err := toRun(ctx, r)
	if err != nil {
		ctx.APIErrorInternal(err)
		return
	}
	ctx.JSON(http.StatusCreated, run)
}

// CancelSession cancels the active and queued runs of a session
func CancelSession(ctx *context.APIContext) {
	// swagger:operation POST /repos/{owner}/{repo}/agents/sessions/{id}/cancel repository agentCancelSession
	// ---
	// summary: Cancel the active and queued runs of an agent session
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
	//   description: id of the session
	//   type: integer
	//   format: int64
	//   required: true
	// responses:
	//   "204":
	//     "$ref": "#/responses/empty"
	//   default:
	//     "$ref": "#/responses/error"
	s := getSession(ctx)
	if s == nil {
		return
	}
	if !agents_service.CanCancel(ctx, s, ctx.Doer, ctx.Repo.Permission) {
		ctx.APIError(http.StatusForbidden, "only code writers and the participants of the session can cancel it")
		return
	}
	if err := agents_service.CancelSession(ctx, s); err != nil {
		if errors.Is(err, util.ErrNotExist) {
			ctx.APIErrorNotFound()
			return
		}
		ctx.APIErrorInternal(err)
		return
	}
	ctx.Status(http.StatusNoContent)
}

func convertUser(ctx *context.APIContext, id int64) (*api.User, error) {
	_, u, err := user_model.GetPossibleUserByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return convert.ToUser(ctx, u, ctx.Doer), nil
}

func runCreated(ctx *context.APIContext, r *agents_model.Run, err error) {
	if err != nil {
		ctx.APIErrorAuto(err)
		return
	}
	run, err := toRun(ctx, r)
	if err != nil {
		ctx.APIErrorInternal(err)
		return
	}
	ctx.JSON(http.StatusCreated, run)
}

// ResumeSession asks the agent to continue the interrupted latest run of a session
func ResumeSession(ctx *context.APIContext) {
	// swagger:operation POST /repos/{owner}/{repo}/agents/sessions/{id}/resume repository agentResumeSession
	// ---
	// summary: Resume an agent session whose latest run, yours, was interrupted, with your own API key and permissions
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
	//   description: id of the session
	//   type: integer
	//   format: int64
	//   required: true
	// responses:
	//   "201":
	//     "$ref": "#/responses/AgentRun"
	//   default:
	//     "$ref": "#/responses/error"
	if s := getSession(ctx); s != nil {
		r, err := agents_service.ResumeSession(ctx, s, ctx.Doer)
		runCreated(ctx, r, err)
	}
}

// RetryRun queues the prompt of one of your finished runs again
func RetryRun(ctx *context.APIContext) {
	// swagger:operation POST /repos/{owner}/{repo}/agents/sessions/{id}/runs/{run}/retry repository agentRetryRun
	// ---
	// summary: Retry one of your finished runs of an agent session, with your own API key and permissions
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
	//   description: id of the session
	//   type: integer
	//   format: int64
	//   required: true
	// - name: run
	//   in: path
	//   description: id of the run
	//   type: integer
	//   format: int64
	//   required: true
	// responses:
	//   "201":
	//     "$ref": "#/responses/AgentRun"
	//   default:
	//     "$ref": "#/responses/error"
	if s := getSession(ctx); s != nil {
		r, err := agents_service.RetryRun(ctx, s, ctx.Doer, ctx.PathParamInt64("run"))
		runCreated(ctx, r, err)
	}
}

// ListEvents lists the Codex events of an agent session
func ListEvents(ctx *context.APIContext) {
	// swagger:operation GET /repos/{owner}/{repo}/agents/sessions/{id}/events repository agentListEvents
	// ---
	// summary: List the Codex events of an agent session, oldest first
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
	//   description: id of the session
	//   type: integer
	//   format: int64
	//   required: true
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
	//     "$ref": "#/responses/AgentEventList"
	//   default:
	//     "$ref": "#/responses/error"
	s := getSession(ctx)
	if s == nil {
		return
	}
	events, total, err := db.FindAndCount[agents_model.Event](ctx, agents_model.FindEventsOptions{ListOptions: utils.GetListOptions(ctx), SessionID: s.ID})
	if err != nil {
		ctx.APIErrorInternal(err)
		return
	}
	result := make([]*api.AgentEvent, 0, len(events))
	for _, e := range events {
		result = append(result, &api.AgentEvent{ID: e.ID, RunID: e.RunID, Type: e.Type, Payload: e.Payload, Created: e.Created.AsTime()})
	}
	ctx.SetTotalCountHeader(total)
	ctx.JSON(http.StatusOK, result)
}

// getPullRequest returns the pull request of the path, nil when it answered
func getPullRequest(ctx *context.APIContext) *issues_model.PullRequest {
	pr, err := issues_model.GetPullRequestByIndex(ctx, ctx.Repo.Repository.ID, ctx.PathParamInt64("index"))
	if err != nil {
		ctx.APIErrorAuto(err)
		return nil
	}
	return pr
}

// findAgent returns the enabled agent of a mention name, nil when it answered
func findAgent(ctx *context.APIContext, name string) *agents_model.Agent {
	name = strings.ToLower(name)
	agents, err := agents_model.GetEnabledAgentsByBotNames(ctx, []string{name})
	if err != nil {
		ctx.APIErrorInternal(err)
		return nil
	}
	if agents[name] == nil {
		ctx.APIErrorNotFound("agent not found")
		return nil
	}
	return agents[name]
}

func sessionCreated(ctx *context.APIContext, s *agents_model.Session, err error) {
	if err == nil {
		s.Repo = ctx.Repo.Repository
		err = s.LoadAttributes(ctx)
	}
	if err != nil {
		ctx.APIErrorAuto(err)
		return
	}
	ctx.JSON(http.StatusCreated, toSession(ctx, s))
}

// AgentReview asks an agent to review a pull request
func AgentReview(ctx *context.APIContext) {
	// swagger:operation POST /repos/{owner}/{repo}/pulls/{index}/agent-review repository agentReviewPullRequest
	// ---
	// summary: Ask an agent to review a pull request, with your own API key and permissions
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
	// - name: index
	//   in: path
	//   description: index of the pull request
	//   type: integer
	//   format: int64
	//   required: true
	// - name: body
	//   in: body
	//   schema:
	//     "$ref": "#/definitions/AgentReviewOption"
	// responses:
	//   "201":
	//     "$ref": "#/responses/AgentSession"
	//   default:
	//     "$ref": "#/responses/error"
	var form api.AgentReviewOption
	if !decode(ctx, &form) {
		return
	}
	pr := getPullRequest(ctx)
	if pr == nil {
		return
	}
	agent := findAgent(ctx, form.Agent)
	if agent == nil {
		return
	}
	s, err := agents_service.StartAgentReview(ctx, ctx.Doer, agent, pr)
	sessionCreated(ctx, s, err)
}

// AgentMerge asks an agent to merge the target branch into a pull request, then schedules its merge
func AgentMerge(ctx *context.APIContext) {
	// swagger:operation POST /repos/{owner}/{repo}/pulls/{index}/agent-merge repository agentMergePullRequest
	// ---
	// summary: Ask an agent to bring a pull request up to date, then to schedule its merge when its checks pass, as you
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
	// - name: index
	//   in: path
	//   description: index of the pull request
	//   type: integer
	//   format: int64
	//   required: true
	// - name: body
	//   in: body
	//   schema:
	//     "$ref": "#/definitions/AgentMergeOption"
	// responses:
	//   "201":
	//     "$ref": "#/responses/AgentSession"
	//   default:
	//     "$ref": "#/responses/error"
	var form api.AgentMergeOption
	if !decode(ctx, &form) {
		return
	}
	pr := getPullRequest(ctx)
	if pr == nil {
		return
	}
	agent := findAgent(ctx, form.Agent)
	if agent == nil {
		return
	}
	style := repo_model.MergeStyle(form.Style)
	if style == "" {
		style = ctx.Repo.Repository.MustGetUnit(ctx, unit.TypePullRequests).PullRequestsConfig().DefaultMergeStyle
	}
	s, err := agents_service.StartAgentMerge(ctx, ctx.Doer, pr, agents_service.MergeOptions{Agent: agent, Style: style, Review: form.Review})
	sessionCreated(ctx, s, err)
}
