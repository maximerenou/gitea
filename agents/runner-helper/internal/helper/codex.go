// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package helper

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// codexBackend drives the Codex CLI (`codex exec --json`).
//
// UNVERIFIED: this was written from memory of the Codex CLI. The documentation could not be reached while it was
// written (the sandbox's egress policy refused the host), so the option names, the provider configuration keys and the
// shape of the JSON events below must be checked against a real `codex` before relying on it. Everything that depends on
// the CLI is in this file, and codex_test.go pins what is assumed.
type codexBackend struct{}

func (codexBackend) Name() string { return "codex" }

const modelTokenEnv = "AGENT_MODEL_TOKEN"

func (codexBackend) Command(in Input) (*exec.Cmd, error) {
	args := []string{"exec"}
	if in.ConversationID != "" {
		args = append(args, "resume", in.ConversationID)
	}
	args = append(args, "--json", "--skip-git-repo-check", "-C", in.WorkDir, "-m", in.Model, "-s", "workspace-write")

	switch in.Provider {
	case "ollama", "openai-compatible":
		base := strings.TrimRight(in.BaseURL, "/")
		if in.Provider == "ollama" && !strings.HasSuffix(base, "/v1") {
			base += "/v1" // Ollama serves its OpenAI-compatible API under /v1
		}
		args = append(args,
			"-c", `model_provider="agent"`,
			"-c", `model_providers.agent.name="agent"`,
			"-c", "model_providers.agent.base_url="+strconv.Quote(base),
			"-c", `model_providers.agent.wire_api="chat"`,
		)
		if in.Token != "" {
			args = append(args, "-c", `model_providers.agent.env_key="`+modelTokenEnv+`"`)
		}
	default:
		return nil, fmt.Errorf("the codex backend does not support the %q provider", in.Provider)
	}

	args = append(args, in.Prompt)
	cmd := exec.Command("codex", args...)
	cmd.Dir = in.WorkDir
	cmd.Env = os.Environ()
	if in.Token != "" {
		cmd.Env = append(cmd.Env, modelTokenEnv+"="+in.Token)
	}
	return cmd, nil
}

// ParseLine understands the JSON-lines events of `codex exec --json`, in the two shapes the CLI has used.
func (codexBackend) ParseLine(line []byte) []Event {
	var ev struct {
		Type     string `json:"type"`
		ThreadID string `json:"thread_id"`
		Message  string `json:"message"`
		Item     struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"item"`
		Msg struct {
			Type      string `json:"type"`
			Message   string `json:"message"`
			SessionID string `json:"session_id"`
		} `json:"msg"`
	}
	if json.Unmarshal(line, &ev) != nil {
		return nil
	}
	var out []Event
	switch {
	case ev.Type == "thread.started" && ev.ThreadID != "":
		out = append(out, Event{Kind: EventConversationID, Text: ev.ThreadID})
	case ev.Type == "item.completed" && ev.Item.Type == "agent_message":
		out = append(out, Event{Kind: EventMessage, Text: ev.Item.Text})
	case ev.Type == "item.completed" || ev.Type == "item.started":
		out = append(out, Event{Kind: EventTool, Text: ev.Item.Type})
	case ev.Type == "error":
		out = append(out, Event{Kind: EventError, Text: ev.Message})
	case ev.Msg.Type == "agent_message":
		out = append(out, Event{Kind: EventMessage, Text: ev.Msg.Message})
	case ev.Msg.Type == "session_configured" && ev.Msg.SessionID != "":
		out = append(out, Event{Kind: EventConversationID, Text: ev.Msg.SessionID})
	case ev.Msg.Type == "error":
		out = append(out, Event{Kind: EventError, Text: ev.Msg.Message})
	}
	return out
}
