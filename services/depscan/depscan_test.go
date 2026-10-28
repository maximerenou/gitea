// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package depscan

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	activities_model "gitea.dev/models/activities"
	agents_model "gitea.dev/models/agents"
	audit_model "gitea.dev/models/audit"
	"gitea.dev/models/db"
	depscan_model "gitea.dev/models/depscan"
	git_model "gitea.dev/models/git"
	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/commitstatus"
	"gitea.dev/modules/git/gitrepo"
	"gitea.dev/modules/json"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/test"
	"gitea.dev/modules/timeutil"
	actions_service "gitea.dev/services/actions"
	agents_service "gitea.dev/services/agents"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	unittest.MainTest(m)
}

func TestParsers(t *testing.T) {
	cases := map[string]struct {
		content string
		want    []Package
	}{
		"go.mod": {"module x\n\nrequire golang.org/x/net v0.1.0\n\nrequire (\n\tgithub.com/a/b v1.2.3 // indirect\n)\n", []Package{
			{EcosystemGo, "golang.org/x/net", "0.1.0"}, {EcosystemGo, "github.com/a/b", "1.2.3"},
		}},
		"web/package-lock.json": {`{"lockfileVersion":3,"packages":{"":{"version":"1.0.0"},"node_modules/lodash":{"version":"4.17.15"},"node_modules/a/node_modules/@s/b":{"version":"2.0.0"},"node_modules/local":{"link":true}}}`, []Package{
			{EcosystemNpm, "lodash", "4.17.15"}, {EcosystemNpm, "@s/b", "2.0.0"},
		}},
		"yarn.lock": {"# yarn lockfile v1\n\n\"@babel/core@^7.0.0\", \"@babel/core@^7.1.0\":\n  version \"7.12.3\"\n\nleft-pad@^1.0.0:\n  version \"1.3.0\"\n", []Package{
			{EcosystemNpm, "@babel/core", "7.12.3"}, {EcosystemNpm, "left-pad", "1.3.0"},
		}},
		"pnpm-lock.yaml": {"lockfileVersion: '9.0'\npackages:\n  '@types/node@20.1.0':\n    resolution: {}\n  react-dom@18.2.0(react@18.2.0):\n    resolution: {}\n  string_decoder@1.3.0:\n    resolution: {}\n", []Package{
			{EcosystemNpm, "@types/node", "20.1.0"}, {EcosystemNpm, "react-dom", "18.2.0"}, {EcosystemNpm, "string_decoder", "1.3.0"},
		}},
		"pnpm5/pnpm-lock.yaml": {"lockfileVersion: 5.4\npackages:\n  /string_decoder/1.3.0:\n    resolution: {}\n  /@babel/core/7.0.0_supports-color@5.5.0:\n    resolution: {}\n", []Package{
			{EcosystemNpm, "string_decoder", "1.3.0"}, {EcosystemNpm, "@babel/core", "7.0.0"},
		}},
		"composer.lock": {`{"packages":[{"name":"laravel/framework","version":"v10.1.0"}],"packages-dev":[{"name":"phpunit/phpunit","version":"10.0.0"}]}`, []Package{
			{EcosystemPackagist, "laravel/framework", "10.1.0"}, {EcosystemPackagist, "phpunit/phpunit", "10.0.0"},
		}},
		"requirements.txt": {"# deps\nDjango==4.2.1\nrequests[socks]==2.31.0 ; python_version > '3'\nflask>=2\n-r other.txt\n", []Package{
			{EcosystemPyPI, "django", "4.2.1"}, {EcosystemPyPI, "requests", "2.31.0"},
		}},
		"poetry.lock": {"[[package]]\nname = \"jinja2\"\nversion = \"3.1.2\"\n\n[package.dependencies]\nx = \"*\"\n\n[[package]]\nname = \"idna\"\nversion = \"3.4\"\n", []Package{
			{EcosystemPyPI, "jinja2", "3.1.2"}, {EcosystemPyPI, "idna", "3.4"},
		}},
		"Cargo.lock": {"version = 3\n\n[[package]]\nname = \"app\"\nversion = \"0.1.0\"\n\n[[package]]\nname = \"serde\"\nversion = \"1.0.150\"\nsource = \"registry+https://github.com/rust-lang/crates.io-index\"\n\n[[package]]\nname = \"fork\"\nversion = \"1.0.0\"\nsource = \"git+https://example.com/fork#abc\"\n", []Package{{EcosystemCrates, "serde", "1.0.150"}}},
		"Gemfile.lock": {"PATH\n  remote: .\n  specs:\n    mygem (0.1.0)\n\nGIT\n  remote: https://example.com/x.git\n  specs:\n    xgem (2.0.0)\n\nGEM\n  remote: https://rubygems.org/\n  specs:\n    nokogiri (1.15.4-x86_64-linux)\n      racc (~> 1.4)\n    rack (2.2.7)\n\nPLATFORMS\n  ruby\n", []Package{
			{EcosystemRubyGems, "nokogiri", "1.15.4"}, {EcosystemRubyGems, "rack", "2.2.7"},
		}},
	}
	for file, c := range cases {
		t.Run(file, func(t *testing.T) {
			got, err := Parse(file, []byte(c.content))
			require.NoError(t, err)
			assert.ElementsMatch(t, c.want, got)
		})
	}
	assert.True(t, IsManifest("app/composer.lock"))
	assert.False(t, IsManifest("node_modules/x/package-lock.json"))
	assert.False(t, IsManifest("vendor/a/go.mod"))
	assert.False(t, IsManifest("package.json"))
}

