// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gitea.dev/modules/actions/jobparser"
	"gitea.dev/modules/agents"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"
)

func parseWorkflow(t *testing.T, content []byte) workflowFile {
	var wf workflowFile
	require.NoError(t, yaml.Unmarshal(content, &wf))
	return wf
}

func TestBuildWorkflow(t *testing.T) {
	content, err := BuildWorkflow(42, agents.DefaultSettings())
	require.NoError(t, err)

	_, err = jobparser.ValidateWorkflowStatic(content) // the runner side parser must accept it
	require.NoError(t, err)

	wf := parseWorkflow(t, content)
	job := wf.Jobs["agent"]
	require.Len(t, wf.Jobs, 1)
	assert.Equal(t, "ubuntu-latest", job.RunsOn)
	assert.Equal(t, "gitea-agents-runner:latest", job.Container.Image)
	assert.Equal(t, "42", job.Env["AGENT_RUN_ID"])
	assert.Equal(t, "${{ secrets.AGENT_SESSION_TOKEN }}", job.Env["AGENT_SESSION_TOKEN"])
	assert.Equal(t, map[string]string{"contents": "read"}, wf.Permissions)
	var names []string
	for _, s := range job.Steps {
		names = append(names, s.Name)
	}
	assert.Equal(t, []string{"Install helper", "Checkout", "Prepare", "Run agent", "Report result"}, names)
	assert.Equal(t, "always()", job.Steps[4].If) // the result is reported even when the agent failed

	_, err = BuildWorkflow(0, agents.DefaultSettings())
	assert.Error(t, err)
}

// Settings reach the workflow, so hostile ones are refused and merely awkward ones survive a YAML round trip unchanged.
func TestBuildWorkflowIsInjectionSafe(t *testing.T) {
	for _, hostile := range []string{
		"x\n  steps: []", "x\r\nevil: 1", "${{ secrets.AGENT_SESSION_TOKEN }}", "a ${{ vars.X }} b", "x\x00y",
	} {
		s := agents.DefaultSettings()
		s.RunnerLabel = hostile
		_, err := BuildWorkflow(1, s)
		assert.Error(t, err, "runner label %q", hostile)
		s = agents.DefaultSettings()
		s.JobImage = hostile
		_, err = BuildWorkflow(1, s)
		assert.Error(t, err, "image %q", hostile)
		s = agents.DefaultSettings()
		s.CheckoutAction = hostile
		_, err = BuildWorkflow(1, s)
		assert.Error(t, err, "checkout %q", hostile)
		s = agents.DefaultSettings()
		s.HelperDownloadURL = hostile
		_, err = BuildWorkflow(1, s)
		assert.Error(t, err, "helper url %q", hostile)
	}

	for _, odd := range []string{
		`a: b`, `- item`, `"quoted"`, `'single'`, `# comment`, `x: {y: [1, 2]}`, `ubuntu: latest # not a comment`,
		`{{ not-an-expression }}`, `$(touch /tmp/pwned)`, "`id`", `a; rm -rf /`, `ключ-é-日本`, `&anchor *alias !tag`, `%DIRECTIVE`,
	} {
		s := agents.DefaultSettings()
		s.RunnerLabel, s.JobImage, s.CheckoutAction = odd, odd, odd
		content, err := BuildWorkflow(7, s)
		require.NoError(t, err, odd)
		wf := parseWorkflow(t, content)
		require.Len(t, wf.Jobs, 1, odd)
		job := wf.Jobs["agent"]
		assert.Equal(t, odd, job.RunsOn, odd)
		assert.Equal(t, odd, job.Container.Image, odd)
		assert.Equal(t, odd, job.Steps[1].Uses, odd)
		assert.Len(t, job.Steps, 5, odd) // nothing was smuggled in as an extra step
		assert.Equal(t, "agent-run prepare", job.Steps[2].Run, odd)
		assert.Equal(t, "7", job.Env["AGENT_RUN_ID"], odd)
	}
}

func TestInstallHelperScript(t *testing.T) {
	for _, tool := range []string{"sh", "curl", "sha256sum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not available", tool)
		}
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "agent-run-linux-amd64")
	require.NoError(t, os.WriteFile(binary, []byte("#!/bin/sh\necho helper\n"), 0o644))
	sum := sha256.Sum256([]byte("#!/bin/sh\necho helper\n"))
	good := "linux-amd64=" + hex.EncodeToString(sum[:])
	wrong := "linux-amd64=" + strings.Repeat("0", 64)

	run := func(url, sums string) (string, error) {
		home := t.TempDir()
		ghPath := filepath.Join(home, "github_path")
		require.NoError(t, os.WriteFile(ghPath, nil, 0o644))
		cmd := exec.Command("sh", "-c", installHelperScript)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "GITHUB_PATH=" + ghPath, "AGENT_HELPER_URL=" + url, "AGENT_HELPER_SHA256=" + sums}
		out, err := cmd.CombinedOutput()
		return string(out) + readFile(t, filepath.Join(home, ".agent/bin/agent-run")), err
	}
	// pretend to be linux/amd64 whatever the host is: the script only reads {os}/{arch} placeholders and the checksum line
	if out, err := exec.Command("sh", "-c", "uname -s; uname -m").Output(); err != nil || !strings.HasPrefix(string(out), "Linux\nx86_64") {
		t.Skip("the functional check assumes a linux/amd64 host")
	}
	url := "file://" + dir + "/agent-run-{os}-{arch}"

	out, err := run(url, good)
	require.NoError(t, err, out)
	assert.Contains(t, out, "echo helper")

	_, err = run(url, wrong) // a checksum mismatch must stop the run
	assert.Error(t, err)
	_, err = run(url, "linux-arm64="+strings.Repeat("a", 64)) // sums are configured but none for this platform
	assert.Error(t, err)
	out, err = run("", "") // no URL: nothing to do, the image already ships the helper
	assert.NoError(t, err, out)
	_, err = run(url, "") // no sums configured: no verification, as documented
	assert.NoError(t, err)
}

func readFile(t *testing.T, p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}
