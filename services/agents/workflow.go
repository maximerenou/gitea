// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	agents_model "gitea.dev/models/agents"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/util"

	_ "embed"

	"go.yaml.in/yaml/v4"
)

//go:embed driver.mjs
var DriverScript []byte

// the driver is served by Gitea so the workflow stays small and runners need no extra setup beyond Node.js
const bootstrapScript = `set -eu
dir="$(mktemp -d)"
node -e 'fetch(process.env.GITEA_AGENT_API + "/driver.mjs", {headers: {Authorization: "Bearer " + process.env.GITEA_AGENT_TOKEN}}).then(async (r) => { if (!r.ok) throw new Error("HTTP " + r.status); require("fs").writeFileSync(process.argv[1], Buffer.from(await r.arrayBuffer())); }).catch((e) => { console.error(e); process.exit(1); })' "$dir/driver.mjs"
exec node "$dir/driver.mjs" %s
`

// setupShell runs a setup step as the unprivileged agent user, the driver installs it in the checkout step
const setupShell = "/var/lib/gitea-agent/setup-shell"

// isolatesSetup reports whether the setup steps run unprivileged, without the run's tokens
func isolatesSetup() bool {
	return setting.Agents.Firewall && !setting.Agents.SetupAsRoot
}

var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// isolateSetupStep runs a step through setupShell as root, so the step's env and timeout must not reach that root shell:
// LD_PRELOAD or PATH would run the agent user's code as root, and act's timeout leaves the step's processes running.
// The script exports the env itself, and setupShell enforces the timeout.
func isolateSetupStep(step map[string]any, shell string) (map[string]any, error) {
	step = maps.Clone(step)
	minutes := 0
	if value, ok := step["timeout-minutes"]; ok {
		if minutes, ok = value.(int); !ok || minutes <= 0 {
			return nil, errors.New("needs a positive whole number of timeout-minutes")
		}
		delete(step, "timeout-minutes")
	}
	step["shell"] = fmt.Sprintf("%s %s %d {0}", setupShell, shell, minutes)
	env, _ := step["env"].(map[string]any)
	if _, ok := step["env"]; ok && env == nil {
		return nil, errors.New("needs env to be a map")
	}
	delete(step, "env")
	if len(env) == 0 {
		return step, nil
	}
	script, _ := step["run"].(string)
	exports := make([]string, 0, len(env))
	for _, name := range slices.Sorted(maps.Keys(env)) {
		if !envNamePattern.MatchString(name) {
			return nil, fmt.Errorf("has the invalid env name %q", name)
		}
		value := env[name]
		if value == nil {
			value = ""
		}
		exports = append(exports, name+"='"+strings.ReplaceAll(fmt.Sprint(value), "'", `'\''`)+"'")
	}
	step["run"] = "export " + strings.Join(exports, " ") + "\n" + script
	return step, nil
}

// buildWorkflow runs the repository's setup steps between the checkout and Codex, only the Codex step gets the run's model token
func buildWorkflow(setup *RepoSetup) ([]byte, error) {
	secret := func(name string) string { return "${{ secrets." + name + " }}" }
	env := map[string]string{
		"GITEA_AGENT_API":       strings.TrimSuffix(setting.AppURL, "/") + "/api/agents",
		"GITEA_AGENT_TOKEN":     secret("GITEA_TOKEN"),
		"GITEA_AGENT_GIT_TOKEN": secret(agents_model.GitTokenSecretName),
	}
	steps := []any{map[string]any{"name": "Check out", "env": env, "run": fmt.Sprintf(bootstrapScript, "prepare")}}
	if setup != nil {
		for i, step := range setup.Steps {
			if isolatesSetup() {
				if _, ok := step["uses"]; ok {
					return nil, util.NewInvalidArgumentErrorf("%s: step %d uses an action, actions run as root and need [agents] SETUP_AS_ROOT", SetupFile, i+1)
				}
				shell, _ := step["shell"].(string)
				if shell == "" {
					shell = "bash"
				} else if shell != "bash" && shell != "sh" {
					return nil, util.NewInvalidArgumentErrorf("%s: step %d uses the shell %q, only bash and sh are supported", SetupFile, i+1, shell)
				}
				isolated, err := isolateSetupStep(step, shell)
				if err != nil {
					return nil, util.NewInvalidArgumentErrorf("%s: step %d %v", SetupFile, i+1, err)
				}
				step = isolated
			}
			steps = append(steps, step)
		}
	}
	codexEnv := maps.Clone(env)
	codexEnv["GITEA_AGENT_API_KEY"] = secret(agents_model.APIKeySecretName)
	steps = append(steps, map[string]any{"name": "Codex", "env": codexEnv, "run": fmt.Sprintf(bootstrapScript, "run")})

	job := map[string]any{
		"runs-on":         setting.Agents.RunsOn,
		"timeout-minutes": int(setting.Agents.JobTimeout.Minutes()),
		"permissions":     map[string]string{"contents": "read"}, // pushes use the bot's run token
		"steps":           steps,
	}
	if setting.Agents.ContainerImage != "" {
		options := setting.Agents.ContainerOptions
		if setting.Agents.Firewall {
			options = strings.TrimSpace(options + " --cap-add=NET_ADMIN")
		}
		container := map[string]any{"image": setting.Agents.ContainerImage}
		if options != "" {
			container["options"] = options
		}
		job["container"] = container
	}
	return yaml.Marshal(map[string]any{
		"name": "Agent session",
		"on":   "workflow_dispatch",
		"jobs": map[string]any{"codex": job},
	})
}