func TestDirectDependencies(t *testing.T) {
	cases := map[string]struct {
		content string
		want    []string
	}{
		"go.mod":        {"require golang.org/x/net v0.1.0 // indirect\nrequire (\n\tgithub.com/a/b v1.2.3\n\tgithub.com/c/d v1.0.0 // indirect\n)\n", []string{"github.com/a/b"}},
		"composer.lock": {`{"require":{"php":"^8.2","laravel/framework":"^10.0"},"require-dev":{"phpunit/phpunit":"^10"}}`, []string{"php", "laravel/framework", "phpunit/phpunit"}},
		"Pipfile.lock":  {"[[source]]\nname = \"pypi\"\n\n[packages]\nrequests = \"*\"\n\n[dev-packages]\npytest = \">=7\"\n", []string{"requests", "pytest"}},
		"poetry.lock": {
			"[project]\nname = \"app\"\ndependencies = [\n  \"Django>=4\",\n  'requests[socks]>=2',\n]\n\n[tool.poetry.group.dev.dependencies]\nblack = \"^23\"\n",
			[]string{"Django", "requests", "black"},
		},
		"Cargo.lock":   {"[package]\nname = \"app\"\n\n[dependencies]\nserde = { version = \"1\" }\n\n[target.'cfg(unix)'.dev-dependencies]\nlibc = \"0.2\"\n\n[dependencies.tokio]\nversion = \"1\"\n", []string{"serde", "libc", "tokio"}},
		"Gemfile.lock": {"GEM\n  specs:\n    rails (7.0.0)\n      rack (>= 2)\n\nDEPENDENCIES\n  rails (~> 7.0)\n  mygem!\n", []string{"rails", "mygem"}},
	}
	for lockfile, c := range cases {
		t.Run(lockfile, func(t *testing.T) {
			assert.ElementsMatch(t, c.want, declarations[lockfile].parse([]byte(c.content)))
		})
	}
}

func TestCVSS(t *testing.T) {
	assert.InDelta(t, 9.8, cvss3BaseScore("CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H"), 0.001)
	assert.InDelta(t, 6.1, cvss3BaseScore("CVSS:3.1/AV:N/AC:L/PR:N/UI:R/S:C/C:L/I:L/A:N"), 0.001)
	assert.InDelta(t, 0, cvss3BaseScore("CVSS:4.0/AV:N"), 0.001)
}

