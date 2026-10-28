// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package depscan

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	audit_model "gitea.dev/models/audit"
	"gitea.dev/models/db"
	depscan_model "gitea.dev/models/depscan"
	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"
	"gitea.dev/modules/graceful"
	"gitea.dev/modules/log"
	"gitea.dev/modules/queue"
	"gitea.dev/modules/repository"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/util"
	agents_service "gitea.dev/services/agents"
	"gitea.dev/services/audit"
	"gitea.dev/services/cron"
	notify_service "gitea.dev/services/notify"
	"gitea.dev/services/pubsub"

	"xorm.io/builder"
)

var (
	scanQueue *queue.WorkerPoolQueue[string]
	// reviews have their own queue, the pull requests of any author must not delay the scans
	reviewQueue *queue.WorkerPoolQueue[int64]
)

// Init starts the scan queue, rescans repositories when their default branch changes and every night, as new vulnerabilities are published
func Init(ctx context.Context) error {
	if !IsEnabled() {
		return nil
	}
	if err := depscan_model.BackfillAlertKeys(ctx); err != nil {
		return fmt.Errorf("Init: backfill the dependency alert keys: %w", err)
	}
	scanQueue = queue.CreateUniqueQueue(graceful.GetManager().ShutdownContext(), "dependency_scanning", handleScans)
	if scanQueue == nil {
		return errors.New("unable to create the dependency_scanning queue")
	}
	go graceful.GetManager().RunWithCancel(scanQueue)
	reviewQueue = queue.CreateUniqueQueue(graceful.GetManager().ShutdownContext(), "dependency_review", handleReviews)
	if reviewQueue == nil {
		return errors.New("unable to create the dependency_review queue")
	}
	go graceful.GetManager().RunWithCancel(reviewQueue)
	notify_service.RegisterNotifier(&notifier{})
	agents_service.RegisterCommandHandler(handleIgnoreCommand)
	cron.RegisterTaskFatal("dependency_scanning", &cron.BaseConfig{Enabled: true, Schedule: "@midnight"},
		func(ctx context.Context, _ *user_model.User, _ *cron.BaseConfig) error { return QueueAllRepos(ctx) })
	cron.RegisterTaskFatal("dependency_updates", &cron.BaseConfig{Enabled: true, Schedule: "@every 1h"},
		func(ctx context.Context, _ *user_model.User, _ *cron.BaseConfig) error { return StartDueUpdates(ctx) })
	return nil
}

func handleReviews(pullIDs ...int64) []int64 {
	ctx := graceful.GetManager().ShutdownContext()
	for _, id := range pullIDs {
		pr, err := issues_model.GetPullRequestByID(ctx, id)
		if err != nil {
			continue
		}
		if err := ReviewPull(ctx, pr); err != nil {
			log.Warn("depscan: review pull request %d: %v", id, err)
		}
	}
	return nil
}

func handleScans(items ...string) []string {
	ctx := graceful.GetManager().ShutdownContext()
	for _, item := range items {
		id, _ := strconv.ParseInt(item, 10, 64)
		repo, err := repo_model.GetRepositoryByID(ctx, id)
		if err != nil {
			continue
		}
		if err := ScanRepo(ctx, repo); err != nil {
			log.Warn("depscan: scan %s: %v", repo.FullName(), err)
		}
		if pubsub.DefaultBroker != nil && pubsub.DefaultBroker.HasTopicSubscribers(ScanTopic(repo.ID)) {
			pubsub.DefaultBroker.Publish(ScanTopic(repo.ID), []byte("scanned"))
		}
	}
	return nil
}

// ScanTopic is the pubsub topic telling the open Security tabs of a repository that a scan ended
func ScanTopic(repoID int64) string {
	return fmt.Sprintf("depscan-repo-%d", repoID)
}

// QueueScan schedules a scan of the repository
func QueueScan(repo *repo_model.Repository) {
	if scanQueue == nil {
		return
	}
	if err := scanQueue.Push(strconv.FormatInt(repo.ID, 10)); err != nil && !errors.Is(err, queue.ErrAlreadyInQueue) {
		log.Error("depscan: queue %s: %v", repo.FullName(), err)
	}
}

// QueueReview schedules a dependency review of a pull request
func QueueReview(ctx context.Context, pr *issues_model.PullRequest) {
	if reviewQueue == nil {
		return
	}
	if err := pr.LoadBaseRepo(ctx); err != nil || !IsRepoScanned(ctx, pr.BaseRepo) {
		return
	}
	if err := reviewQueue.Push(pr.ID); err != nil && !errors.Is(err, queue.ErrAlreadyInQueue) {
		log.Error("depscan: queue pull request %d: %v", pr.ID, err)
		return
	}
	markReviewPending(ctx, pr)
}

// QueueAllRepos schedules every scanned repository
func QueueAllRepos(ctx context.Context) error {
	settings, err := depscan_model.FindExplicitRepoSettings(ctx)
	if err != nil {
		return err
	}
	for _, rs := range settings {
		if rs.Enabled {
			QueueScan(&repo_model.Repository{ID: rs.RepoID})
		}
	}
	if !setting.DependencyScanning.EnableByDefault {
		return nil
	}
	explicit := builder.Select("repo_id").From((&depscan_model.RepoSetting{}).TableName()).Where(depscan_model.ExplicitRepoSettingsCond())
	cond := builder.Eq{"is_empty": false}.And(builder.NotIn("id", explicit))
	return db.Iterate(ctx, cond, func(ctx context.Context, repo *repo_model.Repository) error {
		QueueScan(repo)
		return nil
	})
}

