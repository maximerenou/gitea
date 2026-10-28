// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	actions_model "gitea.dev/models/actions"
	agents_model "gitea.dev/models/agents"
	auth_model "gitea.dev/models/auth"
	"gitea.dev/models/db"
	git_model "gitea.dev/models/git"
	issues_model "gitea.dev/models/issues"
	pull_model "gitea.dev/models/pull"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/commitstatus"
	"gitea.dev/modules/git"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/storage"
	"gitea.dev/modules/test"
	"gitea.dev/modules/timeutil"
	"gitea.dev/modules/util"
	actions_service "gitea.dev/services/actions"
	"gitea.dev/services/automergequeue"
	pull_service "gitea.dev/services/pull"
	user_service "gitea.dev/services/user"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"
)

func TestRepoSetup(t *testing.T) {
	setup, err := ParseRepoSetup(`
steps:
  - uses: shivammathur/setup-php@v2
    with:
      php-version: "8.3"
  - run: composer install
mcp_servers:
  docs:
    command: npx
    args: [-y, docs-mcp]
    env: {LEVEL: debug}
`)
	require.NoError(t, err)
	assert.Len(t, setup.Steps, 2)
	assert.Equal(t, "npx", setup.MCPServers["docs"]["command"])

	for name, content := range map[string]string{
		"expressions could read the job's secrets": "steps:\n  - run: echo ${{ secrets.GITEA_AGENT_API_KEY }}\n",
		"escaped expressions too":                  "steps:\n  - run: \"echo $\\x7B{ secrets.GITEA_AGENT_API_KEY }}\"\n",
		"expressions in keys":                      "steps:\n  - run: echo\n    env:\n      \"$\\x7B{ secrets.X }}\": a\n",
		"unknown step keys":                        "steps:\n  - run: x\n    services: {}\n",
		"a step needs run or uses":                 "steps:\n  - name: nothing\n",
		"MCP servers need a command or a url":      "mcp_servers:\n  bad: {args: [x]}\n",
		"MCP server values are typed":              "mcp_servers:\n  bad: {command: x, env: {A: [1]}}\n",
	} {
		_, err := ParseRepoSetup(content)
		assert.Error(t, err, name)
	}

	agent, err := ParseMCPServers("docs:\n  url: https://admin.example.com/mcp\n")
	require.NoError(t, err)
	merged := mergeMCPServers(agent, setup.MCPServers)
	assert.Equal(t, "https://admin.example.com/mcp", merged["docs"]["url"], "the administrator's servers win over the repository's")
}

