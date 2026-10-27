# Decisions

Status: **proposed** until validated by the maintainer.

## D1 — How runs are created (decided, no user input needed)
Call `actions_service.PrepareRunAndInsert` directly with generated YAML (see DESIGN §5). No file written in the target repo, no change in existing files, rerun works (payload stored per job).
Rejected: committing YAML to a hidden ref + `DispatchActionWorkflow` (writes git); fake `ActionSchedule` rows (invasive, `ScheduleID != 0` side effects).
Risk: upstream could change `PrepareRunAndInsert`'s signature; the call site is one function in `services/agents`, so rebase cost is low.

## D2 — Built-in `gitea` MCP server: stdio first
Milestone 5 ships `gitea-mcp` (stdio) in the job image with the session token. An HTTP MCP served by Gitea is deferred (needs its own authorization surface). Read-only tools only, authorized by the same session-token intersection as everything else.

## D3 — Default agent CLI backend: **needs your decision**
Constraint: open source, MCP support, non-interactive JSON event stream, resumable conversation, works with a local model.
- Option A (recommended): **Codex CLI** default (Apache-2.0; `exec` mode with JSON events, MCP stdio, resume, OpenAI-compatible and Ollama endpoints) + **opencode** as the local-model backend.
- Option B: **OpenHands** default (heavier, container-in-container) + Aider (no native MCP).
I have not been able to verify CLI flags offline; M3 starts with a spike that records the exact invocation per backend in `modules/agents` tests. Claude Code is not open source, so it is not the default; it can be added as a third backend behind the same interface.

## D4 — Locale files
User request: ship `en-US` and `fr-FR`. This overrides AGENTS.md ("only edit en-US"). Risk: Crowdin syncs `fr-FR` from `en-US` (`crowdin.yml`) and may overwrite hand-written French keys with the Crowdin state (untranslated keys are skipped).
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
Not created in M0: the prompt only exists in the chat; please add it verbatim to avoid me altering it.
