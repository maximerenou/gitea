// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package depscan

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	activities_model "gitea.dev/models/activities"
	agents_model "gitea.dev/models/agents"
	audit_model "gitea.dev/models/audit"
	"gitea.dev/models/db"
	depscan_model "gitea.dev/models/depscan"
	issues_model "gitea.dev/models/issues"
	access_model "gitea.dev/models/perm/access"
	repo_model "gitea.dev/models/repo"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/container"
	"gitea.dev/modules/git"
	"gitea.dev/modules/globallock"
	"gitea.dev/modules/log"
	"gitea.dev/modules/timeutil"
	"gitea.dev/modules/util"
	agents_service "gitea.dev/services/agents"
	"gitea.dev/services/audit"
)

// outdatedCommands tell the agent how each package manager lists the outdated direct dependencies
var outdatedCommands = map[string]string{
	EcosystemGo:        "`go list -u -m -f '{{if and (not .Indirect) .Update}}{{.Path}}{{end}}' all`",
	EcosystemNpm:       "`npm outdated`, `yarn outdated` or `pnpm outdated`, depending on the lockfile",
	EcosystemPyPI:      "`pip list --outdated`, `poetry show --outdated --top-level` or `uv tree --outdated --depth 1`, depending on the project",
	EcosystemPackagist: "`composer outdated --direct`",
	EcosystemCrates:    "`cargo update --dry-run` and the versions in `Cargo.toml`",
	EcosystemRubyGems:  "`bundle outdated --only-explicit`",
}

// UpdateOptions are how a repository's administrators set up the updates of its dependencies
type UpdateOptions struct {
	Agent    *agents_model.Agent // nil turns the updates off
	Interval depscan_model.UpdatesInterval
	Major    bool
	Ignore   []string
	Scope    depscan_model.UpdatesScope
	OnAlert  bool
	TakeOver bool // use the doer's key even when nothing changed
}

func updatesLockKey(repoID int64) string {
	return fmt.Sprintf("depscan_updates_%d", repoID)
}

// checkUpdater checks that the updates can run with the doer's key
func checkUpdater(ctx context.Context, doer *user_model.User, repo *repo_model.Repository, agent *agents_model.Agent) error {
	if agent.IsDisabled || !agents_service.IsAgentAllowed(ctx, repo, agent) {
		return util.NewInvalidArgumentErrorf("@%s is not enabled in %s", agent.MentionName(), repo.FullName())
	}
	if _, err := agents_model.GetUserKey(ctx, agent.ID, doer.ID); errors.Is(err, util.ErrNotExist) {
		return agents_service.ErrNoUserKey
	} else if err != nil {
		return err
	}
	return nil
}

// SaveUpdateSettings sets up the updates of a repository's dependencies.
// Whoever changes them becomes the updater: the agent then works with their key, so nobody changes what another's key does.
func SaveUpdateSettings(ctx context.Context, doer *user_model.User, repo *repo_model.Repository, opts UpdateOptions) error {
	return globallock.LockAndDo(ctx, updatesLockKey(repo.ID), func(ctx context.Context) error {
		rs, err := depscan_model.GetRepoSetting(ctx, repo.ID)
		if err != nil {
			return err
		}
		if !slices.Contains(depscan_model.UpdatesIntervals, opts.Interval) {
			opts.Interval = depscan_model.UpdatesWeekly
		}
		if opts.Scope != depscan_model.UpdatesScopeSecurity {
			opts.Scope = depscan_model.UpdatesScopeAll
		}
		for _, entry := range opts.Ignore {
			if _, err := parseIgnoreRule(entry); err != nil {
				return err
			}
		}
		var agentID int64
		if opts.Agent != nil {
			agentID = opts.Agent.ID
		}
		changed := agentID != rs.UpdatesAgentID || opts.Interval != rs.UpdatesInterval || opts.Major != rs.UpdatesMajor || !slices.Equal(opts.Ignore, rs.UpdatesIgnore) ||
			(opts.Scope == depscan_model.UpdatesScopeSecurity) != rs.UpdatesSecurityOnly() || opts.OnAlert != rs.UpdatesOnAlert
		if !changed && !opts.TakeOver {
			return nil
		}
		if agentID != rs.UpdatesAgentID {
			rs.UpdateSessions, rs.UpdateInputs = nil, nil // the sessions of another agent are not continued
		}
		previousUpdater := rs.UpdatesUserID
		rs.UpdatesAgentID, rs.UpdatesUserID = 0, 0
		agentName := "none"
		if opts.Agent != nil {
			if err := checkUpdater(ctx, doer, repo, opts.Agent); err != nil {
				return err
			}
			rs.UpdatesAgentID, rs.UpdatesUserID = opts.Agent.ID, doer.ID
			agentName = opts.Agent.MentionName()
		}
		rs.UpdatesInterval, rs.UpdatesMajor, rs.UpdatesIgnore = opts.Interval, opts.Major, opts.Ignore
		rs.UpdatesScope, rs.UpdatesOnAlert = opts.Scope, opts.OnAlert
		rs.UpdatesLastError = "" // the next updates tell whether the change fixed it
		if err := depscan_model.SaveRepoSetting(ctx, rs, "updates_agent_id", "updates_user_id", "updates_interval", "updates_major", "updates_ignore",
			"updates_scope", "updates_on_alert", "update_sessions", "update_inputs", "updates_last_error"); err != nil {
			return err
		}
		if changed {
			audit.Record(ctx, audit_model.RepositoryDependencyUpdates, repo, "agent", agentName, "interval", string(rs.UpdatesInterval), "major", rs.UpdatesMajor,
				"ignore", strings.Join(rs.UpdatesIgnore, ", "), "scope", string(rs.UpdatesScope), "on_alert", rs.UpdatesOnAlert)
		}
		recordTakeOver(ctx, repo, previousUpdater, rs.UpdatesUserID)
		return nil
	})
}

