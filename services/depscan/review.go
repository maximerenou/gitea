// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package depscan

import (
	"context"
	"fmt"
	"slices"
	"strings"

	depscan_model "gitea.dev/models/depscan"
	git_model "gitea.dev/models/git"
	issues_model "gitea.dev/models/issues"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/commitstatus"
	"gitea.dev/modules/container"
	"gitea.dev/modules/git"
	"gitea.dev/modules/globallock"
	"gitea.dev/modules/log"
	"gitea.dev/modules/util"
	commitstatus_service "gitea.dev/services/repository/commitstatus"
)

const reviewStatusContext = "Dependency review"

// ReviewPull flags the vulnerable package versions a pull request adds to its merge base, with a commit status on its head.
// The status always ends, as queuing the review set it pending.
func ReviewPull(ctx context.Context, pr *issues_model.PullRequest) error {
	if err := pr.LoadBaseRepo(ctx); err != nil {
		return err
	}
	repo := pr.BaseRepo
	return globallock.LockAndDo(ctx, fmt.Sprintf("depscan_pull_%d", pr.ID), func(ctx context.Context) error {
		status := &git_model.CommitStatus{State: commitstatus.CommitStatusSkipped, Description: "Not reviewed", Context: reviewStatusContext}
		var headSHA string
		err := pr.LoadIssue(ctx)
		if err == nil && IsRepoScanned(ctx, repo) && !pr.HasMerged && !pr.Issue.IsClosed {
			headSHA, err = saveReview(ctx, pr, status)
		}
		if err != nil {
			log.Warn("depscan: review pull request %d: %v", pr.ID, err)
			status.State, status.Description = commitstatus.CommitStatusError, "The dependencies could not be reviewed"
		}
		if headSHA == "" {
			if headSHA, err = pullHeadSHA(ctx, pr); err != nil {
				return err
			}
		}
		return commitstatus_service.CreateCommitStatus(ctx, repo, user_model.NewActionsUser(), headSHA, status)
	})
}

// saveReview reviews a pull request and fills the status of its head, which it returns
func saveReview(ctx context.Context, pr *issues_model.PullRequest, status *git_model.CommitStatus) (string, error) {
	rs, err := depscan_model.GetRepoSetting(ctx, pr.BaseRepoID)
	if err != nil {
		return "", err
	}
	review, err := reviewPull(ctx, pr, newOSVClient(reviewMaxVulnFetches), rs.ReviewAllowList)
	if review == nil {
		return "", err
	}
	if err != nil {
		review.Error = util.EllipsisDisplayString(err.Error(), 1024)
	}
	if err := depscan_model.SavePullReview(ctx, review); err != nil {
		return review.HeadSHA, err
	}
	status.TargetURL = fmt.Sprintf("%s/security/pulls/%d", pr.BaseRepo.Link(), pr.Index)
	failing := 0
	for _, f := range review.Findings {
		if rs.ReviewFailsOn(f.Severity) {
			failing++
		}
	}
	switch {
	case failing == 1:
		status.State, status.Description = commitstatus.CommitStatusFailure, "1 new vulnerable dependency"
	case failing > 1:
		status.State, status.Description = commitstatus.CommitStatusFailure, fmt.Sprintf("%d new vulnerable dependencies", failing)
	case len(review.Findings) > 0:
		status.State, status.Description = commitstatus.CommitStatusSuccess, "No new vulnerable dependency severe enough to fail"
	default:
		status.State, status.Description = commitstatus.CommitStatusSuccess, "No new vulnerable dependency"
	}
	return review.HeadSHA, err
}

// readPullManifests reads the dependencies of a pull request's head and of its merge base
func readPullManifests(ctx context.Context, gitRepo *git.Repository, pr *issues_model.PullRequest) (head, base *git.Commit, baseDeps, headDeps []*depscan_model.Dependency, err error) {
	repo := pr.BaseRepo
	if head, err = gitRepo.GetCommit(ctx, pr.GetGitHeadRefName()); err != nil {
		return nil, nil, nil, nil, err
	}
	baseBranch, err := gitRepo.GetBranchCommit(ctx, pr.BaseBranch)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	baseSHA, err := git.MergeBase(ctx, repo, baseBranch.ID.String(), head.ID.String())
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if base, err = gitRepo.GetCommit(ctx, baseSHA); err != nil {
		return nil, nil, nil, nil, err
	}
	if baseDeps, err = readCommitManifests(ctx, repo, gitRepo, base); err != nil {
		return nil, nil, nil, nil, err
	}
	if headDeps, err = readCommitManifests(ctx, repo, gitRepo, head); err != nil {
		return nil, nil, nil, nil, err
	}
	return head, base, baseDeps, headDeps, nil
}