func TestWorkflowSteps(t *testing.T) {
	defer test.MockVariableValue(&setting.Agents.ContainerImage, "codex-runner:latest")()
	defer test.MockVariableValue(&setting.Agents.Firewall, true)()
	setup, err := ParseRepoSetup("steps:\n  - run: composer install\n")
	require.NoError(t, err)
	content, err := buildWorkflow(setup)
	require.NoError(t, err)

	var wf struct {
		Jobs map[string]struct {
			Container map[string]string `yaml:"container"`
			Steps     []struct {
				Name  string            `yaml:"name"`
				Run   string            `yaml:"run"`
				Shell string            `yaml:"shell"`
				Env   map[string]string `yaml:"env"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	require.NoError(t, yaml.Unmarshal(content, &wf))
	job := wf.Jobs["codex"]
	require.Len(t, job.Steps, 3)
	assert.Equal(t, "Check out", job.Steps[0].Name)
	assert.Equal(t, "composer install", job.Steps[1].Run)
	assert.Equal(t, setupShell+" bash 0 {0}", job.Steps[1].Shell, "behind the firewall, setup steps run unprivileged")
	assert.Equal(t, "Codex", job.Steps[2].Name)
	assert.NotContains(t, job.Steps[0].Env, agents_model.APIKeySecretName, "the API key never reaches the steps before Codex")
	assert.Contains(t, job.Steps[2].Env, agents_model.APIKeySecretName)
	assert.Contains(t, job.Container["options"], "--cap-add=NET_ADMIN")

	withEnv, err := ParseRepoSetup("steps:\n  - run: make\n    shell: sh\n    timeout-minutes: 5\n    env:\n      LD_PRELOAD: /home/x.so\n      MSG: \"it's\"\n")
	require.NoError(t, err)
	content, err = buildWorkflow(withEnv)
	require.NoError(t, err)
	assert.NotContains(t, string(content), "timeout-minutes: 5", "act would leave the step's processes running")
	require.NoError(t, yaml.Unmarshal(content, &wf))
	step := wf.Jobs["codex"].Steps[1]
	assert.Empty(t, step.Env, "the env of a step never reaches the shell running as root")
	assert.Equal(t, "export LD_PRELOAD='/home/x.so' MSG='it'\\''s'\nmake", step.Run)
	assert.Equal(t, setupShell+" sh 5 {0}", step.Shell)
	for _, invalid := range []string{"timeout-minutes: 0.5", "env: [A]", "env:\n      A-B: x"} {
		setup, err := ParseRepoSetup("steps:\n  - run: make\n    " + invalid + "\n")
		require.NoError(t, err)
		_, err = buildWorkflow(setup)
		require.ErrorIs(t, err, util.ErrInvalidArgument, invalid)
	}

	actions, err := ParseRepoSetup("steps:\n  - uses: actions/setup-go@v5\n")
	require.NoError(t, err)
	_, err = buildWorkflow(actions)
	require.ErrorContains(t, err, "SETUP_AS_ROOT", "actions run as root")
	defer test.MockVariableValue(&setting.Agents.SetupAsRoot, true)()
	_, err = buildWorkflow(actions)
	require.NoError(t, err)
}

func TestReasoningEffort(t *testing.T) {
	prepare(t)
	alice := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	codex := newAgent(t, "codex")
	setKey(t, codex, alice, "sk-alice")
	require.NoError(t, UpdateAgent(t.Context(), codex, AgentOptions{Name: "codex", Provider: agents_model.ProviderOpenAI, Model: "m", ReasoningEffort: "medium"}))

	s, err := StartTaskSession(t.Context(), alice, repo, StartOptions{Agent: codex, Prompt: "a"})
	require.NoError(t, err)
	runCtx, err := GetRunContext(t.Context(), latestRun(t, s))
	require.NoError(t, err)
	assert.Equal(t, "medium", runCtx.ReasoningEffort)

	s, err = StartTaskSession(t.Context(), alice, repo, StartOptions{Agent: codex, Prompt: "b", ReasoningEffort: "high"})
	require.NoError(t, err)
	runCtx, err = GetRunContext(t.Context(), latestRun(t, s))
	require.NoError(t, err)
	assert.Equal(t, "high", runCtx.ReasoningEffort)

	_, err = StartTaskSession(t.Context(), alice, repo, StartOptions{Agent: codex, Prompt: "c", ReasoningEffort: "extreme"})
	assert.Error(t, err)
}

func TestCleanup(t *testing.T) {
	prepare(t)
	ctx := t.Context()
	alice := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	codex := newAgent(t, "codex")
	setKey(t, codex, alice, "sk-alice")
	s, err := StartTaskSession(ctx, alice, repo, StartOptions{Agent: codex, Prompt: "a"})
	require.NoError(t, err)
	r := latestRun(t, s)
	require.NoError(t, AppendEvents(ctx, r, []string{`{"type":"thread.started","thread_id":"t"}`}))
	state := stateArchive(t, &tar.Header{Name: "sessions/", Typeflag: tar.TypeDir, Mode: 0o755})
	require.NoError(t, SaveState(ctx, r, state, int64(state.Len())))
	endRun(t, r)

	require.NoError(t, Cleanup(ctx, time.Hour))
	unittest.AssertCount(t, &agents_model.Event{RunID: r.ID}, 1) // recent runs keep their log

	old := timeutil.TimeStamp(time.Now().Add(-48 * time.Hour).Unix())
	_, err = unittest.GetXORMEngine().Exec("UPDATE agent_run SET stopped = ? WHERE id = ?", old, r.ID)
	require.NoError(t, err)
	_, err = unittest.GetXORMEngine().Exec("UPDATE agent_session SET updated = ? WHERE id = ?", old, s.ID)
	require.NoError(t, err)
	require.NoError(t, Cleanup(ctx, time.Hour))
	unittest.AssertCount(t, &agents_model.Event{RunID: r.ID}, 0)
	s = unittest.AssertExistsAndLoadBean(t, &agents_model.Session{ID: s.ID})
	assert.False(t, s.HasState)
	unittest.AssertExistsAndLoadBean(t, &agents_model.Run{ID: r.ID}) // runs stay as history
}

func TestCIFailureRetries(t *testing.T) {
	prepare(t)
	ctx := t.Context()
	alice := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	codex := newAgent(t, "codex")
	setKey(t, codex, alice, "sk-alice")
	s, err := StartTaskSession(ctx, alice, repo, StartOptions{Agent: codex, Prompt: "a"})
	require.NoError(t, err)
	r, _ := startRun(t, latestRun(t, s))
	branch2, err := git.GetBranchCommitID(ctx, repo, "branch2")
	require.NoError(t, err)
	require.NoError(t, git.UpdateRef(ctx, repo, git.BranchPrefix+s.Branch, branch2)) // the agent's first push
	_, err = Publish(ctx, r, PublishOptions{}, true)
	require.NoError(t, err)
	endRun(t, r)
	head, err := git.GetBranchCommitID(ctx, repo, s.Branch)
	require.NoError(t, err)

	failBy := func(sender *user_model.User, sha string) {
		status := &git_model.CommitStatus{State: commitstatus.CommitStatusFailure, Context: "ci / test", Description: "2 tests failed"}
		require.NoError(t, handleCIFailure(ctx, repo, sha, sender, status))
	}
	fail := func(sha string) { failBy(user_model.NewActionsUser(), sha) }
	fail("0000000000000000000000000000000000000000") // not the head of the agent's branch
	unittest.AssertCount(t, &agents_model.Run{SessionID: s.ID}, 1)
	failBy(unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 4}), head) // another writer cannot spend alice's key
	unittest.AssertCount(t, &agents_model.Run{SessionID: s.ID}, 1)

	fail(head)
	fix := latestRun(t, s)
	assert.Contains(t, fix.Prompt, "The CI check `ci / test` failed")
	assert.Equal(t, alice.ID, fix.TriggerUserID)
	fail(head) // the same commit is only handled once
	unittest.AssertCount(t, &agents_model.Run{SessionID: s.ID}, 2)

	t.Run("AttemptsAreLimited", func(t *testing.T) {
		defer test.MockVariableValue(&setting.Agents.MaxCIFixAttempts, 1)()
		fix, _ = startRun(t, fix)
		endRun(t, fix)
		other, err := git.GetBranchCommitID(ctx, repo, "master")
		require.NoError(t, err)
		require.NoError(t, git.UpdateRef(ctx, repo, git.BranchPrefix+s.Branch, other))
		fail(other)
		unittest.AssertCount(t, &agents_model.Run{SessionID: s.ID}, 2)
		pr := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{HeadBranch: s.Branch})
		stopped := func() int64 {
			n, err := db.GetEngine(ctx).Where("issue_id = ? AND poster_id = ? AND content LIKE ?", pr.IssueID, codex.BotID, "%I stopped%").Count(new(issues_model.Comment))
			require.NoError(t, err)
			return n
		}
		assert.EqualValues(t, 1, stopped(), "the agent tells it gave up")

		require.NoError(t, git.UpdateRef(ctx, repo, git.BranchPrefix+s.Branch, branch2))
		fail(branch2)
		assert.EqualValues(t, 1, stopped(), "only once")
	})
}

func TestDeletingAUserDeletesTheirKeys(t *testing.T) {
	prepare(t)
	user := &user_model.User{Name: "leaving", Email: "leaving@example.com"}
	require.NoError(t, user_model.CreateUser(t.Context(), user, &user_model.Meta{}))
	codex := newAgent(t, "codex")
	setKey(t, codex, user, "sk-leaving")
	require.NoError(t, user_service.DeleteUser(t.Context(), user, false))
	unittest.AssertNotExistsBean(t, &agents_model.UserKey{UserID: user.ID})
}

func TestRepoActivation(t *testing.T) {
	prepare(t)
	ctx := t.Context()
	alice := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	codex := newAgent(t, "codex")
	claude := newAgent(t, "claude")
	setKey(t, codex, alice, "sk-alice")
	setKey(t, claude, alice, "sk-alice-2")

	require.NoError(t, SaveRepoSetting(ctx, repo, &agents_model.RepoSetting{RepoID: repo.ID, Enabled: true, AllowedAgentIDs: []int64{claude.ID}}))
	_, err := StartTaskSession(ctx, alice, repo, StartOptions{Agent: codex, Prompt: "a"})
	assert.Error(t, err, "codex is not among the repository's agents")
	assert.False(t, Assignable(ctx, codex.Bot, repo))
	_, err = StartTaskSession(ctx, alice, repo, StartOptions{Agent: claude, Prompt: "a"})
	assert.NoError(t, err)

	rs, err := agents_model.GetRepoSetting(ctx, repo.ID)
	require.NoError(t, err)
	rs.Enabled = false
	require.NoError(t, SaveRepoSetting(ctx, repo, rs))
	assert.False(t, IsRepoUsable(ctx, repo))
	_, err = StartTaskSession(ctx, alice, repo, StartOptions{Agent: claude, Prompt: "b"})
	assert.Error(t, err)

	t.Run("InstanceDefault", func(t *testing.T) {
		defer test.MockVariableValue(&setting.Agents.EnableByDefault, false)()
		other := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 3})
		assert.False(t, IsRepoUsable(ctx, other))
	})
}

func TestUsage(t *testing.T) {
	prepare(t)
	ctx := t.Context()
	alice := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	codex := newAgent(t, "codex")
	setKey(t, codex, alice, "sk-alice")
	s, err := StartTaskSession(ctx, alice, repo, StartOptions{Agent: codex, Prompt: "a"})
	require.NoError(t, err)
	r := latestRun(t, s)
	require.NoError(t, AppendEvents(ctx, r, []string{
		`{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":40,"output_tokens":10}}`,
		`{"type":"turn.completed","usage":{"input_tokens":50,"cached_input_tokens":0,"output_tokens":5}}`,
		`{"type":"turn.completed","usage":{"input_tokens":-1000,"cached_input_tokens":-40,"output_tokens":-15}}`,
	}))
	r = unittest.AssertExistsAndLoadBean(t, &agents_model.Run{ID: r.ID})
	assert.Zero(t, r.InputTokens, "job events cannot forge provider usage")
	for _, event := range []string{
		`{"type":"response.completed","response":{"usage":{"input_tokens":100,"output_tokens":10,"input_tokens_details":{"cached_tokens":40}}}}`,
		`{"type":"response.incomplete","response":{"usage":{"input_tokens":50,"output_tokens":5}}}`,
	} {
		data := "data: " + event + "\n\n"
		body := &modelResponseBody{ReadCloser: io.NopCloser(iotest.OneByteReader(strings.NewReader(data + data))), ctx: ctx, runID: r.ID, stream: true, cleanup: func() {}}
		got, err := io.ReadAll(body)
		require.NoError(t, err)
		assert.Equal(t, data+data, string(got))
		require.NoError(t, body.Close())
	}
	jsonBody := &modelResponseBody{
		ReadCloser: io.NopCloser(strings.NewReader(`{"usage":{"input_tokens":20,"output_tokens":2,"input_tokens_details":{"cached_tokens":40}}}`)),
		ctx:        ctx, runID: r.ID, cleanup: func() {},
	}
	_, err = io.Copy(io.Discard, jsonBody)
	require.NoError(t, err)
	require.NoError(t, jsonBody.Close())
	r = unittest.AssertExistsAndLoadBean(t, &agents_model.Run{ID: r.ID})
	assert.Equal(t, []int64{170, 60, 17}, []int64{r.InputTokens, r.CachedInputTokens, r.OutputTokens})

	usage, err := agents_model.FindUsage(ctx, 0, alice.ID)
	require.NoError(t, err)
	require.Len(t, usage, 1)
	assert.Equal(t, agents_model.Usage{UserID: alice.ID, AgentID: codex.ID, Runs: 1, InputTokens: 170, CachedInputTokens: 60, OutputTokens: 17}, *usage[0])
}

func TestRunsAgentCode(t *testing.T) {
	prepare(t)
	ctx := t.Context()
	codex := newAgent(t, "codex")
	runsAgentCode := func(run *actions_model.ActionRun) bool {
		ok, err := agents_model.RunsAgentCode(ctx, run)
		require.NoError(t, err)
		return ok
	}
	assert.True(t, runsAgentCode(&actions_model.ActionRun{RepoID: 1, TriggerUserID: codex.BotID, Ref: "refs/heads/codex/x", TriggerEvent: "push"}))
	assert.False(t, runsAgentCode(&actions_model.ActionRun{RepoID: 1, TriggerUserID: 2, Ref: "refs/heads/master", TriggerEvent: "push"}))

	pull := &actions_model.ActionRun{RepoID: 1, TriggerUserID: 2, Ref: "refs/pull/3/head", TriggerEvent: "pull_request", CommitSHA: unittest.AssertExistsAndLoadBean(t, &git_model.Branch{RepoID: 1, Name: "branch2"}).CommitID} // head branch2
	assert.False(t, runsAgentCode(pull))
	_, err := db.GetEngine(ctx).Exec("UPDATE branch SET pusher_id = ? WHERE repo_id = 1 AND name = 'branch2'", codex.BotID)
	require.NoError(t, err)
	assert.True(t, runsAgentCode(pull), "the agent pushed the head of the pull request")
	pull.WorkflowRepoID, pull.OwnerID, pull.WorkflowID = 1, 2, "ci.yml"
	require.NoError(t, actions_service.PrepareRunAndInsert(ctx, []byte("on: pull_request\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n"), pull, nil))
	pull = unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRun{ID: pull.ID})
	assert.Equal(t, "restricted", pull.AgentCodeTrust)
	_, err = db.GetEngine(ctx).Exec("UPDATE branch SET pusher_id = ?, commit_id = ? WHERE repo_id = 1 AND name = 'branch2'", 2, strings.Repeat("b", 40))
	require.NoError(t, err)
	assert.True(t, runsAgentCode(pull), "a new human push cannot release the old run's secrets")
	pull.AgentCodeTrust = ""
	pull.ID = 0
	assert.True(t, runsAgentCode(pull), "a moved head cannot establish trust for an older commit")
	pull.ID = 1
	assert.True(t, runsAgentCode(pull), "legacy runs have no saved trust decision")
	pull.TriggerEvent = "pull_request_target"
	pull.ID = 0
	assert.False(t, runsAgentCode(pull), "like for forks, the base's code runs")
}

