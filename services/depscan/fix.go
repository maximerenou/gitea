// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package depscan

import (
	"context"
	"fmt"
	"strings"

	agents_model "gitea.dev/models/agents"
	depscan_model "gitea.dev/models/depscan"
	repo_model "gitea.dev/models/repo"
	user_model "gitea.dev/models/user"
	agents_service "gitea.dev/services/agents"
)

// FixWithAgent asks an agent to upgrade the vulnerable dependency in a pull request, like a Dependabot security update,
// with the key and the permissions of the user asking
func FixWithAgent(ctx context.Context, doer *user_model.User, repo *repo_model.Repository, a *depscan_model.Alert, agent *agents_model.Agent) (*agents_model.Session, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "Fix the vulnerability %s in the %s package %s %s used by %s.\n", a.Title(), a.Ecosystem, a.Package, a.Version, a.Manifest)
	if a.Summary != "" {
		fmt.Fprintf(&b, "\n%s\n", a.Summary)
	}
	if len(a.FixedVersions) > 0 {
		fmt.Fprintf(&b, "\nIt is fixed in %s: upgrade to the closest fixed version compatible with the project.", strings.Join(a.FixedVersions, ", "))
	} else {
		b.WriteString("\nNo fixed version is published: upgrade if a newer release is not affected, otherwise mitigate it or explain why it cannot be fixed yet.")
	}
	b.WriteString(" Update the manifest and regenerate its lockfile with the package manager, do not edit lockfiles by hand, then run the relevant tests.")
	return agents_service.StartTaskSession(ctx, doer, repo, agents_service.StartOptions{Agent: agent, Prompt: b.String()})
}
