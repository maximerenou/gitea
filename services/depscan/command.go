// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package depscan

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	agents_model "gitea.dev/models/agents"
	audit_model "gitea.dev/models/audit"
	depscan_model "gitea.dev/models/depscan"
	issues_model "gitea.dev/models/issues"
	access_model "gitea.dev/models/perm/access"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/container"
	"gitea.dev/modules/globallock"
	"gitea.dev/modules/log"
	"gitea.dev/services/audit"
	issue_service "gitea.dev/services/issue"
)

var ignoreCommand = regexp.MustCompile(`(?im)^[ \t]*@([\w.-]+)[ \t]+ignore[ \t]+(\S.*?)[ \t]*$`)

// handleIgnoreCommand adds the entries of "@agent ignore <entry>" lines in an update pull request to the ignore list,
// so the next update brings these packages back instead of starting a run on the comment
func handleIgnoreCommand(ctx context.Context, doer *user_model.User, agent *agents_model.Agent, issue *issues_model.Issue, content string) bool {
	var entries []string
	for _, m := range ignoreCommand.FindAllStringSubmatch(content, -1) {
		if strings.EqualFold(m[1], agent.MentionName()) && !slices.Contains(entries, m[2]) {
			entries = append(entries, m[2])
		}
	}
	if len(entries) == 0 || !issue.IsPull || issue.LoadPullRequest(ctx) != nil || issue.LoadRepo(ctx) != nil {
		return false
	}
	pr := issue.PullRequest
	var reply string
	handled := false
	err := globallock.LockAndDo(ctx, updatesLockKey(issue.RepoID), func(ctx context.Context) error {
		rs, err := depscan_model.GetRepoSetting(ctx, issue.RepoID)
		if err != nil || rs.UpdatesAgentID != agent.ID || pr.HeadRepoID != pr.BaseRepoID {
			return err
		}
		ecosystem, err := updateSessionEcosystem(ctx, rs, pr.HeadBranch)
		if err != nil || ecosystem == "" {
			return err
		}
		handled = true
		if perm, err := access_model.GetDoerRepoPermission(ctx, issue.Repo, doer); err != nil {
			return err
		} else if !perm.IsAdmin() {
			reply = "only the administrators of this repository can change the ignored dependencies."
			return nil
		}
		for _, entry := range entries {
			if _, err := parseIgnoreRule(entry); err != nil {
				reply = err.Error() + ", nothing was ignored."
				return nil
			}
		}
		added := slices.DeleteFunc(slices.Clone(entries), func(e string) bool { return slices.Contains(rs.UpdatesIgnore, e) })
		rs.UpdatesIgnore = append(rs.UpdatesIgnore, added...)
		if err := depscan_model.SaveRepoSetting(ctx, rs, "updates_ignore"); err != nil {
			return err
		}
		audit.RecordAs(ctx, doer, audit_model.RepositoryDependencyUpdates, issue.Repo, "agent", agent.MentionName(), "interval", string(rs.UpdatesInterval),
			"major", rs.UpdatesMajor, "ignore", strings.Join(rs.UpdatesIgnore, ", "), "scope", string(rs.UpdatesScope), "on_alert", rs.UpdatesOnAlert)
		reply = fmt.Sprintf("the dependency updates now ignore `%s`.", strings.Join(entries, "`, `"))
		if doer.ID != rs.UpdatesUserID { // the updates run with the updater's key, only they start them out of schedule
			reply += " The next update brings this pull request in line."
			return nil
		}
		if started, err := startUpdates(ctx, issue.Repo, rs, updateRun{ecosystems: container.SetOf(ecosystem)}); err != nil {
			reply += fmt.Sprintf(" I could not update this pull request: %v", err)
		} else if started > 0 {
			reply += " I am bringing this pull request in line."
		}
		return nil
	})
	if err != nil {
		log.Error("depscan: ignore command on pull request %d: %v", pr.ID, err)
		return handled
	}
	if reply != "" {
		if _, err := issue_service.CreateIssueComment(ctx, agent.Bot, issue.Repo, issue, fmt.Sprintf("@%s %s", doer.Name, reply), nil); err != nil {
			log.Warn("depscan: reply to the ignore command on pull request %d: %v", pr.ID, err)
		}
	}
	return handled
}
