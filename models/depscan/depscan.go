// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package depscan stores the dependencies of repositories and the vulnerability alerts found in them
package depscan

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"time"

	"gitea.dev/models/db"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/timeutil"
	"gitea.dev/modules/util"

	"xorm.io/builder"
)

func init() {
	db.RegisterModel(new(RepoSetting))
	db.RegisterModel(new(Dependency))
	db.RegisterModel(new(Alert))
	db.RegisterModel(new(PullReview))
}

// RepoSetting is whether a repository is scanned, and the outcome of its latest scan
type RepoSetting struct {
	ID      int64
	RepoID  int64 `xorm:"UNIQUE NOT NULL"`
	Enabled bool  `xorm:"NOT NULL DEFAULT false"`
	// FollowsDefault marks a setting a scan created, Enabled is then the instance default rather than the administrators' choice
	FollowsDefault bool `xorm:"NOT NULL DEFAULT false"`

	LastScanSHA          string `xorm:"VARCHAR(64) NOT NULL DEFAULT ''"`
	LastScanUnix         timeutil.TimeStamp
	LastScanError        string `xorm:"TEXT"`
	LastScanDependencies int    `xorm:"NOT NULL DEFAULT 0"`
	// AlertNotifySeverity is the least severe alert a scan notifies the administrators of, empty for critical
	AlertNotifySeverity Severity `xorm:"VARCHAR(16) NOT NULL DEFAULT ''"`
	// ReviewFailSeverity is the least severe vulnerability failing a pull request's dependency review, empty for any
	ReviewFailSeverity Severity `xorm:"VARCHAR(16) NOT NULL DEFAULT ''"`
	// ReviewAllowList are the vulnerability IDs or aliases the dependency reviews do not report
	ReviewAllowList []string `xorm:"TEXT JSON"`

	// UpdatesAgentID updates the dependencies every UpdatesInterval, with the API key and the permissions of UpdatesUserID who chose it
	UpdatesAgentID  int64           `xorm:"NOT NULL DEFAULT 0"`
	UpdatesUserID   int64           `xorm:"NOT NULL DEFAULT 0"`
	UpdatesInterval UpdatesInterval `xorm:"VARCHAR(16) NOT NULL DEFAULT ''"`
	UpdatesMajor    bool            `xorm:"NOT NULL DEFAULT false"`
	UpdatesIgnore   []string        `xorm:"TEXT JSON"` // package names, with * wildcards
	UpdatesScope    UpdatesScope    `xorm:"VARCHAR(16) NOT NULL DEFAULT ''"`
	// UpdatesOnAlert starts the updates of an ecosystem as soon as a scan opens a high or critical alert in it
	UpdatesOnAlert  bool `xorm:"NOT NULL DEFAULT false"`
	UpdatesLastUnix timeutil.TimeStamp
	// UpdatesRefused are the upgrades of update pull requests closed without merging, later updates never propose them again
	UpdatesRefused []*RefusedVersion `xorm:"TEXT JSON"`
	// UpdatesLastError is why the latest updates failed, such as the updater losing their key or the agent being disabled
	UpdatesLastError string `xorm:"TEXT"`
	// UpdateSessions is the agent session updating each ecosystem, it keeps updating its pull request while it is open
	UpdateSessions map[string]int64 `xorm:"TEXT JSON"`
	// UpdateInputs is what each ecosystem's last update saw: the default branch's commit and the open alerts
	UpdateInputs map[string]string `xorm:"TEXT JSON"`

	Updated timeutil.TimeStamp `xorm:"updated"`
}

func (*RepoSetting) TableName() string {
	return "depscan_repo_setting"
}

// RefusedVersion is a package version the dependency updates no longer propose
type RefusedVersion struct {
	Ecosystem string `json:"ecosystem"`
	Package   string `json:"package"`
	Version   string `json:"version"`
}

// UpdatesScope is which dependencies the updates upgrade
type UpdatesScope string

const (
	UpdatesScopeAll      UpdatesScope = "all" // every outdated one, the vulnerable ones first
	UpdatesScopeSecurity UpdatesScope = "security"
)

var UpdatesScopes = []UpdatesScope{UpdatesScopeAll, UpdatesScopeSecurity}

type UpdatesInterval string

const (
	UpdatesDaily   UpdatesInterval = "daily"
	UpdatesWeekly  UpdatesInterval = "weekly"
	UpdatesMonthly UpdatesInterval = "monthly"
)

var UpdatesIntervals = []UpdatesInterval{UpdatesDaily, UpdatesWeekly, UpdatesMonthly}

