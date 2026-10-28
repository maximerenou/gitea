// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package audit

// Codex agents, kept apart from action.go so the fork rebases cleanly
var (
	SystemAgentAdd            = define("system:agent:add", "Added agent {agent}.")
	SystemAgentUpdate         = define("system:agent:update", "Updated agent {agent}.")
	SystemAgentRemove         = define("system:agent:remove", "Removed agent {agent}.")
	UserAgentKeySet           = define("user:agentkey:set", "Set the API key of user {scope} for agent {agent}.")
	UserAgentKeyRemove        = define("user:agentkey:remove", "Removed the API key of user {scope} for agent {agent}.")
	RepositoryAgentSettings   = define("repository:agents:update", "Changed the agents settings of repository {scope}: enabled {enabled}, allowed agents {agents}.")
	RepositoryAgentSessionRun = define("repository:agents:run", "Queued a run of agent {agent} in session {session} of repository {scope}.")
)
