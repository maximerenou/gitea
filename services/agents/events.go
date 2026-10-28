// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"context"
	"fmt"
	"html/template"
	"strings"

	agents_model "gitea.dev/models/agents"
	"gitea.dev/models/renderhelper"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/modules/json"
	"gitea.dev/modules/markup/markdown"
)

// EventView is a Codex event prepared for display, events sharing a Key update the same entry
type EventView struct {
	ID     int64      `json:"id"`
	RunID  int64      `json:"runId"`
	Key    string     `json:"key"`
	Kind   string     `json:"kind"`
	Title  string     `json:"title"`
	Body   string     `json:"body"`
	Status string     `json:"status"`
	Todos  []TodoView `json:"todos,omitempty"`
	// HTML is the rendered Markdown of messages, Diff tells that Body is a unified diff
	HTML template.HTML `json:"html,omitempty"`
	Diff bool          `json:"diff,omitempty"`
}

type TodoView struct {
	Text      string `json:"text"`
	Completed bool   `json:"completed"`
}

type codexEvent struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Message  string `json:"message"`
	Error    *struct {
		Message string `json:"message"`
	} `json:"error"`
	Usage *struct {
		InputTokens       int64 `json:"input_tokens"`
		CachedInputTokens int64 `json:"cached_input_tokens"`
		OutputTokens      int64 `json:"output_tokens"`
	} `json:"usage"`
	Item *struct {
		ID               string `json:"id"`
		Type             string `json:"type"`
		Text             string `json:"text"`
		Command          string `json:"command"`
		AggregatedOutput string `json:"aggregated_output"`
		ExitCode         *int   `json:"exit_code"`
		Status           string `json:"status"`
		Message          string `json:"message"`
		Query            string `json:"query"`
		Server           string `json:"server"`
		Tool             string `json:"tool"`
		Changes          []struct {
			Path string `json:"path"`
			Kind string `json:"kind"`
		} `json:"changes"`
		Items []TodoView `json:"items"`
		Diff  string     `json:"diff"`
	} `json:"item"`
}

// ToEventView maps the structured `codex exec --json` events, unknown ones are skipped
func ToEventView(e *agents_model.Event) *EventView {
	var ev codexEvent
	if err := json.Unmarshal([]byte(e.Payload), &ev); err != nil {
		return nil
	}
	view := &EventView{ID: e.ID, RunID: e.RunID, Key: fmt.Sprintf("event-%d", e.ID)}
	switch ev.Type {
	case "thread.started":
		view.Kind = "thread"
	case "turn.completed":
		view.Kind = "turn"
		if ev.Usage != nil {
			view.Body = fmt.Sprintf("↑ %d (%d cached) · ↓ %d", ev.Usage.InputTokens, ev.Usage.CachedInputTokens, ev.Usage.OutputTokens)
		}
	case "turn.failed":
		view.Kind, view.Status = "error", "failed"
		if ev.Error != nil {
			view.Body = ev.Error.Message
		}
	case "error":
		view.Kind, view.Body, view.Status = "error", ev.Message, "failed"
	case "item.started", "item.updated", "item.completed":
		if ev.Item == nil {
			return nil
		}
		item := ev.Item
		view.Key = fmt.Sprintf("run-%d-%s", e.RunID, item.ID)
		view.Status = item.Status
		if view.Status == "" && ev.Type == "item.completed" {
			view.Status = "completed"
		}
		switch item.Type {
		case "agent_message":
			view.Kind, view.Body = "message", item.Text
		case "reasoning":
			view.Kind, view.Body = "reasoning", item.Text
		case "command_execution":
			view.Kind, view.Title, view.Body = "command", item.Command, item.AggregatedOutput
			if item.ExitCode != nil && *item.ExitCode != 0 {
				view.Status = fmt.Sprintf("exit %d", *item.ExitCode)
			}
		case "file_change":
			view.Kind = "files"
			lines := make([]string, 0, len(item.Changes))
			for _, change := range item.Changes {
				lines = append(lines, change.Kind+" "+change.Path)
			}
			view.Body = strings.Join(lines, "\n")
			if item.Diff != "" {
				view.Body, view.Diff = item.Diff, true
			}
		case "todo_list":
			view.Kind, view.Todos = "plan", item.Items
		case "web_search":
			view.Kind, view.Title = "search", item.Query
		case "mcp_tool_call":
			view.Kind, view.Title = "tool", item.Server+" · "+item.Tool
		case "error":
			view.Kind, view.Body, view.Status = "error", item.Message, "failed"
		default:
			return nil
		}
	default:
		return nil
	}
	return view
}

// RenderEventViews renders the Markdown of Codex messages like comments of the repository
func RenderEventViews(ctx context.Context, repo *repo_model.Repository, views []*EventView) {
	for _, view := range views {
		if (view.Kind == "message" || view.Kind == "reasoning") && view.Body != "" {
			view.HTML, _ = markdown.RenderString(renderhelper.NewRenderContextRepoComment(ctx, repo), view.Body)
		}
	}
}

// RunUsage is the token consumption of a run
type RunUsage struct {
	InputTokens       int64
	CachedInputTokens int64
	OutputTokens      int64
}

func SessionUsage(runs []*agents_model.Run) map[int64]*RunUsage {
	usage := make(map[int64]*RunUsage, len(runs))
	for _, r := range runs {
		if r.InputTokens+r.OutputTokens > 0 {
			usage[r.ID] = &RunUsage{InputTokens: r.InputTokens, CachedInputTokens: r.CachedInputTokens, OutputTokens: r.OutputTokens}
		}
	}
	return usage
}
