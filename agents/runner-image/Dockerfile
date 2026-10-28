# Job image of the agent runs: the agent-run helper, the agent CLI, git, and Node for the checkout action.
# Build from the agents/ directory:
#   docker build -f runner-image/Dockerfile --build-arg CODEX_VERSION=<version> -t gitea-agents-runner:latest .
# The gitea MCP server is added with milestone 5.

FROM golang:1.24-bookworm AS helper
WORKDIR /src
COPY runner-helper/ ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/agent-run .

FROM node:22-bookworm-slim
ARG CODEX_VERSION
RUN test -n "${CODEX_VERSION}" || { echo "pass --build-arg CODEX_VERSION=<version>: the agent CLI must be pinned" >&2; exit 1; }
RUN apt-get update \
 && apt-get install -y --no-install-recommends git curl ca-certificates coreutils \
 && rm -rf /var/lib/apt/lists/*
RUN npm install -g "@openai/codex@${CODEX_VERSION}" && npm cache clean --force
COPY --from=helper /out/agent-run /usr/local/bin/agent-run
# runs as the user the runner starts the job with; nothing here needs root