// recordTakeOver audits that the updates now use another administrator's key
func recordTakeOver(ctx context.Context, repo *repo_model.Repository, previousUpdater, updater int64) {
	if previousUpdater == 0 || updater == 0 || previousUpdater == updater {
		return
	}
	previous := strconv.FormatInt(previousUpdater, 10)
	if _, u, err := user_model.GetPossibleUserByID(ctx, previousUpdater); err == nil {
		previous = u.Name
	}
	audit.Record(ctx, audit_model.RepositoryDependencyUpdatesTakeOver, repo, "previous_updater", previous)
}

// StartDueUpdates updates the dependencies of the repositories whose interval elapsed, it is run by the dependency_updates cron task
func StartDueUpdates(ctx context.Context) error {
	settings, err := depscan_model.FindUpdatedRepoSettings(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	for _, rs := range settings {
		// an hour of slack, so a daily task does not drift to every other day
		if now.Sub(rs.UpdatesLastUnix.AsTime()) < rs.UpdatesInterval.Duration()-time.Hour {
			continue
		}
		repo, err := repo_model.GetRepositoryByID(ctx, rs.RepoID)
		if err != nil {
			log.Warn("depscan: repository %d: %v", rs.RepoID, err)
			continue
		}
		if !IsRepoScanned(ctx, repo) {
			continue // the updates resume with the scans
		}
		if _, err := StartUpdates(ctx, repo, false); err != nil {
			log.Warn("depscan: update the dependencies of %s: %v", repo.FullName(), err)
		}
	}
	return nil
}

// fullUpdateEvery is how often an open update pull request is checked for new releases when nothing else changed
const fullUpdateEvery = 28 * 24 * time.Hour

// StartUpdates asks the repository's update agent to update the dependencies of each ecosystem it uses, in one pull request each,
// like Dependabot. The agent finds the outdated packages with the package managers, and upgrades the vulnerable ones first.
// Unless forced, an open update pull request is only updated when the default branch, the alerts or the settings changed,
// or every fullUpdateEvery. It returns how many updates it started.
func StartUpdates(ctx context.Context, repo *repo_model.Repository, force bool) (started int, err error) {
	err = globallock.LockAndDo(ctx, updatesLockKey(repo.ID), func(ctx context.Context) error {
		rs, err := depscan_model.GetRepoSetting(ctx, repo.ID)
		if err != nil {
			return err
		}
		started, err = startUpdates(ctx, repo, rs, updateRun{force: force})
		return err
	})
	return started, err
}

// startAlertUpdates starts the updates of the ecosystems where a scan opened high or critical alerts, when the
// repository asks for it. They continue the open update pull requests, which already fix the alerts they knew.
func startAlertUpdates(ctx context.Context, repo *repo_model.Repository, opened []*depscan_model.Alert) {
	ecosystems := container.Set[string]{}
	for _, a := range opened {
		if a.Severity == depscan_model.SeverityCritical || a.Severity == depscan_model.SeverityHigh {
			ecosystems.Add(a.Ecosystem)
		}
	}
	if len(ecosystems) == 0 {
		return
	}
	err := globallock.LockAndDo(ctx, updatesLockKey(repo.ID), func(ctx context.Context) error {
		rs, err := depscan_model.GetRepoSetting(ctx, repo.ID)
		if err != nil || !rs.UpdatesOnAlert || rs.UpdatesAgentID == 0 {
			return err
		}
		_, err = startUpdates(ctx, repo, rs, updateRun{ecosystems: ecosystems})
		return err
	})
	if err != nil {
		log.Warn("depscan: update the dependencies of %s after new alerts: %v", repo.FullName(), err)
	}
}

// UpdateNow starts the updates at once, the doer takes them over with their key
func UpdateNow(ctx context.Context, doer *user_model.User, repo *repo_model.Repository) (started int, err error) {
	err = globallock.LockAndDo(ctx, updatesLockKey(repo.ID), func(ctx context.Context) error {
		rs, err := depscan_model.GetRepoSetting(ctx, repo.ID)
		if err != nil {
			return err
		}
		if rs.UpdatesAgentID != 0 && rs.UpdatesUserID != doer.ID {
			agent, err := agents_model.GetAgentByID(ctx, rs.UpdatesAgentID)
			if err != nil {
				return err
			}
			if err := agent.LoadBot(ctx); err != nil {
				return err
			}
			if err := checkUpdater(ctx, doer, repo, agent); err != nil {
				return err
			}
			previousUpdater := rs.UpdatesUserID
			rs.UpdatesUserID = doer.ID
			if err := depscan_model.SaveRepoSetting(ctx, rs, "updates_user_id"); err != nil {
				return err
			}
			recordTakeOver(ctx, repo, previousUpdater, doer.ID)
		}
		started, err = startUpdates(ctx, repo, rs, updateRun{force: true})
		return err
	})
	return started, err
}

type updateRun struct {
	force bool // continue the open update pull requests even when nothing changed
	// ecosystems limits an update started by new alerts, which leaves the schedule of the full updates as it is
	ecosystems container.Set[string]
}

func startUpdates(ctx context.Context, repo *repo_model.Repository, rs *depscan_model.RepoSetting, run updateRun) (int, error) {
	if rs.UpdatesAgentID == 0 || !IsRepoScanned(ctx, repo) {
		return 0, util.NewInvalidArgumentErrorf("no agent updates the dependencies of %s", repo.FullName())
	}
	if run.ecosystems == nil {
		rs.UpdatesLastUnix = timeutil.TimeStampNow()
		if err := depscan_model.SaveRepoSetting(ctx, rs, "updates_last_unix"); err != nil {
			return 0, err
		}
	}
	started, err := runUpdates(ctx, repo, rs, run)
	saveUpdatesError(ctx, rs, err)
	return started, err
}

// saveUpdatesError keeps why the latest updates failed, for the settings page, and tells the updater once per cause
func saveUpdatesError(ctx context.Context, rs *depscan_model.RepoSetting, err error) {
	message := ""
	if err != nil {
		message = util.EllipsisDisplayString(err.Error(), 1024)
	}
	if message == rs.UpdatesLastError {
		return
	}
	rs.UpdatesLastError = message
	if err := depscan_model.SaveRepoSetting(ctx, rs, "updates_last_error"); err != nil {
		log.Error("depscan: save the updates error of repository %d: %v", rs.RepoID, err)
	}
	if message == "" || rs.UpdatesUserID == 0 {
		return
	}
	subject := util.EllipsisDisplayString("The dependency updates failed: "+message, 255)
	if err := activities_model.NotifySubject(ctx, rs.UpdatesUserID, rs.RepoID, activities_model.NotificationSourceSecurityAlert, subject, "settings/dependency-scanning", 0); err != nil {
		log.Error("depscan: notify the updater of repository %d: %v", rs.RepoID, err)
	}
}

func runUpdates(ctx context.Context, repo *repo_model.Repository, rs *depscan_model.RepoSetting, run updateRun) (int, error) {
	agent, err := agents_model.GetAgentByID(ctx, rs.UpdatesAgentID)
	if err != nil {
		return 0, err
	}
	if err := agent.LoadBot(ctx); err != nil {
		return 0, err
	}
	doer, err := user_model.GetUserByID(ctx, rs.UpdatesUserID)
	if err != nil {
		return 0, fmt.Errorf("the user who set the updates up: %w", err)
	}
	if perm, err := access_model.GetDoerRepoPermission(ctx, repo, doer); err != nil {
		return 0, err
	} else if !perm.IsAdmin() {
		return 0, util.NewPermissionDeniedErrorf("@%s, who set the updates up, no longer administers %s", doer.Name, repo.FullName())
	}
	if err := checkUpdater(ctx, doer, repo, agent); errors.Is(err, agents_service.ErrNoUserKey) {
		return 0, fmt.Errorf("@%s, who set the updates up, has no API key for @%s: %w", doer.Name, agent.MentionName(), err)
	} else if err != nil {
		return 0, err
	}

	deps, err := db.Find[depscan_model.Dependency](ctx, depscan_model.FindDependenciesOptions{ListOptions: db.ListOptionsAll, RepoID: repo.ID})
	if err != nil {
		return 0, err
	}
	manifests := map[string][]string{}
	for _, d := range deps {
		if !slices.Contains(manifests[d.Ecosystem], d.Manifest) {
			manifests[d.Ecosystem] = append(manifests[d.Ecosystem], d.Manifest)
		}
	}
	alerts, err := db.Find[depscan_model.Alert](ctx, depscan_model.FindAlertsOptions{ListOptions: db.ListOptionsAll, RepoID: repo.ID, State: depscan_model.AlertOpen})
	if err != nil {
		return 0, err
	}

	rules := updateRules{refused: rs.UpdatesRefused, ignore: parseIgnoreRules(rs.UpdatesIgnore)}
	if !rs.UpdatesMajor || rules.ignore.hasNoMajor() {
		rules.allowed = allowedLines(deps, alerts)
		if rs.UpdatesMajor { // only the packages of the no-major rules keep their line
			maps.DeleteFunc(rules.allowed, func(key string, _ versionLine) bool {
				_, name, _ := strings.Cut(key, "\x00")
				return !rules.ignore.keepsMajor(name)
			})
		}
	}
	if rs.UpdateSessions == nil {
		rs.UpdateSessions = map[string]int64{}
	}
	if rs.UpdateInputs == nil {
		rs.UpdateInputs = map[string]string{}
	}
	started := 0
	var errs []error
	for _, ecosystem := range slices.Sorted(maps.Keys(manifests)) {
		if run.ecosystems != nil && !run.ecosystems.Contains(ecosystem) {
			continue
		}
		if rs.UpdatesSecurityOnly() && len(vulnerableUpgrades(ecosystem, alerts, rules.allowed, rs)) == 0 {
			continue // an open pull request stays as it is, its upgrades may still be needed until it is merged
		}
		prompt := updatePrompt(ecosystem, manifests[ecosystem], alerts, rules.allowed, rs)
		inputs := updateInputs(rs, ecosystem, alerts)
		unchanged := !run.force && rs.UpdateInputs[ecosystem] == inputs
		s, sent, err := continueUpdateSession(ctx, repo, rs.UpdatesAgentID, rs.UpdateSessions[ecosystem], doer, prompt, rules, unchanged)
		if err == nil && s == nil {
			s, err = agents_service.StartTaskSession(ctx, doer, repo, agents_service.StartOptions{Agent: agent, Prompt: prompt})
			sent = err == nil
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", ecosystem, err))
			continue
		}
		rs.UpdateSessions[ecosystem] = s.ID
		if sent {
			rs.UpdateInputs[ecosystem] = inputs
			started++
		}
	}
	if err := depscan_model.SaveRepoSetting(ctx, rs, "update_sessions", "update_inputs"); err != nil {
		return started, err
	}
	return started, errors.Join(errs...)
}

// updateInputs is what an update depends on, it is continued when they change
func updateInputs(rs *depscan_model.RepoSetting, ecosystem string, alerts []*depscan_model.Alert) string {
	var ids []string
	for _, a := range alerts {
		if a.Ecosystem == ecosystem {
			ids = append(ids, strconv.FormatInt(a.ID, 10))
		}
	}
	slices.Sort(ids)
	return fmt.Sprintf("%s %s %t %s %s %s", rs.LastScanSHA, strings.Join(ids, ","), rs.UpdatesMajor, strings.Join(rs.UpdatesIgnore, ","),
		strings.Join(refusedVersions(rs.UpdatesRefused, ecosystem), ","), rs.UpdatesScope)
}

// refusedVersions names the refused versions of an ecosystem's packages
func refusedVersions(refused []*depscan_model.RefusedVersion, ecosystem string) []string {
	var names []string
	for _, r := range refused {
		if r.Ecosystem == ecosystem {
			names = append(names, r.Package+" "+r.Version)
		}
	}
	return names
}

// continueUpdateSession queues the update on the previous session of the ecosystem, unless its pull request was merged or closed,
// and returns whether it did. It returns no session when a new one is needed.
// When its inputs are unchanged, the session is left alone until its last run is fullUpdateEvery old.
func continueUpdateSession(ctx context.Context, repo *repo_model.Repository, agentID, sessionID int64, doer *user_model.User, prompt string, rules updateRules, unchanged bool) (*agents_model.Session, bool, error) {
	if sessionID == 0 {
		return nil, false, nil
	}
	s, err := agents_model.GetSessionByID(ctx, sessionID)
	if errors.Is(err, util.ErrNotExist) {
		return nil, false, nil
	} else if err != nil {
		return nil, false, err
	}
	if s.AgentID != agentID {
		return nil, false, nil
	}
	if s.Branch != "" {
		pr, err := issues_model.GetLatestPullRequestByHeadInfo(ctx, s.RepoID, s.Branch)
		if err != nil {
			return nil, false, err
		}
		if pr != nil {
			if err := pr.LoadIssue(ctx); err != nil {
				return nil, false, err
			}
			if pr.HasMerged || pr.Issue.IsClosed {
				return nil, false, nil
			}
		}
	}
	if busy, err := isSessionBusy(ctx, s.ID); err != nil || busy {
		return s, false, err // still running on older inputs, the next due update continues it
	}
	if unchanged {
		latest, err := agents_model.GetLatestRun(ctx, s.ID)
		if err != nil {
			return nil, false, err
		}
		// a failed or cancelled update is retried at once
		if latest != nil && latest.Status == agents_model.StatusSuccess && time.Since(latest.Created.AsTime()) < fullUpdateEvery {
			return s, false, nil
		}
	}
	broken, err := brokenRules(ctx, repo, s.Branch, rules)
	if err != nil {
		return nil, false, err
	}
	// the first line names the commits
	title, rest, _ := strings.Cut(prompt, "\n")
	prompt = fmt.Sprintf("%s again\n\nFirst merge `origin/%s` into your branch if it is behind, resolving the conflicts in lockfiles by regenerating them.\n%s%s", title, s.BaseBranch, broken, rest)
	if _, err := agents_service.SendInstruction(ctx, s, doer, prompt); err != nil {
		return nil, false, err
	}
	return s, true, nil
}

func isSessionBusy(ctx context.Context, sessionID int64) (bool, error) {
	if active, err := agents_model.HasActiveRun(ctx, sessionID); err != nil || active {
		return active, err
	}
	queued, err := agents_model.GetFirstQueuedRun(ctx, sessionID)
	return queued != nil, err
}

// versionLine is what an update without major versions keeps: the major version, the minor one for 0.x versions
type versionLine struct{ major, minor int }

func lineOf(version string) versionLine {
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(version), "v"), ".")
	major, _ := strconv.Atoi(parts[0])
	if major == 0 && len(parts) > 1 {
		minor, _ := strconv.Atoi(parts[1])
		return versionLine{0, minor}
	}
	return versionLine{major: major}
}

