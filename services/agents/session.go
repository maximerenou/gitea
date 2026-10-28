// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	actions_model "gitea.dev/models/actions"
	activities_model "gitea.dev/models/activities"
	agents_model "gitea.dev/models/agents"
	audit_model "gitea.dev/models/audit"
	"gitea.dev/models/db"
	git_model "gitea.dev/models/git"
	issues_model "gitea.dev/models/issues"
	access_model "gitea.dev/models/perm/access"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unit"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"
	"gitea.dev/modules/globallock"
	"gitea.dev/modules/log"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/util"
	actions_service "gitea.dev/services/actions"
	"gitea.dev/services/audit"
)

// IsEnabled reports whether agent sessions are available on this instance
func IsEnabled() bool {
	return setting.Agents.Enabled && setting.Actions.Enabled
}

// IsRepoUsable reports whether sessions can run in the repository, they need its Actions unit
func IsRepoUsable(ctx context.Context, repo *repo_model.Repository) bool {
	return usableRepoSetting(ctx, repo) != nil
}

// IsAgentAllowed reports whether the repository's administrators let the agent work in it
func IsAgentAllowed(ctx context.Context, repo *repo_model.Repository, agent *agents_model.Agent) bool {
	rs := usableRepoSetting(ctx, repo)
	return rs != nil && !agent.IsDisabled && rs.AllowsAgent(agent.ID)
}

func usableRepoSetting(ctx context.Context, repo *repo_model.Repository) *agents_model.RepoSetting {
	if !IsEnabled() || repo.IsEmpty || repo.IsArchived || repo.IsMirror || !repo.UnitEnabled(ctx, unit.TypeActions) {
		return nil
	}
	rs, err := agents_model.GetRepoSetting(ctx, repo.ID)
	if err != nil {
		log.Error("agents: setting of repository %d: %v", repo.ID, err)
		return nil
	}
	return util.Iif(rs.Enabled, rs, nil)
}

func sessionBranch(bot *user_model.User, suffix string) string {
	return "agents/" + bot.LowerName + "/" + suffix
}

func firstLine(s string, limit int) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return util.EllipsisDisplayString(strings.TrimSpace(line), limit)
}

// ErrNoUserKey is returned when a user triggers an agent without having set their own API key for it
var ErrNoUserKey = util.NewInvalidArgumentErrorf("you have no API key for this agent, add one in your settings")

// checkCanUse enforces that a run only ever consumes the API key of the user who triggers it, in a repository open to the agent
func checkCanUse(ctx context.Context, repo *repo_model.Repository, agent *agents_model.Agent, doer *user_model.User) error {
	if agent == nil || agent.IsDisabled {
		return util.NewInvalidArgumentErrorf("this agent is not available")
	}
	if !IsAgentAllowed(ctx, repo, agent) {
		return util.NewPermissionDeniedErrorf("@%s is not enabled in %s", agent.MentionName(), repo.FullName())
	}
	if doer == nil || !doer.IsIndividual() {
		return util.NewPermissionDeniedErrorf("only users can trigger agents")
	}
	if _, err := agents_model.GetUserKey(ctx, agent.ID, doer.ID); errors.Is(err, util.ErrNotExist) {
		return ErrNoUserKey
	} else if err != nil {
		return err
	}
	return nil
}

type StartOptions struct {
	Agent           *agents_model.Agent
	Prompt          string
	BaseBranch      string
	ReasoningEffort string // empty keeps the agent's
}