// fakeOSV answers like api.osv.dev for lodash 4.17.15 only
func fakeOSV(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/querybatch":
			var req struct {
				Queries []osvQuery `json:"queries"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			results := make([]map[string]any, len(req.Queries))
			for i, q := range req.Queries {
				results[i] = map[string]any{}
				if q.Package.Name == "lodash" && (q.Version == "4.17.15" || q.Version == "4.17.11") {
					results[i]["vulns"] = []map[string]string{{"id": "GHSA-p6mc-m468-83gw"}}
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
		case "/v1/vulns/GHSA-p6mc-m468-83gw":
			_, _ = w.Write([]byte(`{"id":"GHSA-p6mc-m468-83gw","summary":"Prototype Pollution in lodash","aliases":["CVE-2020-8203"],
				"severity":[{"type":"CVSS_V3","score":"CVSS:3.1/AV:N/AC:H/PR:N/UI:N/S:U/C:N/I:H/A:H"}],
				"affected":[{"package":{"ecosystem":"npm","name":"lodash"},"ranges":[{"type":"SEMVER","events":[{"introduced":"3.7.0"},{"fixed":"4.17.19"}]}]}],
				"database_specific":{"severity":"HIGH"},"references":[{"url":"https://nvd.nist.gov/vuln/detail/CVE-2020-8203"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
}

// commitLock commits web/package-lock.json to ref with git plumbing, the fixture repositories have no working hooks.
// Its web/package.json declares lodash and ms.
func commitLock(t *testing.T, repo *repo_model.Repository, ref, packages string, parents ...string) string {
	gitCmd := func(stdin string, args ...string) string {
		cmd := exec.Command("git", append([]string{"--git-dir", gitrepo.RepoLocalPath(repo)}, args...)...)
		cmd.Stdin = strings.NewReader(stdin)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.Output()
		require.NoError(t, err, args)
		return strings.TrimSpace(string(out))
	}
	blob := gitCmd(`{"lockfileVersion":3,"packages":{`+packages+`}}`, "hash-object", "-w", "--stdin")
	manifest := gitCmd(`{"dependencies":{"lodash":"^4.17.0"},"devDependencies":{"ms":"*"}}`, "hash-object", "-w", "--stdin")
	web := gitCmd("100644 blob "+blob+"\tpackage-lock.json\n100644 blob "+manifest+"\tpackage.json\n", "mktree")
	root := gitCmd("040000 tree "+web+"\tweb\n", "mktree")
	args := []string{"commit-tree", root, "-m", "lockfile"}
	for _, p := range parents {
		args = append(args, "-p", p)
	}
	commit := gitCmd("", args...)
	gitCmd("", "update-ref", ref, commit)
	return commit
}

func TestScanRepo(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := t.Context()
	server := fakeOSV(t)
	defer server.Close()
	defer test.MockVariableValue(&setting.DependencyScanning.Enabled, true)()
	defer test.MockVariableValue(&setting.DependencyScanning.OSVURL, server.URL)()
	defer test.MockVariableValue(&setting.DependencyScanning.AllowedHostList, "loopback")()

	user := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	repo.DefaultBranch = "depscan-test"
	writeLock := func(version string) {
		commitLock(t, repo, "refs/heads/depscan-test", `"node_modules/lodash":{"version":"`+version+`"},"node_modules/left-pad":{"version":"1.3.0"}`)
	}
	writeLock("4.17.15")

	require.NoError(t, ScanRepo(ctx, repo))
	unittest.AssertNotExistsBean(t, &depscan_model.Alert{RepoID: repo.ID}) // not enabled for the repository

	require.NoError(t, depscan_model.SaveRepoSetting(ctx, &depscan_model.RepoSetting{RepoID: repo.ID, Enabled: true}))
	require.NoError(t, ScanRepo(ctx, repo))
	unittest.AssertCount(t, &depscan_model.Dependency{RepoID: repo.ID}, 2)
	a := unittest.AssertExistsAndLoadBean(t, &depscan_model.Alert{RepoID: repo.ID, VulnID: "GHSA-p6mc-m468-83gw"})
	assert.Equal(t, depscan_model.AlertOpen, a.State)
	assert.Equal(t, depscan_model.SeverityHigh, a.Severity)
	assert.InDelta(t, 7.4, a.Score, 0.001)
	assert.Equal(t, "CVE-2020-8203", a.Title())
	assert.Equal(t, []string{"4.17.19"}, a.FixedVersions)
	assert.Equal(t, "web/package-lock.json", a.Manifest)
	assert.Equal(t, a.Key(), a.AlertKey)

	t.Run("AlertKeysAreBackfilled", func(t *testing.T) {
		_, err := db.GetEngine(ctx).Exec("UPDATE depscan_alert SET alert_key = NULL")
		require.NoError(t, err)
		require.NoError(t, depscan_model.BackfillAlertKeys(ctx))
		assert.Equal(t, a.Key(), unittest.AssertExistsAndLoadBean(t, &depscan_model.Alert{ID: a.ID}).AlertKey)
	})

	t.Run("TheLowestAffectedVersionIsKept", func(t *testing.T) {
		commitLock(t, repo, "refs/heads/depscan-test", `"node_modules/lodash":{"version":"4.17.15"},"node_modules/a/node_modules/lodash":{"version":"4.17.11"}`)
		for range 3 {
			require.NoError(t, ScanRepo(ctx, repo))
			assert.Equal(t, "4.17.11", unittest.AssertExistsAndLoadBean(t, &depscan_model.Alert{ID: a.ID}).Version)
		}
	})

	for _, failure := range []string{"size", "count", "parse"} {
		t.Run("IncompleteScan/"+failure, func(t *testing.T) {
			writeLock("4.17.15")
			require.NoError(t, ScanRepo(ctx, repo))
			before := unittest.AssertExistsAndLoadBean(t, &depscan_model.RepoSetting{RepoID: repo.ID})
			switch failure {
			case "size":
				defer test.MockVariableValue(&setting.DependencyScanning.MaxManifestSize, int64(1))()
			case "count":
				defer test.MockVariableValue(&setting.DependencyScanning.MaxManifests, 0)()
			case "parse":
				commitLock(t, repo, "refs/heads/depscan-test", "invalid json")
			}
			require.Error(t, ScanRepo(ctx, repo))
			assert.Equal(t, depscan_model.AlertOpen, unittest.AssertExistsAndLoadBean(t, &depscan_model.Alert{ID: a.ID}).State)
			unittest.AssertCount(t, &depscan_model.Dependency{RepoID: repo.ID}, 2)
			after := unittest.AssertExistsAndLoadBean(t, &depscan_model.RepoSetting{RepoID: repo.ID})
			assert.Equal(t, before.LastScanSHA, after.LastScanSHA)
			assert.NotEmpty(t, after.LastScanError)
		})
	}

	t.Run("UpgradingClosesTheAlert", func(t *testing.T) {
		writeLock("4.17.21")
		require.NoError(t, ScanRepo(ctx, repo))
		assert.Equal(t, depscan_model.AlertFixed, unittest.AssertExistsAndLoadBean(t, &depscan_model.Alert{ID: a.ID}).State)

		writeLock("4.17.15")
		require.NoError(t, ScanRepo(ctx, repo))
		assert.Equal(t, depscan_model.AlertOpen, unittest.AssertExistsAndLoadBean(t, &depscan_model.Alert{ID: a.ID}).State, "a regression reopens it")
	})

	t.Run("DismissalsSurviveScans", func(t *testing.T) {
		a := unittest.AssertExistsAndLoadBean(t, &depscan_model.Alert{ID: a.ID})
		require.NoError(t, DismissAlert(ctx, user, repo, a, "not reachable"))
		require.NoError(t, ScanRepo(ctx, repo))
		a = unittest.AssertExistsAndLoadBean(t, &depscan_model.Alert{ID: a.ID})
		assert.Equal(t, depscan_model.AlertDismissed, a.State)
		assert.Equal(t, "not reachable", a.DismissedReason)
	})

	t.Run("OSVErrorsAreReported", func(t *testing.T) {
		defer test.MockVariableValue(&setting.DependencyScanning.AllowedHostList, "")()
		assert.Error(t, ScanRepo(ctx, repo), "the egress policy blocks a loopback OSV endpoint unless allowed")
		rs, err := depscan_model.GetRepoSetting(ctx, repo.ID)
		require.NoError(t, err)
		assert.NotEmpty(t, rs.LastScanError)
	})

	t.Run("ScansKeepTheInstanceDefault", func(t *testing.T) {
		_, err := db.DeleteByBean(ctx, &depscan_model.RepoSetting{RepoID: repo.ID})
		require.NoError(t, err)
		defer test.MockVariableValue(&setting.DependencyScanning.EnableByDefault, true)()
		require.NoError(t, ScanRepo(ctx, repo))
		assert.True(t, unittest.AssertExistsAndLoadBean(t, &depscan_model.RepoSetting{RepoID: repo.ID}).FollowsDefault)
		explicit, err := depscan_model.FindExplicitRepoSettings(ctx)
		require.NoError(t, err)
		assert.Empty(t, explicit)

		setting.DependencyScanning.EnableByDefault = false
		assert.False(t, IsRepoScanned(ctx, repo), "the scan did not turn the default into a choice")
	})
}

func TestReviewPull(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := t.Context()
	server := fakeOSV(t)
	defer server.Close()
	defer test.MockVariableValue(&setting.DependencyScanning.Enabled, true)()
	defer test.MockVariableValue(&setting.DependencyScanning.OSVURL, server.URL)()
	defer test.MockVariableValue(&setting.DependencyScanning.AllowedHostList, "loopback")()

	pr := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: 2})
	require.NoError(t, pr.LoadBaseRepo(ctx))
	require.NoError(t, depscan_model.SaveRepoSetting(ctx, &depscan_model.RepoSetting{RepoID: pr.BaseRepoID, Enabled: true}))
	pr.BaseBranch = "depscan-base"
	base := commitLock(t, pr.BaseRepo, "refs/heads/depscan-base", `"node_modules/lodash":{"version":"4.17.21"}`)
	review := func(packages string) (*depscan_model.PullReview, *git_model.CommitStatus) {
		head := commitLock(t, pr.BaseRepo, pr.GetGitHeadRefName(), packages, base)
		require.NoError(t, ReviewPull(ctx, pr))
		r, err := depscan_model.GetPullReview(ctx, pr.ID)
		require.NoError(t, err)
		assert.Equal(t, head, r.HeadSHA)
		return r, unittest.AssertExistsAndLoadBean(t, &git_model.CommitStatus{RepoID: pr.BaseRepoID, SHA: head, Context: reviewStatusContext})
	}

	t.Run("QueuedReviewsArePending", func(t *testing.T) {
		head := commitLock(t, pr.BaseRepo, pr.GetGitHeadRefName(), `"node_modules/lodash":{"version":"4.17.21"}`, base)
		markReviewPending(ctx, pr)
		status := unittest.AssertExistsAndLoadBean(t, &git_model.CommitStatus{RepoID: pr.BaseRepoID, SHA: head, Context: reviewStatusContext})
		assert.Equal(t, commitstatus.CommitStatusPending, status.State, "the pull request page refreshes until the review ends")
	})

	r, status := review(`"node_modules/lodash":{"version":"4.17.15"},"node_modules/left-pad":{"version":"1.3.0"}`)
	assert.Equal(t, base, r.BaseSHA)
	require.Len(t, r.Findings, 1)
	assert.Equal(t, depscan_model.ReviewFinding{
		VulnID: "GHSA-p6mc-m468-83gw", Summary: "Prototype Pollution in lodash", Severity: depscan_model.SeverityHigh,
		Ecosystem: EcosystemNpm, Package: "lodash", Version: "4.17.15", Manifest: "web/package-lock.json", FixedVersions: []string{"4.17.19"},
	}, *r.Findings[0])
	assert.Equal(t, commitstatus.CommitStatusFailure, status.State)
	assert.Equal(t, fmt.Sprintf("%s/security/pulls/%d", pr.BaseRepo.Link(), pr.Index), status.TargetURL)

	for _, failure := range []string{"size", "count", "parse"} {
		t.Run("IncompleteReview/"+failure, func(t *testing.T) {
			packages := `"node_modules/lodash":{"version":"4.17.15"}`
			switch failure {
			case "size":
				packages += strings.Repeat(" ", int(setting.DependencyScanning.MaxManifestSize))
			case "count":
				defer test.MockVariableValue(&setting.DependencyScanning.MaxManifests, 0)()
			case "parse":
				packages = "invalid json"
			}
			head := commitLock(t, pr.BaseRepo, pr.GetGitHeadRefName(), packages, base)
			require.NoError(t, ReviewPull(ctx, pr))
			status := unittest.AssertExistsAndLoadBean(t, &git_model.CommitStatus{RepoID: pr.BaseRepoID, SHA: head, Context: reviewStatusContext})
			assert.Equal(t, commitstatus.CommitStatusError, status.State)
		})
	}

	t.Run("VersionsOfTheMergeBaseAreNotReported", func(t *testing.T) {
		commitLock(t, pr.BaseRepo, "refs/heads/depscan-base", `"node_modules/lodash":{"version":"4.17.15"}`, base)
		r, status := review(`"node_modules/lodash":{"version":"4.17.21"},"node_modules/left-pad":{"version":"1.3.0"}`)
		assert.Equal(t, base, r.BaseSHA, "the base branch moved on, the merge base did not")
		assert.Empty(t, r.Findings)
		assert.Equal(t, commitstatus.CommitStatusSuccess, status.State)
	})

	t.Run("RepositoriesChooseWhatFails", func(t *testing.T) {
		rs := unittest.AssertExistsAndLoadBean(t, &depscan_model.RepoSetting{RepoID: pr.BaseRepoID})
		rs.ReviewFailSeverity = depscan_model.SeverityCritical
		require.NoError(t, depscan_model.SaveRepoSetting(ctx, rs, "review_fail_severity"))
		r, status := review(`"node_modules/lodash":{"version":"4.17.11"}`)
		assert.Len(t, r.Findings, 1, "listed, as high")
		assert.Equal(t, commitstatus.CommitStatusSuccess, status.State, "but not critical")

		rs.ReviewFailSeverity, rs.ReviewAllowList = "", []string{"cve-2020-8203"}
		require.NoError(t, depscan_model.SaveRepoSetting(ctx, rs, "review_fail_severity", "review_allow_list"))
		r, status = review(`"node_modules/lodash":{"version":"4.17.11"},"node_modules/left-pad":{"version":"1.3.0"}`)
		assert.Empty(t, r.Findings, "allowed by an alias")
		assert.Equal(t, commitstatus.CommitStatusSuccess, status.State)
	})

	t.Run("PendingReviewsAlwaysEnd", func(t *testing.T) {
		head := commitLock(t, pr.BaseRepo, pr.GetGitHeadRefName(), `"node_modules/left-pad":{"version":"1.1.0"}`, base)
		markReviewPending(ctx, pr)
		defer test.MockVariableValue(&setting.DependencyScanning.Enabled, false)()
		require.NoError(t, ReviewPull(ctx, pr))
		statuses, err := git_model.GetLatestCommitStatus(ctx, pr.BaseRepoID, head, db.ListOptionsAll)
		require.NoError(t, err)
		require.Len(t, statuses, 1)
		assert.Equal(t, commitstatus.CommitStatusSkipped, statuses[0].State, "scanning was disabled meanwhile")
	})
}

func TestScannedReposCond(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := t.Context()
	for _, rs := range []*depscan_model.RepoSetting{{RepoID: 1, Enabled: true}, {RepoID: 2}, {RepoID: 3, Enabled: true, FollowsDefault: true}} {
		require.NoError(t, depscan_model.SaveRepoSetting(ctx, rs))
	}
	for repoID := range int64(4) {
		require.NoError(t, db.Insert(ctx, &depscan_model.Alert{RepoID: repoID + 1, VulnID: "GHSA-1", Ecosystem: EcosystemNpm, Package: "a", Manifest: "m", Version: "1", State: depscan_model.AlertOpen, Severity: depscan_model.SeverityLow}))
	}
	scanned := func() []int64 {
		alerts, err := db.Find[depscan_model.Alert](ctx, depscan_model.FindAlertsOptions{RepoCond: depscan_model.ScannedReposCond()})
		require.NoError(t, err)
		ids := make([]int64, 0, len(alerts))
		for _, a := range alerts {
			ids = append(ids, a.RepoID)
		}
		return ids
	}
	assert.ElementsMatch(t, []int64{1}, scanned())
	defer test.MockVariableValue(&setting.DependencyScanning.EnableByDefault, true)()
	assert.ElementsMatch(t, []int64{1, 3, 4}, scanned())
}

func TestNotifyAlerts(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := t.Context()
	high := &depscan_model.Alert{RepoID: 1, VulnID: "GHSA-1", Ecosystem: EcosystemNpm, Package: "a", Manifest: "m", Version: "1", State: depscan_model.AlertOpen, Severity: depscan_model.SeverityHigh}
	critical := &depscan_model.Alert{RepoID: 1, VulnID: "GHSA-2", Ecosystem: EcosystemNpm, Package: "b", Manifest: "m", Version: "2", State: depscan_model.AlertOpen, Severity: depscan_model.SeverityCritical}
	require.NoError(t, db.Insert(ctx, high))
	require.NoError(t, db.Insert(ctx, critical))
	notifyAlerts(ctx, 1, []*depscan_model.Alert{high, critical})
	n := unittest.AssertExistsAndLoadBean(t, &activities_model.Notification{UserID: 2, Source: activities_model.NotificationSourceSecurityAlert})
	assert.Equal(t, fmt.Sprintf("security/alerts/%d", critical.ID), n.SubjectLink)
	assert.Equal(t, "b 2: GHSA-2", n.Subject)
	unittest.AssertCount(t, &activities_model.Notification{Source: activities_model.NotificationSourceSecurityAlert}, 1) // user2 is the only administrator, only critical alerts by default

	require.NoError(t, depscan_model.SaveRepoSetting(ctx, &depscan_model.RepoSetting{RepoID: 1, AlertNotifySeverity: depscan_model.SeverityHigh}))
	notifyAlerts(ctx, 1, []*depscan_model.Alert{high})
	unittest.AssertExistsAndLoadBean(t, &activities_model.Notification{UserID: 2, SubjectLink: fmt.Sprintf("security/alerts/%d", high.ID)})

	t.Run("BrokenUpdatesNotifyTheUpdaterOnce", func(t *testing.T) {
		rs := &depscan_model.RepoSetting{RepoID: 2, UpdatesUserID: 2}
		saveUpdatesError(ctx, rs, errors.New("no key"))
		n := unittest.AssertExistsAndLoadBean(t, &activities_model.Notification{UserID: 2, RepoID: 2, SubjectLink: "settings/dependency-scanning"})
		assert.Equal(t, "The dependency updates failed: no key", n.Subject)
		_, err := db.GetEngine(ctx).ID(n.ID).Cols("status").Update(&activities_model.Notification{Status: activities_model.NotificationStatusRead})
		require.NoError(t, err)
		saveUpdatesError(ctx, rs, errors.New("no key"))
		assert.Equal(t, activities_model.NotificationStatusRead, unittest.AssertExistsAndLoadBean(t, &activities_model.Notification{ID: n.ID}).Status, "the same cause is told once")
	})
}

func TestStartUpdates(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := t.Context()
	server := fakeOSV(t)
	defer server.Close()
	defer test.MockVariableValue(&setting.DependencyScanning.Enabled, true)()
	defer test.MockVariableValue(&setting.DependencyScanning.OSVURL, server.URL)()
	defer test.MockVariableValue(&setting.DependencyScanning.AllowedHostList, "loopback")()
	defer test.MockVariableValue(&setting.Agents.Enabled, true)()
	defer test.MockVariableValue(&setting.Actions.Enabled, true)()
	defer test.MockVariableValue(&actions_service.EmitJobsIfReadyByRun, func(int64) error { return nil })()
	defer test.MockVariableValue(&setting.Audit.RecordOutput, setting.AuditRecordOutputDatabase)()

	alice := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	require.NoError(t, depscan_model.SaveRepoSetting(ctx, &depscan_model.RepoSetting{RepoID: repo.ID, Enabled: true}))
	repo.DefaultBranch = "depscan-test"
	base := commitLock(t, repo, "refs/heads/depscan-test", `"node_modules/lodash":{"version":"4.17.15"},"node_modules/debug":{"version":"4.3.4"},"node_modules/ms":{"version":"2.1.2"}`)
	require.NoError(t, ScanRepo(ctx, repo))
	repo.DefaultBranch = "master"

	codex, err := agents_service.CreateAgent(ctx, agents_service.AgentOptions{BotName: "codex", Name: "codex", Provider: agents_model.ProviderOpenAI, Model: "m"})
	require.NoError(t, err)
	require.ErrorIs(t, SaveUpdateSettings(ctx, alice, repo, UpdateOptions{Agent: codex}), agents_service.ErrNoUserKey, "updates use the key of who sets them up")
	_, err = agents_service.SetUserKey(ctx, codex, alice, agents_service.KeyOptions{APIKey: "sk-alice"})
	require.NoError(t, err)
	require.NoError(t, SaveUpdateSettings(ctx, alice, repo, UpdateOptions{Agent: codex, Ignore: []string{"@types/*"}}))
	event := unittest.AssertExistsAndLoadBean(t, &audit_model.Event{Action: audit_model.RepositoryDependencyUpdates, ScopeID: repo.ID})
	assert.Contains(t, event.Message, "run by agent codex")

	scanning, err := depscan_model.GetRepoSetting(ctx, repo.ID)
	require.NoError(t, err)
	startUpdates := func(force bool) int {
		started, err := StartUpdates(ctx, repo, force)
		require.NoError(t, err)
		return started
	}
	assert.Equal(t, 1, startUpdates(true))
	scanning.LastScanError = "a scan ending meanwhile"
	require.NoError(t, depscan_model.SaveRepoSetting(ctx, scanning, "last_scan_error"))
	rs, err := depscan_model.GetRepoSetting(ctx, repo.ID)
	require.NoError(t, err)
	assert.Equal(t, depscan_model.UpdatesWeekly, rs.UpdatesInterval)
	require.Contains(t, rs.UpdateSessions, EcosystemNpm)
	s := unittest.AssertExistsAndLoadBean(t, &agents_model.Session{ID: rs.UpdateSessions[EcosystemNpm]})
	assert.Equal(t, "Update the npm dependencies", s.Title)
	r := unittest.AssertExistsAndLoadBean(t, &agents_model.Run{SessionID: s.ID})
	assert.Equal(t, alice.ID, r.TriggerUserID)
	assert.Contains(t, r.Prompt, "- lodash 4.17.15 in web/package-lock.json, up to 4.x: CVE-2020-8203 (fixed in 4.17.19)")
	assert.Contains(t, r.Prompt, "@types/*")
	assert.Contains(t, r.Prompt, "within their current major version")

	assert.Zero(t, startUpdates(true))
	unittest.AssertCount(t, &agents_model.Run{SessionID: s.ID}, 1) // the update is still running, the next due one continues it
	unittest.AssertCount(t, &agents_model.Session{RepoID: repo.ID, AgentID: codex.ID}, 1)

	require.NoError(t, StartDueUpdates(ctx))
	unittest.AssertCount(t, &agents_model.Run{SessionID: s.ID}, 1) // not due before a week

	_, err = db.GetEngine(ctx).ID(r.ID).Cols("status").Update(&agents_model.Run{Status: agents_model.StatusSuccess})
	require.NoError(t, err)
	commitLock(t, repo, "refs/heads/"+s.Branch, `"node_modules/lodash":{"version":"5.0.0"},"node_modules/debug":{"version":"5.0.0"},"node_modules/ms":{"version":"3.0.0"},"node_modules/x/node_modules/ms":{"version":"2.1.3"}`)
	assert.Equal(t, 1, startUpdates(true))
	unittest.AssertCount(t, &agents_model.Session{RepoID: repo.ID, AgentID: codex.ID}, 1)
	again, err := agents_model.GetLatestRun(ctx, s.ID)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(again.Prompt, "Update the npm dependencies again\n"), "the next update continues the session, its commits keep a short title")
	assert.Contains(t, again.Prompt, "- lodash 5.0.0 in web/package-lock.json: go back to 4.x", "upgrades beyond the rules are named")
	assert.NotContains(t, again.Prompt, "- debug", "indirect dependencies follow the direct ones")
	assert.NotContains(t, again.Prompt, "- ms", "a version is left on the allowed line")

	t.Run("UnchangedInputsWait", func(t *testing.T) {
		_, err := db.GetEngine(ctx).Where("session_id = ?", s.ID).Cols("status").Update(&agents_model.Run{Status: agents_model.StatusSuccess})
		require.NoError(t, err)
		assert.Zero(t, startUpdates(false))
		unittest.AssertCount(t, &agents_model.Run{SessionID: s.ID}, 2) // nothing changed since the last update

		rs, err := depscan_model.GetRepoSetting(ctx, repo.ID)
		require.NoError(t, err)
		rs.LastScanSHA = "a-new-commit"
		require.NoError(t, depscan_model.SaveRepoSetting(ctx, rs, "last_scan_sha"))
		assert.Equal(t, 1, startUpdates(false))
		unittest.AssertCount(t, &agents_model.Run{SessionID: s.ID}, 3) // the default branch moved

		_, err = db.GetEngine(ctx).Where("session_id = ?", s.ID).Cols("status").Update(&agents_model.Run{Status: agents_model.StatusFailure})
		require.NoError(t, err)
		assert.Equal(t, 1, startUpdates(false), "a failed update is retried")
		unittest.AssertCount(t, &agents_model.Run{SessionID: s.ID}, 4)

		require.NoError(t, SaveUpdateSettings(ctx, alice, repo, UpdateOptions{Agent: codex, Major: true, Ignore: []string{"@types/*"}}))
		assert.Zero(t, startUpdates(false), "the previous update is still running")
		_, err = db.GetEngine(ctx).Where("session_id = ?", s.ID).Cols("status").Update(&agents_model.Run{Status: agents_model.StatusSuccess})
		require.NoError(t, err)
		assert.Equal(t, 1, startUpdates(false), "the settings changed while it ran")
		unittest.AssertCount(t, &agents_model.Run{SessionID: s.ID}, 5)

		_, err = db.GetEngine(ctx).Where("session_id = ?", s.ID).Cols("status", "created").Update(&agents_model.Run{Status: agents_model.StatusSuccess, Created: timeutil.TimeStamp(time.Now().Add(-fullUpdateEvery).Unix())})
		require.NoError(t, err)
		assert.Equal(t, 1, startUpdates(false))
		unittest.AssertCount(t, &agents_model.Run{SessionID: s.ID}, 6) // new releases are checked regularly anyway
	})

	t.Run("ClosedPullRequestsRefuseTheirUpgrades", func(t *testing.T) {
		commitLock(t, repo, "refs/pull/9999/head", `"node_modules/lodash":{"version":"5.0.0"},"node_modules/debug":{"version":"5.0.0"},"node_modules/ms":{"version":"3.0.0"},"node_modules/x/node_modules/ms":{"version":"2.1.3"}`, base)
		pr := &issues_model.PullRequest{Index: 9999, BaseRepoID: repo.ID, HeadRepoID: repo.ID, BaseRepo: repo, BaseBranch: "depscan-test", HeadBranch: s.Branch}
		require.NoError(t, refuseUpgrades(ctx, pr))
		refused := []*depscan_model.RefusedVersion{{Ecosystem: EcosystemNpm, Package: "lodash", Version: "5.0.0"}, {Ecosystem: EcosystemNpm, Package: "ms", Version: "3.0.0"}}
		assert.Equal(t, refused, unittest.AssertExistsAndLoadBean(t, &depscan_model.RepoSetting{RepoID: repo.ID}).UpdatesRefused, "the direct dependencies the pull request upgraded")

		_, err := db.GetEngine(ctx).Where("session_id = ?", s.ID).Cols("status").Update(&agents_model.Run{Status: agents_model.StatusSuccess})
		require.NoError(t, err)
		assert.Equal(t, 1, startUpdates(true))
		latest, err := agents_model.GetLatestRun(ctx, s.ID)
		require.NoError(t, err)
		assert.Contains(t, latest.Prompt, "These upgrades were refused, never make them: lodash 5.0.0, ms 3.0.0.")
		assert.Contains(t, latest.Prompt, "- ms 3.0.0 in web/package-lock.json: this version was refused")

		require.NoError(t, RemoveRefusedVersion(ctx, repo, *refused[1]))
		assert.Equal(t, refused[:1], unittest.AssertExistsAndLoadBean(t, &depscan_model.RepoSetting{RepoID: repo.ID}).UpdatesRefused)
	})

	t.Run("IgnoreCommands", func(t *testing.T) {
		_, err := db.GetEngine(ctx).Where("session_id = ?", s.ID).Cols("status").Update(&agents_model.Run{Status: agents_model.StatusSuccess})
		require.NoError(t, err)
		issue := unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{ID: 2})
		issue.PullRequest = &issues_model.PullRequest{BaseRepoID: repo.ID, HeadRepoID: repo.ID, HeadBranch: "branch1"}
		assert.False(t, handleIgnoreCommand(ctx, alice, codex, issue, "@codex ignore lodash"), "only in update pull requests")
		issue.PullRequest.HeadBranch = s.Branch
		assert.False(t, handleIgnoreCommand(ctx, alice, codex, issue, "@codex why `@codex ignore lodash`?"), "a request is not a command")

		lastReply := func() string {
			c := &issues_model.Comment{IssueID: issue.ID, PosterID: codex.BotID}
			_, err := db.GetEngine(ctx).Desc("id").Get(c)
			require.NoError(t, err)
			return c.Content
		}
		bob := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 4})
		assert.True(t, handleIgnoreCommand(ctx, bob, codex, issue, "@codex ignore lodash"))
		assert.Equal(t, "@user4 only the administrators of this repository can change the ignored dependencies.", lastReply())
		assert.True(t, handleIgnoreCommand(ctx, alice, codex, issue, "@codex ignore lodash\n@codex ignore ms@"))
		assert.Contains(t, lastReply(), "nothing was ignored")

		runs := unittest.GetCount(t, &agents_model.Run{SessionID: s.ID})
		assert.True(t, handleIgnoreCommand(ctx, alice, codex, issue, "Too risky.\n@Codex ignore ms@>=3\n@codex ignore @types/*"))
		assert.Equal(t, []string{"@types/*", "ms@>=3"}, unittest.AssertExistsAndLoadBean(t, &depscan_model.RepoSetting{RepoID: repo.ID}).UpdatesIgnore)
		assert.Equal(t, "@user2 the dependency updates now ignore `ms@>=3`, `@types/*`. I am bringing this pull request in line.", lastReply())
		unittest.AssertCount(t, &agents_model.Run{SessionID: s.ID}, runs+1) // the updater's command updates the pull request at once
	})

	t.Run("NewAlertsStartSecurityUpdates", func(t *testing.T) {
		require.NoError(t, SaveUpdateSettings(ctx, alice, repo, UpdateOptions{Agent: codex, Major: true, Ignore: []string{"lodash@no-major"}, Scope: depscan_model.UpdatesScopeSecurity, OnAlert: true}))
		_, err := db.GetEngine(ctx).Where("session_id = ?", s.ID).Cols("status").Update(&agents_model.Run{Status: agents_model.StatusSuccess})
		require.NoError(t, err)
		lastUnix := unittest.AssertExistsAndLoadBean(t, &depscan_model.RepoSetting{RepoID: repo.ID}).UpdatesLastUnix
		runs := unittest.GetCount(t, &agents_model.Run{SessionID: s.ID})

		repo.DefaultBranch = "depscan-test"
		defer func() { repo.DefaultBranch = "master" }()
		commitLock(t, repo, "refs/heads/depscan-test", `"node_modules/lodash":{"version":"4.17.21"}`)
		require.NoError(t, ScanRepo(ctx, repo))
		unittest.AssertCount(t, &agents_model.Run{SessionID: s.ID}, runs) // fixing an alert starts nothing

		commitLock(t, repo, "refs/heads/depscan-test", `"node_modules/lodash":{"version":"4.17.15"}`)
		require.NoError(t, ScanRepo(ctx, repo))
		unittest.AssertCount(t, &agents_model.Run{SessionID: s.ID}, runs+1)
		latest, err := agents_model.GetLatestRun(ctx, s.ID)
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(latest.Prompt, "Update the vulnerable npm dependencies again\n"))
		assert.Contains(t, latest.Prompt, "and nothing else")
		assert.Contains(t, latest.Prompt, "- lodash 4.17.15 in web/package-lock.json, up to 4.x: CVE-2020-8203 (fixed in 4.17.19)", "kept within its major version by its rule")
		assert.Equal(t, lastUnix, unittest.AssertExistsAndLoadBean(t, &depscan_model.RepoSetting{RepoID: repo.ID}).UpdatesLastUnix, "the schedule of the full updates is kept")
	})

	t.Run("BrokenUpdatesTellWhy", func(t *testing.T) {
		require.NoError(t, agents_model.DeleteUserKey(ctx, codex.ID, alice.ID))
		_, err := StartUpdates(ctx, repo, true)
		require.ErrorIs(t, err, agents_service.ErrNoUserKey)
		rs := unittest.AssertExistsAndLoadBean(t, &depscan_model.RepoSetting{RepoID: repo.ID})
		assert.Contains(t, rs.UpdatesLastError, "@user2, who set the updates up, has no API key for @codex")

		rs.Enabled, rs.UpdatesLastUnix = false, 0
		require.NoError(t, depscan_model.SaveRepoSetting(ctx, rs, "enabled", "updates_last_unix"))
		require.NoError(t, StartDueUpdates(ctx))
		assert.Zero(t, unittest.AssertExistsAndLoadBean(t, &depscan_model.RepoSetting{RepoID: repo.ID}).UpdatesLastUnix, "skipped while scanning is off")
	})
}

