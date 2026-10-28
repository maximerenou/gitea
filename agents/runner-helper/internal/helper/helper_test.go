// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package helper

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testToken = "gsess_0123456789012345678901234567890123456789"

type fakeGitea struct {
	t        *testing.T
	srv      *httptest.Server
	mu       sync.Mutex
	rc       RunContext
	cancel   bool // answer "cancelled" to status checks
	progress []progress
	results  [][]byte
	auth     []string
}

func newFakeGitea(t *testing.T, rc RunContext) *fakeGitea {
	f := &fakeGitea{t: t, rc: rc}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/agents/runs/7/context", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		_ = json.NewEncoder(w).Encode(f.rc)
	})
	mux.HandleFunc("/api/v1/agents/runs/7/progress", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		var p progress
		_ = json.NewDecoder(r.Body).Decode(&p)
		f.mu.Lock()
		f.progress = append(f.progress, p)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/v1/agents/runs/7/status", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		f.mu.Lock()
		c := f.cancel
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(runStatus{Status: "running", Cancelled: c})
	})
	mux.HandleFunc("/api/v1/agents/runs/7/result", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.results = append(f.results, body)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(resultResponse{Status: "success", Notes: []string{"a note"}})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitea) record(r *http.Request) {
	f.mu.Lock()
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	f.mu.Unlock()
}

func (f *fakeGitea) text() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var sb strings.Builder
	for _, p := range f.progress {
		sb.WriteString(p.Text)
	}
	return sb.String()
}

func (f *fakeGitea) conversationIDs() (ids []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.progress {
		if p.ConversationID != "" {
			ids = append(ids, p.ConversationID)
		}
	}
	return ids
}