// StartTaskSession starts a development session from the repository Agents tab
func StartTaskSession(ctx context.Context, doer *user_model.User, repo *repo_model.Repository, opts StartOptions) (*agents_model.Session, error) {
	if !IsRepoUsable(ctx, repo) {
		return nil, util.NewInvalidArgumentErrorf("agent sessions are not available in this repository")
	}
	if err := checkCanUse(ctx, repo, opts.Agent, doer); err != nil {
		return nil, err
	}
	opts.Prompt = strings.TrimSpace(opts.Prompt)
	if opts.Prompt == "" {
		return nil, util.NewInvalidArgumentErrorf("the task is required")
	}
	if !agents_model.IsValidReasoningEffort(opts.ReasoningEffort) {
		return nil, util.NewInvalidArgumentErrorf("unsupported reasoning effort %q", opts.ReasoningEffort)
	}
	perm, err := access_model.GetDoerRepoPermission(ctx, repo, doer)
	if err != nil {
		return nil, err
	}
	if !perm.CanWrite(unit.TypeCode) {
		return nil, util.NewPermissionDeniedErrorf("starting a development session requires write access to the code")
	}
	if opts.BaseBranch == "" {
		opts.BaseBranch = repo.DefaultBranch
	}
	if exist, err := git_model.IsBranchExist(ctx, repo.ID, opts.BaseBranch); err != nil {
		return nil, err
	} else if !exist {
		return nil, util.NewNotExistErrorf("branch %q does not exist", opts.BaseBranch)
	}
	if err := opts.Agent.LoadBot(ctx); err != nil {
		return nil, err
	}

	s := &agents_model.Session{
		RepoID:          repo.ID,
		CreatorID:       doer.ID,
		AgentID:         opts.Agent.ID,
		Kind:            agents_model.SessionKindTask,
		Title:           firstLine(opts.Prompt, 200),
		Status:          agents_model.StatusQueued,
		BaseBranch:      opts.BaseBranch,
		Repo:            repo,
		ReasoningEffort: opts.ReasoningEffort,
		Creator:         doer,
		Agent:           opts.Agent,
	}
	err = db.WithTx(ctx, func(ctx context.Context) error {
		if err := agents_model.InsertSession(ctx, s); err != nil {
			return err
		}
		s.Branch = sessionBranch(opts.Agent.Bot, strconv.FormatInt(s.ID, 10))
		return agents_model.UpdateSession(ctx, s, "branch")
	})
	if err != nil {
		return nil, err
	}
	if _, err := EnqueueRun(ctx, s, doer, opts.Prompt, 0); err != nil {
		return nil, err
	}
	return s, nil
}

// SendInstruction queues new instructions on a session, they run after its current run with the doer's key and permissions
func SendInstruction(ctx context.Context, s *agents_model.Session, doer *user_model.User, prompt string) (*agents_model.Run, error) {
	if err := s.LoadAttributes(ctx); err != nil {
		return nil, err
	}
	if err := checkCanUse(ctx, s.Repo, s.Agent, doer); err != nil {
		return nil, err
	}
	if perm, err := access_model.GetDoerRepoPermission(ctx, s.Repo, doer); err != nil {
		return nil, err
	} else if !perm.CanRead(unit.TypeCode) || (s.Issue != nil && !perm.CanReadIssuesOrPulls(s.Issue.IsPull)) {
		return nil, util.NewPermissionDeniedErrorf("you cannot read this repository")
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return nil, util.NewInvalidArgumentErrorf("the instructions are required")
	}
	return EnqueueRun(ctx, s, doer, prompt, 0)
}

// RetryRun queues the prompt of a finished run again
func RetryRun(ctx context.Context, s *agents_model.Session, doer *user_model.User, runID int64) (*agents_model.Run, error) {
	r, err := agents_model.GetRunByID(ctx, runID)
	if err != nil {
		return nil, err
	}
	if r.SessionID != s.ID || !r.Status.IsDone() || r.TriggerUserID != doer.ID { // another's prompt may ask for what they could not do
		return nil, util.NewInvalidArgumentErrorf("only your finished runs of this session can be retried")
	}
	return SendInstruction(ctx, s, doer, r.Prompt)
}

const resumePrompt = "Your previous run was interrupted. Check the current state of the branch and continue the task where you left off."

