// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"

	agents_model "gitea.dev/models/agents"
	issues_model "gitea.dev/models/issues"
	access_model "gitea.dev/models/perm/access"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"
	"gitea.dev/modules/globallock"
	"gitea.dev/modules/json"
	"gitea.dev/modules/log"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/storage"
	"gitea.dev/modules/util"
	issue_service "gitea.dev/services/issue"
	pull_service "gitea.dev/services/pull"
)

const (
	maxEventPayload = 64 * 1024
	maxRunEvents    = 20000 // a run's log is a review aid, a job flooding it must not fill the database
)

// RunContext is everything the driver needs to run Codex, it holds no secret: the API key reaches the job as an Actions secret
type RunContext struct {
	RunID              int64  `json:"run_id"`
	SessionID          int64  `json:"session_id"`
	Prompt             string `json:"prompt"`
	FreshPrompt        string `json:"fresh_prompt"`
	Resume             bool   `json:"resume"`
	ThreadID           string `json:"thread_id"`
	CloneURL           string `json:"clone_url"`
	BaseRef            string `json:"base_ref"`
	DiffBase           string `json:"diff_base"`
	PushBranch         string `json:"push_branch"`
	ReadOnlyReason     string `json:"read_only_reason"`
	Model              string `json:"model"`
	ProviderName       string `json:"provider_name"`
	BaseURL            string `json:"base_url"`
	CodexVersion       string `json:"codex_version"`
	CheckpointInterval int    `json:"checkpoint_interval"`
	CommitName         string `json:"commit_name"`
	CommitEmail        string `json:"commit_email"`
	CommitTitle        string `json:"commit_title"`
	CoAuthor           string `json:"co_author"`
	ReasoningEffort    string `json:"reasoning_effort"`
	MergeFrom          string `json:"merge_from"` // the branch a merge run merges into the session branch before Codex starts

	MCPServers           map[string]map[string]any `json:"mcp_servers"`
	TrustedMCPServers    []string                  `json:"trusted_mcp_servers"` // defined by an administrator, they get direct access through the firewall
	Firewall             bool                      `json:"firewall"`
	IsolateSetup         bool                      `json:"isolate_setup"`
	FirewallAllowedHosts []string                  `json:"firewall_allowed_hosts"`
}

func loadRunSession(ctx context.Context, r *agents_model.Run) (*agents_model.Session, error) {
	s, err := agents_model.GetSessionByID(ctx, r.SessionID)
	if err != nil {
		return nil, err
	}
	if err := s.LoadAttributes(ctx); err != nil {
		return nil, err
	}
	if s.Agent == nil || s.Agent.Bot == nil {
		return nil, util.NewNotExistErrorf("the agent of session %d was deleted", s.ID)
	}
	return s, nil
}

func GetRunContext(ctx context.Context, r *agents_model.Run) (*RunContext, error) {
	s, err := loadRunSession(ctx, r)
	if err != nil {
		return nil, err
	}
	resume := s.HasState && s.CodexThreadID != ""
	prompt, err := buildPrompt(ctx, s, r, resume)
	if err != nil {
		return nil, err
	}
	freshPrompt := prompt
	if resume {
		if freshPrompt, err = buildPrompt(ctx, s, r, false); err != nil {
			return nil, err
		}
	}
	diffBase := s.BaseBranch
	if s.Kind == agents_model.SessionKindPull && s.Issue != nil {
		if err := s.Issue.LoadPullRequest(ctx); err != nil {
			return nil, err
		}
		diffBase = s.Issue.PullRequest.BaseBranch
	}
	_, trigger, err := user_model.GetPossibleUserByID(ctx, r.TriggerUserID)
	if err != nil {
		return nil, err
	}
	userKey, err := agents_model.GetUserKey(ctx, s.AgentID, r.TriggerUserID)
	if err != nil {
		return nil, err
	}
	agentServers, err := ParseMCPServers(s.Agent.MCPServers)
	if err != nil {
		return nil, err
	}
	setup, err := LoadRepoSetup(ctx, s.Repo)
	if err != nil {
		return nil, err
	}
	return &RunContext{
		RunID:              r.ID,
		SessionID:          s.ID,
		Prompt:             prompt,
		FreshPrompt:        freshPrompt,
		Resume:             resume,
		ThreadID:           s.CodexThreadID,
		CloneURL:           s.Repo.CloneLinkGeneral(ctx).HTTPS,
		BaseRef:            r.BaseRef,
		DiffBase:           diffBase,
		PushBranch:         r.PushBranch,
		ReadOnlyReason:     r.ReadOnlyReason,
		Model:              userKey.EffectiveModel(s.Agent),
		ProviderName:       s.Agent.Name,
		BaseURL:            strings.TrimSuffix(setting.AppURL, "/") + "/api/agents/model", // see ProxyModel
		CodexVersion:       setting.Agents.CodexVersion,
		CheckpointInterval: int(setting.Agents.CheckpointInterval.Seconds()),
		CommitName:         s.Agent.Bot.Name,
		CommitEmail:        s.Agent.Bot.GetEmail(),
		CommitTitle:        firstLine(r.Prompt, 72),
		CoAuthor:           fmt.Sprintf("%s <%s>", trigger.GetDisplayName(), trigger.GetEmail()),
		ReasoningEffort:    s.EffectiveReasoningEffort(),
		MergeFrom:          util.Iif(r.Purpose == agents_model.RunPurposeMerge && r.PushBranch != "", diffBase, ""),

		MCPServers:           mergeMCPServers(agentServers, setup.MCPServers),
		TrustedMCPServers:    slices.Sorted(maps.Keys(agentServers)),
		Firewall:             setting.Agents.Firewall,
		IsolateSetup:         isolatesSetup(),
		FirewallAllowedHosts: setting.Agents.FirewallAllowedHosts,
	}, nil
}

