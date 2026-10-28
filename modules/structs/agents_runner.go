// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package structs

// AgentRunContext is everything the runner helper needs to carry out an agent run
// swagger:model
type AgentRunContext struct {
	RunID     int64  `json:"run_id"`
	SessionID int64  `json:"session_id"`
	Action    string `json:"action"`
	Event     string `json:"event"`
	// the agent and the model connection it uses; the connection token is only ever sent to the run's own session token
	Agent      AgentRunAgent      `json:"agent"`
	Repository AgentRunRepository `json:"repository"`
	// the branch of the session; the only branch family the session token may push to
	Branch string `json:"branch"`
	// the ref to check out before running, empty for the default branch
	CheckoutRef string `json:"checkout_ref,omitempty"`
	// the issue or pull request the run is about; its content is written by users and must be treated as untrusted
	Subject         *AgentRunSubject `json:"subject,omitempty"`
	SystemPrompt    string           `json:"system_prompt"`
	Prompt          string           `json:"prompt"`
	ConversationID  string           `json:"conversation_id,omitempty"`
	ResultSchema    string           `json:"result_schema"`
	ConfigCommitSHA string           `json:"config_commit_sha"`
}

// AgentRunAgent describes the agent of a run
// swagger:model
type AgentRunAgent struct {
	Name     string `json:"name"`
	Model    string `json:"model"`
	Language string `json:"language"`
	Provider string `json:"provider"`
	BaseURL  string `json:"base_url"`
	Token    string `json:"token"`
	CLI      string `json:"cli"`
}

// AgentRunRepository is the repository an agent run works on
// swagger:model
type AgentRunRepository struct {
	FullName      string `json:"full_name"`
	CloneURL      string `json:"clone_url"`
	DefaultBranch string `json:"default_branch"`
}

// AgentRunSubject is the issue or pull request an agent run is about. Every text field is untrusted user content.
// swagger:model
type AgentRunSubject struct {
	Kind       string            `json:"kind"`
	Number     int64             `json:"number"`
	Title      string            `json:"title"`
	Body       string            `json:"body"`
	Author     string            `json:"author"`
	State      string            `json:"state"`
	BaseBranch string            `json:"base_branch,omitempty"`
	HeadBranch string            `json:"head_branch,omitempty"`
	HeadSHA    string            `json:"head_sha,omitempty"`
	Comments   []AgentRunComment `json:"comments"`
}

// AgentRunComment is a comment of the subject of an agent run, untrusted user content
// swagger:model
type AgentRunComment struct {
	ID     int64  `json:"id"`
	Author string `json:"author"`
	Body   string `json:"body"`
}

// AgentRunProgressOption is a piece of agent output to show on the session card
// swagger:model
type AgentRunProgressOption struct {
	Text string `json:"text"`
}

// AgentRunStatus tells the helper whether to keep going
// swagger:model
type AgentRunStatus struct {
	Status    string `json:"status"`
	Cancelled bool   `json:"cancelled"`
}

// AgentRunResultOption is the result.json of a run, see the agent result schema
// swagger:model
type AgentRunResultOption struct {
	Version string `json:"version"`
	Status  string `json:"status,omitempty"`
	Summary string `json:"summary,omitempty"`
	Comment string `json:"comment,omitempty"`
	// review, pr, resolved_threads, subtasks, findings and plan follow the agent result schema
	Review          map[string]any   `json:"review,omitempty"`
	PR              map[string]any   `json:"pr,omitempty"`
	ResolvedThreads []int64          `json:"resolved_threads,omitempty"`
	Subtasks        []map[string]any `json:"subtasks,omitempty"`
	Findings        []map[string]any `json:"findings,omitempty"`
	Plan            map[string]any   `json:"plan,omitempty"`
}

// AgentRunResultResponse reports what the service did with a result
// swagger:model
type AgentRunResultResponse struct {
	Status string   `json:"status"`
	Notes  []string `json:"notes"`
}