func sh(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// scenario: an origin repository, a workspace checked out from it, a fake gitea and a fake codex on PATH.
type scenario struct {
	t         *testing.T
	origin    string
	env       Env
	gitea     *fakeGitea
	argvFile  string
	envFile   string
	stdout    bytes.Buffer
	pullRefOK bool
}

func newScenario(t *testing.T, codexScript string, mutate func(rc *RunContext)) *scenario {
	t.Helper()
	for _, tool := range []string{"git", "sh"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not available", tool)
		}
	}
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	sh(t, root, "init", "--bare", "-b", "main", origin)
	seed := filepath.Join(root, "seed")
	sh(t, root, "clone", origin, seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sh(t, seed, "add", ".")
	sh(t, seed, "commit", "-m", "init")
	sh(t, seed, "push", "origin", "HEAD:refs/heads/main")
	// a pull request head
	sh(t, seed, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(seed, "feature.txt"), []byte("f\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sh(t, seed, "add", ".")
	sh(t, seed, "commit", "-m", "feature")
	sh(t, seed, "push", "origin", "HEAD:refs/pull/2/head")

	work := filepath.Join(root, "work")
	sh(t, root, "clone", origin, work)

	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	s := &scenario{t: t, origin: origin, argvFile: filepath.Join(root, "argv.txt"), envFile: filepath.Join(root, "env.txt")}
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + s.argvFile + "\nenv > " + s.envFile + "\n" + codexScript
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	rc := RunContext{RunID: 7, SessionID: 3, Action: "manage_pr", Event: "assigned", Branch: "agent/3/work", SystemPrompt: "SYSTEM", Prompt: "TASK", ResultSchema: `{"schema":true}`}
	rc.Agent.Name, rc.Agent.Model, rc.Agent.Provider, rc.Agent.BaseURL, rc.Agent.Token, rc.Agent.CLI = "bot", "llama3", "ollama", "http://ollama:11434", "model-secret", "codex"
	rc.Repository.FullName, rc.Repository.CloneURL, rc.Repository.DefaultBranch = "o/r", origin, "main"
	if mutate != nil {
		mutate(&rc)
	}
	s.gitea = newFakeGitea(t, rc)
	s.env = Env{RunID: 7, APIURL: s.gitea.srv.URL, Token: testToken, Workspace: work, StateDir: filepath.Join(root, "state"), StatusEvery: 1}
	return s
}

func (s *scenario) step(name string) error {
	return Run(context.Background(), name, s.env, &s.stdout, io.Discard)
}

// a codex that talks, commits, and writes its result where the prompt says
const happyCodex = `
dir="$PWD"
path=$(printf '%s\n' "$@" | sed -n 's/.*exact path, not inside the repository: //p' | head -n1)
echo '{"type":"thread.started","thread_id":"thread-42"}'
echo '{"type":"item.completed","item":{"type":"agent_message","text":"looking around"}}'
echo '{"type":"item.completed","item":{"type":"command_execution"}}'
echo change > "$dir/change.txt"
git -C "$dir" add change.txt
GIT_AUTHOR_NAME=a GIT_AUTHOR_EMAIL=a@a GIT_COMMITTER_NAME=a GIT_COMMITTER_EMAIL=a@a git -C "$dir" commit -q -m "agent change"
printf '{"version":"v1","status":"done","summary":"all good"}' > "$path"
`

func TestRunEndToEnd(t *testing.T) {
	s := newScenario(t, happyCodex, nil)
	mainBefore := sh(t, s.origin, "rev-parse", "main")
	for _, step := range []string{"prepare", "exec", "finish"} {
		if err := s.step(step); err != nil {
			t.Fatalf("%s: %v\n%s", step, err, s.stdout.String())
		}
	}
	// the session branch exists on the origin with the agent's commit, nothing else moved
	if got := sh(t, s.origin, "log", "--format=%s", "-n1", "refs/heads/agent/3/work"); got != "agent change" {
		t.Errorf("session branch tip = %q", got)
	}
	if sh(t, s.origin, "rev-parse", "main") != mainBefore {
		t.Error("the default branch must not move")
	}
	if strings.Contains(sh(t, s.origin, "branch", "--list"), "feature") {
		t.Error("unexpected branch on the origin")
	}
	s.gitea.mu.Lock()
	defer s.gitea.mu.Unlock()
	if len(s.gitea.results) != 1 || !strings.Contains(string(s.gitea.results[0]), `"all good"`) {
		t.Fatalf("results = %q", s.gitea.results)
	}
	for _, a := range s.gitea.auth {
		if a != "Bearer "+testToken {
			t.Errorf("request authenticated with %q", a)
		}
	}
	if !strings.Contains(s.gitea.textLocked(), "looking around") {
		t.Errorf("the agent message was not relayed: %q", s.gitea.textLocked())
	}
	if strings.Contains(s.gitea.textLocked(), "command_execution") {
		t.Error("tool events must only show in verbose mode")
	}
	if ids := s.gitea.conversationIDsLocked(); len(ids) != 1 || ids[0] != "thread-42" {
		t.Errorf("conversation ids = %v", ids)
	}

	// the model token reaches the CLI by environment only; the session token never does
	argv, _ := os.ReadFile(s.argvFile)
	env, _ := os.ReadFile(s.envFile)
	if strings.Contains(string(argv), "model-secret") || strings.Contains(string(argv), testToken) {
		t.Error("a token is on the command line")
	}
	if !strings.Contains(string(env), "AGENT_MODEL_TOKEN=model-secret") {
		t.Error("the model token is missing from the CLI environment")
	}
	if !strings.Contains(string(argv), "SYSTEM") || !strings.Contains(string(argv), "TASK") || !strings.Contains(string(argv), `{"schema":true}`) {
		t.Error("the prompt is incomplete")
	}
	if _, err := os.Stat(filepath.Join(s.env.Workspace, "result.json")); err == nil {
		t.Error("result.json must not be written inside the repository")
	}
}

func (f *fakeGitea) textLocked() string {
	var sb strings.Builder
	for _, p := range f.progress {
		sb.WriteString(p.Text)
	}
	return sb.String()
}

func (f *fakeGitea) conversationIDsLocked() (ids []string) {
	for _, p := range f.progress {
		if p.ConversationID != "" {
			ids = append(ids, p.ConversationID)
		}
	}
	return ids
}

func TestReviewIsDetachedAndNeverPushes(t *testing.T) {
	s := newScenario(t, happyCodex, func(rc *RunContext) { rc.Action, rc.CheckoutRef = "review", "refs/pull/2/head" })
	before := sh(t, s.origin, "for-each-ref", "--format=%(refname) %(objectname)")
	for _, step := range []string{"prepare", "exec", "finish"} {
		if err := s.step(step); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
	}
	if head := sh(t, s.env.Workspace, "log", "--format=%s", "-n2"); !strings.Contains(head, "feature") {
		t.Errorf("the pull request head was not checked out: %s", head)
	}
	if after := sh(t, s.origin, "for-each-ref", "--format=%(refname) %(objectname)"); after != before {
		t.Errorf("a review must not push:\nbefore %s\nafter  %s", before, after)
	}
	if len(s.gitea.results) != 1 {
		t.Errorf("results = %d", len(s.gitea.results))
	}
}

func TestCancellationStopsTheAgent(t *testing.T) {
	s := newScenario(t, "echo '{\"type\":\"thread.started\",\"thread_id\":\"t\"}'\nsleep 60\n", nil)
	if err := s.step("prepare"); err != nil {
		t.Fatal(err)
	}
	s.gitea.mu.Lock()
	s.gitea.cancel = true
	s.gitea.mu.Unlock()

	done := make(chan error, 1)
	go func() { done <- s.step("exec") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the agent was not stopped after the run was cancelled")
	}
	if err := s.step("finish"); err != nil {
		t.Fatal(err)
	}
	s.gitea.mu.Lock()
	defer s.gitea.mu.Unlock()
	if len(s.gitea.results) != 0 {
		t.Error("a cancelled run must not post a result")
	}
}

func TestAgentFailureStillReportsAResult(t *testing.T) {
	s := newScenario(t, "echo oops >&2\nexit 3\n", nil)
	for _, step := range []string{"prepare", "exec"} {
		if err := s.step(step); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
	}
	err := s.step("finish")
	if err == nil {
		t.Fatal("finish should fail when the agent produced no result")
	}
	s.gitea.mu.Lock()
	defer s.gitea.mu.Unlock()
	if len(s.gitea.results) != 1 {
		t.Fatalf("results = %d", len(s.gitea.results))
	}
	var res map[string]string
	_ = json.Unmarshal(s.gitea.results[0], &res)
	if res["status"] != "failed" || !strings.Contains(res["summary"], "did not write a result") {
		t.Errorf("result = %v", res)
	}
}

func TestUnsupportedProviderIsReportedNotFatal(t *testing.T) {
	s := newScenario(t, "exit 0\n", func(rc *RunContext) { rc.Agent.Provider = "anthropic" })
	for _, step := range []string{"prepare", "exec"} {
		if err := s.step(step); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
	}
	_ = s.step("finish")
	s.gitea.mu.Lock()
	defer s.gitea.mu.Unlock()
	if len(s.gitea.results) != 1 || !strings.Contains(string(s.gitea.results[0]), "does not support") {
		t.Errorf("results = %q", s.gitea.results)
	}
}

func TestLogEventsOffSendsNoText(t *testing.T) {
	s := newScenario(t, happyCodex, nil)
	s.env.LogEvents = "off"
	for _, step := range []string{"prepare", "exec", "finish"} {
		if err := s.step(step); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
	}
	if txt := s.gitea.text(); txt != "" {
		t.Errorf("text was relayed with AGENT_LOG_EVENTS=off: %q", txt)
	}
	if ids := s.gitea.conversationIDs(); len(ids) != 1 {
		t.Errorf("the conversation id is still needed to resume: %v", ids)
	}
}

func TestMissingConfigurationIsExplained(t *testing.T) {
	for name, env := range map[string]Env{
		"no run id": {APIURL: "http://x", Token: "t"},
		"no url":    {RunID: 1, Token: "t"},
		"no token":  {RunID: 1, APIURL: "http://x"},
	} {
		if err := Run(context.Background(), "prepare", env, io.Discard, io.Discard); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if err := Run(context.Background(), "bogus", Env{RunID: 1, APIURL: "http://x", Token: "t"}, io.Discard, io.Discard); err == nil || !strings.Contains(fmt.Sprint(err), "unknown step") {
		t.Errorf("unknown step: %v", err)
	}
}
