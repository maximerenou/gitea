// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package agents serves the API the agent driver uses from inside its Gitea Actions job
package agents

import (
	"bufio"
	"errors"
	"io"
	"net/http"
	"strings"

	actions_model "gitea.dev/models/actions"
	agents_model "gitea.dev/models/agents"
	"gitea.dev/modules/json"
	"gitea.dev/modules/log"
	"gitea.dev/modules/reqctx"
	"gitea.dev/modules/util"
	"gitea.dev/modules/web"
	"gitea.dev/modules/web/routing"
	web_types "gitea.dev/modules/web/types"
	agents_service "gitea.dev/services/agents"
	"gitea.dev/services/context"

	"github.com/go-chi/chi/v5"
)

type JobContext struct {
	*context.Base
	Run *agents_model.Run
}

type jobContextKeyType struct{}

var jobContextKey = jobContextKeyType{}

func init() {
	web.RegisterResponseStatusProvider[*JobContext](func(req *http.Request) web_types.ResponseStatusProvider {
		return reqctx.MustContextValue[*JobContext](req.Context(), jobContextKey)
	})
}

// jobContexter authenticates the running task by its Actions token, only the first task of an agent run is accepted
func jobContexter() func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
			ctx := &JobContext{Base: context.NewBaseContext(resp, req)}
			ctx.SetContextValue(jobContextKey, ctx)

			token, ok := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
			if !ok {
				ctx.HTTPError(http.StatusUnauthorized, "bad authorization header")
				return
			}
			task, err := actions_model.GetRunningTaskByToken(ctx, token)
			if err != nil {
				ctx.HTTPError(http.StatusUnauthorized, "invalid task token")
				return
			}
			ctx.Run, err = agents_model.GetRunOfTask(ctx, task)
			if err != nil {
				if !errors.Is(err, util.ErrNotExist) && !errors.Is(err, util.ErrPermissionDenied) {
					log.Error("agents: run of task %d: %v", task.ID, err)
				}
				ctx.HTTPError(http.StatusForbidden, "the task is not an agent run")
				return
			}
			next.ServeHTTP(ctx.Resp, ctx.Req)
		})
	}
}

func JobRoutes() *web.Router {
	m := web.NewRouter()
	m.Methods("GET,POST", "/model/*", routing.MarkLongPolling(), modelProxy)
	m.Group("", func() {
		m.Get("/driver.mjs", driverScript)
		m.Get("/run", runContext)
		m.Post("/run/events", appendEvents)
		m.Post("/run/publish", publish)
		m.Post("/run/finish", finish)
		m.Get("/run/state", getState)
		m.Put("/run/state", putState)
	}, jobContexter())
	return m
}

const maxModelRequestSize = 64 * 1024 * 1024

// modelProxy lets Codex reach the model through Gitea with its run's model token, Gitea adds the trigger user's key
func modelProxy(http.Handler) http.Handler {
	return http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		token, ok := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
		if !ok {
			http.Error(resp, "bad authorization header", http.StatusUnauthorized)
			return
		}
		r, err := agents_model.GetActiveRunByModelToken(req.Context(), token)
		if err != nil {
			http.Error(resp, "invalid model token", http.StatusUnauthorized)
			return
		}
		upstream, err := agents_service.ProxyModel(req.Context(), r, req.Method, chi.URLParam(req, "*"), req.Header, http.MaxBytesReader(resp, req.Body, maxModelRequestSize))
		if err != nil {
			if errors.Is(err, util.ErrInvalidArgument) || errors.Is(err, util.ErrPermissionDenied) || errors.Is(err, util.ErrNotExist) {
				http.Error(resp, err.Error(), http.StatusForbidden)
			} else {
				log.Warn("agents: model request of run %d: %v", r.ID, err)
				http.Error(resp, "the model endpoint cannot be reached", http.StatusBadGateway)
			}
			return
		}
		defer upstream.Body.Close()
		for name, values := range upstream.Header {
			switch http.CanonicalHeaderKey(name) {
			case "Connection", "Keep-Alive", "Transfer-Encoding", "Upgrade", "Set-Cookie", "Content-Length":
				continue
			}
			resp.Header()[name] = values
		}
		resp.WriteHeader(upstream.StatusCode)
		// the Responses API streams Server-Sent Events, each chunk must reach Codex at once
		rc := http.NewResponseController(resp)
		buf := make([]byte, 32*1024)
		for {
			n, err := upstream.Body.Read(buf)
			if n > 0 {
				if _, werr := resp.Write(buf[:n]); werr != nil {
					return
				}
				_ = rc.Flush()
			}
			if err != nil {
				return
			}
		}
	})
}

