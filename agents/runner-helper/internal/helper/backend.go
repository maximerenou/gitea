// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package helper

import (
	"fmt"
	"os/exec"
)

// EventKind classifies what an agent CLI reported.
type EventKind int

const (
	EventMessage        EventKind = iota // something the agent said: shown in the default log
	EventTool                            // a tool call or command: only shown in verbose mode
	EventError                           // an error reported by the CLI
	EventConversationID                  // the id that lets a later run resume this conversation
)

type Event struct {
	Kind EventKind
	Text string
}

// Input is everything a backend needs to build its command line.
type Input struct {
	WorkDir        string
	Prompt         string // system prompt and task already combined
	ConversationID string // empty for a new conversation
	Model          string
	Provider       string // anthropic, openai-compatible or ollama
	BaseURL        string
	Token          string // never put on the command line: pass it through the environment
}

// Backend adapts one agent CLI: how to start it, how to read its event stream, how to find its conversation id.
type Backend interface {
	Name() string
	// Command builds the process to run; the helper sets its process group and wires its output.
	Command(in Input) (*exec.Cmd, error)
	// ParseLine turns one line of the CLI's output into events; lines that are not events return nothing.
	ParseLine(line []byte) []Event
}

var backends = map[string]func() Backend{
	"codex": func() Backend { return codexBackend{} },
}

func backendFor(name string) (Backend, error) {
	mk, ok := backends[name]
	if !ok {
		return nil, fmt.Errorf("unknown agent CLI %q", name)
	}
	return mk(), nil
}
