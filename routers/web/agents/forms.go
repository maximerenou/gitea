// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import "gitea.dev/modules/web/middleware"

type SettingsForm struct {
	middleware.FormDefaultValidator
	Enabled              bool   `form:"enabled"`
	RunnerLabel          string `form:"runner_label" binding:"MaxSize(255)"`
	RunsPerIssuePerHour  int    `form:"runs_per_issue_per_hour"`
	MaxAgentRoundTrips   int    `form:"max_agent_round_trips"`
	AutoRetries          int    `form:"auto_retries"`
	FailureMemoryHours   int    `form:"failure_memory_hours"`
	QueueTTLHours        int    `form:"queue_ttl_hours"`
	CheckoutAction       string `form:"checkout_action" binding:"MaxSize(2048)"`
	CIRepairsPerCommit   int    `form:"ci_repairs_per_commit"`
	BlockedReminderHours int    `form:"blocked_reminder_hours"`
	BlockedExpiryHours   int    `form:"blocked_expiry_hours"`
	JobImage             string `form:"job_image" binding:"MaxSize(2048)"`
	HelperDownloadURL    string `form:"helper_download_url" binding:"MaxSize(2048)"`
	HelperSHA256         string `form:"helper_sha256" binding:"MaxSize(8192)"`
}

type ConnectionForm struct {
	middleware.FormDefaultValidator
	Name       string `form:"name" binding:"Required;MaxSize(255)"`
	Provider   string `form:"provider" binding:"Required"`
	BaseURL    string `form:"base_url" binding:"Required;MaxSize(2048)"`
	Token      string `form:"token" binding:"MaxSize(8192)"`
	ClearToken bool   `form:"clear_token"`
	CLI        string `form:"cli" binding:"Required"`
	AccessMode string `form:"access_mode"`
}

type AgentForm struct {
	middleware.FormDefaultValidator
	Name         string   `form:"name" binding:"MaxSize(40)"` // only used on creation
	ConnectionID int64    `form:"connection_id" binding:"Required"`
	Model        string   `form:"model" binding:"Required;MaxSize(255)"`
	Description  string   `form:"description" binding:"MaxSize(2048)"`
	Language     string   `form:"language" binding:"MaxSize(32)"`
	Action       string   `form:"action"` // only used on creation
	Events       []string `form:"events"`
	AccessMode   string   `form:"access_mode"`
}
