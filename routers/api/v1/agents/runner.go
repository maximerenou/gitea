// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package agents holds the API used by the runner helper of an agent run. It only accepts the session token of that run.
package agents

import (
	"errors"
	"io"
	"net/http"

	agents_model "gitea.dev/models/agents"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/agents"
	"gitea.dev/modules/json"
	api "gitea.dev/modules/structs"
	"gitea.dev/modules/util"
	"gitea.dev/modules/web"
	"gitea.dev/services/context"

	agents_service "gitea.dev/services/agents"
)

const runDataKey = "AgentRun"

// RegisterRoutes adds the runner routes. They answer 404 while agents are disabled.
func RegisterRoutes(m *web.Router) {
	m.Group("/agents/runs/{id}", func() {
		m.Get("/context", GetContext)
		m.Post("/progress", PostProgress)
		m.Get("/status", GetStatus)
		m.Post("/result", PostResult)
	}, requireEnabled, requireRunToken)
}

func requireEnabled(ctx *context.APIContext) {
	if !agents.IsEnabled(ctx) {
		ctx.APIErrorNotFound()
	}
}

// requireRunToken accepts nothing but the session token of the run named in the URL.
// A token of another run, a personal token or an Actions token get the same 404 as an unknown run.
func requireRunToken(ctx *context.APIContext) {
	if !ctx.IsSigned {
		ctx.APIError(http.StatusUnauthorized, "authentication is required")
		return
	}
	sessionID, _, ok := user_model.GetDoerSessionToken(ctx.Doer)
	if !ok {
		ctx.APIError(http.StatusForbidden, "an agent session token is required")
		return
	}
	run, err := agents_model.GetRunByID(ctx, ctx.PathParamInt64("id"))
	if err != nil || run.SessionID != sessionID {
		if err != nil && !errors.Is(err, util.ErrNotExist) {
			ctx.APIErrorInternal(err)
		} else {
			ctx.APIErrorNotFound()
		}
		return
	}
	ctx.Data[runDataKey] = run
}

func runOf(ctx *context.APIContext) *agents_model.Run {
	return ctx.Data[runDataKey].(*agents_model.Run)
}

// GetContext returns what the helper needs to run the agent
func GetContext(ctx *context.APIContext) {
	// swagger:operation GET /agents/runs/{id}/context agents agentRunGetContext
	// ---
	// summary: Get the context of an agent run
	// description: Only accepts the session token of that run.
	// produces:
	// - application/json
	// parameters:
	// - name: id
	//   in: path
	//   description: id of the agent run
	//   type: integer
	//   format: int64
	//   required: true
	// responses:
	//   "200":
	//     "$ref": "#/responses/AgentRunContext"
	//   "403":
	//     "$ref": "#/responses/forbidden"
	//   "404":
	//     "$ref": "#/responses/notFound"
	run := runOf(ctx)
	if run.Status.IsTerminal() {
		ctx.APIError(http.StatusConflict, "the run has ended")
		return
	}
	rc, err := agents_service.BuildRunContext(ctx, run)
	if err != nil {
		ctx.APIErrorInternal(err)
		return
	}
	ctx.JSON(http.StatusOK, rc)
}

// PostProgress records a piece of agent output
func PostProgress(ctx *context.APIContext) {
	// swagger:operation POST /agents/runs/{id}/progress agents agentRunPostProgress
	// ---
	// summary: Report the progress of an agent run
	// consumes:
	// - application/json
	// parameters:
	// - name: id
	//   in: path
	//   description: id of the agent run
	//   type: integer
	//   format: int64
	//   required: true
	// - name: body
	//   in: body
	//   schema:
	//     "$ref": "#/definitions/AgentRunProgressOption"
	// responses:
	//   "204":
	//     "$ref": "#/responses/empty"
	//   "403":
	//     "$ref": "#/responses/forbidden"
	//   "404":
	//     "$ref": "#/responses/notFound"
	//   "409":
	//     "$ref": "#/responses/conflict"
	run := runOf(ctx)
	if run.Status.IsTerminal() {
		ctx.APIError(http.StatusConflict, "the run has ended")
		return
	}
	opt := &api.AgentRunProgressOption{}
	if err := json.NewDecoder(io.LimitReader(ctx.Req.Body, 2*agents_model.MaxProgressLength)).Decode(opt); err != nil {
		ctx.APIError(http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err := agents_model.AppendProgress(ctx, run.ID, util.TruncateRunes(opt.Text, agents_model.MaxProgressLength)); err != nil {
		ctx.APIErrorInternal(err)
		return
	}
	ctx.Status(http.StatusNoContent)
}

// GetStatus tells the helper whether the run was cancelled
func GetStatus(ctx *context.APIContext) {
	// swagger:operation GET /agents/runs/{id}/status agents agentRunGetStatus
	// ---
	// summary: Get the status of an agent run
	// produces:
	// - application/json
	// parameters:
	// - name: id
	//   in: path
	//   description: id of the agent run
	//   type: integer
	//   format: int64
	//   required: true
	// responses:
	//   "200":
	//     "$ref": "#/responses/AgentRunStatus"
	//   "403":
	//     "$ref": "#/responses/forbidden"
	//   "404":
	//     "$ref": "#/responses/notFound"
	run := runOf(ctx)
	ctx.JSON(http.StatusOK, &api.AgentRunStatus{Status: string(run.Status), Cancelled: run.Status == agents_model.RunCancelled})
}

// PostResult receives result.json, applies it and ends the run
func PostResult(ctx *context.APIContext) {
	// swagger:operation POST /agents/runs/{id}/result agents agentRunPostResult
	// ---
	// summary: Submit the result of an agent run
	// consumes:
	// - application/json
	// produces:
	// - application/json
	// parameters:
	// - name: id
	//   in: path
	//   description: id of the agent run
	//   type: integer
	//   format: int64
	//   required: true
	// - name: body
	//   in: body
	//   required: true
	//   schema:
	//     "$ref": "#/definitions/AgentRunResultOption"
	// responses:
	//   "200":
	//     "$ref": "#/responses/AgentRunResultResponse"
	//   "403":
	//     "$ref": "#/responses/forbidden"
	//   "404":
	//     "$ref": "#/responses/notFound"
	//   "409":
	//     "$ref": "#/responses/conflict"
	//   "422":
	//     "$ref": "#/responses/validationError"
	run := runOf(ctx)
	raw, err := io.ReadAll(io.LimitReader(ctx.Req.Body, agents.MaxResultSize+1))
	if err != nil {
		ctx.APIError(http.StatusBadRequest, err.Error())
		return
	}
	notes, err := agents_service.SubmitResult(ctx, run, raw)
	switch {
	case errors.Is(err, agents_service.ErrResultAlreadySubmitted):
		ctx.APIError(http.StatusConflict, err.Error())
	case err != nil && notes == nil && isInvalidResult(err):
		ctx.APIError(http.StatusUnprocessableEntity, err.Error())
	case err != nil:
		ctx.APIErrorInternal(err)
	default:
		got, getErr := agents_model.GetRunByID(ctx, run.ID)
		if getErr != nil {
			ctx.APIErrorInternal(getErr)
			return
		}
		ctx.JSON(http.StatusOK, &api.AgentRunResultResponse{Status: string(got.Status), Notes: notes})
	}
}

// isInvalidResult tells a result the agent got wrong (the helper should report it) from a failure on our side
func isInvalidResult(err error) bool {
	return err != nil && (errors.Is(err, agents.ErrInvalidResult))
}
