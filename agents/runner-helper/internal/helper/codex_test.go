// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package helper

import (
	"strings"
	"testing"
)

// These pin what the Codex backend assumes about the CLI; they cannot prove the CLI behaves that way.
func TestCodexParseLine(t *testing.T) {
	b := codexBackend{}
	cases := []struct {
		line string
		want []Event
	}{
		{`{"type":"thread.started","thread_id":"abc"}`, []Event{{EventConversationID, "abc"}}},
		{`{"type":"item.completed","item":{"type":"agent_message","text":"hello"}}`, []Event{{EventMessage, "hello"}}},
		{`{"type":"item.completed","item":{"type":"command_execution"}}`, []Event{{EventTool, "command_execution"}}},
		{`{"type":"error","message":"boom"}`, []Event{{EventError, "boom"}}},
		{`{"msg":{"type":"agent_message","message":"old shape"}}`, []Event{{EventMessage, "old shape"}}},
		{`{"msg":{"type":"session_configured","session_id":"s1"}}`, []Event{{EventConversationID, "s1"}}},
		{`not json at all`, nil},
		{`{"type":"turn.completed"}`, nil},
	}
	for _, c := range cases {
		got := b.ParseLine([]byte(c.line))
		if len(got) != len(c.want) || (len(got) == 1 && got[0] != c.want[0]) {
			t.Errorf("%s: got %v, want %v", c.line, got, c.want)
		}
	}
}

func TestCodexCommand(t *testing.T) {
	b := codexBackend{}
	cmd, err := b.Command(Input{WorkDir: "/w", Prompt: "do it", Model: "llama3", Provider: "ollama", BaseURL: "http://ollama:11434", Token: "s3cret-token"})
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(cmd.Args, " ")
	for _, want := range []string{"exec", "--json", "-m llama3", `base_url="http://ollama:11434/v1"`, "do it"} {
		if !strings.Contains(args, want) {
			t.Errorf("missing %q in %s", want, args)
		}
	}
	if strings.Contains(args, "s3cret-token") {
		t.Error("the model token must not be on the command line")
	}
	if !strings.Contains(strings.Join(cmd.Env, "\n"), modelTokenEnv+"=s3cret-token") {
		t.Error("the model token must be passed through the environment")
	}
	if strings.Contains(args, "resume") {
		t.Error("a new conversation must not resume")
	}

	cmd, err = b.Command(Input{WorkDir: "/w", Prompt: "p", Model: "m", Provider: "openai-compatible", BaseURL: "https://api.example/v1/", ConversationID: "thread-9"})
	if err != nil {
		t.Fatal(err)
	}
	args = strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "exec resume thread-9") || !strings.Contains(args, `base_url="https://api.example/v1"`) {
		t.Errorf("unexpected command: %s", args)
	}

	if _, err := b.Command(Input{Provider: "anthropic"}); err == nil {
		t.Error("the anthropic provider is not supported by this backend and must say so")
	}
}

func TestBackendRegistry(t *testing.T) {
	if _, err := backendFor("codex"); err != nil {
		t.Error(err)
	}
	if _, err := backendFor("nope"); err == nil {
		t.Error("unknown CLIs must be refused")
	}
}
