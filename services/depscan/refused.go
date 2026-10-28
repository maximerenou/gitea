// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package depscan

import (
	"context"
	"errors"
	"slices"
	"strings"

	agents_model "gitea.dev/models/agents"
	depscan_model "gitea.dev/models/depscan"
	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/container"
	"gitea.dev/modules/git"
	"gitea.dev/modules/globallock"
	"gitea.dev/modules/log"
	"gitea.dev/modules/util"
)

// IssueChangeStatus remembers the upgrades of an update pull request closed without merging, the next updates skip them.
// A merge closes the pull request without this notification.
func (n *notifier) IssueChangeStatus(ctx context.Context, _ *user_model.User, _ string, issue *issues_model.Issue, _ *issues_model.Comment, isClosed bool) {
	if !isClosed || !issue.IsPull {
		return
	}
	if err := recordRefusedUpdates(ctx, issue); err != nil {
		log.Warn("depscan: refused upgrades of pull request %d: %v", issue.ID, err)
	}
}

func recordRefusedUpdates(ctx context.Context, issue *issues_model.Issue) error {
	if err := issue.LoadPullRequest(ctx); err != nil {
		return err
	}
	return refuseUpgrades(ctx, issue.PullRequest)
}

func refuseUpgrades(ctx context.Context, pr *issues_model.PullRequest) error {
	if pr.HasMerged || pr.HeadRepoID != pr.BaseRepoID {
		return nil
	}
	if err := pr.LoadBaseRepo(ctx); err != nil {
		return err
	}
	return globallock.LockAndDo(ctx, updatesLockKey(pr.BaseRepoID), func(ctx context.Context) error {
		rs, err := depscan_model.GetRepoSetting(ctx, pr.BaseRepoID)
		if err != nil || rs.UpdatesAgentID == 0 {
			return err
		}
		ecosystem, err := updateSessionEcosystem(ctx, rs, pr.HeadBranch)
		if err != nil || ecosystem == "" {
			return err
		}
		refused, err := pullUpgrades(ctx, pr, ecosystem)
		if err != nil {
			return err
		}
		added := false
		for _, r := range refused {
			if !slices.ContainsFunc(rs.UpdatesRefused, func(o *depscan_model.RefusedVersion) bool { return *o == *r }) {
				rs.UpdatesRefused, added = append(rs.UpdatesRefused, r), true
			}
		}
		if !added {
			return nil
		}
		return depscan_model.SaveRepoSetting(ctx, rs, "updates_refused")
	})
}

// updateSessionEcosystem returns the ecosystem whose update session pushes to a branch, empty for other branches
func updateSessionEcosystem(ctx context.Context, rs *depscan_model.RepoSetting, branch string) (string, error) {
	for ecosystem, sessionID := range rs.UpdateSessions {
		s, err := agents_model.GetSessionByID(ctx, sessionID)
		if errors.Is(err, util.ErrNotExist) {
			continue
		} else if err != nil {
			return "", err
		}
		if s.Branch != "" && s.Branch == branch {
			return ecosystem, nil
		}
	}
	return "", nil
}

// pullUpgrades lists the direct dependencies of an ecosystem a pull request upgrades
func pullUpgrades(ctx context.Context, pr *issues_model.PullRequest, ecosystem string) ([]*depscan_model.RefusedVersion, error) {
	gitRepo, err := git.OpenRepository(ctx, pr.BaseRepo)
	if err != nil {
		return nil, err
	}
	defer gitRepo.Close()
	head, _, baseDeps, headDeps, err := readPullManifests(ctx, gitRepo, pr)
	if err != nil {
		return nil, err
	}
	// the newest version of each package, deps are sorted by version within a manifest
	newest := func(deps []*depscan_model.Dependency) map[string]*depscan_model.Dependency {
		m := map[string]*depscan_model.Dependency{}
		for _, d := range deps {
			if d.Ecosystem == ecosystem {
				m[lineKey(d.Manifest, d.Name)] = d
			}
		}
		return m
	}
	before := newest(baseDeps)
	direct := map[string]container.Set[string]{}
	var upgrades []*depscan_model.RefusedVersion
	for key, d := range newest(headDeps) {
		if b, ok := before[key]; !ok || compareVersions(d.Version, b.Version) <= 0 {
			continue
		}
		if _, ok := direct[d.Manifest]; !ok {
			direct[d.Manifest] = directDependencies(ctx, gitRepo, head, d.Manifest)
		}
		r := &depscan_model.RefusedVersion{Ecosystem: d.Ecosystem, Package: d.Name, Version: d.Version}
		if direct[d.Manifest].Contains(normalizeName(d.Ecosystem, d.Name)) && !slices.ContainsFunc(upgrades, func(o *depscan_model.RefusedVersion) bool { return *o == *r }) {
			upgrades = append(upgrades, r)
		}
	}
	slices.SortFunc(upgrades, func(a, b *depscan_model.RefusedVersion) int { return strings.Compare(a.Package, b.Package) })
	return upgrades, nil
}

// isRefused reports whether a package version was refused by closing its update pull request
func isRefused(refused []*depscan_model.RefusedVersion, ecosystem, name, version string) bool {
	return slices.ContainsFunc(refused, func(r *depscan_model.RefusedVersion) bool {
		return r.Ecosystem == ecosystem && normalizeName(ecosystem, r.Package) == normalizeName(ecosystem, name) && r.Version == version
	})
}

// RemoveRefusedVersion lets the updates propose a refused version again
func RemoveRefusedVersion(ctx context.Context, repo *repo_model.Repository, refused depscan_model.RefusedVersion) error {
	return globallock.LockAndDo(ctx, updatesLockKey(repo.ID), func(ctx context.Context) error {
		rs, err := depscan_model.GetRepoSetting(ctx, repo.ID)
		if err != nil {
			return err
		}
		n := len(rs.UpdatesRefused)
		rs.UpdatesRefused = slices.DeleteFunc(rs.UpdatesRefused, func(r *depscan_model.RefusedVersion) bool { return *r == refused })
		if len(rs.UpdatesRefused) == n {
			return util.NewNotExistErrorf("%s %s is not refused", refused.Package, refused.Version)
		}
		return depscan_model.SaveRepoSetting(ctx, rs, "updates_refused")
	})
}
