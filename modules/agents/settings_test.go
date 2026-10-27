// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSettingsBounds(t *testing.T) {
	def := DefaultSettings()
	assert.NoError(t, def.Validate())
	assert.False(t, def.Enabled)

	cases := map[string]func(s *Settings){
		"RunsPerIssuePerHour":  func(s *Settings) { s.RunsPerIssuePerHour = 0 },
		"MaxAgentRoundTrips":   func(s *Settings) { s.MaxAgentRoundTrips = 51 },
		"AutoRetries":          func(s *Settings) { s.AutoRetries = 21 },
		"FailureMemoryHours":   func(s *Settings) { s.FailureMemoryHours = 169 },
		"QueueTTLHours":        func(s *Settings) { s.QueueTTLHours = 0 },
		"CIRepairsPerCommit":   func(s *Settings) { s.CIRepairsPerCommit = 21 },
		"BlockedReminderHours": func(s *Settings) { s.BlockedReminderHours = 23 },
		"BlockedExpiryHours":   func(s *Settings) { s.BlockedExpiryHours = 8761 },
	}
	for name, mutate := range cases {
		s := DefaultSettings()
		mutate(&s)
		assert.ErrorContains(t, s.Validate(), name, name)
		s.Clamp()
		assert.NoError(t, s.Validate(), name)
	}

	s := DefaultSettings()
	s.BlockedExpiryHours = 0 // disabled is valid and must survive clamping
	assert.NoError(t, s.Validate())
	s.Clamp()
	assert.Equal(t, 0, s.BlockedExpiryHours)
	s.BlockedExpiryHours = 5
	s.Clamp()
	assert.Equal(t, 24, s.BlockedExpiryHours)

	s = DefaultSettings()
	s.RunnerLabel = " "
	assert.Error(t, s.Validate())
	s.Clamp()
	assert.Equal(t, "ubuntu-latest", s.RunnerLabel)
}

func TestEvents(t *testing.T) {
	assert.NoError(t, ValidateEvents(ActionReview, []Event{EventPRCreated, EventMentioned}))
	assert.NoError(t, ValidateEvents(ActionReview, []Event{EventAll}))
	assert.Error(t, ValidateEvents(ActionReview, nil))
	assert.Error(t, ValidateEvents(ActionReview, []Event{EventReplied}))
	assert.Error(t, ValidateEvents(ActionAnalyze, []Event{EventPRCreated}))
	assert.Error(t, ValidateEvents(Action("nope"), []Event{EventAll}))

	assert.ElementsMatch(t, ActionAnalyze.AcceptedEvents(), EffectiveEvents(ActionAnalyze, []Event{EventAll}))
	// a stored event the action no longer accepts is filtered out at run time
	assert.Equal(t, []Event{EventMentioned}, EffectiveEvents(ActionReview, []Event{EventMentioned, EventReplied}))
	assert.False(t, Accepts(ActionReview, []Event{EventMentioned}, EventReplied))
	assert.True(t, Accepts(ActionSecurityScan, []Event{EventAll}, EventScheduledScan))

	for _, a := range Actions() {
		assert.NotEmpty(t, a.Role())
	}
}

func TestHelperSums(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	got, err := ParseHelperSums("linux-amd64=" + sum + "\n\n linux-arm64 = " + strings.ToUpper(sum) + " \n")
	assert.NoError(t, err)
	assert.Equal(t, map[string]string{"linux-amd64": sum, "linux-arm64": sum}, got)
	assert.Equal(t, "linux-amd64="+sum+"\nlinux-arm64="+sum+"\n", FormatHelperSums(got))

	for _, bad := range []string{"linux-amd64", "linux-amd64=abc", "=" + sum, "linux-amd64=" + strings.Repeat("zz", 32)} {
		_, err = ParseHelperSums(bad)
		assert.Error(t, err, bad)
	}
	empty, err := ParseHelperSums("")
	assert.NoError(t, err)
	assert.Empty(t, empty)
}