type notifier struct {
	notify_service.NullNotifier
}

func (n *notifier) PushCommits(ctx context.Context, _ *user_model.User, repo *repo_model.Repository, opts *repository.PushUpdateOptions, _ *repository.PushCommits) {
	queueOnDefaultBranch(ctx, repo, opts.RefFullName)
}

func (n *notifier) SyncPushCommits(ctx context.Context, _ *user_model.User, repo *repo_model.Repository, opts *repository.PushUpdateOptions, _ *repository.PushCommits) {
	queueOnDefaultBranch(ctx, repo, opts.RefFullName)
}

func (n *notifier) NewPullRequest(ctx context.Context, pr *issues_model.PullRequest, _ []*user_model.User) {
	QueueReview(ctx, pr)
}

func (n *notifier) PullRequestSynchronized(ctx context.Context, _ *user_model.User, pr *issues_model.PullRequest, _, _ string) {
	QueueReview(ctx, pr)
}

func (n *notifier) PullRequestChangeTargetBranch(ctx context.Context, _ *user_model.User, pr *issues_model.PullRequest, _ string) {
	QueueReview(ctx, pr)
}

func (n *notifier) ChangeDefaultBranch(ctx context.Context, repo *repo_model.Repository) {
	if IsRepoScanned(ctx, repo) {
		QueueScan(repo)
	}
}

func queueOnDefaultBranch(ctx context.Context, repo *repo_model.Repository, ref git.RefName) {
	if ref.IsBranch() && ref.BranchName() == repo.DefaultBranch && IsRepoScanned(ctx, repo) {
		QueueScan(repo)
	}
}

// ScanOptions are how a repository's administrators set up its scans
type ScanOptions struct {
	Enabled            bool
	NotifySeverity     depscan_model.Severity // the least severe new alert notified, empty for critical
	ReviewFailSeverity depscan_model.Severity // the least severe vulnerability failing a review, empty for any
	ReviewAllowList    []string               // vulnerability IDs or aliases the reviews accept
}

// SaveRepoSetting turns the scanning of a repository on or off, turning it on scans it at once
func SaveRepoSetting(ctx context.Context, repo *repo_model.Repository, opts ScanOptions) error {
	for _, s := range []depscan_model.Severity{opts.NotifySeverity, opts.ReviewFailSeverity} {
		if s != "" && !s.IsThreshold() {
			return util.NewInvalidArgumentErrorf("invalid severity %q", s)
		}
	}
	rs, err := depscan_model.GetRepoSetting(ctx, repo.ID)
	if err != nil {
		return err
	}
	rs.Enabled, rs.FollowsDefault, rs.AlertNotifySeverity = opts.Enabled, false, opts.NotifySeverity
	rs.ReviewFailSeverity, rs.ReviewAllowList = opts.ReviewFailSeverity, opts.ReviewAllowList
	if err := depscan_model.SaveRepoSetting(ctx, rs, "enabled", "follows_default", "alert_notify_severity", "review_fail_severity", "review_allow_list"); err != nil {
		return err
	}
	audit.Record(ctx, audit_model.RepositoryDependencyScanning, repo, "enabled", opts.Enabled)
	if opts.Enabled {
		QueueScan(repo)
	}
	return nil
}

// DismissAlert hides an alert the repository does not consider a risk, it is not reopened by later scans
func DismissAlert(ctx context.Context, doer *user_model.User, repo *repo_model.Repository, a *depscan_model.Alert, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return util.NewInvalidArgumentErrorf("a reason is required to dismiss an alert")
	}
	a.State, a.DismissedBy, a.DismissedReason = depscan_model.AlertDismissed, doer.ID, util.EllipsisDisplayString(reason, 1024)
	if dismissed, err := depscan_model.ChangeAlertState(ctx, a, depscan_model.AlertOpen, "state", "dismissed_by", "dismissed_reason"); err != nil {
		return err
	} else if !dismissed {
		return util.NewInvalidArgumentErrorf("only open alerts can be dismissed")
	}
	audit.Record(ctx, audit_model.RepositoryDependencyAlertDismiss, repo, "alert", a.VulnID, "package", a.Package)
	return nil
}

// ReopenAlert reopens a dismissed alert, the next scan closes it if the dependency is no longer affected
func ReopenAlert(ctx context.Context, repo *repo_model.Repository, a *depscan_model.Alert) error {
	a.State, a.DismissedBy, a.DismissedReason = depscan_model.AlertOpen, 0, ""
	if reopened, err := depscan_model.ChangeAlertState(ctx, a, depscan_model.AlertDismissed, "state", "dismissed_by", "dismissed_reason"); err != nil {
		return err
	} else if !reopened {
		return util.NewInvalidArgumentErrorf("only dismissed alerts can be reopened")
	}
	audit.Record(ctx, audit_model.RepositoryDependencyAlertReopen, repo, "alert", a.VulnID, "package", a.Package)
	QueueScan(repo)
	return nil
}
