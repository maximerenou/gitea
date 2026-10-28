// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"fmt"

	"gitea.dev/services/pubsub"
)

// SessionTopic is the pubsub topic telling the open pages of a session that it changed
func SessionTopic(sessionID int64) string {
	return fmt.Sprintf("agent-session-%d", sessionID)
}

// RepoTopic is the pubsub topic telling the Agents tab of a repository that a session started or changed status
func RepoTopic(repoID int64) string {
	return fmt.Sprintf("agent-repo-%d", repoID)
}

// notifySession signals a change, the pages then fetch what changed, so nothing of the session crosses the broker
func notifySession(sessionID int64) {
	publish(SessionTopic(sessionID))
}

func notifyRepo(repoID int64) {
	publish(RepoTopic(repoID))
}

func publish(topic string) {
	if pubsub.DefaultBroker == nil || !pubsub.DefaultBroker.HasTopicSubscribers(topic) {
		return
	}
	pubsub.DefaultBroker.Publish(topic, []byte("changed"))
}
