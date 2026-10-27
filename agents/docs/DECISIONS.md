# Decisions

Status: validated by the maintainer on milestone 0 (D3, D4, D6, D11 settled below).

## D1 — How runs are created (decided, no user input needed)
Call `actions_service.PrepareRunAndInsert` directly with generated YAML (see DESIGN §5). No file written in the target repo, no change in existing files, rerun works (payload stored per job).
Rejected: committing YAML to a hidden ref + `DispatchActionWorkflow` (writes git); fake `ActionSchedule` rows (invasive, `ScheduleID != 0` side effects).
Risk: upstream could change `PrepareRunAndInsert`'s signature; the call site is one function in `services/agents`, so rebase cost is low.

## D2 — Built-in `gitea` MCP server: stdio first
Milestone 5 ships `gitea-mcp` (stdio) in the job image with the session token. An HTTP MCP served by Gitea is deferred (needs its own authorization surface). Read-only tools only, authorized by the same session-token intersection as everything else.

## D3 — Default agent CLI backend: **Codex CLI** (decided)
Local models: Codex itself against an Ollama/OpenAI-compatible endpoint; a second backend (opencode) is optional and only added if Codex+Ollama proves insufficient in the M3 spike.
Constraint: open source, MCP support, non-interactive JSON event stream, resumable conversation, works with a local model.
- Option A (recommended): **Codex CLI** default (Apache-2.0; `exec` mode with JSON events, MCP stdio, resume, OpenAI-compatible and Ollama endpoints) + **opencode** as the local-model backend.
- Option B: **OpenHands** default (heavier, container-in-container) + Aider (no native MCP).
I have not been able to verify CLI flags offline; M3 starts with a spike that records the exact invocation per backend in `modules/agents` tests. Claude Code is not open source, so it is not the default; it can be added as a third backend behind the same interface.

## D4 — Locale files: **en-US only** (decided, follows AGENTS.md; fr-FR comes from Crowdin)
Previous analysis kept for the record. Risk: Crowdin syncs `fr-FR` from `en-US` (`crowdin.yml`) and may overwrite hand-written French keys with the Crowdin state (untranslated keys are skipped).
- Option A (recommended): write both files; keep the agents keys under a unique prefix (`agents.*`, `admin.agents.*`) so a rebase conflict is confined to one block, and re-run `make` locale checks after each upstream sync.
- Option B: en-US only, French delivered by Crowdin after upstream acceptance (not possible for a fork).

## D5 — Session tokens are a new credential type, not task tokens
Task-token cache does not recheck status (`models/actions/task.go:202`). Session tokens have their own table, are checked per request and revoked on job/run completion and global disable.

## D6 — Bot personal/OAuth tokens
Refused by adding `IsTypeBot()` rejection in the PAT paths of `oauth2.go` and `basic.go`. This also changes stock behaviour when agents are disabled (bot PATs currently work; admin UI can create them). To keep "disabled == original Gitea", the rejection applies only to bots that own an `agent` row.

## D7 — Notifier is synchronous
Handlers only enqueue; all agent logic runs in the queue worker (spec requirement, also protects request latency).

## D8 — New dependencies
None planned for the main module. Helper has its own `go.mod`.

## D9 — Placeholders in the prompt
Base version = `main` @ 9e27bce (2026-10-26, no release tag in the clone). Runner job image registry and default runner label stay configurable settings.

## D10 — Commit trailers
AGENTS.md (`Assisted-by:`, no `Co-Authored-By`) takes precedence over the generic session attribution.

## D11 — `agents/PROMPT.md`
Not created (maintainer: not needed).

## D12 — `.agent` repository template lives in `services/agents/repo_template/`
`go:embed` cannot reach `agents/examples/`, and nothing under `agents/` may be imported by Gitea. The embedded copy is the single source; `agents/examples/` only holds the target-repo example (added with milestone 5).

## D13 — Milestone 1 scope cuts (to finish properly in later milestones)
- Model list from the provider (5 min / 30 s cache): manual model entry only until M6.
- Connection sharing "list" mode and agent access "list" mode: only None/All and Writers/Owner are offered until the user/group picker exists (M6). The columns already exist.
- Event checkboxes are not filtered by action on the *create* form (no JS yet); the server refuses invalid pairs. The edit form is filtered. JS filtering lands with M6.
- Per-action settings, avatar upload and the Access / Secrets buttons: M4–M6.
- Bot personal/OAuth token refusal (D6) is M2.

## D14 — Test environment limitation
Gitea's test harness refuses to run as root, and this sandbox runs as root with the Go caches under `/root` (mode 700). DB-backed tests (`models/agents`, `tests/integration/agents_admin_test.go`) compile (`go vet`) but were **not executed** here. `golangci-lint` (`make fmt` / `lint-go`) also fails to start in this sandbox (pinned v2.15.0 vs Go 1.27.3 toolchain mismatch). `gofmt` and `go vet` are clean.

## D15 — Session token details (milestone 2)
- Stored as SHA-256 of a 160-bit random token (prefix `gsess_`), not PBKDF2: the token has full entropy, and a slow hash plus a cache would either cost every request or bypass revocation. Nothing is cached; revocation, expiry, run status, bot and triggering user are re-checked on every request.
- A token is valid only while its Actions run is not done; also revoked when the run ends (`RevokeSessionTokensOfRun`, wired in M3), when agents are switched off (done in the admin handler) and after at most 24 h.
- The scope can never exceed `write:repository,write:issue,write:package`; public-only is refused.
- Repository search/list: the triggering user is treated as *restricted* when building the visibility condition (no user object is available there). The list can be narrower than what an unrestricted user sees (public repos of private orgs reachable only through org membership are hidden), never wider.
- "Direct commit" push mode (spec) is not available yet: the session record that configures it arrives in M4, until then the branch rule is strict.
- A session cannot merge pull requests: merging pushes to a branch outside `agent/<session>/`. The result-application service (M3/M4) acts as the bot itself, with no token.
