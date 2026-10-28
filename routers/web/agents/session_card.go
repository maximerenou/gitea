// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"fmt"
	"strings"

	actions_model "gitea.dev/models/actions"
	agents_model "gitea.dev/models/agents"
	issues_model "gitea.dev/models/issues"
	"gitea.dev/models/unit"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/agents"
	"gitea.dev/modules/log"
	"gitea.dev/services/context"
)

// SessionCard is what the issue and pull request page shows for one run of an agent.
type SessionCard struct {
	AgentName   string
	Role        string
	Status      agents_model.RunStatus
	BadgeColor  string
	Trigger     string
	Summary     string
	Error       string
	Notes       []string
	Progress    string
	ActionsLink string
	IsActive    bool
}

// PrepareIssueView puts the cards of the issue's agent runs in the page data. It does nothing while agents are disabled.
func PrepareIssueView(ctx *context.Context, issue *issues_model.Issue) {
	if !agents.IsEnabled(ctx) {
		return
	}
	runs, err := agents_model.ListRunsOfIssue(ctx, issue.ID)
	if err != nil {
		log.Error("agents: ListRunsOfIssue: %v", err)
		return
	}
	if len(runs) == 0 {
		return
	}
	canSeeOutput := ctx.Repo.Permission.CanWrite(unit.TypeCode) // the live output is for people who can command the agent
	names := map[int64]*agents_model.Agent{}
	cards := make([]*SessionCard, 0, len(runs))
	for _, run := range runs {
		agent := names[run.AgentID]
		if agent == nil {
			if agent, err = agents_model.GetAgentByID(ctx, run.AgentID); err != nil {
				continue // the agent was deleted: its history is not shown
			}
			names[run.AgentID] = agent
		}
		card := &SessionCard{
			AgentName: agent.Name, Role: agent.Action.Role(), Status: run.Status, BadgeColor: badgeColor(run.Status),
			Summary: run.Summary, Error: run.Error, IsActive: !run.Status.IsTerminal(),
		}
		if run.Notes != "" {
			card.Notes = strings.Split(run.Notes, "\n")
		}
		if canSeeOutput {
			card.Progress = run.Progress
		}
		if trigger, err := user_model.GetUserByID(ctx, run.TriggerUserID); err == nil {
			card.Trigger = trigger.Name
		}
		if run.ActionRunID != 0 {
			if ar, err := actions_model.GetRunByRepoAndID(ctx, run.RepoID, run.ActionRunID); err == nil {
				card.ActionsLink = fmt.Sprintf("%s/actions/runs/%d", ctx.Repo.RepoLink, ar.Index)
			}
		}
		cards = append(cards, card)
	}
	ctx.Data["AgentSessionCards"] = cards
}

func badgeColor(s agents_model.RunStatus) string {
	switch s {
	case agents_model.RunSuccess:
		return "tw-bg-green"
	case agents_model.RunFailed:
		return "tw-bg-red"
	case agents_model.RunNeedsHuman:
		return "tw-bg-orange"
	case agents_model.RunCancelled:
		return "tw-bg-grey"
	}
	return "tw-bg-yellow"
}
