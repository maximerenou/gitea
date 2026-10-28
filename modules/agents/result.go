// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"slices"
	"strings"

	"gitea.dev/modules/json"
)

// ErrInvalidResult marks a result.json the agent got wrong, as opposed to a failure on the server side.
var ErrInvalidResult = errors.New("invalid agent result")

// ResultVersion is the only result.json version accepted.
const ResultVersion = "v1"

const (
	MaxResultSize     = 1024 * 1024
	MaxTextLength     = 64 * 1024
	MaxTitleLength    = 255
	MaxReviewComments = 200
	MaxSubtasks       = 50
	MaxFindings       = 200
	MaxPlanSteps      = 12
)

type ResultStatus string

const (
	ResultDone       ResultStatus = "done"
	ResultNeedsHuman ResultStatus = "needs_human"
	ResultFailed     ResultStatus = "failed"
)

type ReviewVerdict string

const (
	VerdictApprove        ReviewVerdict = "approve"
	VerdictRequestChanges ReviewVerdict = "request_changes"
	VerdictComment        ReviewVerdict = "comment"
)

type Severity string

const (
	SeverityLow      Severity = "low"
	SeverityMedium   Severity = "medium"
	SeverityHigh     Severity = "high"
	SeverityCritical Severity = "critical"
)

type Confidence string

const (
	ConfidenceLow    Confidence = "low"
	ConfidenceMedium Confidence = "medium"
	ConfidenceHigh   Confidence = "high"
)

// Result is what a run reports back; every field except the version is optional and the service only applies what the action allows.
type Result struct {
	Version         string          `json:"version"`
	Status          ResultStatus    `json:"status,omitempty"`
	Summary         string          `json:"summary,omitempty"`
	Comment         string          `json:"comment,omitempty"`
	PR              *ResultPR       `json:"pr,omitempty"`
	Review          *ResultReview   `json:"review,omitempty"`
	ResolvedThreads []int64         `json:"resolved_threads,omitempty"`
	Subtasks        []ResultSubtask `json:"subtasks,omitempty"`
	Findings        []ResultFinding `json:"findings,omitempty"`
	Plan            *ResultPlan     `json:"plan,omitempty"`
}

type ResultPR struct {
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
}

type ResultReview struct {
	Verdict  ReviewVerdict         `json:"verdict"`
	Body     string                `json:"body,omitempty"`
	Comments []ResultReviewComment `json:"comments,omitempty"`
}

type ResultReviewComment struct {
	Path string `json:"path"`
	Line int64  `json:"line"` // negative: a line of the old side of the diff
	Body string `json:"body"`
}

type ResultSubtask struct {
	Title     string `json:"title"`
	Body      string `json:"body,omitempty"`
	DependsOn []int  `json:"depends_on,omitempty"` // zero-based indexes of earlier subtasks
}

type ResultFinding struct {
	Severity      Severity   `json:"severity"`
	File          string     `json:"file"`
	Line          int64      `json:"line,omitempty"`
	Explanation   string     `json:"explanation"`
	Fix           string     `json:"fix,omitempty"`
	FalsePositive bool       `json:"false_positive,omitempty"`
	Confidence    Confidence `json:"confidence,omitempty"`
}

type ResultPlan struct {
	Steps []ResultSubtask `json:"steps"`
}

//go:embed result.schema.json
var resultSchema []byte

// ResultSchema is the JSON Schema of result.json, handed to agents so they know the expected format.
func ResultSchema() []byte {
	return slices.Clone(resultSchema)
}

// allowedFields lists, per action, the result fields the service applies; the others are reported as ignored.
var allowedFields = map[Action][]string{
	ActionAnalyze:      {"status", "summary", "comment", "subtasks"},
	ActionManagePR:     {"status", "summary", "comment", "pr", "resolved_threads", "plan"},
	ActionReview:       {"status", "summary", "comment", "review"},
	ActionSecurityScan: {"status", "summary", "comment", "findings"},
}

// ParseResult decodes and validates a result.json. Unknown top-level fields are not an error, they are returned as notes.
func ParseResult(data []byte) (*Result, []string, error) {
	if len(data) > MaxResultSize {
		return nil, nil, fmt.Errorf("ParseResult: result is larger than %d bytes", MaxResultSize)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, nil, fmt.Errorf("ParseResult: not a JSON object: %w", err)
	}
	var notes []string
	known := map[string]bool{}
	for _, f := range resultTopLevelFields {
		known[f] = true
	}
	for _, k := range slices.Sorted(mapsKeys(raw)) {
		if !known[k] {
			notes = append(notes, fmt.Sprintf("unknown field %q ignored", k))
		}
	}
	res := &Result{}
	if err := json.NewDecoder(bytes.NewReader(data)).Decode(res); err != nil {
		return nil, nil, fmt.Errorf("ParseResult: %w", err)
	}
	if err := res.Validate(); err != nil {
		return nil, nil, err
	}
	return res, notes, nil
}

var resultTopLevelFields = []string{"version", "status", "summary", "comment", "pr", "review", "resolved_threads", "subtasks", "findings", "plan"}

