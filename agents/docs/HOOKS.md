# AGENTS-HOOK inventory

Planned hooks (milestone in brackets). Every hook gets a `// AGENTS-HOOK: <reason>` comment (`{{/* AGENTS-HOOK */}}` in templates); `agents/deploy/rebase-check.sh` (M9) verifies this list against `grep`.
Status: milestone 1 hooks implemented (marked *done*); the rest is planned.

| File | Where | Reason | M |
| --- | --- | --- | --- |
| `routers/init.go` | next to `webhook.Init` | register agents notifier/queues | 4 (not needed in M1) |
| `modelmigration/migrations.go` (+ `v29/v365.go`) | `newMigration(365, …)` | agent + agent_connection tables — *done* | 1 |
| ~~`modules/setting/config.go`~~, ~~`routers/web/admin/config.go`~~ | — | **dropped**: `config.NewOption` self-registers, bounds are enforced by `modules/agents` (clamp on read, validate on save) | 1 |
| `routers/web/web.go` | admin group: one `agents_web.RegisterAdminRoutes(m)` call, one `agents_web.AdminContext` middleware, one import — *done*; user / org / repo groups later | 1,6 |
| `templates/admin/navbar.tmpl` (*done*), `templates/user/settings/navbar.tmpl`, `templates/org/settings/navbar.tmpl`, `templates/repo/settings/navbar.tmpl` | one link each | menu entries | 1,6 |
| `routers/api/v1/api.go` | admin/user/org groups, `buildAuthGroup`, `reqOrgOwnership`/`reqOrgMembership` | API + session auth + deny org-owner routes | 1,2 |
| `models/user/user_extdata.go` | `extDoerToken` | session token state | 2 |
| `services/auth/oauth2.go`, `services/auth/basic.go` | PAT branches | refuse agent bot tokens | 2 |
| `routers/web/web.go` | `webAuth` method list | session token auth for git HTTP / LFS | 2 |
| `models/perm/access/repo_permission.go` | `GetDoerRepoPermission` | permission intersection | 2 |
| `models/repo/repo_list.go` | `DoerTokenRepoCond` | repo visibility intersection | 2 |
| `routers/private/hook_pre_receive.go` | ref loop | session branch rule, `.agent` read-only | 2 |
| `services/actions/task.go` | after `GetSecretsOfTask` | inject agent secrets | 5 |
| `services/cron/cron.go` | `Init` | agents cron tasks | 4,7 |
| `options/locale/locale_en-US.json` | trailing `agents.*` block (en-US only, D4) — *done* | strings | 1+ |
| `routers/web/repo/issue_view.go` | `prepareFuncs` | session card data | 3 |
| `templates/repo/issue/view_content.tmpl` | after comments (l.67) | session card | 3 |
| `web_src/js/index.ts` | `callInitFunctions` | agents UI init | 3 |