// stateArchive builds a Codex state archive like the driver uploads
func stateArchive(t *testing.T, entries ...*tar.Header) *bytes.Buffer {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, h := range entries {
		require.NoError(t, tw.WriteHeader(h))
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return &buf
}

func TestCheckStateArchive(t *testing.T) {
	dir := func(name string) *tar.Header { return &tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0o755} }
	file := func(name string) *tar.Header { return &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644} }
	require.NoError(t, checkStateArchive(stateArchive(t, dir("sessions/"), dir("sessions/2026/"), file("sessions/2026/rollout.jsonl"))))
	for name, h := range map[string]*tar.Header{
		"config":   file("config.toml"),
		"escape":   file("sessions/../config.toml"),
		"absolute": file("/sessions/x"),
		"symlink":  {Name: "sessions/x", Typeflag: tar.TypeSymlink, Linkname: "../config.toml"},
	} {
		assert.Error(t, checkStateArchive(stateArchive(t, dir("sessions/"), h)), name)
	}
	assert.Error(t, checkStateArchive(strings.NewReader("not gzip")))
	assert.Error(t, checkStateArchiveLimits(stateArchive(t, dir("sessions/"), file("sessions/a"), file("sessions/b")), maxStateSize, 2))
	assert.Error(t, checkStateArchiveLimits(stateArchive(t, dir("sessions/")), 512, 10), "tar headers and padding count towards the expanded limit")
	var oversized bytes.Buffer
	gz := gzip.NewWriter(&oversized)
	tw := tar.NewWriter(gz)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "sessions/large", Mode: 0o600, Size: maxStateSize + 1}))
	require.NoError(t, gz.Close())
	assert.Error(t, checkStateArchive(&oversized), "reject an oversized member before decompressing its contents")
	valid := stateArchive(t, dir("sessions/"), file("sessions/x")).Bytes()
	valid[len(valid)-1] ^= 1
	assert.Error(t, checkStateArchive(bytes.NewReader(valid)), "gzip checksums are validated after tar EOF")
}