// threadIDPattern accepts Codex's UUIDs: the next run, maybe another user's, passes it to Codex's command line,
// where a forged event must not slip in an option
var threadIDPattern = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z_-]{0,127}$`)

// AppendEvents stores Codex JSONL events, it also records the Codex thread so later runs resume the conversation
func AppendEvents(ctx context.Context, r *agents_model.Run, lines []string) error {
	if r.Status.IsDone() {
		return util.NewInvalidArgumentErrorf("the run is finished")
	}
	if count, err := agents_model.CountRunEvents(ctx, r.ID); err != nil {
		return err
	} else if count+int64(len(lines)) > maxRunEvents {
		return util.NewInvalidArgumentErrorf("the run reached its limit of %d events", maxRunEvents)
	}
	events := make([]*agents_model.Event, 0, len(lines))
	for _, line := range lines {
		payload := []byte(strings.TrimSpace(line))
		var head struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
			Item     struct {
				ID string `json:"id"`
			} `json:"item"`
		}
		if err := json.Unmarshal(payload, &head); err != nil || head.Type == "" {
			continue
		}
		if head.Type == "thread.started" && threadIDPattern.MatchString(head.ThreadID) {
			s := &agents_model.Session{ID: r.SessionID, CodexThreadID: head.ThreadID}
			if err := agents_model.UpdateSession(ctx, s, "codex_thread_id"); err != nil {
				return err
			}
		}
		text := string(payload)
		if len(text) > maxEventPayload {
			text = text[:maxEventPayload] // the driver already trims command outputs, this only guards the database
		}
		events = append(events, &agents_model.Event{
			SessionID: r.SessionID,
			RunID:     r.ID,
			Type:      util.EllipsisDisplayString(head.Type, 64),
			ItemID:    util.EllipsisDisplayString(head.Item.ID, 255),
			Payload:   text,
		})
	}
	if r.Status == agents_model.StatusWaiting {
		if _, err := agents_model.UpdateRunStatus(ctx, r, agents_model.StatusRunning); err != nil {
			return err
		}
	}
	if err := agents_model.InsertEvents(ctx, events); err != nil {
		return err
	}
	notifySession(r.SessionID)
	return nil
}

type PublishOptions struct {
	Progress string `json:"progress"`
	Summary  string `json:"summary"`
	Head     string `json:"head"` // the commit the job checked out
}

// FindSessionPull returns the open pull request the run's pushes land in
func FindSessionPull(ctx context.Context, s *agents_model.Session, r *agents_model.Run) *issues_model.PullRequest {
	var pr *issues_model.PullRequest
	var err error
	if s.Kind == agents_model.SessionKindPull && !s.OwnsPull && r.PushBranch != "" && r.PushBranch != s.Branch {
		pr, err = issues_model.GetPullRequestByIssueID(ctx, s.IssueID)
	} else if r.PushBranch != "" && r.PullTarget != "" {
		pr, err = issues_model.GetUnmergedPullRequest(ctx, s.RepoID, s.RepoID, r.PushBranch, r.PullTarget, issues_model.PullRequestFlowGithub)
	} else {
		return nil
	}
	if err != nil {
		if !issues_model.IsErrPullRequestNotExist(err) {
			log.Error("agents: pull request of session %d: %v", s.ID, err)
		}
		return nil
	}
	if err := pr.LoadIssue(ctx); err != nil {
		log.Error("agents: issue of pull request %d: %v", pr.ID, err)
		return nil
	}
	pr.Issue.Repo = s.Repo
	return pr
}

// Publish is called by the driver after each push: it opens the session's draft pull request on the first changes and keeps it up to date
func Publish(ctx context.Context, r *agents_model.Run, opts PublishOptions, final bool) (string, error) {
	if r.Status.IsDone() {
		return "", util.NewInvalidArgumentErrorf("the run is finished")
	}
	var pullURL string
	err := globallock.LockAndDo(ctx, fmt.Sprintf("agents_session_%d", r.SessionID), func(ctx context.Context) error {
		s, err := loadRunSession(ctx, r)
		if err != nil {
			return err
		}
		r.Progress = util.EllipsisDisplayString(opts.Progress, 16*1024)
		cols := []string{"progress"}
		if _, err := git.NewIDFromString(opts.Head); err == nil && r.StartHead == "" {
			r.StartHead = opts.Head
			cols = append(cols, "start_head")
		}
		if final {
			r.Summary = util.EllipsisDisplayString(opts.Summary, 64*1024)
			cols = append(cols, "summary")
		}
		if err := agents_model.UpdateRun(ctx, r, cols...); err != nil {
			return err
		}

		if r.CanPush() && git.IsBranchExist(ctx, s.Repo, r.PushBranch) {
			pr := FindSessionPull(ctx, s, r)
			if pr == nil && r.PullTarget != "" {
				if pr, err = createSessionPull(ctx, s, r); err != nil {
					return err
				}
			}
			if pr != nil {
				pullURL = pr.Issue.HTMLURL(ctx)
				if pr.HeadBranch == s.Branch {
					if err := updateSessionPull(ctx, s, r, pr, final); err != nil {
						return err
					}
				}
			}
		}
		if s.Issue != nil && !s.OwnsPull && !final && r.Purpose != agents_model.RunPurposeReview { // a review answers once, with the review
			return upsertReply(ctx, s, r, replyWorking(ctx, r))
		}
		return nil
	})
	return pullURL, err
}

func createSessionPull(ctx context.Context, s *agents_model.Session, r *agents_model.Run) (*issues_model.PullRequest, error) {
	bot := actingBot(s, r)
	prefix := "WIP: "
	if len(setting.Repository.PullRequest.WorkInProgressPrefixes) > 0 {
		prefix = setting.Repository.PullRequest.WorkInProgressPrefixes[0] + " "
	}
	issue := &issues_model.Issue{
		RepoID:   s.RepoID,
		Repo:     s.Repo,
		Title:    prefix + s.Title,
		PosterID: bot.ID,
		Poster:   bot,
		IsPull:   true,
		Content:  pullDescription(ctx, s, r),
	}
	pr := &issues_model.PullRequest{
		HeadRepoID: s.RepoID,
		BaseRepoID: s.RepoID,
		HeadBranch: r.PushBranch,
		BaseBranch: r.PullTarget,
		HeadRepo:   s.Repo,
		BaseRepo:   s.Repo,
		Type:       issues_model.PullRequestGitea,
		Flow:       issues_model.PullRequestFlowGithub,
	}
	if err := pull_service.NewPullRequest(ctx, &pull_service.NewPullRequestOptions{Repo: s.Repo, Issue: issue, PullRequest: pr}); err != nil {
		return nil, err
	}
	pr.Issue = issue
	if s.Kind == agents_model.SessionKindTask && s.IssueID == 0 {
		s.IssueID, s.OwnsPull, s.Issue = issue.ID, true, issue
		if err := agents_model.UpdateSession(ctx, s, "issue_id", "owns_pull"); err != nil {
			return nil, err
		}
	}
	return pr, nil
}

func updateSessionPull(ctx context.Context, s *agents_model.Session, r *agents_model.Run, pr *issues_model.PullRequest, final bool) error {
	bot := actingBot(s, r)
	if content := pullDescription(ctx, s, r); content != pr.Issue.Content {
		if err := issue_service.ChangeContent(ctx, pr.Issue, bot, content, pr.Issue.ContentVersion); err != nil {
			return err
		}
	}
	if !final || r.Error != "" {
		return nil
	}
	if title, ok := issues_model.CutWorkInProgressPrefix(pr.Issue.Title); ok {
		if err := issue_service.ChangeTitle(ctx, pr.Issue, bot, strings.TrimSpace(title)); err != nil {
			return err
		}
	}
	return requestReview(ctx, s, r, pr)
}

// requestReview hands the finished pull request to the requester, like a colleague would
func requestReview(ctx context.Context, s *agents_model.Session, r *agents_model.Run, pr *issues_model.PullRequest) error {
	_, trigger, err := user_model.GetPossibleUserByID(ctx, r.TriggerUserID)
	if err != nil || !trigger.IsIndividual() || pr.Issue.PosterID == trigger.ID {
		return err
	}
	bot := actingBot(s, r)
	perm, err := access_model.GetDoerRepoPermission(ctx, s.Repo, bot)
	if err != nil {
		return err
	}
	if _, err := issue_service.ReviewRequest(ctx, pr.Issue, bot, &perm, trigger, true); err != nil && !issues_model.IsErrNotValidReviewRequest(err) {
		return err
	}
	return nil
}

type FinishOptions struct {
	Status  string `json:"status"`
	Summary string `json:"summary"`
	Error   string `json:"error"`
}

// Finish records the outcome the driver reports, the Actions run status is the fallback when the driver cannot report
func Finish(ctx context.Context, r *agents_model.Run, opts FinishOptions) error {
	status := agents_model.StatusFailure
	if opts.Status == string(agents_model.StatusSuccess) {
		status = agents_model.StatusSuccess
	}
	r.Summary = util.EllipsisDisplayString(strings.TrimSpace(opts.Summary), 64*1024)
	r.Error = util.EllipsisDisplayString(strings.TrimSpace(opts.Error), 4*1024)
	if ok, err := agents_model.UpdateRunStatus(ctx, r, status, "summary", "error"); err != nil || !ok {
		return err
	}
	afterRunDone(ctx, r)
	return nil
}

const maxStateSize = 256 * 1024 * 1024

// SaveState keeps the Codex sessions directory between ephemeral jobs
func SaveState(ctx context.Context, r *agents_model.Run, body io.Reader, size int64) error {
	if r.Status.IsDone() {
		return util.NewInvalidArgumentErrorf("the run is finished")
	}
	if size > maxStateSize {
		return util.NewInvalidArgumentErrorf("the Codex state is too large")
	}
	f, cleanup, err := setting.AppDataTempDir("agents").CreateTempFileRandom("state")
	if err != nil {
		return err
	}
	defer cleanup()
	written, err := io.Copy(f, io.LimitReader(body, maxStateSize+1))
	if err != nil {
		return err
	} else if written > maxStateSize {
		return util.NewInvalidArgumentErrorf("the Codex state is too large")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := checkStateArchive(f); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := storage.Actions.Save(agents_model.StatePath(r.SessionID), f, written); err != nil {
		return err
	}
	s := &agents_model.Session{ID: r.SessionID, HasState: true}
	return agents_model.UpdateSession(ctx, s, "has_state")
}

func OpenState(r *agents_model.Run) (storage.Object, error) {
	obj, err := storage.Actions.Open(agents_model.StatePath(r.SessionID))
	if err != nil {
		return nil, err
	}
	if err = checkStateArchive(io.LimitReader(obj, maxStateSize+1)); err == nil {
		_, err = obj.Seek(0, io.SeekStart)
	}
	if err != nil {
		_ = obj.Close()
		return nil, err
	}
	return obj, nil
}

// checkStateArchive only accepts the files of Codex's sessions directory: the next run of the session may be another user's,
// a config.toml or a link in the archive would hand their API key to a model provider or a command of the previous user
func checkStateArchive(r io.Reader) error {
	return checkStateArchiveLimits(r, maxStateSize, 10000)
}

func checkStateArchiveLimits(r io.Reader, maxSize int64, maxEntries int) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return util.NewInvalidArgumentErrorf("the Codex state is not a gzip archive")
	}
	defer gz.Close()
	expanded := &io.LimitedReader{R: gz, N: maxSize + 1}
	tr := tar.NewReader(expanded)
	var size int64
	entries := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			if _, err := io.Copy(io.Discard, expanded); err != nil {
				return util.NewInvalidArgumentErrorf("the Codex state is not a valid gzip archive")
			}
			if expanded.N <= 0 {
				return util.NewInvalidArgumentErrorf("the expanded Codex state is too large")
			}
			return nil
		} else if err != nil {
			return util.NewInvalidArgumentErrorf("the Codex state is not a tar archive")
		}
		entries++
		if entries > maxEntries || h.Size > maxSize-size {
			return util.NewInvalidArgumentErrorf("the Codex state exceeds its file or expanded size limit")
		}
		size += h.Size
		name := strings.TrimSuffix(h.Name, "/")
		if (name != "sessions" && !strings.HasPrefix(name, "sessions/")) || path.Clean(name) != name ||
			(h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir) {
			return util.NewInvalidArgumentErrorf("the Codex state can only hold the files of the sessions directory, not %q", h.Name)
		}
	}
}