func (l versionLine) less(o versionLine) bool {
	return l.major < o.major || (l.major == o.major && l.minor < o.minor)
}

func (l versionLine) String() string {
	if l.major == 0 {
		return fmt.Sprintf("0.%d.x", l.minor)
	}
	return fmt.Sprintf("%d.x", l.major)
}

func lineKey(manifest, name string) string {
	return manifest + "\x00" + name
}

// allowedLines is the newest version line each dependency may reach without major versions:
// its own, or the first one fixing each of its vulnerabilities
func allowedLines(deps []*depscan_model.Dependency, alerts []*depscan_model.Alert) map[string]versionLine {
	allowed := make(map[string]versionLine, len(deps))
	for _, d := range deps {
		// a lockfile can pin several versions of a package, the newest line is kept
		key, line := lineKey(d.Manifest, d.Name), lineOf(d.Version)
		if current, ok := allowed[key]; !ok || current.less(line) {
			allowed[key] = line
		}
	}
	for _, a := range alerts {
		key := lineKey(a.Manifest, a.Package)
		current, ok := allowed[key]
		if !ok {
			continue
		}
		var first *versionLine
		for _, v := range a.FixedVersions {
			if line := lineOf(v); !line.less(lineOf(a.Version)) && (first == nil || line.less(*first)) {
				first = &line
			}
		}
		if first != nil && current.less(*first) {
			allowed[key] = *first
		}
	}
	return allowed
}