func TestDeleteSessions(t *testing.T) {
	prepare(t)
	ctx := t.Context()
	alice := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	codex := newAgent(t, "codex")
	setKey(t, codex, alice, "sk-alice")
	s, err := StartTaskSession(ctx, alice, repo, StartOptions{Agent: codex, Prompt: "a"})
	require.NoError(t, err)
	r, _ := startRun(t, latestRun(t, s))
	require.NoError(t, AppendEvents(ctx, r, []string{`{"type":"thread.started","thread_id":"t"}`}))
	state := stateArchive(t, &tar.Header{Name: "sessions/", Typeflag: tar.TypeDir, Mode: 0o755})
	require.NoError(t, SaveState(ctx, r, state, int64(state.Len())))

	issue := unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{ID: 1})
	is, err := startConversationRun(ctx, alice, codex, issue, 0, "look")
	require.NoError(t, err)
	issueRun := latestRun(t, is)
	require.NotZero(t, issueRun.ActionRunID)
	(&notifier{}).DeleteIssue(ctx, alice, issue)
	assert.True(t, unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRun{ID: issueRun.ActionRunID}).Status.IsDone(), "the job of a deleted issue stops")
	unittest.AssertNotExistsBean(t, &agents_model.Session{ID: is.ID})
	unittest.AssertNotExistsBean(t, &agents_model.Run{SessionID: is.ID})
	unittest.AssertExistsAndLoadBean(t, &agents_model.Session{ID: s.ID})

	withState, err := agents_model.DeleteRepoSessions(ctx, repo.ID)
	require.NoError(t, err)
	assert.Equal(t, []int64{s.ID}, withState)
	unittest.AssertNotExistsBean(t, &agents_model.Session{ID: s.ID})
	unittest.AssertNotExistsBean(t, &agents_model.Run{ID: r.ID})
	unittest.AssertNotExistsBean(t, &agents_model.Event{SessionID: s.ID})
	unittest.AssertNotExistsBean(t, &auth_model.AccessToken{ID: r.AccessTokenID}) // the running job cannot push anymore
}

