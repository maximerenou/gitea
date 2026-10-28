// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package structs

import "time"

// Agent is a mentionable Codex agent defined by the site administrator
type Agent struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	// the bot account to mention
	MentionName string `json:"mention_name"`
	Model       string `json:"model"`
	// whether the authenticated user has an API key for the agent, runs always use the key of whoever triggers them
	HasKey bool `json:"has_key"`
}

// AgentSession is the conversation of an agent on an issue, a pull request or a task
type AgentSession struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
	// task, issue or pull
	Kind string `json:"kind"`
	// queued, waiting, running, success, failure or cancelled
	Status     string `json:"status"`
	Agent      string `json:"agent"`
	Creator    *User  `json:"creator"`
	Branch     string `json:"branch"`
	BaseBranch string `json:"base_branch"`
	// the issue or pull request the session talks in
	IssueNumber int64  `json:"issue_number,omitempty"`
	HTMLURL     string `json:"html_url"`
	// swagger:strfmt date-time
	Created time.Time `json:"created_at"`
	// swagger:strfmt date-time
	Updated time.Time `json:"updated_at"`
}

// AgentRun is one Codex execution of a session
type AgentRun struct {
	ID          int64  `json:"id"`
	Status      string `json:"status"`
	Prompt      string `json:"prompt"`
	TriggerUser *User  `json:"trigger_user"`
	Summary     string `json:"summary"`
	Error       string `json:"error"`
	// set when the run could not push, with the reason
	ReadOnlyReason string `json:"read_only_reason"`
	// swagger:strfmt date-time
	Created time.Time `json:"created_at"`
	// swagger:strfmt date-time
	Started *time.Time `json:"started_at,omitempty"`
	// swagger:strfmt date-time
	Stopped *time.Time `json:"stopped_at,omitempty"`
}

// CreateAgentSessionOption starts a development session, it runs with the authenticated user's key and permissions
type CreateAgentSessionOption struct {
	// the mention name of the agent
	// required: true
	Agent string `json:"agent" binding:"Required"`
	// the task
	// required: true
	Prompt string `json:"prompt" binding:"Required"`
	// defaults to the default branch
	BaseBranch string `json:"base_branch"`
	// minimal, low, medium or high, defaults to the agent's
	ReasoningEffort string `json:"reasoning_effort"`
}

// AgentInstructionOption sends new instructions to a session
type AgentInstructionOption struct {
	// required: true
	Prompt string `json:"prompt" binding:"Required"`
}
