// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"embed"
	"fmt"

	"gitea.dev/modules/agents"
)

//go:embed repo_template
var repoTemplate embed.FS

// repoTemplateFiles returns the initial content of an agent's ".agent" repository: tree path => content.
func repoTemplateFiles(action agents.Action) (map[string]string, error) {
	files := map[string]string{}
	for _, name := range []string{"settings.yml", "mcp.json"} {
		b, err := repoTemplate.ReadFile("repo_template/" + name)
		if err != nil {
			return nil, fmt.Errorf("repoTemplateFiles: %w", err)
		}
		files[name] = string(b)
	}
	prompt, err := repoTemplate.ReadFile("repo_template/system-prompt.md")
	if err != nil {
		return nil, fmt.Errorf("repoTemplateFiles: %w", err)
	}
	role, err := repoTemplate.ReadFile("repo_template/roles/" + string(action) + ".md")
	if err != nil {
		return nil, fmt.Errorf("repoTemplateFiles: role %q: %w", action, err)
	}
	files["system-prompt.md"] = string(prompt) + "\n" + string(role)
	return files, nil
}