func TestAgentMerge(t *testing.T) {
	prepare(t)
	ctx := t.Context()
	var queued []automergequeue.AutoMergeItem
	defer test.MockVariableValue(&automergequeue.AddToQueue, func(item automergequeue.AutoMergeItem) { queued = append(queued, item) })()
	alice := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	reader := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 5})
	codex := newAgent(t, "codex")
	setKey(t, codex, alice, "sk-alice")
	setKey(t, codex, reader, "sk-reader")
	pr := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: 2})

	_, err := StartAgentMerge(ctx, reader, pr, MergeOptions{Agent: codex, Style: repo_model.MergeStyleMerge})
	require.Error(t, err, "only those who can merge the pull request can ask an agent to")
	fork := *pr
	fork.HeadRepoID = 11
	_, err = StartAgentMerge(ctx, alice, &fork, MergeOptions{Agent: codex, Style: repo_model.MergeStyleMerge})
	require.ErrorContains(t, err, "forks", "the agent cannot push the merge to a fork")
	t.Run("ProtectedHead", func(t *testing.T) {
		rule := &git_model.ProtectedBranch{RepoID: pr.BaseRepoID, RuleName: pr.HeadBranch}
		require.NoError(t, git_model.UpdateProtectBranch(ctx, pr.BaseRepo, rule, git_model.WhitelistOptions{}))
		defer func() { require.NoError(t, git_model.DeleteProtectedBranch(ctx, pr.BaseRepo, rule.ID)) }()
		ok, err := CanAgentMerge(ctx, alice, pr)
		require.NoError(t, err)
		assert.False(t, ok, "the agent would push the merge with the requester's permissions")
	})

	s, err := StartAgentMerge(ctx, alice, pr, MergeOptions{Agent: codex, Style: repo_model.MergeStyleSquash, Review: true})
	require.NoError(t, err)
	r, _ := startRun(t, latestRun(t, s))
	assert.Equal(t, agents_model.RunPurposeMerge, r.Purpose)
	runCtx, err := GetRunContext(ctx, r)
	require.NoError(t, err)
	assert.Equal(t, pr.BaseBranch, runCtx.MergeFrom)
	assert.Equal(t, pr.HeadBranch, runCtx.PushBranch)
	assert.Contains(t, runCtx.Prompt, "MERGE-VERDICT")

	require.NoError(t, Finish(ctx, r, FinishOptions{Status: "success", Summary: "The tests fail on main.\nMERGE-VERDICT: block"}))
	scheduled, _, err := pull_model.GetScheduledMergeByPullID(ctx, pr.ID)
	require.NoError(t, err)
	assert.False(t, scheduled, "a blocking review keeps the pull request unmerged")
	reply := unittest.AssertExistsAndLoadBean(t, &issues_model.Comment{ID: unittest.AssertExistsAndLoadBean(t, &agents_model.Run{ID: r.ID}).ReplyCommentID})
	assert.Contains(t, reply.Content, "The tests fail on main.\n⛔ My review blocks the merge.")
	assert.NotContains(t, reply.Content, "MERGE-VERDICT", "the marker is for Gitea")

	endRun(t, r)
	again, err := StartAgentMerge(ctx, alice, pr, MergeOptions{Agent: codex, Style: repo_model.MergeStyleSquash, Review: true})
	require.NoError(t, err)
	require.Equal(t, s.ID, again.ID, "the merge continues the agent's conversation on the pull request")
	r, _ = startRun(t, latestRun(t, s))
	require.NoError(t, Finish(ctx, r, FinishOptions{Status: "success", Summary: "Looks good.\nMERGE-VERDICT: approve"}))
	scheduled, autoMerge, err := pull_model.GetScheduledMergeByPullID(ctx, pr.ID)
	require.NoError(t, err)
	require.True(t, scheduled)
	assert.Equal(t, alice.ID, autoMerge.DoerID, "the merge is the requester's")
	assert.Equal(t, repo_model.MergeStyleSquash, autoMerge.MergeStyle)
	assert.NotEmpty(t, queued)

	head := unittest.AssertExistsAndLoadBean(t, &agents_model.Run{ID: r.ID}).MergeHead
	require.NotEmpty(t, head)
	n := &notifier{}
	n.PullRequestSynchronized(ctx, codex.Bot, pr, "", head)
	scheduled, _, err = pull_model.GetScheduledMergeByPullID(ctx, pr.ID)
	require.NoError(t, err)
	assert.True(t, scheduled, "the agent's own pushes, notified late, are what it reviewed")
	endRun(t, r)
	_, err = StartAgentMerge(ctx, alice, pr, MergeOptions{Agent: codex, Style: repo_model.MergeStyleSquash, Review: true})
	require.NoError(t, err)
	n.PullRequestSynchronized(ctx, alice, pr, head, "0000000000000000000000000000000000000001")
	scheduled, _, err = pull_model.GetScheduledMergeByPullID(ctx, pr.ID)
	require.NoError(t, err)
	assert.False(t, scheduled, "the review does not cover new commits, even with a newer merge run on the pull request")

	r, _ = startRun(t, latestRun(t, s))
	require.NoError(t, AppendEvents(ctx, r, []string{`{"type":"turn.started"}`}))
	_, err = Publish(ctx, r, PublishOptions{Head: head}, false)
	require.NoError(t, err)
	_, err = Publish(ctx, r, PublishOptions{Head: "0000000000000000000000000000000000000003"}, false)
	require.NoError(t, err)
	n.PullRequestSynchronized(ctx, alice, pr, head, "0000000000000000000000000000000000000002")
	reviewing := unittest.AssertExistsAndLoadBean(t, &agents_model.Run{ID: r.ID})
	assert.Equal(t, head, reviewing.ReviewedHead)
	assert.Equal(t, head, reviewing.StartHead, "the commit the job checked out is the one it first reported")
	require.NoError(t, Finish(ctx, r, FinishOptions{Status: "success", Summary: "MERGE-VERDICT: approve"}))
	scheduled, _, err = pull_model.GetScheduledMergeByPullID(ctx, pr.ID)
	require.NoError(t, err)
	assert.False(t, scheduled, "commits pushed by others during the review are not reviewed")
}

