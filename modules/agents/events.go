// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"fmt"
	"slices"
)

type Action string

const (
	ActionAnalyze      Action = "analyze"
	ActionManagePR     Action = "manage_pr"
	ActionReview       Action = "review"
	ActionSecurityScan Action = "security_scan"
)

type Event string

const (
	EventAll                   Event = "all"
	EventIssueCreated          Event = "issue_created"
	EventAssigned              Event = "assigned"
	EventMentioned             Event = "mentioned"
	EventReplied               Event = "replied"
	EventDefaultBranchCIFailed Event = "default_branch_ci_failed"
	EventReviewSubmitted       Event = "review_submitted"
	EventReviewComment         Event = "review_comment"
	EventNewFinding            Event = "new_finding"
	EventRequestedOnFinding    Event = "requested_on_finding"
	EventConfirmedFinding      Event = "confirmed_finding"
	EventPRCreated             Event = "pr_created"
	EventReviewRequested       Event = "review_requested"
	EventScheduledScan         Event = "scheduled_scan"
	EventPushDefaultBranch     Event = "push_default_branch"
)

var acceptedEvents = map[Action][]Event{
	ActionAnalyze: {EventIssueCreated, EventAssigned, EventMentioned, EventReplied, EventDefaultBranchCIFailed},
	ActionManagePR: {
		EventIssueCreated, EventAssigned, EventMentioned, EventReplied, EventReviewSubmitted, EventReviewComment,
		EventNewFinding, EventRequestedOnFinding, EventConfirmedFinding, EventDefaultBranchCIFailed,
	},
	ActionReview: {EventPRCreated, EventReviewRequested, EventAssigned, EventMentioned},
	ActionSecurityScan: {
		EventScheduledScan, EventPushDefaultBranch, EventPRCreated, EventReviewRequested, EventAssigned,
		EventMentioned, EventNewFinding, EventRequestedOnFinding,
	},
}

// Actions lists the actions in display order.
func Actions() []Action {
	return []Action{ActionAnalyze, ActionManagePR, ActionReview, ActionSecurityScan}
}

func (a Action) IsValid() bool {
	_, ok := acceptedEvents[a]
	return ok
}

// Role is the label shown in the agent list.
func (a Action) Role() string {
	switch a {
	case ActionAnalyze:
		return "Plan"
	case ActionManagePR:
		return "Code"
	case ActionReview:
		return "Review"
	case ActionSecurityScan:
		return "Security"
	}
	return ""
}

func (a Action) AcceptedEvents() []Event {
	return slices.Clone(acceptedEvents[a])
}

// ValidateEvents checks a configured event list: at least one event, each accepted by the action ("all" is always accepted).
func ValidateEvents(action Action, events []Event) error {
	if !action.IsValid() {
		return fmt.Errorf("ValidateEvents: unknown action %q", action)
	}
	if len(events) == 0 {
		return fmt.Errorf("ValidateEvents: at least one event is required")
	}
	for _, e := range events {
		if e != EventAll && !slices.Contains(acceptedEvents[action], e) {
			return fmt.Errorf("ValidateEvents: action %q does not accept event %q", action, e)
		}
	}
	return nil
}

// EffectiveEvents expands "all" and drops events the action does not accept (stored data may predate a rule change).
func EffectiveEvents(action Action, events []Event) []Event {
	if slices.Contains(events, EventAll) {
		return action.AcceptedEvents()
	}
	return slices.DeleteFunc(slices.Clone(events), func(e Event) bool {
		return !slices.Contains(acceptedEvents[action], e)
	})
}

// Accepts reports whether a triggered event should run for an agent configured with the given events.
func Accepts(action Action, events []Event, event Event) bool {
	return slices.Contains(EffectiveEvents(action, events), event)
}
