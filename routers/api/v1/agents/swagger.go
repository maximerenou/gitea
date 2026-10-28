// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	api "gitea.dev/modules/structs"
)

// AgentList
// swagger:response AgentList
type swaggerAgentList struct {
	// in:body
	Body []api.Agent `json:"body"`
}

// AgentSession
// swagger:response AgentSession
type swaggerAgentSession struct {
	// in:body
	Body api.AgentSession `json:"body"`
}

// AgentSessionList
// swagger:response AgentSessionList
type swaggerAgentSessionList struct {
	// in:body
	Body []api.AgentSession `json:"body"`
}

// AgentRun
// swagger:response AgentRun
type swaggerAgentRun struct {
	// in:body
	Body api.AgentRun `json:"body"`
}

// AgentRunList
// swagger:response AgentRunList
type swaggerAgentRunList struct {
	// in:body
	Body []api.AgentRun `json:"body"`
}

// swagger:parameters agentCreateSession
type swaggerAgentCreateSessionBody struct {
	// in:body
	Body api.CreateAgentSessionOption
}

// swagger:parameters agentSendInstruction
type swaggerAgentSendInstructionBody struct {
	// in:body
	Body api.AgentInstructionOption
}

// AgentEventList
// swagger:response AgentEventList
type swaggerAgentEventList struct {
	// in:body
	Body []api.AgentEvent `json:"body"`
}

// swagger:parameters agentReviewPullRequest
type swaggerAgentReviewBody struct {
	// in:body
	Body api.AgentReviewOption
}

// swagger:parameters agentMergePullRequest
type swaggerAgentMergeBody struct {
	// in:body
	Body api.AgentMergeOption
}
