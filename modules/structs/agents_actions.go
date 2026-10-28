// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package structs

import "time"

// AgentReviewOption asks an agent to review a pull request, it runs with the authenticated user's key and permissions
type AgentReviewOption struct {
	// the mention name of the agent
	// required: true
	Agent string `json:"agent" binding:"Required"`
}

// AgentMergeOption asks an agent to merge the target branch into a pull request, then schedules its merge
type AgentMergeOption struct {
	// the mention name of the agent
	// required: true
	Agent string `json:"agent" binding:"Required"`
	// merge, rebase, rebase-merge, squash or fast-forward-only, defaults to the repository's default
	Style string `json:"style"`
	// whether the agent reviews the pull request first, its verdict can then block the merge
	Review bool `json:"review"`
}

// AgentEvent is a Codex CLI event of a run, as the agent reported it
type AgentEvent struct {
	ID    int64  `json:"id"`
	RunID int64  `json:"run_id"`
	Type  string `json:"type"`
	// the Codex JSONL event
	Payload string `json:"payload"`
	// swagger:strfmt date-time
	Created time.Time `json:"created_at"`
}
