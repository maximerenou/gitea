// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"gitea.dev/modules/setting/config"
)

// Settings are the instance-wide agent settings, stored in the database as one JSON value.
type Settings struct {
	Enabled bool

	RunnerLabel          string
	RunsPerIssuePerHour  int
	MaxAgentRoundTrips   int
	AutoRetries          int
	FailureMemoryHours   int
	QueueTTLHours        int
	CheckoutAction       string
	CIRepairsPerCommit   int
	BlockedReminderHours int
	BlockedExpiryHours   int // 0 disables expiry
	JobImage             string

	HelperDownloadURL string
	HelperSHA256      map[string]string // "<os>-<arch>" => hex digest; when set, the runner verifies the binary
}

func DefaultSettings() Settings {
	return Settings{
		RunnerLabel:          "ubuntu-latest",
		RunsPerIssuePerHour:  200,
		MaxAgentRoundTrips:   20,
		AutoRetries:          0,
		FailureMemoryHours:   1,
		QueueTTLHours:        24,
		CheckoutAction:       "actions/checkout@v4",
		CIRepairsPerCommit:   3,
		BlockedReminderHours: 168,
		BlockedExpiryHours:   0,
		JobImage:             "gitea-agents-runner:latest",
	}
}

type intBound struct {
	name     string
	value    *int
	min, max int
	zeroOK   bool
}

func (s *Settings) bounds() []intBound {
	return []intBound{
		{"RunsPerIssuePerHour", &s.RunsPerIssuePerHour, 1, 1000, false},
		{"MaxAgentRoundTrips", &s.MaxAgentRoundTrips, 1, 50, false},
		{"AutoRetries", &s.AutoRetries, 0, 20, false},
		{"FailureMemoryHours", &s.FailureMemoryHours, 1, 168, false},
		{"QueueTTLHours", &s.QueueTTLHours, 1, 168, false},
		{"CIRepairsPerCommit", &s.CIRepairsPerCommit, 1, 20, false},
		{"BlockedReminderHours", &s.BlockedReminderHours, 24, 720, false},
		{"BlockedExpiryHours", &s.BlockedExpiryHours, 24, 8760, true},
	}
}

// Validate reports the first out-of-range or empty value; used when an administrator saves the form.
func (s *Settings) Validate() error {
	for _, b := range s.bounds() {
		v := *b.value
		if (b.zeroOK && v == 0) || (v >= b.min && v <= b.max) {
			continue
		}
		return fmt.Errorf("%s must be between %d and %d", b.name, b.min, b.max)
	}
	if strings.TrimSpace(s.RunnerLabel) == "" || strings.TrimSpace(s.CheckoutAction) == "" || strings.TrimSpace(s.JobImage) == "" {
		return fmt.Errorf("runner label, checkout action and job image must not be empty")
	}
	if !safeForWorkflow(s.RunnerLabel) || !safeForWorkflow(s.CheckoutAction) || !safeForWorkflow(s.JobImage) || !safeForWorkflow(s.HelperDownloadURL) {
		return fmt.Errorf("runner label, checkout action, job image and helper URL must be a single line without ${{ }} expressions")
	}
	for platform, sum := range s.HelperSHA256 {
		if !validHelperPlatform(platform) || !validSHA256(sum) {
			return fmt.Errorf("invalid helper checksum entry %q", platform)
		}
	}
	return nil
}

func validHelperPlatform(p string) bool {
	return p != "" && len(p) <= 32 && strings.Trim(p, "abcdefghijklmnopqrstuvwxyz0123456789_-.") == ""
}

func validSHA256(sum string) bool {
	return len(sum) == 64 && strings.Trim(sum, "0123456789abcdef") == ""
}

// safeForWorkflow guards the values written into the generated workflow: the runner evaluates ${{ }} expressions
// in them, and a line break could not be part of a single scalar.
func safeForWorkflow(v string) bool {
	return !strings.ContainsAny(v, "\r\n") && !strings.Contains(v, "${{") && !strings.Contains(v, "\x00")
}

// Clamp forces every value into its bounds so a hand-edited database value cannot disable a guardrail.
func (s *Settings) Clamp() {
	def := DefaultSettings()
	for _, b := range s.bounds() {
		switch {
		case b.zeroOK && *b.value == 0:
		default:
			*b.value = min(max(*b.value, b.min), b.max)
		}
	}
	if strings.TrimSpace(s.RunnerLabel) == "" || !safeForWorkflow(s.RunnerLabel) {
		s.RunnerLabel = def.RunnerLabel
	}
	if strings.TrimSpace(s.CheckoutAction) == "" || !safeForWorkflow(s.CheckoutAction) {
		s.CheckoutAction = def.CheckoutAction
	}
	if strings.TrimSpace(s.JobImage) == "" || !safeForWorkflow(s.JobImage) {
		s.JobImage = def.JobImage
	}
	if !safeForWorkflow(s.HelperDownloadURL) {
		s.HelperDownloadURL = ""
	}
	for platform, sum := range s.HelperSHA256 {
		if !validHelperPlatform(platform) || !validSHA256(sum) {
			delete(s.HelperSHA256, platform)
		}
	}
}

// the option registers itself under its key, so no change to modules/setting is needed
var settingsOption = config.NewOption[Settings]("agents.settings").WithDefaultFunc(DefaultSettings)

func GetSettings(ctx context.Context) Settings {
	s := settingsOption.Value(ctx)
	s.Clamp()
	return s
}

func IsEnabled(ctx context.Context) bool {
	return GetSettings(ctx).Enabled
}

// SettingsKey is the system_setting key holding the JSON-encoded Settings.
func SettingsKey() string {
	return settingsOption.DynKey()
}

// ParseHelperSums parses one "<os>-<arch>=<sha256 hex>" entry per line.
func ParseHelperSums(text string) (map[string]string, error) {
	sums := map[string]string{}
	for line := range strings.Lines(text) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		platform, sum, ok := strings.Cut(line, "=")
		platform, sum = strings.TrimSpace(platform), strings.ToLower(strings.TrimSpace(sum))
		if !ok || !validHelperPlatform(platform) || !validSHA256(sum) {
			return nil, fmt.Errorf("invalid helper checksum line %q, expected <os>-<arch>=<sha256 hex>", line)
		}
		sums[platform] = sum
	}
	return sums, nil
}

func FormatHelperSums(sums map[string]string) string {
	var sb strings.Builder
	for _, platform := range slices.Sorted(maps.Keys(sums)) {
		sb.WriteString(platform + "=" + sums[platform] + "\n")
	}
	return sb.String()
}
