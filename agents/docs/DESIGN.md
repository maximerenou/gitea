# Agents — design (milestone 0)

Base: Gitea `main` as of 2026-10-26 (commit 9e27bce), module `gitea.dev`, Go 1.27, migrations in `modelmigration/` (last: 364, `v29`).
Every symbol below was read in this tree; file:line references are from that commit.

## 1. Corrections to the spec caused by this Gitea version

| Spec says | Reality | Consequence |
| --- | --- | --- |
| module `code.gitea.io/gitea` | `gitea.dev` | use `gitea.dev/...` |
| `models/migrations` | `modelmigration/` (`migrations.go`, `newMigration(N, ...)`) | one migration file per milestone in `modelmigration/v29` (or new `v30`) |
| eventsource | none. Live updates = WebSocket `/-/ws` over a pubsub broker (`routers/web/websocket/websocket.go`, `services/pubsub`); a client only subscribes to its own `UserTopic` | see §8 |
| `system.GetSetting` | none. Typed DB settings = `config.NewOption[T]` (`modules/setting/config/value.go`), stored in `system_setting`; the option self-registers, so it lives in `modules/agents` with no hook | see §3 |
| `ActionPerm` env / `IsActionsToken` ctx data | none. Credential state lives on the doer: `user_model.User.ExtDoerData` (`models/user/user_extdata.go`), forwarded to git hooks via `GITEA_PUSHER_EXT_DOER_DATA` | see §6 |

## 2. Package layout (all new)

```
models/agents/        Agent, Connection, Session, Run, QueueItem, Secret, SessionToken, Alert, CommentMeta
modules/agents/       config (.agent, .gitea/agents) parse/validate/merge, result.json schema, events/actions matrix, settings types, chain markers, cli backends spec
modules/structs/      agents_*.go (API structs)
services/agents/      notifier, discovery, sessions, queue, guardrails, commands, workflow generation, result application, cron, plans, perms, bots
routers/web/agents/   admin / user / org / repo pages, session live endpoint
routers/api/v1/agents/  admin API + runner API
templates/agents/  web_src/js/features/agents-*.ts
agents/               docs, runner-helper (own go.mod), runner-image, examples, deploy, e2e
```

## 3. Data model (M1 migration 365 creates only `agent` and `agent_connection`; each later milestone adds its tables via its own migration)

- `agent`: id, owner_id (0 = global), bot_user_id (unique), name (= bot username, ≤40), connection_id, model, description, language, action (immutable), events (JSON), settings (JSON, per action), access_mode + access_list (JSON), created/updated.
- `agent_connection`: id, owner_id (0 = global), name, provider (anthropic|openai-compatible|ollama), base_url, token (encrypted with `modules/secret.EncryptSecret`, write-only), cli (backend name), access_mode (none|all|list) + access_list (JSON) for global ones.
- `agent_session`: id, agent_id, repo_id, issue_id (0 = repo level), trigger_user_id, cli_conversation_id, chain_depth, status, config_commit_sha, created/updated.
- `agent_run`: id, session_id, action_run_id, status, trigger_event, trigger_user_id (0 = scheduled), attempt, result (JSON), error, write_requests, started/ended.
- `agent_queue_item`: id, session_id, payload (JSON), created; expiry by TTL.
- `agent_secret`: id, scope(agent) agent_id, name, data (encrypted). Instance/owner/repo scopes reuse existing `secret` / instance-global handling (see §7).
- `agent_session_token`: id, session_id, run_id, token_hash, token_salt, last_eight, scope, expires/revoked (§6).
- `agent_comment_meta`: comment_id (unique), agent_id, chain_depth. `Comment.CommentMetaData` is a closed JSON struct and has no free field, so a side table keyed by `comment_id` is used (no existing column conflicts).
- `agent_alert`: minimal security alert (milestone 7).
- Global settings: `config.NewOption[AgentSettings]("agent.settings")` in a new file; hook = one field in `ConfigStruct` (`modules/setting/config.go`). Bounds are enforced on read (clamp) and in `validateConfigKeyValue` for the `agent.` prefix (`routers/web/admin/config.go:115`) — the stock validator only checks key/JSON/64 KiB.

## 4. Event flow

1. A dedicated `Notifier` embeds `notify_service.NullNotifier` and is registered in `agents_service.Init` (hook: one `mustInit` line in `routers/init.go`, next to `webhook.Init`). Dispatch is synchronous (`services/notify/notify.go`), so handlers only filter and push to a persistent queue (`queue.CreateUniqueQueue`, level backend by default; example `services/actions/init.go:63`).
2. Mapping:
   - issue_created ← `NewIssue` (poster via `issue.LoadPoster`); assigned ← `IssueChangeAssignee`; mentioned/replied ← `CreateIssueComment` (mentions are pre-resolved and passed in; `FindAllMentionsMarkdown` for raw parse); review_requested ← `PullRequestReviewRequest(isRequest)`; pr_created ← `NewPullRequest`; review_submitted ← `PullRequestReview`; review_comment ← `PullRequestCodeComment`; push_default_branch ← `PushCommits` + `opts.RefFullName`; default_branch_ci_failed ← `WorkflowRunStatusUpdate`; stop-on-close ← `IssueChangeStatus`, `MergePullRequest`.
   - Mentions are not passed on comment edit; edits do not trigger.
