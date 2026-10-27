# AGENTS-HOOK inventory

Planned hooks (milestone in brackets). Every hook gets a `// AGENTS-HOOK: <reason>` comment (`{{/* AGENTS-HOOK */}}` in templates); `agents/deploy/rebase-check.sh` (M9) verifies this list against `grep`.
Status: milestone 1 and 2 hooks implemented (marked *done*); the rest is planned.

| File | Where | Reason | M |
| --- | --- | --- | --- |
| `routers/init.go` | next to `webhook.Init` | register agents notifier/queues | 4 (not needed in M1) |
| `modelmigration/migrations.go` (+ `v29/v365.go`) | `newMigration(365, …)` | agent + agent_connection tables — *done* | 1 |
| ~~`modules/setting/config.go`~~, ~~`routers/web/admin/config.go`~~ | — | **dropped**: `config.NewOption` self-registers, bounds are enforced by `modules/agents` (clamp on read, validate on save) | 1 |
| `routers/web/web.go` | admin group: one `agents_web.RegisterAdminRoutes(m)` call, one `agents_web.AdminContext` middleware, one import — *done*; user / org / repo groups later | 1,6 |
| `templates/admin/navbar.tmpl` (*done*), `templates/user/settings/navbar.tmpl`, `templates/org/settings/navbar.tmpl`, `templates/repo/settings/navbar.tmpl` | one link each | menu entries | 1,6 |
| `routers/api/v1/api.go` | `reqOrgOwnership`, `reqOrgMembership` (deny session tokens) — *done*; admin/user/org API groups later | deny org routes; API | 2,6 |
| `models/user/user_extdata.go` | `extDoerToken` (+ `CredentialAgentSession`, `NewSessionTokenExtDoerData`, `GetDoerSessionToken`) | session token state — *done* | 2 |
| `services/auth/oauth2.go`, `services/auth/basic.go` | PAT branches, one `rejectAgentBotToken` call each | refuse agent bot tokens — *done* | 2 |
| `routers/web/web.go` | `webAuth` method list, one line; `routers/api/v1/api.go` `buildAuthGroup`, one line | session token auth for API, git HTTP, LFS — *done* | 2 |
| `models/perm/access/repo_permission.go` | `GetDoerRepoPermission`, 3 lines (logic in `agent_session_permission.go`) | permission intersection — *done* | 2 |
| `models/repo/repo_list.go` | `DoerTokenRepoCond` | repo visibility intersection — *done* | 2 |
| `routers/private/hook_pre_receive.go` | ref loop, 3 lines (logic in `hook_pre_receive_agents.go`) | session branch rule, no force push, `.agent` read-only — *done* | 2 |
| `services/actions/task.go` | after `GetSecretsOfTask` | inject agent secrets | 5 |
| `services/cron/cron.go` | `Init` | agents cron tasks | 4,7 |
| `options/locale/locale_en-US.json` | trailing `agents.*` block (en-US only, D4) — *done* | strings | 1+ |
| `routers/web/repo/issue_view.go` | `prepareFuncs` | session card data | 3 |
| `templates/repo/issue/view_content.tmpl` | after comments (l.67) | session card | 3 |
| `web_src/js/index.ts` | `callInitFunctions` | agents UI init | 3 |