func mapsKeys[V any](m map[string]V) func(yield func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

func checkText(name, s string, max int) error {
	if len(s) > max {
		return fmt.Errorf("%s is longer than %d bytes", name, max)
	}
	return nil
}

// Validate checks versions, enums and sizes; it does not check what an action allows.
func (r *Result) Validate() error {
	if r.Version != ResultVersion {
		return fmt.Errorf("Validate: unsupported result version %q, expected %q", r.Version, ResultVersion)
	}
	switch r.Status {
	case "", ResultDone, ResultNeedsHuman, ResultFailed:
	default:
		return fmt.Errorf("Validate: unknown status %q", r.Status)
	}
	for name, s := range map[string]string{"summary": r.Summary, "comment": r.Comment} {
		if err := checkText(name, s, MaxTextLength); err != nil {
			return fmt.Errorf("Validate: %w", err)
		}
	}
	if r.PR != nil {
		if strings.TrimSpace(r.PR.Title) == "" || len(r.PR.Title) > MaxTitleLength {
			return fmt.Errorf("Validate: pr.title is required (max %d bytes)", MaxTitleLength)
		}
		if err := checkText("pr.body", r.PR.Body, MaxTextLength); err != nil {
			return fmt.Errorf("Validate: %w", err)
		}
	}
	if rv := r.Review; rv != nil {
		if !slices.Contains([]ReviewVerdict{VerdictApprove, VerdictRequestChanges, VerdictComment}, rv.Verdict) {
			return fmt.Errorf("Validate: unknown review verdict %q", rv.Verdict)
		}
		if err := checkText("review.body", rv.Body, MaxTextLength); err != nil {
			return fmt.Errorf("Validate: %w", err)
		}
		if len(rv.Comments) > MaxReviewComments {
			return fmt.Errorf("Validate: more than %d review comments", MaxReviewComments)
		}
		for i, c := range rv.Comments {
			if strings.TrimSpace(c.Path) == "" || c.Line == 0 || strings.TrimSpace(c.Body) == "" {
				return fmt.Errorf("Validate: review.comments[%d] needs a path, a line and a body", i)
			}
			if err := checkText("review comment", c.Body, MaxTextLength); err != nil {
				return fmt.Errorf("Validate: %w", err)
			}
		}
	}
	if err := validateSteps("subtasks", r.Subtasks, MaxSubtasks); err != nil {
		return err
	}
	if r.Plan != nil {
		if len(r.Plan.Steps) == 0 {
			return fmt.Errorf("Validate: plan needs at least one step")
		}
		if err := validateSteps("plan.steps", r.Plan.Steps, MaxPlanSteps); err != nil {
			return err
		}
	}
	if len(r.Findings) > MaxFindings {
		return fmt.Errorf("Validate: more than %d findings", MaxFindings)
	}
	for i, f := range r.Findings {
		if !slices.Contains([]Severity{SeverityLow, SeverityMedium, SeverityHigh, SeverityCritical}, f.Severity) {
			return fmt.Errorf("Validate: findings[%d] has unknown severity %q", i, f.Severity)
		}
		if f.Confidence != "" && !slices.Contains([]Confidence{ConfidenceLow, ConfidenceMedium, ConfidenceHigh}, f.Confidence) {
			return fmt.Errorf("Validate: findings[%d] has unknown confidence %q", i, f.Confidence)
		}
		if strings.TrimSpace(f.File) == "" || strings.TrimSpace(f.Explanation) == "" {
			return fmt.Errorf("Validate: findings[%d] needs a file and an explanation", i)
		}
		if err := checkText("finding explanation", f.Explanation+f.Fix, MaxTextLength); err != nil {
			return fmt.Errorf("Validate: %w", err)
		}
	}
	return nil
}

func validateSteps(name string, steps []ResultSubtask, max int) error {
	if len(steps) > max {
		return fmt.Errorf("Validate: %s has more than %d entries", name, max)
	}
	for i, s := range steps {
		if strings.TrimSpace(s.Title) == "" || len(s.Title) > MaxTitleLength {
			return fmt.Errorf("Validate: %s[%d] needs a title (max %d bytes)", name, i, MaxTitleLength)
		}
		if err := checkText(fmt.Sprintf("%s[%d].body", name, i), s.Body, MaxTextLength); err != nil {
			return fmt.Errorf("Validate: %w", err)
		}
		for _, d := range s.DependsOn {
			if d < 0 || d >= i {
				return fmt.Errorf("Validate: %s[%d] can only depend on earlier entries", name, i)
			}
		}
	}
	return nil
}

// ForAction returns a copy holding only what the action may apply, and a note for every dropped field that was set.
func (r *Result) ForAction(action Action) (*Result, []string) {
	allowed := allowedFields[action]
	out := &Result{Version: r.Version}
	var ignored []string
	keep := func(field string, set bool, apply func()) {
		if !set {
			return
		}
		if slices.Contains(allowed, field) {
			apply()
		} else {
			ignored = append(ignored, fmt.Sprintf("%q is not applied by a %s agent", field, action))
		}
	}
	keep("status", r.Status != "", func() { out.Status = r.Status })
	keep("summary", r.Summary != "", func() { out.Summary = r.Summary })
	keep("comment", r.Comment != "", func() { out.Comment = r.Comment })
	keep("pr", r.PR != nil, func() { out.PR = r.PR })
	keep("review", r.Review != nil, func() { out.Review = r.Review })
	keep("resolved_threads", len(r.ResolvedThreads) > 0, func() { out.ResolvedThreads = r.ResolvedThreads })
	keep("subtasks", len(r.Subtasks) > 0, func() { out.Subtasks = r.Subtasks })
	keep("findings", len(r.Findings) > 0, func() { out.Findings = r.Findings })
	keep("plan", r.Plan != nil, func() { out.Plan = r.Plan })
	return out, ignored
}
