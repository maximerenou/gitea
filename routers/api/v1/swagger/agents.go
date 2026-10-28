// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package swagger

import api "gitea.dev/modules/structs"

// AgentRunContext
// swagger:response AgentRunContext
type swaggerResponseAgentRunContext struct {
	// in:body
	Body api.AgentRunContext `json:"body"`
}

// AgentRunStatus
// swagger:response AgentRunStatus
type swaggerResponseAgentRunStatus struct {
	// in:body
	Body api.AgentRunStatus `json:"body"`
}

// AgentRunResultResponse
// swagger:response AgentRunResultResponse
type swaggerResponseAgentRunResultResponse struct {
	// in:body
	Body api.AgentRunResultResponse `json:"body"`
}
