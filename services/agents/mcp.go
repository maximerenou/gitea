// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"maps"
	"regexp"

	"gitea.dev/modules/util"

	"go.yaml.in/yaml/v4"
)

var mcpNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// validateMCPServer only accepts the keys of Codex's [mcp_servers.<name>] tables, with their types
func validateMCPServer(name string, server map[string]any) error {
	invalid := func(format string, args ...any) error {
		return util.NewInvalidArgumentErrorf("MCP server %q: "+format, append([]any{name}, args...)...)
	}
	if !mcpNamePattern.MatchString(name) {
		return invalid("the name must be letters, digits, dashes or underscores")
	}
	_, hasCommand := server["command"]
	_, hasURL := server["url"]
	if hasCommand == hasURL {
		return invalid("needs either command or url")
	}
	for key, value := range server {
		switch key {
		case "command", "url", "cwd":
			if _, ok := value.(string); !ok {
				return invalid("%s must be a string", key)
			}
		case "args":
			list, ok := value.([]any)
			if !ok {
				return invalid("args must be a list")
			}
			for _, arg := range list {
				if _, ok := arg.(string); !ok {
					return invalid("args must be strings")
				}
			}
		case "env":
			env, ok := value.(map[string]any)
			if !ok {
				return invalid("env must be a map")
			}
			for _, v := range env {
				if _, ok := v.(string); !ok {
					return invalid("env values must be strings")
				}
			}
		case "startup_timeout_sec", "tool_timeout_sec":
			if _, ok := value.(int); !ok {
				return invalid("%s must be a number of seconds", key)
			}
		default:
			return invalid("unsupported key %q", key)
		}
	}
	return nil
}

// ParseMCPServers reads the MCP servers an administrator defines for an agent, in the format of the mcp_servers section of SetupFile
func ParseMCPServers(content string) (map[string]map[string]any, error) {
	servers := map[string]map[string]any{}
	if err := yaml.Unmarshal([]byte(content), &servers); err != nil {
		return nil, util.NewInvalidArgumentErrorf("invalid MCP servers: %v", err)
	}
	for name, server := range servers {
		if err := validateMCPServer(name, server); err != nil {
			return nil, err
		}
	}
	return servers, nil
}

// mergeMCPServers adds the repository's servers to the agent's, the administrator's definitions win on a name clash
func mergeMCPServers(agent, repo map[string]map[string]any) map[string]map[string]any {
	merged := maps.Clone(repo)
	if merged == nil {
		merged = map[string]map[string]any{}
	}
	maps.Copy(merged, agent)
	return merged
}