func TestCIFixAfterAgentMerge(t *testing.T) {
	prepare(t)
	ctx := t.Context()
	defer test.MockVariableValue(&automergequeue.AddToQueue, func(automergequeue.AutoMergeItem) {})()
	alice := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	codex := newAgent(t, "codex")
	setKey(t, codex, alice, "sk-alice")
	pr := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: 2})
	require.NoError(t, pr.LoadBaseRepo(ctx))
	s, err := StartAgentMerge(ctx, alice, pr, MergeOptions{Agent: codex, Style: repo_model.MergeStyleMerge})
	require.NoError(t, err)
	r, _ := startRun(t, latestRun(t, s))
	require.NoError(t, Finish(ctx, r, FinishOptions{Status: "success"}))
	endRun(t, r)

	head, err := git.GetBranchCommitID(ctx, pr.BaseRepo, pr.HeadBranch)
	require.NoError(t, err)
	status := &git_model.CommitStatus{State: commitstatus.CommitStatusFailure, Context: "ci / test"}
	require.NoError(t, handleCIFailure(ctx, pr.BaseRepo, head, user_model.NewActionsUser(), status))
	fix := latestRun(t, s)
	assert.Contains(t, fix.Prompt, "The CI check `ci / test` failed", "the agent keeps the pull request it merges green")
	assert.Equal(t, pr.HeadBranch, fix.PushBranch)
}