// CanResume reports whether the user triggered the interrupted latest run of the session, resuming another's request
// would carry it out with the user's permissions, which may exceed theirs
func CanResume(ctx context.Context, s *agents_model.Session, doer *user_model.User) (bool, error) {
	if doer == nil {
		return false, nil
	}
	latest, err := agents_model.GetLatestRun(ctx, s.ID)
	if err != nil || latest == nil {
		return false, err
	}
	return latest.Status.IsDone() && latest.Status != agents_model.StatusSuccess && latest.TriggerUserID == doer.ID, nil
}

// ResumeSession asks the agent to continue the interrupted latest run of the session
func ResumeSession(ctx context.Context, s *agents_model.Session, doer *user_model.User) (*agents_model.Run, error) {
	if ok, err := CanResume(ctx, s, doer); err != nil {
		return nil, err
	} else if !ok {
		return nil, util.NewInvalidArgumentErrorf("only the user whose run was interrupted can resume the session")
	}
	return SendInstruction(ctx, s, doer, resumePrompt)
}

// EnqueueRun persists a run requested by a user, it is dispatched as soon as the session and the user's limits allow
func EnqueueRun(ctx context.Context, s *agents_model.Session, trigger *user_model.User, prompt string, commentID int64, setup ...func(*agents_model.Run)) (*agents_model.Run, error) {
	if s.CIFixAttempts > 0 { // a human is back in the loop
		s.CIFixAttempts = 0
		if err := agents_model.UpdateSession(ctx, s, "ci_fix_attempts"); err != nil {
			return nil, err
		}
	}
	return enqueueRun(ctx, s, trigger, prompt, commentID, setup...)
}

func enqueueRun(ctx context.Context, s *agents_model.Session, trigger *user_model.User, prompt string, commentID int64, setup ...func(*agents_model.Run)) (*agents_model.Run, error) {
	if err := s.LoadAttributes(ctx); err != nil {
		return nil, err
	}
	if s.Agent != nil {
		audit.RecordAs(ctx, trigger, audit_model.RepositoryAgentSessionRun, s.Repo, "agent", s.Agent.MentionName(), "session", s.ID)
	}
	r := &agents_model.Run{
		SessionID:     s.ID,
		RepoID:        s.RepoID,
		TriggerUserID: trigger.ID,
		Prompt:        util.EllipsisDisplayString(prompt, 64*1024),
		Status:        agents_model.StatusQueued,
		CommentID:     commentID,
	}
	for _, f := range setup {
		f(r)
	}
	if err := agents_model.InsertRun(ctx, r); err != nil {
		return nil, err
	}
	if active, err := agents_model.HasActiveRun(ctx, s.ID); err != nil {
		return nil, err
	} else if !active {
		s.Status = agents_model.StatusQueued
		if err := agents_model.UpdateSession(ctx, s, "status"); err != nil {
			return nil, err
		}
	}
	DispatchQueued(ctx)
	notifySession(s.ID)
	notifyRepo(s.RepoID)
	return r, nil
}

// DispatchQueued starts the oldest queued run of every idle session within the owners' concurrency limits
func DispatchQueued(ctx context.Context) {
	if !IsEnabled() {
		return
	}
	err := globallock.LockAndDo(ctx, "agents_dispatch", func(ctx context.Context) error {
		sessionIDs, err := agents_model.FindQueuedSessionIDs(ctx)
		if err != nil {
			return err
		}
		for _, sessionID := range sessionIDs {
			r, err := agents_model.GetFirstQueuedRun(ctx, sessionID)
			if err != nil || r == nil {
				if err != nil {
					log.Error("agents: queued run of session %d: %v", sessionID, err)
				}
				continue
			}
			active, err := agents_model.CountActiveRunsByUser(ctx, r.TriggerUserID)
			if err != nil {
				log.Error("agents: active runs of user %d: %v", r.TriggerUserID, err)
				continue
			}
			if active >= int64(setting.Agents.MaxActiveRunsPerUser) {
				continue
			}
			busy, err := isBranchBusy(ctx, sessionID)
			if err == nil && !busy {
				err = dispatchRun(ctx, r)
			}
			if err != nil { // a session that cannot dispatch must not hold the others back
				log.Warn("agents: dispatch run %d: %v", r.ID, err)
				failRun(ctx, r, err.Error())
			}
		}
		return nil
	})
	if err != nil {
		log.Error("agents: dispatch queued runs: %v", err)
	}
}