// updateRules are what the updates may not do, checked again on the branch of an open update pull request
type updateRules struct {
	allowed map[string]versionLine // the newest version line of the dependencies kept within their major version
	refused []*depscan_model.RefusedVersion
	ignore  ignoreRules
}

// brokenRules lists the direct dependencies of a branch that left their allowed version line or reached a refused
// version, for the agent to bring them back. An indirect one follows the direct dependency requiring it, and a package
// that still has a version on its line was only added next to it, so neither can be downgraded.
func brokenRules(ctx context.Context, repo *repo_model.Repository, branch string, rules updateRules) (string, error) {
	if (len(rules.allowed) == 0 && len(rules.refused) == 0 && len(rules.ignore) == 0) || branch == "" {
		return "", nil
	}
	gitRepo, err := git.OpenRepository(ctx, repo)
	if err != nil {
		return "", err
	}
	defer gitRepo.Close()
	commit, err := gitRepo.GetBranchCommit(ctx, branch)
	if git.IsErrNotExist(err) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	deps, err := readCommitManifests(ctx, repo, gitRepo, commit)
	if err != nil {
		return "", err
	}
	direct := map[string]container.Set[string]{}
	isDirect := func(d *depscan_model.Dependency) bool {
		if _, ok := direct[d.Manifest]; !ok {
			direct[d.Manifest] = directDependencies(ctx, gitRepo, commit, d.Manifest)
		}
		return direct[d.Manifest].Contains(normalizeName(d.Ecosystem, d.Name))
	}
	var broken []string
	checked := container.Set[string]{}
	for _, d := range deps {
		// deps are sorted by version within a manifest, so the first of a package is its lowest version
		lowest := checked.Add(lineKey(d.Manifest, d.Name))
		line, ok := rules.allowed[lineKey(d.Manifest, d.Name)]
		switch {
		case lowest && ok && line.less(lineOf(d.Version)) && isDirect(d):
			broken = append(broken, fmt.Sprintf("- %s %s in %s: go back to %s", d.Name, d.Version, d.Manifest, line))
		case isRefused(rules.refused, d.Ecosystem, d.Name, d.Version) && isDirect(d):
			broken = append(broken, fmt.Sprintf("- %s %s in %s: this version was refused, go back to the previous one", d.Name, d.Version, d.Manifest))
		case rules.ignore.skips(d.Name, d.Version) && isDirect(d):
			broken = append(broken, fmt.Sprintf("- %s %s in %s: this version is ignored, go back to the previous one", d.Name, d.Version, d.Manifest))
		}
	}
	if len(broken) == 0 {
		return "", nil
	}
	slices.Sort(broken)
	return fmt.Sprintf("Your branch upgraded these packages beyond what the rules below allow, downgrade them first:\n%s\n\n", strings.Join(broken, "\n")), nil
}

