// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"fmt"
	"strconv"

	agents_model "gitea.dev/models/agents"
	"gitea.dev/modules/agents"

	"go.yaml.in/yaml/v4"
)

const (
	// SessionTokenSecretName is the secret that carries the session token to the job, added when the task starts.
	SessionTokenSecretName = agents_model.SessionTokenSecretName

	// WorkflowIDPrefix marks the Actions runs created for agents; they are not backed by a file in any repository.
	WorkflowIDPrefix = "agent-"

	workflowTimeoutMinutes = 60
)

type workflowFile struct {
	Name        string                 `yaml:"name"`
	On          string                 `yaml:"on"`
	Permissions map[string]string      `yaml:"permissions"`
	Jobs        map[string]workflowJob `yaml:"jobs"`
}

type workflowJob struct {
	Name           string            `yaml:"name"`
	RunsOn         string            `yaml:"runs-on"`
	TimeoutMinutes int               `yaml:"timeout-minutes"`
	Container      workflowContainer `yaml:"container"`
	Env            map[string]string `yaml:"env"`
	Steps          []workflowStep    `yaml:"steps"`
}

type workflowContainer struct {
	Image string `yaml:"image"`
}

type workflowStep struct {
	Name string            `yaml:"name"`
	If   string            `yaml:"if,omitempty"`
	Uses string            `yaml:"uses,omitempty"`
	With map[string]string `yaml:"with,omitempty"`
	Run  string            `yaml:"run,omitempty"`
}

// BuildWorkflow generates the workflow of one run. Nothing the repository or its users wrote can reach it: it only
// holds the numeric run id and the administrator's settings, which Settings.Validate keeps free of expressions and
// line breaks. Issue, pull request and comment text is fetched by the helper through the API instead.
// The document is built as a data structure and serialized, never assembled from strings.
func BuildWorkflow(runID int64, s agents.Settings) ([]byte, error) {
	if runID <= 0 {
		return nil, fmt.Errorf("BuildWorkflow: invalid run id %d", runID)
	}
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("BuildWorkflow: %w", err)
	}
	token := "${{ secrets." + SessionTokenSecretName + " }}"
	wf := workflowFile{
		Name:        "agent run " + strconv.FormatInt(runID, 10),
		On:          "workflow_dispatch",
		Permissions: map[string]string{"contents": "read"},
		Jobs: map[string]workflowJob{
			"agent": {
				Name:           "agent",
				RunsOn:         s.RunnerLabel,
				TimeoutMinutes: workflowTimeoutMinutes,
				Container:      workflowContainer{Image: s.JobImage},
				Env: map[string]string{
					"AGENT_RUN_ID":        strconv.FormatInt(runID, 10),
					"AGENT_API_URL":       "${{ github.server_url }}",
					"AGENT_SESSION_TOKEN": token,
					"AGENT_LOG_EVENTS":    "${{ vars.AGENT_LOG_EVENTS }}",
					"AGENT_HELPER_URL":    s.HelperDownloadURL,
					"AGENT_HELPER_SHA256": agents.FormatHelperSums(s.HelperSHA256),
				},
				Steps: []workflowStep{
					{Name: "Install helper", Run: installHelperScript},
					{Name: "Checkout", Uses: s.CheckoutAction, With: map[string]string{"token": token, "fetch-depth": "0"}},
					{Name: "Prepare", Run: "agent-run prepare"},
					{Name: "Run agent", Run: "agent-run exec"},
					{Name: "Report result", If: "always()", Run: "agent-run finish"},
				},
			},
		},
	}
	out, err := yaml.Marshal(wf)
	if err != nil {
		return nil, fmt.Errorf("BuildWorkflow: %w", err)
	}
	return out, nil
}

// installHelperScript downloads agent-run when an administrator configured a URL, and verifies it against the
// configured checksum of the runner's platform. It is constant text: the URL and the checksums only reach it
// through environment variables, so nothing is interpolated into the shell.
const installHelperScript = `set -eu
[ -n "${AGENT_HELPER_URL:-}" ] || exit 0
os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in x86_64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; esac
dest="$HOME/.agent/bin/agent-run"
mkdir -p "$(dirname "$dest")"
url=$(printf '%s' "$AGENT_HELPER_URL" | sed "s/{os}/$os/g; s/{arch}/$arch/g")
curl -fsSL "$url" -o "$dest"
if [ -n "${AGENT_HELPER_SHA256:-}" ]; then
  want=$(printf '%s\n' "$AGENT_HELPER_SHA256" | sed -n "s/^$os-$arch=//p" | head -n 1)
  [ -n "$want" ] || { echo "no checksum configured for $os-$arch" >&2; exit 1; }
  echo "$want  $dest" | sha256sum -c -
fi
chmod +x "$dest"
echo "$(dirname "$dest")" >> "$GITHUB_PATH"
`