// isBranchBusy reports whether another agent run works on a branch this session may push to, agents take turns on a branch
func isBranchBusy(ctx context.Context, sessionID int64) (bool, error) {
	s, err := agents_model.GetSessionByID(ctx, sessionID)
	if err != nil {
		return false, err
	}
	branches := []string{s.Branch}
	if s.Kind == agents_model.SessionKindPull {
		pr, err := issues_model.GetPullRequestByIssueID(ctx, s.IssueID)
		if err != nil {
			return false, err
		}
		if pr.HeadRepoID == s.RepoID {
			branches = append(branches, pr.HeadBranch)
		}
	}
	for _, branch := range branches {
		if branch == "" {
			continue
		}
		if busy, err := agents_model.HasActiveRunOnBranch(ctx, s.RepoID, branch); err != nil || busy {
			return busy, err
		}
	}
	return false, nil
}

type pushPlan struct {
	baseRef, pushBranch, pullTarget, readOnly string
}

func canPushBranch(ctx context.Context, repo *repo_model.Repository, user *user_model.User, perm access_model.Permission, branch string) bool {
	if branch == "" || !perm.CanWrite(unit.TypeCode) {
		return false
	}
	rule, err := git_model.GetFirstMatchProtectedBranchRule(ctx, repo.ID, branch)
	if err != nil {
		log.Error("agents: protected branch rule of %q: %v", branch, err)
		return false
	}
	return rule == nil || (!rule.RequireSignedCommits && rule.CanUserPush(ctx, user, perm))
}

// planRun decides where a run may push, never beyond what the trigger user could push themself
func planRun(ctx context.Context, s *agents_model.Session, trigger *user_model.User) (pushPlan, error) {
	perm, err := access_model.GetDoerRepoPermission(ctx, s.Repo, trigger)
	if err != nil {
		return pushPlan{}, err
	}
	if !perm.CanRead(unit.TypeCode) {
		return pushPlan{}, util.NewPermissionDeniedErrorf("%s cannot read the code of %s", trigger.Name, s.Repo.FullName())
	}

	if s.Kind == agents_model.SessionKindPull && !s.OwnsPull {
		pr, err := issues_model.GetPullRequestByIssueID(ctx, s.IssueID)
		if err != nil {
			return pushPlan{}, err
		}
		if pr.HasMerged || s.Issue.IsClosed {
			return pushPlan{baseRef: pr.GetGitHeadRefName(), readOnly: "the pull request is closed"}, nil
		}
		if pr.HeadRepoID == s.RepoID && canPushBranch(ctx, s.Repo, trigger, perm, pr.HeadBranch) {
			return pushPlan{baseRef: pr.HeadBranch, pushBranch: pr.HeadBranch}, nil
		}
		if pr.HeadRepoID != s.RepoID { // a fix branch would bring the fork's commits, and their workflows, into this repository
			return pushPlan{baseRef: pr.GetGitHeadRefName(), readOnly: "the pull request comes from a fork"}, nil
		}
		if s.Branch == "" {
			s.Branch = sessionBranch(s.Agent.Bot, fmt.Sprintf("pr-%d-fix", s.Issue.Index))
			if err := agents_model.UpdateSession(ctx, s, "branch"); err != nil {
				return pushPlan{}, err
			}
		}
		if !canPushBranch(ctx, s.Repo, trigger, perm, s.Branch) {
			return pushPlan{baseRef: pr.GetGitHeadRefName(), readOnly: fmt.Sprintf("@%s cannot push to the branch of this pull request nor to this repository", trigger.Name)}, nil
		}
		return pushPlan{
			baseRef:    pr.GetGitHeadRefName(),
			pushBranch: s.Branch,
			pullTarget: pr.HeadBranch,
			readOnly:   "", // the head branch is not writable, changes are proposed in a separate pull request
		}, nil
	}

	if s.OwnsPull && s.Issue != nil && s.Issue.IsClosed { // a push would open a new pull request for a finished one
		if err := s.Issue.LoadPullRequest(ctx); err != nil {
			return pushPlan{}, err
		}
		return pushPlan{baseRef: s.Issue.PullRequest.GetGitHeadRefName(), readOnly: "its pull request is closed"}, nil
	}
	if !canPushBranch(ctx, s.Repo, trigger, perm, s.Branch) {
		return pushPlan{baseRef: s.BaseBranch, readOnly: fmt.Sprintf("@%s cannot push to branch %s", trigger.Name, s.Branch)}, nil
	}
	return pushPlan{baseRef: s.BaseBranch, pushBranch: s.Branch, pullTarget: s.BaseBranch}, nil
}