// isAllowed reports whether the repository accepts a vulnerability, known by any of its identifiers
func isAllowed(allowed []string, ids ...string) bool {
	return slices.ContainsFunc(allowed, func(a string) bool {
		return slices.ContainsFunc(ids, func(id string) bool { return strings.EqualFold(a, id) })
	})
}

// reviewPull returns a review without findings along with the error when the vulnerability database fails.
// The allowed vulnerabilities are left out.
func reviewPull(ctx context.Context, pr *issues_model.PullRequest, client *osvClient, allowed []string) (*depscan_model.PullReview, error) {
	repo := pr.BaseRepo
	gitRepo, err := git.OpenRepository(ctx, repo)
	if err != nil {
		return nil, err
	}
	defer gitRepo.Close()
	head, base, baseDeps, headDeps, err := readPullManifests(ctx, gitRepo, pr)
	if err != nil {
		return nil, err
	}
	review := &depscan_model.PullReview{RepoID: repo.ID, PullID: pr.ID, HeadSHA: head.ID.String(), BaseSHA: base.ID.String()}

	known := container.Set[Package]{}
	for _, d := range baseDeps {
		known.Add(Package{d.Ecosystem, d.Name, d.Version})
	}
	var added []*depscan_model.Dependency
	var packages []Package
	for _, d := range headDeps {
		p := Package{d.Ecosystem, d.Name, d.Version}
		if known.Add(p) {
			added = append(added, d)
			packages = append(packages, p)
		}
	}
	if len(packages) == 0 {
		return review, nil
	}
	vulnIDs, err := client.QueryBatch(ctx, packages)
	if err != nil {
		return review, err
	}
	vulns := map[string]*OSVVuln{}
	for i, d := range added {
		for _, id := range vulnIDs[i] {
			if isAllowed(allowed, id) {
				continue
			}
			v := vulns[id]
			if v == nil {
				if v, err = client.GetVuln(ctx, id); err != nil {
					return review, err
				}
				vulns[id] = v
			}
			if isAllowed(allowed, v.Aliases...) {
				continue
			}
			severity, _ := v.Rate()
			review.Findings = append(review.Findings, &depscan_model.ReviewFinding{
				VulnID: id, Summary: util.EllipsisDisplayString(v.Summary, 1024), Severity: severity,
				Ecosystem: d.Ecosystem, Package: d.Name, Version: d.Version, Manifest: d.Manifest, FixedVersions: v.FixedVersions(packages[i]),
			})
		}
	}
	return review, nil
}

func pullHeadSHA(ctx context.Context, pr *issues_model.PullRequest) (string, error) {
	gitRepo, err := git.OpenRepository(ctx, pr.BaseRepo)
	if err != nil {
		return "", err
	}
	defer gitRepo.Close()
	return gitRepo.GetRefCommitID(ctx, pr.GetGitHeadRefName())
}

// markReviewPending sets a pending status on the head of a queued review, so the pull request page refreshes until the review ends
func markReviewPending(ctx context.Context, pr *issues_model.PullRequest) {
	head, err := pullHeadSHA(ctx, pr)
	if err != nil {
		log.Warn("depscan: head of pull request %d: %v", pr.ID, err)
		return
	}
	status := &git_model.CommitStatus{State: commitstatus.CommitStatusPending, Description: "Reviewing the added dependencies", Context: reviewStatusContext}
	if err := commitstatus_service.CreateCommitStatus(ctx, pr.BaseRepo, user_model.NewActionsUser(), head, status); err != nil {
		log.Error("depscan: pending status of pull request %d: %v", pr.ID, err)
	}
}