func (i UpdatesInterval) Duration() time.Duration {
	switch i {
	case UpdatesDaily:
		return 24 * time.Hour
	case UpdatesMonthly:
		return 30 * 24 * time.Hour
	default:
		return 7 * 24 * time.Hour
	}
}

// UpdatesSecurityOnly reports whether the updates only upgrade the vulnerable dependencies
func (rs *RepoSetting) UpdatesSecurityOnly() bool {
	return rs.UpdatesScope == UpdatesScopeSecurity
}

// GetRepoSetting returns the setting of a repository, the instance default when it has none
func GetRepoSetting(ctx context.Context, repoID int64) (*RepoSetting, error) {
	rs := &RepoSetting{}
	has, err := db.GetEngine(ctx).Where("repo_id = ?", repoID).Get(rs)
	if err != nil {
		return nil, err
	} else if !has {
		return &RepoSetting{RepoID: repoID, Enabled: setting.DependencyScanning.EnableByDefault}, nil
	}
	if rs.FollowsDefault {
		rs.Enabled = setting.DependencyScanning.EnableByDefault
	}
	return rs, nil
}

// SaveRepoSetting updates only the given columns of an existing setting: scans, updates and administrators change it concurrently
func SaveRepoSetting(ctx context.Context, rs *RepoSetting, cols ...string) error {
	if rs.ID == 0 {
		err := db.Insert(ctx, rs)
		if err == nil {
			return nil
		}
		// another writer may have created it meanwhile
		existing := &RepoSetting{}
		if has, getErr := db.GetEngine(ctx).Where("repo_id = ?", rs.RepoID).Get(existing); getErr != nil || !has {
			return err
		}
		rs.ID = existing.ID
	}
	_, err := db.GetEngine(ctx).ID(rs.ID).Cols(cols...).Update(rs)
	return err
}

// ExplicitRepoSettingsCond selects the repositories whose administrators chose to scan them or not
func ExplicitRepoSettingsCond() builder.Cond {
	return builder.Eq{"follows_default": false}
}

// ScannedReposCond selects the rows of the repositories still scanned by their repo_id, the alerts of the others are stale
func ScannedReposCond() builder.Cond {
	explicit := func(enabled bool) *builder.Builder {
		return builder.Select("repo_id").From("depscan_repo_setting").Where(builder.Eq{"enabled": enabled, "follows_default": false})
	}
	if setting.DependencyScanning.EnableByDefault {
		return builder.NotIn("repo_id", explicit(false))
	}
	return builder.In("repo_id", explicit(true))
}

// FindExplicitRepoSettings returns the repositories whose administrators chose to scan them or not
func FindExplicitRepoSettings(ctx context.Context) ([]*RepoSetting, error) {
	settings := make([]*RepoSetting, 0, 16)
	return settings, db.GetEngine(ctx).Where(ExplicitRepoSettingsCond()).Find(&settings)
}

// FindUpdatedRepoSettings returns the repositories whose dependencies an agent updates
func FindUpdatedRepoSettings(ctx context.Context) ([]*RepoSetting, error) {
	settings := make([]*RepoSetting, 0, 16)
	return settings, db.GetEngine(ctx).Where("updates_agent_id > 0").Find(&settings)
}

// Dependency is a package version a manifest or lockfile of the default branch pins
type Dependency struct {
	ID        int64
	RepoID    int64  `xorm:"INDEX NOT NULL"`
	Manifest  string `xorm:"VARCHAR(1024) NOT NULL"`
	Ecosystem string `xorm:"VARCHAR(32) NOT NULL"`
	Name      string `xorm:"VARCHAR(512) NOT NULL"`
	Version   string `xorm:"VARCHAR(255) NOT NULL"`
}

func (*Dependency) TableName() string {
	return "depscan_dependency"
}

// ReplaceDependencies stores the dependencies of the latest scan
func ReplaceDependencies(ctx context.Context, repoID int64, deps []*Dependency) error {
	return db.WithTx(ctx, func(ctx context.Context) error {
		if _, err := db.GetEngine(ctx).Where("repo_id = ?", repoID).Delete(new(Dependency)); err != nil {
			return err
		}
		for i := 0; i < len(deps); i += 500 {
			if err := db.Insert(ctx, deps[i:min(i+500, len(deps))]); err != nil {
				return err
			}
		}
		return nil
	})
}

type FindDependenciesOptions struct {
	db.ListOptions
	RepoID  int64
	Keyword string
}

func (opts FindDependenciesOptions) ToConds() builder.Cond {
	cond := builder.NewCond().And(builder.Eq{"repo_id": opts.RepoID})
	if opts.Keyword != "" {
		cond = cond.And(db.BuildCaseInsensitiveLike("name", opts.Keyword))
	}
	return cond
}