func dispatchRun(ctx context.Context, r *agents_model.Run) error {
	s, err := agents_model.GetSessionByID(ctx, r.SessionID)
	if err != nil {
		return err
	}
	if err := s.LoadAttributes(ctx); err != nil {
		return err
	}
	_, trigger, err := user_model.GetPossibleUserByID(ctx, r.TriggerUserID)
	if err != nil {
		return err
	}
	if err := checkCanUse(ctx, s.Repo, s.Agent, trigger); err != nil {
		return err
	}
	if _, err := NormalizeBaseURL(s.Agent.Provider, s.Agent.BaseURL); err != nil {
		return err
	}
	plan, err := planRun(ctx, s, trigger)
	if err != nil {
		return err
	}
	if r.Purpose == agents_model.RunPurposeReview && s.Issue != nil {
		if err := s.Issue.LoadPullRequest(ctx); err != nil {
			return err
		}
		plan = pushPlan{baseRef: s.Issue.PullRequest.GetGitHeadRefName(), readOnly: "it reviews the pull request"}
	}
	r.BaseRef, r.PushBranch, r.PullTarget, r.ReadOnlyReason = plan.baseRef, plan.pushBranch, plan.pullTarget, plan.readOnly

	commitBranch := s.Repo.DefaultBranch
	if s.BaseBranch != "" {
		commitBranch = s.BaseBranch
	}
	branch, err := git_model.GetBranchExisting(ctx, s.RepoID, commitBranch)
	if err != nil {
		return err
	}
	setup, err := LoadRepoSetup(ctx, s.Repo)
	if err != nil {
		return err
	}
	content, err := buildWorkflow(setup)
	if err != nil {
		return err
	}
	actionRun := &actions_model.ActionRun{
		Title:             fmt.Sprintf("Agent session %d: %s", s.ID, s.Title),
		RepoID:            s.RepoID,
		Repo:              s.Repo,
		OwnerID:           s.Repo.OwnerID,
		WorkflowID:        agents_model.WorkflowID,
		TriggerUserID:     trigger.ID,
		TriggerUser:       trigger,
		Ref:               git.RefNameFromBranch(commitBranch).String(),
		CommitSHA:         branch.CommitID,
		Event:             "workflow_dispatch",
		TriggerEvent:      "workflow_dispatch",
		EventPayload:      agents_model.EventPayload(r),
		Status:            actions_model.StatusWaiting,
		WorkflowRepoID:    s.RepoID,
		WorkflowCommitSHA: branch.CommitID,
	}
	if err := actions_service.PrepareRunAndInsert(ctx, content, actionRun, nil); err != nil {
		return err
	}
	r.ActionRunID = actionRun.ID
	if ok, err := agents_model.UpdateRunStatus(ctx, r, agents_model.StatusWaiting, "base_ref", "push_branch", "pull_target", "read_only_reason", "action_run_id"); err != nil {
		return err
	} else if !ok {
		return util.NewInvalidArgumentErrorf("run %d changed while it was dispatched", r.ID)
	}
	s.Status = agents_model.StatusWaiting
	if err := agents_model.UpdateSession(ctx, s, "status"); err != nil {
		return err
	}
	notifySession(s.ID)
	notifyRepo(s.RepoID)
	return nil
}