func TestAllowedLines(t *testing.T) {
	deps := []*depscan_model.Dependency{
		{Manifest: "composer.lock", Name: "guzzlehttp/guzzle", Version: "6.3.3"},
		{Manifest: "composer.lock", Name: "monolog/monolog", Version: "1.26.0"},
		{Manifest: "Cargo.lock", Name: "tiny", Version: "0.3.1"},
		{Manifest: "package-lock.json", Name: "debug", Version: "4.3.4"},
		{Manifest: "package-lock.json", Name: "debug", Version: "2.6.9"},
	}
	alerts := []*depscan_model.Alert{
		{Manifest: "composer.lock", Package: "guzzlehttp/guzzle", Version: "6.3.3", FixedVersions: []string{"6.5.8", "7.4.5"}},
		{Manifest: "composer.lock", Package: "guzzlehttp/guzzle", Version: "6.3.3", FixedVersions: []string{"7.12.1"}},
		{Manifest: "Cargo.lock", Package: "tiny", Version: "0.3.1", FixedVersions: []string{"0.4.2", "1.0.1"}},
	}
	allowed := allowedLines(deps, alerts)
	assert.Equal(t, "7.x", allowed[lineKey("composer.lock", "guzzlehttp/guzzle")].String(), "the first line fixing every vulnerability")
	assert.Equal(t, "1.x", allowed[lineKey("composer.lock", "monolog/monolog")].String())
	assert.Equal(t, "0.4.x", allowed[lineKey("Cargo.lock", "tiny")].String(), "a minor version of 0.x is a major one")
	assert.Equal(t, "4.x", allowed[lineKey("package-lock.json", "debug")].String(), "the newest of the versions a lockfile pins")
}