func TestAgentReview(t *testing.T) {
	prepare(t)
	ctx := t.Context()
	alice := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	codex := newAgent(t, "codex")
	setKey(t, codex, alice, "sk-alice")
	pr := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: 2})
	require.NoError(t, pr.LoadBaseRepo(ctx))
	require.NoError(t, agents_model.SaveRepoSetting(ctx, &agents_model.RepoSetting{RepoID: pr.BaseRepoID, Enabled: true, ReviewAgentID: codex.ID, ReviewUserID: alice.ID, ReviewEveryone: true}))

	func() {
		defer test.MockVariableValue(&maxAutoReviewsPerDay, 0)()
		autoReview(ctx, pr)
		unittest.AssertCount(t, &agents_model.Session{AgentID: codex.ID}, 0) // the daily limit of automatic reviews is reached
	}()
	autoReview(ctx, pr)
	s, err := agents_model.GetConversationSession(ctx, codex.ID, pr.IssueID)
	require.NoError(t, err)
	r, _ := startRun(t, latestRun(t, s))
	assert.Equal(t, agents_model.RunPurposeReview, r.Purpose)
	assert.Equal(t, alice.ID, r.TriggerUserID, "the review uses the key of who set it up")
	assert.False(t, r.CanPush(), "a review is read-only")
	assert.Equal(t, auth_model.AccessTokenScopeReadRepository, unittest.AssertExistsAndLoadBean(t, &auth_model.AccessToken{ID: r.AccessTokenID}).Scope)
	assert.Equal(t, pr.GetGitHeadRefName(), r.BaseRef)
	autoReview(ctx, pr)
	assert.Equal(t, r.ID, latestRun(t, s).ID, "nor twice while it reviews")

	gitRepo, err := git.OpenRepository(ctx, pr.BaseRepo)
	require.NoError(t, err)
	defer gitRepo.Close()
	head, err := gitRepo.GetRefCommitID(ctx, pr.GetGitHeadRefName())
	require.NoError(t, err)
	changed, err := gitRepo.GetFilesChangedBetween(ctx, pr.MergeBase, head)
	require.NoError(t, err)
	require.NotEmpty(t, changed)
	summary := fmt.Sprintf("One problem.\n\n```review-comments\n[{\"path\": %q, \"line\": 1, \"body\": \"Off by one\"}, {\"path\": \"elsewhere.go\", \"line\": 3, \"body\": \"Unused\"}]\n```\n", changed[0])
	require.NoError(t, Finish(ctx, r, FinishOptions{Status: "success", Summary: summary}))

	review := unittest.AssertExistsAndLoadBean(t, &issues_model.Review{IssueID: pr.IssueID, ReviewerID: codex.BotID, Type: issues_model.ReviewTypeComment})
	assert.Contains(t, review.Content, "One problem.")
	assert.Contains(t, review.Content, "`elsewhere.go` line 3", "a comment out of the changes goes to the body")
	assert.NotContains(t, review.Content, "review-comments")
	unittest.AssertExistsAndLoadBean(t, &issues_model.Comment{ReviewID: review.ID, Type: issues_model.CommentTypeCode, TreePath: changed[0], Line: 1, Content: "Off by one"})
	replies, err := db.GetEngine(ctx).Where("issue_id = ? AND poster_id = ? AND type = ?", pr.IssueID, codex.BotID, issues_model.CommentTypeComment).Count(new(issues_model.Comment))
	require.NoError(t, err)
	assert.Zero(t, replies, "the review is the answer")

	autoReview(ctx, pr)
	assert.Equal(t, r.ID, latestRun(t, s).ID, "a head is reviewed once, however often the pull request leaves its work in progress")

	rs, err := agents_model.GetRepoSetting(ctx, pr.BaseRepoID)
	require.NoError(t, err)
	problem, err := AutoReviewProblem(ctx, pr.BaseRepo, rs)
	require.NoError(t, err)
	assert.Empty(t, problem)
	for _, prompt := range []string{"fix it", "then test it"} {
		_, err = SendInstruction(ctx, s, alice, prompt)
		require.NoError(t, err)
	}
	queued := latestRun(t, s)
	require.Equal(t, agents_model.StatusQueued, queued.Status)
	require.NoError(t, DeleteUserKey(ctx, codex, alice))
	assert.Equal(t, agents_model.StatusCancelled, latestRun(t, s).Status, "the runs queued with a deleted key never start")
	problem, err = AutoReviewProblem(ctx, pr.BaseRepo, rs)
	require.NoError(t, err)
	assert.Equal(t, ReviewProblemNoKey, problem, "the settings tell why reviews stopped")
}

func TestMergeVerdict(t *testing.T) {
	for summary, want := range map[string]string{
		"Looks good.\nMERGE-VERDICT: approve":                      "Looks good.\n✅ My review approves the merge.",
		"Review.\n\nMERGE-VERDICT: block — the tests fail on main": "Review.\n\n⛔ My review blocks the merge: the tests fail on main",
		"MERGE-VERDICT: block\n- fix the migration":                "⛔ My review blocks the merge.\n- fix the migration",
		"No verdict.": "No verdict.",
	} {
		assert.Equal(t, want, presentVerdict(summary), summary)
	}
	assert.Equal(t, "block", mergeVerdict("Review.\n\nMERGE-VERDICT: block — the tests fail"))
	assert.Equal(t, "approve", mergeVerdict("MERGE-VERDICT: block\nFixed.\nMERGE-VERDICT: approve"), "the last verdict wins")
	assert.Equal(t, "block", mergeVerdict("I approve."), "no verdict blocks")
}