// vulnerableUpgrades lists the vulnerable versions of an ecosystem the updates upgrade, one line per package version
// with its vulnerabilities, so the agent picks the lowest version fixing all of them
func vulnerableUpgrades(ecosystem string, alerts []*depscan_model.Alert, allowed map[string]versionLine, rs *depscan_model.RepoSetting) []string {
	vulnerable := map[string][]string{}
	for _, a := range alerts {
		if a.Ecosystem == ecosystem && len(a.FixedVersions) > 0 && !isIgnored(rs.UpdatesIgnore, a.Package) {
			key := fmt.Sprintf("%s %s in %s", a.Package, a.Version, a.Manifest)
			if line, ok := allowed[lineKey(a.Manifest, a.Package)]; ok {
				key += fmt.Sprintf(", up to %s", line)
			}
			vulnerable[key] = append(vulnerable[key], fmt.Sprintf("%s (fixed in %s)", a.Title(), strings.Join(a.FixedVersions, ", ")))
		}
	}
	upgrades := make([]string, 0, len(vulnerable))
	for _, key := range slices.Sorted(maps.Keys(vulnerable)) {
		upgrades = append(upgrades, fmt.Sprintf("- %s: %s", key, strings.Join(vulnerable[key], "; ")))
	}
	return upgrades
}