func TestUpdatePromptSkipsIgnoredPackages(t *testing.T) {
	alerts := []*depscan_model.Alert{
		{Ecosystem: EcosystemNpm, Manifest: "package-lock.json", Package: "@types/node", Version: "1.0.0", VulnID: "GHSA-1", FixedVersions: []string{"1.0.1"}},
		{Ecosystem: EcosystemNpm, Manifest: "package-lock.json", Package: "lodash", Version: "4.17.15", VulnID: "GHSA-2", FixedVersions: []string{"4.17.19"}},
	}
	prompt := updatePrompt(EcosystemNpm, []string{"package-lock.json"}, alerts, nil, &depscan_model.RepoSetting{UpdatesMajor: true, UpdatesIgnore: []string{"@Types/*", "react@no-major", "vue@>=4"}})
	assert.NotContains(t, prompt, "- @types/node")
	assert.Contains(t, prompt, "- lodash 4.17.15 in package-lock.json: GHSA-2 (fixed in 4.17.19)")
	assert.Contains(t, prompt, "Leave these packages as they are: @Types/*.\n")
	assert.Contains(t, prompt, "Keep these packages within their current major version, or minor version for versions 0.x: react.\n")
	assert.Contains(t, prompt, "Never upgrade to these versions, given as package@range: vue@>=4.\n")
}