func failRun(ctx context.Context, r *agents_model.Run, message string) {
	r.Error = message
	if ok, err := agents_model.UpdateRunStatus(ctx, r, agents_model.StatusFailure, "error"); err != nil || !ok {
		if err != nil {
			log.Error("agents: fail run %d: %v", r.ID, err)
		}
		return
	}
	afterRunDone(ctx, r)
}

// afterRunDone reports the outcome where the run was requested and updates the session status
func afterRunDone(ctx context.Context, r *agents_model.Run) {
	defer notifySession(r.SessionID)
	defer notifyRepo(r.RepoID)
	if err := agents_model.RevokeRunToken(ctx, r); err != nil {
		log.Error("agents: revoke token of run %d: %v", r.ID, err)
	}
	s, err := agents_model.GetSessionByID(ctx, r.SessionID)
	if err == nil {
		err = s.LoadAttributes(ctx)
	}
	if err != nil {
		log.Error("agents: load session of run %d: %v", r.ID, err)
		return
	}
	if active, err := agents_model.HasActiveRun(ctx, s.ID); err == nil && !active {
		latest, err := agents_model.GetLatestRun(ctx, s.ID)
		if err == nil && latest != nil {
			s.Status = latest.Status
			if err := agents_model.UpdateSession(ctx, s, "status"); err != nil {
				log.Error("agents: update session %d: %v", s.ID, err)
			}
		}
	}
	if fresh, err := agents_model.GetRunByID(ctx, r.ID); err == nil {
		r.ReviewedHead = fresh.ReviewedHead // recorded by a push notified while the caller held the run
	}
	if r.Purpose == agents_model.RunPurposeReview && r.Status == agents_model.StatusSuccess && s.Issue != nil && s.Agent != nil {
		if err := submitAgentReview(ctx, s, r); err != nil {
			log.Error("agents: review of run %d: %v", r.ID, err)
		}
	} else if s.IssueID > 0 && (r.ReplyCommentID > 0 || r.Summary != "" || r.Error != "") {
		if err := upsertReply(ctx, s, r, replyFinal(ctx, s, r)); err != nil {
			log.Error("agents: final reply of run %d: %v", r.ID, err)
		}
	}
	finishAgentMerge(ctx, s, r)
	// the reply notifies the participants of an issue, the trigger user of a task waits on the Agents tab
	if s.IssueID == 0 && r.Status != agents_model.StatusCancelled && s.Agent != nil {
		if err := activities_model.NotifySubject(ctx, r.TriggerUserID, s.RepoID, activities_model.NotificationSourceAgentSession,
			util.EllipsisDisplayString(s.Title, 255), fmt.Sprintf("agents/%d", s.ID), s.Agent.BotID); err != nil {
			log.Error("agents: notify the end of run %d: %v", r.ID, err)
		}
	}
}

// CanCancel reports whether a user can cancel a session: the code writers and its participants
func CanCancel(ctx context.Context, s *agents_model.Session, doer *user_model.User, perm access_model.Permission) bool {
	if doer == nil {
		return false
	}
	if perm.CanWrite(unit.TypeCode) {
		return true
	}
	participant, err := agents_model.IsParticipant(ctx, s.ID, doer.ID)
	return err == nil && participant
}

var pendingStatuses = []agents_model.Status{agents_model.StatusQueued, agents_model.StatusWaiting, agents_model.StatusRunning}