3. Loop guard: bot comments fire the notifier too → ignore events whose doer is an agent bot, except the agent-to-agent path (mention/assign/review request by a bot), which carries `chain_depth+1` from `agent_comment_meta`; any human doer resets it to 0.
4. Worker: resolve agents (global + repo owner scope), check event ∈ agent.events ∩ action matrix, write permission + agent access mode (§6), guardrails, add bot as write collaborator (`AddOrUpdateCollaborator`, `services/repository/collaboration.go:23`), upsert session, enqueue or start run, react 👀 via `issue_service.CreateCommentReaction(ctx, bot, comment, "eyes")` (`eyes` is in default `[ui] REACTIONS`; if removed, skip silently).
5. Blocked-by-dependency: `issues_model.IssueNoDependenciesLeft` (`models/issues/dependency.go:191`); it ignores whether the repo enables dependencies, check the unit config ourselves.
6. Cron (`services/cron`, `RegisterTask`, requires locale key `admin.dashboard.<name>`): scheduled scans, queue expiry, blocked-request reminders. Hook: a call in `cron.Init`.

## 5. Creating the run (see DECISIONS D1)

`actions_service.PrepareRunAndInsert(ctx, content []byte, run *ActionRun, inputs)` (`services/actions/run.go:25`) is exported and takes arbitrary YAML bytes; it needs `WorkflowRepoID != 0`. The payload is stored in `ActionRunJob.WorkflowPayload`; reruns do not read git. No existing file changes.
- Run fields: RepoID/Repo, OwnerID, WorkflowID=`agent-<session>.yml` (free string), TriggerUserID (human, or Actions user -2 for scans), Ref=default branch, CommitSHA=HEAD, Event = `workflow_dispatch` (any event outside push/PR/release avoids commit statuses), TriggerEvent, EventPayload.
- Do not use `uses:` (reads git) and no `id-token` permission (OIDC reads the workflow file). Explicit `permissions:` block in YAML (can only lower).
- Consequences: no `workflow_run` webhook for synthetic runs (silent), "view workflow file" link on the run page 404s. Acceptable.
- Actions unit is not checked on the insert path; an owner-level runner needs the unit on the target repo (`models/actions/task.go:277`), repo-level/global runners do not. Admin docs must say: use a global/repo runner with label from settings.
- Untrusted text never enters YAML: the generated YAML contains only fixed text plus ids; issue/PR content is fetched by the helper through the API.
- Cancel: `actions_service.CancelRun(ctx, run, jobs)` (`cancel.go:19`), jobs from `GetLatestAttemptJobsByRun`; refuse when `run.Status.IsDone()`.
- Completion/invalidation: notifier overriding `WorkflowJobStatusUpdate`/`WorkflowRunStatusUpdate` (task may be nil; key on job/run ids; reruns create several tasks per run).

## 6. Security design (milestone 2)

Findings that drive it:
- A bot's personal access token authenticates today (`services/auth/oauth2.go` PAT branch and `basic.go` have no `IsIndividual` check). → refuse when `u.IsTypeBot()` in both branches (2 lines each).
- The Actions task token hex cache (`models/actions/task.go:202`) does not recheck status, so a used token can survive the task. → do **not** reuse task tokens as session tokens; session tokens have their own table, checked on every request.
- `tokenRequiresScopes`/`CheckTokenScopes` are no-ops unless `GetDoerTokenScope` is true, i.e. unless ExtDoerData is `*extDoerToken`. → session state is added to `extDoerToken` (fields `sessionID`, `triggerUserID`), not a sibling type; encoding becomes 3-part (`SplitN(s,"|",3)`, backward compatible).