func (opts FindDependenciesOptions) ToOrders() string {
	return "ecosystem, name, id"
}

type AlertState string

const (
	AlertOpen      AlertState = "open"
	AlertFixed     AlertState = "fixed" // the vulnerable version is no longer used
	AlertDismissed AlertState = "dismissed"
)

// IsValid reports whether a state can be used to filter alerts
func (s AlertState) IsValid() bool {
	return slices.Contains([]AlertState{AlertOpen, AlertFixed, AlertDismissed}, s)
}

type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityHigh     Severity = "high"
	SeverityModerate Severity = "moderate"
	SeverityLow      Severity = "low"
	SeverityUnknown  Severity = "unknown"
)

// Severities are ordered from the most severe
var Severities = []Severity{SeverityCritical, SeverityHigh, SeverityModerate, SeverityLow, SeverityUnknown}

// SeverityNone is a threshold no alert reaches
const SeverityNone Severity = "none"

// Reaches reports whether an alert of this severity reaches a threshold, unknown severities only reach an unknown threshold
func (s Severity) Reaches(threshold Severity) bool {
	i, limit := slices.Index(Severities, s), slices.Index(Severities, threshold)
	return i >= 0 && limit >= 0 && i <= limit
}

// IsThreshold reports whether a severity can be used as a threshold
func (s Severity) IsThreshold() bool {
	return s == SeverityNone || slices.Contains(Severities, s)
}

// ReviewFailsOn reports whether a vulnerability added by a pull request fails its dependency review
func (rs *RepoSetting) ReviewFailsOn(s Severity) bool {
	return s.Reaches(util.IfZero(rs.ReviewFailSeverity, SeverityUnknown))
}

// NotifiesOf reports whether a new alert of a severity is notified to the administrators
func (rs *RepoSetting) NotifiesOf(s Severity) bool {
	return s.Reaches(util.IfZero(rs.AlertNotifySeverity, SeverityCritical))
}

// Alert is a known vulnerability affecting a dependency version of a manifest
type Alert struct {
	ID            int64
	RepoID        int64      `xorm:"UNIQUE(key) INDEX NOT NULL"`
	AlertKey      string     `xorm:"UNIQUE(key) CHAR(64)"` // NULL in the rows older than the column until BackfillAlertKeys
	VulnID        string     `xorm:"VARCHAR(255) NOT NULL"`
	Ecosystem     string     `xorm:"VARCHAR(32) NOT NULL"`
	Package       string     `xorm:"VARCHAR(512) NOT NULL"`
	Manifest      string     `xorm:"VARCHAR(1024) NOT NULL"`
	Version       string     `xorm:"VARCHAR(255) NOT NULL"`
	State         AlertState `xorm:"VARCHAR(16) INDEX NOT NULL"`
	Severity      Severity   `xorm:"VARCHAR(16) NOT NULL"`
	Score         float64    `xorm:"NOT NULL DEFAULT 0"`
	Aliases       []string   `xorm:"TEXT JSON"`
	Summary       string     `xorm:"TEXT"`
	Details       string     `xorm:"LONGTEXT"`
	FixedVersions []string   `xorm:"TEXT JSON"`
	References    []string   `xorm:"TEXT JSON"`

	DismissedBy     int64  `xorm:"NOT NULL DEFAULT 0"`
	DismissedReason string `xorm:"TEXT"`
	FixedUnix       timeutil.TimeStamp

	Created timeutil.TimeStamp `xorm:"created"`
	Updated timeutil.TimeStamp `xorm:"updated"`
}

func (*Alert) TableName() string {
	return "depscan_alert"
}

// Key identifies the alert of a vulnerability in a package of a manifest, these columns together exceed MySQL's index size
func (a *Alert) Key() string {
	sum := sha256.Sum256([]byte(a.VulnID + "\x00" + a.Ecosystem + "\x00" + a.Package + "\x00" + a.Manifest))
	return hex.EncodeToString(sum[:])
}

func (a *Alert) BeforeInsert() {
	a.AlertKey = a.Key()
}

// BackfillAlertKeys fills the keys of the alerts stored before the column existed
func BackfillAlertKeys(ctx context.Context) error {
	return db.Iterate(ctx, builder.IsNull{"alert_key"}, func(ctx context.Context, a *Alert) error {
		a.AlertKey = a.Key()
		_, err := db.GetEngine(ctx).ID(a.ID).Cols("alert_key").NoAutoTime().Update(a)
		return err
	})
}

// Title names an alert by its most common identifier, a CVE when it has one
func (a *Alert) Title() string {
	for _, alias := range a.Aliases {
		if strings.HasPrefix(alias, "CVE-") {
			return alias
		}
	}
	return a.VulnID
}