func TestIgnoreRules(t *testing.T) {
	rules := parseIgnoreRules([]string{"@types/*", "react@no-major", "lodash@>=5", "django@4.2.x || >= 5.1, <5.2", "@scope/pkg@1.x"})
	assert.True(t, rules.leaves("@types/node"))
	assert.False(t, rules.leaves("react"))
	assert.False(t, rules.leaves("lodash"))
	assert.True(t, rules.keepsMajor("React"))
	assert.True(t, rules.skips("lodash", "5.0.0"))
	assert.False(t, rules.skips("lodash", "4.17.21"))
	assert.True(t, rules.skips("django", "4.2.7"))
	assert.False(t, rules.skips("django", "4.1.0"))
	assert.True(t, rules.skips("django", "5.1.3"))
	assert.False(t, rules.skips("django", "5.2"))
	assert.True(t, rules.skips("@scope/pkg", "1.4.0"))
	assert.False(t, rules.skips("@scope/pkg", "2.0.0"))
	for _, invalid := range []string{"lodash@>=", "@", "lodash@", "lodash@1.x ||"} {
		_, err := parseIgnoreRule(invalid)
		assert.Error(t, err, invalid)
	}
}

func TestGetVuln(t *testing.T) {
	server := fakeOSV(t)
	defer server.Close()
	defer test.MockVariableValue(&setting.DependencyScanning.OSVURL, server.URL)()
	defer test.MockVariableValue(&setting.DependencyScanning.AllowedHostList, "loopback")()

	_, err := newOSVClient(1).GetVuln(t.Context(), "GHSA-p6mc-m468-83gw")
	require.NoError(t, err)
	limited := newOSVClient(0)
	v, err := limited.GetVuln(t.Context(), "GHSA-p6mc-m468-83gw")
	require.NoError(t, err, "cached records do not count")
	assert.Equal(t, "Prototype Pollution in lodash", v.Summary)
	_, err = limited.GetVuln(t.Context(), "GHSA-other")
	assert.ErrorContains(t, err, "more than 0 vulnerability records")
}

