// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"context"
	"slices"
	"strings"

	repo_model "gitea.dev/models/repo"
	"gitea.dev/modules/git"
	"gitea.dev/modules/util"

	"go.yaml.in/yaml/v4"
)

// SetupFile prepares the agent's environment: its steps run before Codex, with the repository checked out
const SetupFile = ".gitea/agents/setup.yml"

// RepoSetup is the content of SetupFile
type RepoSetup struct {
	Steps      []map[string]any          `yaml:"steps"`
	MCPServers map[string]map[string]any `yaml:"mcp_servers"`
}

var allowedStepKeys = []string{"name", "id", "uses", "with", "run", "shell", "env", "working-directory", "timeout-minutes", "continue-on-error"}

// LoadRepoSetup reads SetupFile from the default branch only: a pull request must not change what runs before the agent
func LoadRepoSetup(ctx context.Context, repo *repo_model.Repository) (*RepoSetup, error) {
	gitRepo, err := git.OpenRepository(ctx, repo)
	if err != nil {
		return nil, err
	}
	defer gitRepo.Close()
	commit, err := gitRepo.GetBranchCommit(ctx, repo.DefaultBranch)
	if err != nil {
		return nil, err
	}
	content, err := commit.GetFileContent(ctx, gitRepo, SetupFile, 64*1024)
	if git.IsErrNotExist(err) {
		return &RepoSetup{}, nil
	} else if err != nil {
		return nil, err
	}
	return ParseRepoSetup(content)
}

// ParseRepoSetup validates the setup steps, expressions are refused so a repository can never read the job's secrets
func ParseRepoSetup(content string) (*RepoSetup, error) {
	setup := &RepoSetup{}
	if err := yaml.Unmarshal([]byte(content), setup); err != nil {
		return nil, util.NewInvalidArgumentErrorf("invalid %s: %v", SetupFile, err)
	}
	var decoded any
	if err := yaml.Unmarshal([]byte(content), &decoded); err != nil {
		return nil, util.NewInvalidArgumentErrorf("invalid %s: %v", SetupFile, err)
	}
	if hasExpression(decoded) { // decoded values, YAML escapes can spell an expression the raw text does not contain
		return nil, util.NewInvalidArgumentErrorf("%s cannot use ${{ }} expressions", SetupFile)
	}
	for i, step := range setup.Steps {
		for key := range step {
			if !slices.Contains(allowedStepKeys, key) {
				return nil, util.NewInvalidArgumentErrorf("%s: step %d has the unsupported key %q", SetupFile, i+1, key)
			}
		}
		_, hasRun := step["run"]
		_, hasUses := step["uses"]
		if hasRun == hasUses {
			return nil, util.NewInvalidArgumentErrorf("%s: step %d needs either run or uses", SetupFile, i+1)
		}
	}
	for name, server := range setup.MCPServers {
		if err := validateMCPServer(name, server); err != nil {
			return nil, err
		}
	}
	return setup, nil
}

func hasExpression(v any) bool {
	switch v := v.(type) {
	case string:
		return strings.Contains(v, "${{")
	case []any:
		return slices.ContainsFunc(v, hasExpression)
	case map[string]any:
		for key, value := range v {
			if hasExpression(key) || hasExpression(value) {
				return true
			}
		}
	case map[any]any:
		for key, value := range v {
			if hasExpression(key) || hasExpression(value) {
				return true
			}
		}
	}
	return false
}