func respondError(ctx *JobContext, err error) {
	switch {
	case errors.Is(err, util.ErrInvalidArgument):
		ctx.HTTPError(http.StatusBadRequest, err.Error())
	case errors.Is(err, util.ErrNotExist):
		ctx.HTTPError(http.StatusNotFound, err.Error())
	case errors.Is(err, util.ErrPermissionDenied):
		ctx.HTTPError(http.StatusForbidden, err.Error())
	default:
		log.Error("agents: run %d: %v", ctx.Run.ID, err)
		ctx.HTTPError(http.StatusInternalServerError, "internal error")
	}
}

func driverScript(ctx *JobContext) {
	ctx.Resp.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	ctx.Resp.WriteHeader(http.StatusOK)
	_, _ = ctx.Resp.Write(agents_service.DriverScript)
}

func runContext(ctx *JobContext) {
	runCtx, err := agents_service.GetRunContext(ctx, ctx.Run)
	if err != nil {
		respondError(ctx, err)
		return
	}
	ctx.JSON(http.StatusOK, runCtx)
}

func appendEvents(ctx *JobContext) {
	scanner := bufio.NewScanner(io.LimitReader(ctx.Req.Body, 8*1024*1024))
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	var lines []string
	for scanner.Scan() {
		if line := scanner.Text(); strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	if err := scanner.Err(); err != nil {
		ctx.HTTPError(http.StatusBadRequest, err.Error())
		return
	}
	if err := agents_service.AppendEvents(ctx, ctx.Run, lines); err != nil {
		respondError(ctx, err)
		return
	}
	ctx.Status(http.StatusNoContent)
}

func decodeJSON(ctx *JobContext, v any) bool {
	if err := json.NewDecoder(io.LimitReader(ctx.Req.Body, 1024*1024)).Decode(v); err != nil {
		ctx.HTTPError(http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

func publish(ctx *JobContext) {
	var opts agents_service.PublishOptions
	if !decodeJSON(ctx, &opts) {
		return
	}
	pullURL, err := agents_service.Publish(ctx, ctx.Run, opts, ctx.Req.URL.Query().Get("final") == "1")
	if err != nil {
		respondError(ctx, err)
		return
	}
	ctx.JSON(http.StatusOK, map[string]string{"pull_url": pullURL})
}

func finish(ctx *JobContext) {
	var opts agents_service.FinishOptions
	if !decodeJSON(ctx, &opts) {
		return
	}
	if err := agents_service.Finish(ctx, ctx.Run, opts); err != nil {
		respondError(ctx, err)
		return
	}
	ctx.Status(http.StatusNoContent)
}

func getState(ctx *JobContext) {
	obj, err := agents_service.OpenState(ctx.Run)
	if err != nil {
		ctx.HTTPError(http.StatusNotFound, "no saved state")
		return
	}
	defer obj.Close()
	ctx.Resp.Header().Set("Content-Type", "application/gzip")
	ctx.Resp.WriteHeader(http.StatusOK)
	if _, err := io.Copy(ctx.Resp, obj); err != nil {
		log.Error("agents: send state of run %d: %v", ctx.Run.ID, err)
	}
}

func putState(ctx *JobContext) {
	if err := agents_service.SaveState(ctx, ctx.Run, ctx.Req.Body, ctx.Req.ContentLength); err != nil {
		respondError(ctx, err)
		return
	}
	ctx.Status(http.StatusNoContent)
}