func GetRepoAlert(ctx context.Context, repoID, id int64) (*Alert, error) {
	a, exist, err := db.GetByID[Alert](ctx, id)
	if err != nil {
		return nil, err
	} else if !exist || a.RepoID != repoID {
		return nil, util.NewNotExistErrorf("alert %d does not exist", id)
	}
	return a, nil
}

type FindAlertsOptions struct {
	db.ListOptions
	RepoID   int64
	RepoCond builder.Cond // on repo_id, used instead of RepoID when RepoID is 0
	State    AlertState
	Severity Severity
}

func (opts FindAlertsOptions) ToConds() builder.Cond {
	cond := builder.NewCond().And(builder.Eq{"repo_id": opts.RepoID})
	if opts.RepoID == 0 && opts.RepoCond != nil {
		cond = builder.NewCond().And(opts.RepoCond)
	}
	if opts.State != "" {
		cond = cond.And(builder.Eq{"state": opts.State})
	}
	if opts.Severity != "" {
		cond = cond.And(builder.Eq{"severity": opts.Severity})
	}
	return cond
}

func (opts FindAlertsOptions) ToOrders() string {
	return "score DESC, id DESC"
}

func UpdateAlert(ctx context.Context, a *Alert, cols ...string) error {
	_, err := db.GetEngine(ctx).ID(a.ID).Cols(cols...).Update(a)
	return err
}

// ChangeAlertState updates an alert only while it is still in the from state, scans and users change states concurrently
func ChangeAlertState(ctx context.Context, a *Alert, from AlertState, cols ...string) (bool, error) {
	n, err := db.GetEngine(ctx).ID(a.ID).Where("state = ?", from).Cols(cols...).Update(a)
	return n > 0, err
}

// CountOpenAlerts returns the open alerts per severity of the repositories matching a condition on repo_id
func CountOpenAlerts(ctx context.Context, repoCond builder.Cond) (map[Severity]int64, error) {
	rows := make([]struct {
		Severity Severity
		Count    int64
	}, 0, 5)
	err := db.GetEngine(ctx).Table("depscan_alert").Where(repoCond).And("state = ?", AlertOpen).
		GroupBy("severity").Select("severity, COUNT(*) AS count").Find(&rows)
	counts := make(map[Severity]int64, len(rows))
	for _, row := range rows {
		counts[row.Severity] = row.Count
	}
	return counts, err
}

// ReviewFinding is a vulnerable package version a pull request adds
type ReviewFinding struct {
	VulnID        string   `json:"vuln_id"`
	Summary       string   `json:"summary"`
	Severity      Severity `json:"severity"`
	Ecosystem     string   `json:"ecosystem"`
	Package       string   `json:"package"`
	Version       string   `json:"version"`
	Manifest      string   `json:"manifest"`
	FixedVersions []string `json:"fixed_versions"`
}

// PullReview is the latest dependency review of a pull request, against its merge base
type PullReview struct {
	ID       int64
	RepoID   int64              `xorm:"INDEX NOT NULL"`
	PullID   int64              `xorm:"UNIQUE NOT NULL"`
	HeadSHA  string             `xorm:"VARCHAR(64) NOT NULL"`
	BaseSHA  string             `xorm:"VARCHAR(64) NOT NULL"`
	Error    string             `xorm:"TEXT"`
	Findings []*ReviewFinding   `xorm:"LONGTEXT JSON"`
	Updated  timeutil.TimeStamp `xorm:"updated"`
}

func (*PullReview) TableName() string {
	return "depscan_pull_review"
}

func GetPullReview(ctx context.Context, pullID int64) (*PullReview, error) {
	r := &PullReview{}
	if has, err := db.GetEngine(ctx).Where("pull_id = ?", pullID).Get(r); err != nil {
		return nil, err
	} else if !has {
		return nil, util.NewNotExistErrorf("no dependency review for pull request %d", pullID)
	}
	return r, nil
}

// SavePullReview replaces the review of a pull request
func SavePullReview(ctx context.Context, r *PullReview) error {
	return db.WithTx(ctx, func(ctx context.Context) error {
		if _, err := db.GetEngine(ctx).Where("pull_id = ?", r.PullID).Delete(&PullReview{}); err != nil {
			return err
		}
		return db.Insert(ctx, r)
	})
}

// DeleteRepoData deletes the scanning setting, dependencies, alerts and pull request reviews of a repository
func DeleteRepoData(ctx context.Context, repoID int64) error {
	return db.DeleteBeans(ctx, &RepoSetting{RepoID: repoID}, &Dependency{RepoID: repoID}, &Alert{RepoID: repoID}, &PullReview{RepoID: repoID})
}
