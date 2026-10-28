// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"bytes"
	"context"
	"io"
	"sync"
	"time"

	agents_model "gitea.dev/models/agents"
	"gitea.dev/modules/json"
	"gitea.dev/modules/log"
)

func monitorModelRun(ctx context.Context, cancel context.CancelFunc, runID int64, finished chan<- struct{}) {
	defer close(finished)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run, err := agents_model.GetRunByID(ctx, runID)
			if err != nil || !run.Status.IsActive() {
				cancel()
				return
			}
		}
	}
}

const maxModelUsageEventSize = 8 * 1024 * 1024

type modelResponseBody struct {
	io.ReadCloser
	ctx      context.Context
	runID    int64
	stream   bool
	cleanup  func()
	once     sync.Once
	pending  []byte
	overflow bool
	recorded bool
}

func (b *modelResponseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if b.stream {
		remaining := p[:n]
		for len(remaining) > 0 {
			line, rest, ended := bytes.Cut(remaining, []byte{'\n'})
			b.appendUsage(line)
			if ended {
				if !b.overflow {
					if data, ok := bytes.CutPrefix(b.pending, []byte("data:")); ok {
						b.recordUsage(data)
					}
				}
				b.pending, b.overflow = b.pending[:0], false
			}
			remaining = rest
		}
	} else {
		b.appendUsage(p[:n])
		if err == io.EOF && !b.overflow {
			b.recordUsage(b.pending)
		}
	}
	return n, err
}

func (b *modelResponseBody) appendUsage(data []byte) {
	if b.overflow {
		return
	}
	if len(b.pending)+len(data) > maxModelUsageEventSize {
		b.pending, b.overflow = nil, true
		return
	}
	b.pending = append(b.pending, data...)
}

func (b *modelResponseBody) recordUsage(data []byte) {
	if b.recorded {
		return
	}
	type usage struct {
		InputTokens        int64 `json:"input_tokens"`
		OutputTokens       int64 `json:"output_tokens"`
		InputTokensDetails struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"input_tokens_details"`
	}
	var event struct {
		Type     string `json:"type"`
		Usage    *usage `json:"usage"`
		Response *struct {
			Usage *usage `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(data, &event) != nil {
		return
	}
	if b.stream {
		if event.Type != "response.completed" && event.Type != "response.incomplete" && event.Type != "response.failed" {
			return
		}
		if event.Response == nil {
			return
		}
		event.Usage = event.Response.Usage
	}
	if u := event.Usage; u != nil {
		b.recorded = true
		input, output := max(u.InputTokens, 0), max(u.OutputTokens, 0)
		cached := min(max(u.InputTokensDetails.CachedTokens, 0), input)
		ctx, cancel := context.WithTimeout(b.ctx, 5*time.Second)
		defer cancel()
		if err := agents_model.AddRunUsage(ctx, b.runID, input, cached, output); err != nil {
			log.Error("agents: record model usage of run %d: %v", b.runID, err)
		}
	}
}

func (b *modelResponseBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.cleanup)
	return err
}
