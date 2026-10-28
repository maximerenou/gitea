// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	gocontext "context"
	"net/http"
	"time"

	"gitea.dev/modules/graceful"
	"gitea.dev/modules/log"
	agents_service "gitea.dev/services/agents"
	"gitea.dev/services/context"
	"gitea.dev/services/pubsub"

	gitea_ws "github.com/coder/websocket"
)

const livePingInterval = 30 * time.Second

// Live tells the session page when the session changes, the page then fetches the new events
func Live(ctx *context.Context) {
	if s := getSession(ctx); s != nil {
		ServeLive(ctx, agents_service.SessionTopic(s.ID))
	}
}

// LiveList tells the Agents tab when a session of the repository starts or changes status, the tab then reloads its list
func LiveList(ctx *context.Context) {
	ServeLive(ctx, agents_service.RepoTopic(ctx.Repo.Repository.ID))
}

// ServeLive tells a page through a websocket when the pubsub topic is published, the page then fetches what changed
func ServeLive(ctx *context.Context, topic string) {
	if ctx.Req.Header.Get("Upgrade") == "" {
		ctx.Resp.Header().Set("Upgrade", "websocket")
		ctx.Resp.WriteHeader(http.StatusUpgradeRequired)
		return
	}
	conn, err := gitea_ws.Accept(ctx.Resp, ctx.Req, nil)
	if err != nil {
		log.Debug("agents: websocket accept: %v", err)
		return
	}
	defer conn.CloseNow() //nolint:errcheck // best-effort close

	ch, cancel := pubsub.DefaultBroker.Subscribe(topic)
	defer cancel()
	wsCtx := conn.CloseRead(ctx.Req.Context())
	shutdownCtx := graceful.GetManager().ShutdownContext()
	ping := time.NewTicker(livePingInterval)
	defer ping.Stop()
	for {
		select {
		case <-wsCtx.Done():
			return
		case <-shutdownCtx.Done():
			return
		case <-ping.C:
			pingCtx, cancelPing := gocontext.WithTimeout(wsCtx, 10*time.Second)
			err := conn.Ping(pingCtx)
			cancelPing()
			if err != nil {
				return
			}
		case msg, ok := <-ch:
			if !ok {
				return
			}
			writeCtx, cancelWrite := gocontext.WithTimeout(wsCtx, 10*time.Second)
			err := conn.Write(writeCtx, gitea_ws.MessageText, msg)
			cancelWrite()
			if err != nil {
				return
			}
		}
	}
}
