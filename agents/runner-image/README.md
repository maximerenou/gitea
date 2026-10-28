# Agent job image

`Dockerfile` builds the image the runner uses for agent jobs (setting "Job image" in Administration > Agents > General).

It contains `agent-run` (built from `../runner-helper`), `git`, `curl`, Node.js (the checkout action runs on it) and the Codex CLI, whose version you must pin with `--build-arg CODEX_VERSION=<version>`.

Status: the Dockerfile has not been built yet (no Docker daemon in the development sandbox), and the Codex CLI invocation in `../runner-helper/internal/helper/codex.go` has not been checked against a real Codex (see `docs/DECISIONS.md`, D3).