Hooks:
1. `models/user/user_extdata.go`: session fields, `NewSessionTokenExtDoerData`, `GetDoerSessionToken`.
2. `services/auth/session_token.go` (new, modelled on `deploy_token.go`): prefix `gsess_`, lookup in `agent_session_token`, bot via `GetValidDoerUserByID` + `IsTypeBot`, not expired/revoked, agents enabled. Returns `nil,nil` for foreign tokens. Registered in `buildAuthGroup` (`api.go:849`) and the web group (`web.go:117-170`, next to `DeployToken`) and LFS flags.
3. `models/perm/access/repo_permission.go:413` `GetDoerRepoPermission`: first branch → `getSessionTokenRepoPermission`: per unit `min(bot, trigger)`, trigger loaded without ExtDoerData, fail closed if trigger missing/inactive, clamped by token scope, `AccessMode` left at None (so admin/owner shortcuts never apply). `<bot>/.agent`: cap all to Read, including `AccessMode`. Cache key already includes `ExtDoerData.EncodeToString()` (`RepoUserPermissionCacheKey`).
4. `models/repo/repo_list.go:774` `DoerTokenRepoCond`: AND with `AccessibleRepositoryCondition(trigger)` → repo/issue search and lists see only repos visible to both.
5. `api.go` `reqOrgOwnership`/`reqOrgMembership`: reject session tokens (bot may be org owner; not intersected otherwise). Bots can never be site admin (`ErrBotCanNotBeAdmin`), so `/admin/*` is denied by `reqSiteAdmin`.
6. `routers/private/hook_pre_receive.go:94` ref loop: for session doers deny anything but `refs/heads/agent/<session>/…` (also `refs/for`, tags, deletions), deny non-fast-forward by copying the `rev-list` check from `preReceiveBranch`, deny `.agent`. All non-HTTP writers (contents API, web editor, merge) push through the same pre-receive with the doer env, so one guard covers them. "Direct commit" mode relaxes the branch rule via agent setting.
7. Scheduled runs: no session token; run as bot only (`WorkflowDispatch` with Actions user as trigger; bot credentials come from the connection, repo access through its collaborator rights).
8. Runner API (`/api/v1/agents/runs/{id}/…`): accepts only session tokens bound to that run id; refuses all other credential kinds.
9. Global disable: auth method checks `Enabled` on each request → all tokens die at once. Token revoked on job/run done (§5).

Every item gets at least one failing-if-violated test (list in the prompt); they ship with milestone 2.

## 7. Secrets

Reuse `secret.Secret` (owner/repo scopes, `modules/secret` AES with `SECRET_KEY`) for owner and repo. Instance and agent scopes: new `agent_secret` table (instance = agent_id 0, owner_id 0). Injection: merge into the map returned by `GetSecretsOfTask` in `services/actions/task.go:141` (3-line hook, filtered to secrets referenced by active MCP servers). Masking is done by the runner, not the server — the helper must also never print resolved values. Precedence: instance < owner < repo < agent.

## 8. UI / live output

- Pages: admin navbar (`templates/admin/navbar.tmpl`), user/org/repo settings navbars (one `<a>` each, guarded). All routes behind a `agentsEnabled` middleware returning `ctx.NotFound` (pattern: `lfsServerEnabled`, `web.go` ~376-438). Admin → Agents → General is the exception (reachable when disabled).
- Session card: `agents_service.PrepareIssueView` appended to `prepareFuncs` in `routers/web/repo/issue_view.go:398` (sets `ctx.Data["AgentSessions"]`); template include after the comments loop in `templates/repo/issue/view_content.tmpl` between lines 67 and 69.
- Live output: broker is topic-generic but `/-/ws` serves only the user topic and ignores client messages. Plan: new long-polling route `GET /{owner}/{repo}/agents/sessions/{id}/stream` (SSE, `routing.MarkLongPolling()`, repo-read check) subscribing to `pubsub.DefaultBroker` topic `agent-session-<id>` (works with Redis broker); a `user event` is not needed. Fallback: JSON poll.
- Frontend: `web_src/js/features/agents-*.ts`, init via `registerGlobalInitFunc` (`data-global-init`), one line in `callInitFunctions` (`web_src/js/index.ts`).
- API: groups mirrored from actions secrets (`api.go` user ~1112, org 1738, admin 1814), swagger structs in `modules/structs/agents_*.go` + `routers/api/v1/swagger`.
- Agent creation: bot via `user_model.CreateUser` (`Type: UserTypeBot`, empty password, private email, `AllowCreateOrganization=false`); `.agent` via `repo_service.CreateRepository` + `files_service.ChangeRepoFiles` (empty-repo behaviour to be tested in M1); avatar via `user_service.UploadAvatar`; deletion `user_service.DeleteUser(ctx, u, true)`. Bots can be assignee/reviewer/collaborator with no changes.

## 9. Hook inventory (planned; kept in `HOOKS.md`)

See `HOOKS.md`. Estimated total touch in existing files: ~120 lines across ~20 files, all marked `AGENTS-HOOK`.

## 10. Test strategy notes

Integration pattern: `defer tests.PrepareTestEnv(t)()`, `loginUser`, `getUserToken`, `test.MockVariableValue(&setting..., v)()`. Notifiers run inline in the request, queues do not: tests flush the agents queue. Fixtures must add bot users. Disabled-mode check = 404 on every route + no queue push.
