// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package structs

import "time"

// DependencyAlert is a known vulnerability affecting a dependency of a repository
type DependencyAlert struct {
	ID int64 `json:"id"`
	// open, fixed or dismissed
	State string `json:"state"`
	// critical, high, moderate, low or unknown
	Severity      string   `json:"severity"`
	Score         float64  `json:"score"`
	VulnID        string   `json:"vuln_id"`
	Aliases       []string `json:"aliases"`
	Summary       string   `json:"summary"`
	Ecosystem     string   `json:"ecosystem"`
	Package       string   `json:"package"`
	Version       string   `json:"version"`
	Manifest      string   `json:"manifest"`
	FixedVersions []string `json:"fixed_versions"`
	// why it was dismissed
	DismissedReason string `json:"dismissed_reason,omitempty"`
	HTMLURL         string `json:"html_url"`
	// swagger:strfmt date-time
	Created time.Time `json:"created_at"`
}

// EditDependencyAlertOption dismisses or reopens a dependency alert
type EditDependencyAlertOption struct {
	// dismissed or open
	// required: true
	State string `json:"state"`
	// why the alert is not a risk, required to dismiss it
	Reason string `json:"reason"`
}

// Dependency is a package version a manifest or lockfile of the default branch pins
type Dependency struct {
	Ecosystem string `json:"ecosystem"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	Manifest  string `json:"manifest"`
}

// RefusedDependencyVersion is a package version the dependency updates no longer propose, as its pull request was closed
type RefusedDependencyVersion struct {
	Ecosystem string `json:"ecosystem"`
	Package   string `json:"package"`
	Version   string `json:"version"`
}

// DependencyUpdates is how an agent updates the dependencies of a repository
type DependencyUpdates struct {
	// mention name of the update agent, empty when the updates are off
	Agent string `json:"agent"`
	// user whose API key and permissions the updates use
	Updater string `json:"updater"`
	// daily, weekly or monthly
	Interval string `json:"interval"`
	// whether major versions are included
	Major bool `json:"major"`
	// package name patterns to leave as they are, optionally followed by @no-major or @ and versions to skip
	Ignore []string `json:"ignore"`
	// all or security
	Scope string `json:"scope"`
	// whether a scan opening a high or critical alert starts the updates of its ecosystem
	OnAlert bool                        `json:"on_alert"`
	Refused []*RefusedDependencyVersion `json:"refused"`
	// why the latest updates failed
	LastError string `json:"last_error"`
	// swagger:strfmt date-time
	LastRun *time.Time `json:"last_run,omitempty"`
}

// DependencyUpdatesOption changes how an agent updates the dependencies of a repository, the doer becomes the updater
type DependencyUpdatesOption struct {
	// mention name of the update agent, empty to turn the updates off
	Agent *string `json:"agent"`
	// daily, weekly or monthly
	Interval *string  `json:"interval"`
	Major    *bool    `json:"major"`
	Ignore   []string `json:"ignore"`
	// all or security
	Scope   *string `json:"scope"`
	OnAlert *bool   `json:"on_alert"`
	// refused versions the updates may propose again
	AllowRefused []*RefusedDependencyVersion `json:"allow_refused"`
	// become the updater, with your API key, even when nothing else changes
	TakeOver bool `json:"take_over"`
}

// DependencyUpdatesRun is how many ecosystems the updates started
type DependencyUpdatesRun struct {
	Started int `json:"started"`
}
