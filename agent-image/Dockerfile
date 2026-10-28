FROM node:22-bookworm
RUN npm install -g @openai/codex && apt-get update && apt-get install -y --no-install-recommends git ca-certificates iptables && rm -rf /var/lib/apt/lists/*
