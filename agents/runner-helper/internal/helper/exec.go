// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package helper

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	flushEvery     = 2 * time.Second
	flushSize      = 4 * 1024
	terminateGrace = 10 * time.Second
)

// execAgent runs the agent CLI, relays its progress and stops it when the run is cancelled or its token dies.
// A failing agent is not an error here: finish reports it.
func execAgent(ctx context.Context, e Env, stdout, stderr io.Writer) error {
	rc, st, err := e.loadAll()
	if err != nil {
		return err
	}
	prompt, err := os.ReadFile(e.path("prompt.txt"))
	if err != nil {
		return err
	}
	backend, err := backendFor(rc.Agent.CLI)
	if err != nil {
		return markAgentError(e, st, err)
	}
	cmd, err := backend.Command(Input{
		WorkDir: e.Workspace, Prompt: string(prompt), ConversationID: st.ConversationID, Model: rc.Agent.Model,
		Provider: rc.Agent.Provider, BaseURL: rc.Agent.BaseURL, Token: rc.Agent.Token,
	})
	if err != nil {
		return markAgentError(e, st, err)
	}
	cmd.Cancel = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // so the whole tree can be stopped
	cmd.Stderr = stderr
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	api := newClient(e)
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	out := newRelay(runCtx, api, e.logLevel())
	if err := cmd.Start(); err != nil {
		return markAgentError(e, st, err)
	}
	st.AgentRan = true

	var cancelled bool
	var mu sync.Mutex
	stop := func() {
		mu.Lock()
		cancelled = true
		mu.Unlock()
		terminate(cmd)
	}
	go func() { // cancellation and token death check
		t := time.NewTicker(time.Duration(e.statusEvery()) * time.Second)
		defer t.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-t.C:
				s, err := api.status(runCtx)
				var apiErr *APIError
				if (err == nil && s.Cancelled) || (errors.As(err, &apiErr) && apiErr.RunEnded()) {
					fmt.Fprintln(stdout, "the run was cancelled or ended, stopping the agent")
					stop()
					return
				}
			}
		}
	}()
	go func() { // the job itself being cancelled
		<-ctx.Done()
		stop()
	}()

	sc := bufio.NewScanner(pipe)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		line := sc.Bytes()
		events := backend.ParseLine(line)
		for _, ev := range events {
			if ev.Kind == EventConversationID && st.ConversationID != ev.Text {
				st.ConversationID = ev.Text
				out.conversation(ev.Text)
			}
			out.event(ev)
		}
		if len(events) == 0 && e.logLevel() == "verbose" {
			out.text(string(line) + "\n")
		}
	}
	waitErr := cmd.Wait()
	out.close()

	mu.Lock()
	st.Cancelled = cancelled
	mu.Unlock()
	if waitErr != nil {
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			st.AgentExit = ee.ExitCode()
		} else {
			st.AgentExit = -1
		}
		st.AgentError = waitErr.Error()
	}
	fmt.Fprintf(stdout, "agent exited with code %d (cancelled: %v)\n", st.AgentExit, st.Cancelled)
	return e.writeJSON("state.json", st)
}

func markAgentError(e Env, st *state, cause error) error {
	st.AgentExit, st.AgentError = -1, cause.Error()
	if err := e.writeJSON("state.json", st); err != nil {
		return err
	}
	return nil // reported by finish, so that the run still gets a result
}

// terminate asks the process group to stop, then forces it.
func terminate(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	pgid := -cmd.Process.Pid
	_ = syscall.Kill(pgid, syscall.SIGTERM)
	time.AfterFunc(terminateGrace, func() { _ = syscall.Kill(pgid, syscall.SIGKILL) })
}

// relay batches agent output into progress posts; what it sends depends on AGENT_LOG_EVENTS.
type relay struct {
	api   *client
	level string
	ctx   context.Context

	mu   sync.Mutex
	buf  strings.Builder
	done chan struct{}
	wg   sync.WaitGroup
}

func newRelay(ctx context.Context, api *client, level string) *relay {
	r := &relay{api: api, level: level, ctx: ctx, done: make(chan struct{})}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		t := time.NewTicker(flushEvery)
		defer t.Stop()
		for {
			select {
			case <-r.done:
				return
			case <-t.C:
				r.flush()
			}
		}
	}()
	return r
}

func (r *relay) event(ev Event) {
	switch ev.Kind {
	case EventMessage:
		r.text(ev.Text + "\n")
	case EventError:
		r.text("error: " + ev.Text + "\n")
	case EventTool:
		if r.level == "verbose" {
			r.text("· " + ev.Text + "\n")
		}
	}
}

func (r *relay) text(s string) {
	if r.level == "off" {
		return
	}
	r.mu.Lock()
	r.buf.WriteString(s)
	big := r.buf.Len() >= flushSize
	r.mu.Unlock()
	if big {
		r.flush()
	}
}

func (r *relay) conversation(id string) {
	_ = r.api.progress(r.ctx, progress{ConversationID: id}) // best effort: a lost id only costs the resume
}

func (r *relay) flush() {
	r.mu.Lock()
	s := r.buf.String()
	r.buf.Reset()
	r.mu.Unlock()
	if s != "" {
		_ = r.api.progress(r.ctx, progress{Text: s})
	}
}

func (r *relay) close() {
	close(r.done)
	r.wg.Wait()
	r.flush()
}
