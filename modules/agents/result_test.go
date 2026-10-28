// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"strings"
	"testing"

	"gitea.dev/modules/json"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const fullResult = `{
  "version": "v1", "status": "done", "summary": "s", "comment": "c",
  "pr": {"title": "t", "body": "b"},
  "review": {"verdict": "request_changes", "body": "rb", "comments": [{"path": "a.go", "line": 3, "body": "x"}]},
  "resolved_threads": [4],
  "subtasks": [{"title": "one"}, {"title": "two", "depends_on": [0]}],
  "findings": [{"severity": "high", "file": "a.go", "line": 1, "explanation": "e", "fix": "f", "false_positive": false, "confidence": "high"}],
  "plan": {"steps": [{"title": "step"}]}
}`

func TestParseResult(t *testing.T) {
	res, notes, err := ParseResult([]byte(fullResult))
	require.NoError(t, err)
	assert.Empty(t, notes)
	assert.Equal(t, ResultDone, res.Status)
	assert.Equal(t, VerdictRequestChanges, res.Review.Verdict)
	assert.Equal(t, []int{0}, res.Subtasks[1].DependsOn)

	// unknown fields are reported, not fatal
	_, notes, err = ParseResult([]byte(`{"version":"v1","status":"done","surprise":1}`))
	require.NoError(t, err)
	assert.Equal(t, []string{`unknown field "surprise" ignored`}, notes)

	bad := map[string]string{
		"not json":         `nope`,
		"no version":       `{"status":"done"}`,
		"wrong version":    `{"version":"v2"}`,
		"unknown status":   `{"version":"v1","status":"great"}`,
		"unknown verdict":  `{"version":"v1","review":{"verdict":"lgtm"}}`,
		"empty comment":    `{"version":"v1","review":{"verdict":"comment","comments":[{"path":"a","line":1,"body":" "}]}}`,
		"zero line":        `{"version":"v1","review":{"verdict":"comment","comments":[{"path":"a","line":0,"body":"x"}]}}`,
		"pr without title": `{"version":"v1","pr":{"body":"b"}}`,
		"bad severity":     `{"version":"v1","findings":[{"severity":"meh","file":"a","explanation":"e"}]}`,
		"bad confidence":   `{"version":"v1","findings":[{"severity":"low","confidence":"sure","file":"a","explanation":"e"}]}`,
		"forward dep":      `{"version":"v1","subtasks":[{"title":"a","depends_on":[0]}]}`,
		"empty plan":       `{"version":"v1","plan":{"steps":[]}}`,
		"13 step plan":     `{"version":"v1","plan":{"steps":[` + strings.Repeat(`{"title":"s"},`, 12) + `{"title":"s"}]}}`,
		"huge comment":     `{"version":"v1","comment":"` + strings.Repeat("a", MaxTextLength+1) + `"}`,
		"huge document":    `{"version":"v1","comment":"` + strings.Repeat("a", MaxResultSize) + `"}`,
	}
	for name, doc := range bad {
		_, _, err := ParseResult([]byte(doc))
		assert.Error(t, err, name)
	}
	// 12 steps is the limit and is fine
	_, _, err = ParseResult([]byte(`{"version":"v1","plan":{"steps":[` + strings.Repeat(`{"title":"s"},`, 11) + `{"title":"s"}]}}`))
	assert.NoError(t, err)
}

func TestResultForAction(t *testing.T) {
	res, _, err := ParseResult([]byte(fullResult))
	require.NoError(t, err)

	kept := func(a Action) (r *Result, ignored []string) { return res.ForAction(a) }

	r, ignored := kept(ActionReview)
	assert.NotNil(t, r.Review)
	assert.Nil(t, r.PR)
	assert.Nil(t, r.Plan)
	assert.Empty(t, r.Findings)
	assert.Empty(t, r.Subtasks)
	assert.Empty(t, r.ResolvedThreads)
	assert.Len(t, ignored, 5) // pr, resolved_threads, subtasks, findings, plan
	assert.Equal(t, "c", r.Comment)
	assert.Equal(t, ResultDone, r.Status)

	r, _ = kept(ActionAnalyze)
	assert.Len(t, r.Subtasks, 2)
	assert.Nil(t, r.Review)

	r, _ = kept(ActionManagePR)
	assert.NotNil(t, r.PR)
	assert.NotNil(t, r.Plan)
	assert.Equal(t, []int64{4}, r.ResolvedThreads)
	assert.Nil(t, r.Review)

	r, _ = kept(ActionSecurityScan)
	assert.Len(t, r.Findings, 1)
	assert.Nil(t, r.PR)

	r, ignored = (&Result{Version: ResultVersion}).ForAction(ActionReview)
	assert.Empty(t, ignored)
	assert.Equal(t, ResultVersion, r.Version)
}

// the schema handed to agents must list exactly the fields the parser knows
func TestResultSchemaMatchesTypes(t *testing.T) {
	var schema struct {
		Properties map[string]any `json:"properties"`
	}
	require.NoError(t, json.Unmarshal(ResultSchema(), &schema))
	var keys []string
	for k := range schema.Properties {
		keys = append(keys, k)
	}
	assert.ElementsMatch(t, resultTopLevelFields, keys)
	for action, fields := range allowedFields {
		for _, f := range fields {
			assert.Contains(t, schema.Properties, f, "%s allows %s", action, f)
		}
	}
}