func updatePrompt(ecosystem string, manifests []string, alerts []*depscan_model.Alert, allowed map[string]versionLine, rs *depscan_model.RepoSetting) string {
	var b strings.Builder
	securityOnly := rs.UpdatesSecurityOnly()
	if securityOnly {
		fmt.Fprintf(&b, "Update the vulnerable %s dependencies\n\n", ecosystem)
		fmt.Fprintf(&b, "Upgrade the vulnerable %s dependencies of this repository listed below, declared next to %s, and nothing else. ", ecosystem, strings.Join(manifests, ", "))
	} else {
		fmt.Fprintf(&b, "Update the %s dependencies\n\n", ecosystem)
		fmt.Fprintf(&b, "Update the outdated %s dependencies of this repository, declared next to %s. ", ecosystem, strings.Join(manifests, ", "))
		if cmd := outdatedCommands[ecosystem]; cmd != "" {
			fmt.Fprintf(&b, "List the outdated direct dependencies with %s. ", cmd)
		}
	}
	switch {
	case securityOnly && rs.UpdatesMajor:
		b.WriteString("Their fix may be in a new major version: adapt the code to its breaking changes.\n")
	case securityOnly:
		b.WriteString("Stay within their current major version, or minor version for versions 0.x, unless the list below allows more.\n")
	case rs.UpdatesMajor:
		b.WriteString("Update them to their latest version, major versions included, and adapt the code to their breaking changes.\n")
	default:
		b.WriteString("Update them to their latest version within their current major version, or minor version for versions 0.x, never to a new major version.\n")
	}
	var leave, keepMajor, skip []string
	for _, entry := range rs.UpdatesIgnore {
		switch rule, err := parseIgnoreRule(entry); {
		case err != nil:
		case rule.noMajor:
			keepMajor = append(keepMajor, strings.TrimSuffix(entry, "@"+noMajorRule))
		case rule.versions != nil:
			skip = append(skip, entry)
		default:
			leave = append(leave, entry)
		}
	}
	if len(leave) > 0 {
		fmt.Fprintf(&b, "Leave these packages as they are: %s.\n", strings.Join(leave, ", "))
	}
	if len(keepMajor) > 0 && rs.UpdatesMajor {
		fmt.Fprintf(&b, "Keep these packages within their current major version, or minor version for versions 0.x: %s.\n", strings.Join(keepMajor, ", "))
	}
	if len(skip) > 0 {
		fmt.Fprintf(&b, "Never upgrade to these versions, given as package@range: %s.\n", strings.Join(skip, ", "))
	}
	if refused := refusedVersions(rs.UpdatesRefused, ecosystem); len(refused) > 0 {
		fmt.Fprintf(&b, "These upgrades were refused, never make them: %s.\n", strings.Join(refused, ", "))
	}
	if vulnerable := vulnerableUpgrades(ecosystem, alerts, allowed, rs); len(vulnerable) > 0 {
		if securityOnly {
			b.WriteString("\nUpgrade each of these vulnerable versions to the lowest version that fixes all its vulnerabilities, even when it is an indirect dependency, and no further than the rule above allows:\n")
		} else {
			b.WriteString("\nThese versions are vulnerable. Upgrade each first to the lowest version that fixes all its vulnerabilities, even when it is an indirect dependency or the upgrade crosses a major version, and no further than the rule above allows:\n")
		}
		b.WriteString(strings.Join(vulnerable, "\n") + "\n")
	}
	b.WriteString("\nUpdate the manifests and regenerate the lockfiles with the package manager, never edit lockfiles by hand. ")
	b.WriteString("Run the tests and fix what the updates break; leave out an update you cannot make work and say why. ")
	if !securityOnly {
		b.WriteString("When nothing is outdated, change nothing and say so. ")
	}
	b.WriteString("Finish with a table of the updated packages, with their previous and new versions.\n")
	return b.String()
}