// CancelSession cancels the queued runs and the active run of a session
func CancelSession(ctx context.Context, s *agents_model.Session) error {
	return cancelRuns(ctx, agents_model.FindRunsOptions{SessionID: s.ID, Statuses: pendingStatuses}, true)
}

// cancelRuns settles the runs under the dispatch lock, so none gets an Actions run meanwhile, and cancels their Actions runs after it:
// cancelling an Actions run notifies its status synchronously, which dispatches again and would wait for the lock.
// Without report, nothing is posted about the runs, their sessions are being deleted.
func cancelRuns(ctx context.Context, opts agents_model.FindRunsOptions, report bool) error {
	var settled []*agents_model.Run
	err := globallock.LockAndDo(ctx, "agents_dispatch", func(ctx context.Context) error {
		runs, err := db.Find[agents_model.Run](ctx, opts)
		if err != nil {
			return err
		}
		for _, r := range runs {
			// settle the run first so the Actions status sync does not report the cancellation as a failure
			if cancelled, err := agents_model.UpdateRunStatus(ctx, r, agents_model.StatusCancelled); err != nil {
				return err
			} else if cancelled {
				settled = append(settled, r)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, r := range settled {
		if err := cancelActionRun(ctx, r); err != nil { // the run is settled anyway, its job loses its tokens
			log.Error("agents: cancel the Actions run of run %d: %v", r.ID, err)
		}
		if report {
			afterRunDone(ctx, r)
		}
	}
	DispatchQueued(ctx)
	return nil
}

func cancelActionRun(ctx context.Context, r *agents_model.Run) error {
	if r.ActionRunID == 0 {
		return nil
	}
	actionRun, err := actions_model.GetRunByRepoAndID(ctx, r.RepoID, r.ActionRunID)
	if errors.Is(err, util.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if actionRun.Status.IsDone() {
		return nil
	}
	jobs, err := actions_model.GetRunJobsByRunAndAttemptID(ctx, actionRun.ID, actionRun.LatestAttemptID)
	if err != nil {
		return err
	}
	_, err = actions_service.CancelRun(ctx, actionRun, jobs)
	return err
}

// syncActionRunStatus follows the Gitea Actions run of an agent run, it settles runs the driver could not report
func syncActionRunStatus(ctx context.Context, actionRun *actions_model.ActionRun) {
	if actionRun.WorkflowID != agents_model.WorkflowID {
		return
	}
	r, err := agents_model.GetRunOfActionRun(ctx, actionRun)
	if err != nil {
		if !errors.Is(err, util.ErrNotExist) {
			log.Error("agents: run of action run %d: %v", actionRun.ID, err)
		}
		return
	}
	switch {
	case actionRun.Status == actions_model.StatusRunning && r.Status == agents_model.StatusWaiting:
		if _, err := agents_model.UpdateRunStatus(ctx, r, agents_model.StatusRunning); err != nil {
			log.Error("agents: run %d running: %v", r.ID, err)
		}
		if s, err := agents_model.GetSessionByID(ctx, r.SessionID); err == nil {
			s.Status = agents_model.StatusRunning
			_ = agents_model.UpdateSession(ctx, s, "status")
		}
		notifySession(r.SessionID)
		notifyRepo(r.RepoID)
	case actionRun.Status.IsDone():
		if !r.Status.IsDone() {
			status := agents_model.StatusFailure
			if actionRun.Status == actions_model.StatusCancelled {
				status = agents_model.StatusCancelled
			}
			if r.Error == "" {
				r.Error = fmt.Sprintf("the Gitea Actions run ended with status %s before Codex reported a result", actionRun.Status)
			}
			if ok, err := agents_model.UpdateRunStatus(ctx, r, status, "error"); err != nil {
				log.Error("agents: settle run %d: %v", r.ID, err)
			} else if ok {
				afterRunDone(ctx, r)
			}
		}
		DispatchQueued(ctx)
	}
}