func TestSessionListsHideUnreadableIssues(t *testing.T) {
	prepare(t)
	ctx := t.Context()
	alice := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	codex := newAgent(t, "codex")
	setKey(t, codex, alice, "sk-alice")
	task, err := StartTaskSession(ctx, alice, repo, StartOptions{Agent: codex, Prompt: "a"})
	require.NoError(t, err)
	issue, err := startConversationRun(ctx, alice, codex, unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{ID: 1}), 0, "look")
	require.NoError(t, err)
	pull, err := startConversationRun(ctx, alice, codex, unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{ID: 3}), 0, "look")
	require.NoError(t, err)

	listed := func(opts agents_model.FindSessionsOptions) []int64 {
		opts.RepoID = repo.ID
		sessions, err := db.Find[agents_model.Session](ctx, opts)
		require.NoError(t, err)
		ids := make([]int64, 0, len(sessions))
		for _, s := range sessions {
			ids = append(ids, s.ID)
		}
		return ids
	}
	assert.ElementsMatch(t, []int64{task.ID, issue.ID, pull.ID}, listed(agents_model.FindSessionsOptions{}))
	assert.ElementsMatch(t, []int64{task.ID, pull.ID}, listed(agents_model.FindSessionsOptions{HideIssues: true}))
	assert.ElementsMatch(t, []int64{task.ID, issue.ID}, listed(agents_model.FindSessionsOptions{HidePulls: true}))
	assert.ElementsMatch(t, []int64{task.ID}, listed(agents_model.FindSessionsOptions{HideIssues: true, HidePulls: true}))
	assert.ElementsMatch(t, []int64{task.ID, issue.ID, pull.ID}, listed(agents_model.FindSessionsOptions{ReadableBy: alice}))
}

func TestReviewMentioningTheAgentQueuesOneRun(t *testing.T) {
	prepare(t)
	ctx := t.Context()
	alice := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	codex := newAgent(t, "codex")
	setKey(t, codex, alice, "sk-alice")
	pr := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: 2})
	require.NoError(t, pr.LoadIssue(ctx))
	require.NoError(t, pr.LoadBaseRepo(ctx))
	pr.Issue.Repo = pr.BaseRepo
	gitRepo, err := git.OpenRepository(ctx, pr.BaseRepo)
	require.NoError(t, err)
	defer gitRepo.Close()
	head, err := gitRepo.GetRefCommitID(ctx, pr.GetGitHeadRefName())
	require.NoError(t, err)
	changed, err := gitRepo.GetFilesChangedBetween(ctx, pr.MergeBase, head)
	require.NoError(t, err)
	require.NotEmpty(t, changed)
	for _, content := range []string{"@codex rename this", "@codex and test that"} {
		_, err := pull_service.CreateCodeComment(ctx, alice, gitRepo, pr.Issue, 1, content, changed[0], true, 0, head, nil)
		require.NoError(t, err)
	}
	review, comment, err := pull_service.SubmitReview(ctx, alice, gitRepo, pr.Issue, issues_model.ReviewTypeComment, "A few things", head, nil)
	require.NoError(t, err)

	n := &notifier{}
	n.PullRequestReview(ctx, pr, review, comment, nil)
	for _, lines := range review.CodeComments {
		for _, comments := range lines {
			for _, c := range comments {
				n.PullRequestCodeComment(ctx, pr, c, nil)
			}
		}
	}
	s := unittest.AssertExistsAndLoadBean(t, &agents_model.Session{AgentID: codex.ID, IssueID: pr.IssueID})
	unittest.AssertCount(t, &agents_model.Run{SessionID: s.ID}, 1)
	r := latestRun(t, s)
	assert.Contains(t, r.Prompt, "A few things")
	assert.Contains(t, r.Prompt, "@codex rename this")
	assert.Contains(t, r.Prompt, "@codex and test that")
	runCtx, err := GetRunContext(ctx, r)
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(runCtx.Prompt, "@codex rename this"), "the comments of the request are not repeated as discussion")
}

func TestEditedCommentMentions(t *testing.T) {
	prepare(t)
	ctx := t.Context()
	alice := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	codex := newAgent(t, "codex")
	setKey(t, codex, alice, "sk-alice")
	issue := unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{ID: 1})
	c := &issues_model.Comment{Type: issues_model.CommentTypeComment, PosterID: alice.ID, IssueID: issue.ID, Content: "@codex look"}
	require.NoError(t, db.Insert(ctx, c))

	n := &notifier{}
	n.UpdateComment(ctx, alice, c, "@codex have a look")
	unittest.AssertNotExistsBean(t, &agents_model.Session{AgentID: codex.ID}) // it was already mentioned
	n.UpdateComment(ctx, alice, c, "look")
	s := unittest.AssertExistsAndLoadBean(t, &agents_model.Session{AgentID: codex.ID, IssueID: issue.ID})
	assert.Equal(t, c.ID, latestRun(t, s).CommentID)

	issue.Title = "Renamed"
	n.IssueChangeTitle(ctx, alice, issue, "issue1")
	assert.Equal(t, "Renamed", unittest.AssertExistsAndLoadBean(t, &agents_model.Session{ID: s.ID}).Title, "the session follows its issue's title")
}

func TestOpenStateValidation(t *testing.T) {
	r := &agents_model.Run{SessionID: 999999}
	key := agents_model.StatePath(r.SessionID)
	defer storage.Actions.Delete(key)
	archive := stateArchive(t, &tar.Header{Name: "sessions/", Typeflag: tar.TypeDir, Mode: 0o755})
	want := append([]byte(nil), archive.Bytes()...)
	_, err := storage.Actions.Save(key, archive, int64(archive.Len()))
	require.NoError(t, err)
	obj, err := OpenState(r)
	require.NoError(t, err)
	got, err := io.ReadAll(obj)
	require.NoError(t, err)
	require.NoError(t, obj.Close())
	assert.Equal(t, want, got, "validation rewinds the archive before download")
	_, err = storage.Actions.Save(key, strings.NewReader("invalid"), 7)
	require.NoError(t, err)
	_, err = OpenState(r)
	require.Error(t, err, "previously stored archives must pass the new validation")
}