func TestCompareVersions(t *testing.T) {
	ordered := []string{"0.9", "1.0-rc.1", "1.0", "1.0.post1", "v1.0.1", "1.2", "1.10.0", "2.0b1", "2.0", "10.0.0"}
	for i := 1; i < len(ordered); i++ {
		assert.Negative(t, compareVersions(ordered[i-1], ordered[i]), "%s < %s", ordered[i-1], ordered[i])
		assert.Positive(t, compareVersions(ordered[i], ordered[i-1]), "%s > %s", ordered[i], ordered[i-1])
	}
	assert.Zero(t, compareVersions("v1.2.3", "1.2.3+build"))
}

func TestFixedVersions(t *testing.T) {
	var v OSVVuln
	require.NoError(t, json.Unmarshal([]byte(`{"affected":[
		{"package":{"ecosystem":"PyPI","name":"Django_Rest.Framework"},"ranges":[
			{"type":"GIT","events":[{"introduced":"0"},{"fixed":"9f3c2a"}]},
			{"type":"ECOSYSTEM","events":[{"introduced":"0"},{"fixed":"3.2.18"},{"introduced":"4.0"},{"fixed":"4.0.10"},{"introduced":"4.1"},{"fixed":"4.1.7"}]}]},
		{"package":{"ecosystem":"PyPI","name":"other"},"ranges":[{"type":"ECOSYSTEM","events":[{"introduced":"0"},{"fixed":"1.0"}]}]}]}`), &v))
	pkg := func(version string) Package { return Package{EcosystemPyPI, "django-rest-framework", version} }
	assert.Equal(t, []string{"4.0.10"}, v.FixedVersions(pkg("4.0.2")), "only the range containing the version, names compared as PEP 503")
	assert.Equal(t, []string{"3.2.18"}, v.FixedVersions(pkg("3.0")))
	assert.Equal(t, []string{"4.0.10", "4.1.7"}, v.FixedVersions(pkg("3.9")), "outside every range, the fixed versions above it")
}
