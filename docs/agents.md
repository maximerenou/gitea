# Codex agents

Agents are mentionable bots, such as `@codex`, defined by the site administrator. Each runs
[Codex CLI](https://github.com/openai/codex) against a model endpoint. Users bring their own API key, and an agent
always works with the key and the permissions of the user who triggers it.
Each run is a Gitea Actions job: Gitea never runs Codex itself.

## How a run executes

1. Gitea creates an Actions run from a generated workflow (`gitea-agent-session.yml`).
2. A Gitea Runner picks the job and starts a fresh container.
3. The job checks out the repository, runs the repository's setup steps if any, then runs `codex exec --json`
   with an isolated `CODEX_HOME`. Codex reaches the model through Gitea, which adds the trigger user's own API key.
   Codex's analytics, feedback and OpenTelemetry exports are turned off, the model requests are all that leaves the job.
4. While Codex works, the driver streams its events to the Agents tab, pushes the first changes at once
   and later ones regularly to the session branch, and keeps the draft pull request up to date.
5. At the end it posts the answer, requests the requester's review and saves the Codex conversation for the next run.

## Identity and permissions

Each agent has a bot account. The agent works under that identity: it opens the pull requests, writes the comments
and authors the commits, crediting the requester with a `Co-authored-by:` trailer.

- On its own, the bot has no access to anything. During a run it borrows the permission of the user who triggered
  the run, capped to write and limited to the run's repository: it never administers a repository and never gets more
  access than that user.
- It has no password. It authenticates only with a token created for one run and deleted when the run ends.
  That token only works with git, never with the API, and can only push to the branch of the run. Runs that cannot
  push, like reviews or the answers to readers, get a read-only token.
- Because pushes come from the bot and not from the Actions token, they trigger the repository workflows like any
  contributor's, so CI runs on agent pull requests. Since the agent reads issues and comments anyone can write, and
  could be led to change a script CI runs, these runs get no repository or owner Actions secrets nor variables, and a
  read-only Actions token, like pull requests from forks: the runs of the bot's pushes, and the pull request runs while the bot pushed their head
  last. Once a person pushes to the branch, its CI gets them again. Nor can the bot's pushes change the workflows:
  Gitea refuses them unless each workflow file matches the branch's previous head or its base. On pull requests from
  forks the agent only answers, it never pushes the fork's commits here.
- The API key never enters the job. Codex gets a model token valid while the run lasts, and Gitea forwards its model
  requests to the agent's endpoint with the trigger user's own key, only for the Responses and models APIs, with the
  run's model. Gitea drops the provider's hosted tools from the requests, such as web search or remote MCP servers, and
  refuses remote images and files: they would reach the network past the [firewall](#network-firewall). The job never gets anybody else's key,
  nor the repository or owner Actions secrets. A user without a key gets a reply asking them to add one.

## Collaboration

An agent keeps one conversation per issue or pull request, shared by everyone who works with it there: Bob can pick up
where Alice left off. Each run uses the key and the permissions of whoever triggered it, so the same session can push
for a writer and only answer for a reader. Runs of a session are queued, and agents take turns on a branch, so `@codex`
and another agent never push to the same pull request at once. Different agents keep separate conversations and share
the pull request itself.

Earlier instructions stay in the conversation: when a writer continues a session, Codex also sees what readers asked
before. Read a session's history before continuing it with your key. Only the user who triggered a run can retry it,
and only the user whose run was interrupted can resume the session: their request would otherwise run with someone
else's permissions.

When a check fails on the head of an agent's pull request, the agent tries to fix it with the key of the user who
triggered its latest run. Only failures reported by Gitea Actions, or by that user, start such a run.

## Requirements

- Gitea Actions enabled (`[actions] ENABLED = true`).
- A Gitea Runner in **docker** mode. Do not use a `host` label: Codex runs without its own sandbox,
  the job container is the isolation between sessions.
- A job image providing **Node.js, npm, git and tar**, ideally with Codex preinstalled, see below.
- Network access from the job containers to:
  - Gitea's `ROOT_URL`, used to clone, push and call the agents API,
  - the model endpoints of the agents.

## The job image

Runners map `ubuntu-latest` to their default image, which usually lacks Node.js
(for example `buildpack-deps`). The job then fails at its first step with `node: not found`.
Set `[agents] CONTAINER_IMAGE` so agent jobs always run in an image made for them,
whatever the runner's labels.

### Build a Codex image

```dockerfile
FROM node:22-bookworm
ARG CODEX_VERSION=latest
RUN npm install --global @openai/codex@${CODEX_VERSION} \
  && apt-get update \
  && apt-get install -y --no-install-recommends git ca-certificates iptables \
  && rm -rf /var/lib/apt/lists/*
```

`iptables` is only needed for the [firewall](#network-firewall). Install the toolchains your projects need in the
image, or let each repository install them with its [setup steps](#repository-setup).

```bash
docker build -t codex-runner:latest .
```

Pin `CODEX_VERSION` to a release to get reproducible runs. When the image already provides `codex`,
jobs skip the installation, otherwise they install `[agents] CODEX_VERSION` with npm at every run.

Check the image:

```bash
docker run --rm codex-runner:latest codex --version
```

### Make the image available to the runner

Either build it on the runner host and keep it local, then disable pulling in the runner's `config.yaml`:

```yaml
container:
  force_pull: false
```

Or push it to a registry the runner can pull from, for example the Gitea container registry:

```bash
docker tag codex-runner:latest git.example.com/infra/codex-runner:latest
docker push git.example.com/infra/codex-runner:latest
```

Keep that package public or log the runner host's Docker in to the registry.

## Gitea configuration

```ini
[server]
ROOT_URL = https://git.example.com/

[actions]
ENABLED = true

[agents]
ENABLED = true
RUNS_ON = ubuntu-latest
CONTAINER_IMAGE = git.example.com/infra/codex-runner:latest
CONTAINER_OPTIONS = --cpus=2 --memory=4g
JOB_TIMEOUT = 60m
```

| Setting | Default | Description |
| :--- | :--- | :--- |
| `ENABLED` | `false` | Enables agent sessions, requires `[actions] ENABLED`. |
| `RUNS_ON` | `ubuntu-latest` | Runner label of the agent jobs. |
| `CONTAINER_IMAGE` | empty | Image of the agent jobs, it needs Node.js, npm, git and tar. Empty uses the runner's image for `RUNS_ON`. |
| `CONTAINER_OPTIONS` | empty | Docker options of the job container, typically resource limits. |
| `CODEX_VERSION` | `latest` | `@openai/codex` version installed when the image has no `codex`. |
| `JOB_TIMEOUT` | `60m` | Maximum duration of a run. |
| `CHECKPOINT_INTERVAL` | `45s` | Minimum delay between two commits and pushes of a running agent. |
| `MAX_MODEL_REQUESTS_PER_RUN` | `2048` | Maximum provider requests per run, including failed calls. One request at a time per run, enforced in the database across Gitea processes. |
| `MAX_MODEL_OUTPUT_TOKENS` | `32768` | Maximum output tokens per generated response. The proxy caps omitted or higher client limits; the compaction API does not accept this parameter. |
| `MAX_ACTIVE_RUNS_PER_USER` | `10` | Sessions a user can have running at once, in all repositories, further runs are queued. |
| `FIREWALL` | `false` | Restricts the network of Codex, see [Network firewall](#network-firewall). |
| `SETUP_AS_ROOT` | `false` | Behind the firewall, runs the [setup steps](#repository-setup) as root, which lets them use actions but also the run's tokens. |
| `FIREWALL_ALLOWED_HOSTS` | empty | Host names Codex may also reach behind the firewall, wildcards like `*.npmjs.org` allowed. |
| `ENABLE_BY_DEFAULT` | `true` | Whether repositories accept agents until their administrators change it. |
| `MAX_CI_FIX_ATTEMPTS` | `2` | How many times in a row an agent fixes a failing check of its pull request on its own, `0` disables it. |
| `ALLOWED_HOST_LIST` | empty | Extra hosts model endpoints may target, same format as `[security] ALLOWED_HOST_LIST`. Private and loopback endpoints, like a local Ollama, need an entry such as `private` or `loopback`. |

Model requests have a 15-minute deadline and stop when the run finishes (within one second across Gitea processes).
Background responses and provider-side conversation storage are disabled. Usage is read from provider responses,
not from events submitted by the job. Requests without provider usage still consume the request allowance.
The allowance bounds calls and output tokens; it is not a monetary spending limit. Configure a spending limit with the provider as well.
If a Gitea process stops during a model request, the run's request reservation stays occupied; start a new run instead of replaying the request.

Saved Codex state is limited to 256 MiB both compressed and expanded, including archive headers and padding,
and 10,000 archive entries. Previously stored archives are validated before download as well.
CI trust is saved when a workflow is created and retained on retries. Existing same-repository pull request runs
without a saved trust decision are restricted to a read-only token without repository secrets.

The Actions unit must also be enabled in each repository using agents.

Old run logs and Codex conversations are deleted by the `agents_cleanup` cron task, after 90 days by default,
configurable in `[cron.agents_cleanup]` with `OLDER_THAN`. Sessions and runs stay as history.

## Network firewall

With `FIREWALL = true`, before starting Codex the job runs it as an unprivileged user behind iptables rules:

- Codex and the code it runs only reach a proxy the job runs, which filters by host name, so hosts behind CDNs keep
  working. Tools reach it through the standard `HTTP_PROXY` and `HTTPS_PROXY` variables, which Codex, npm, pip, git
  and curl honour.
- Through the proxy, Gitea, the agent's model endpoint and the URLs of the MCP servers defined by an administrator are
  reachable on their ports, at the addresses they had when the run started, pinned in the container's `/etc/hosts`.
  MCP servers a repository adds are only reachable through `FIREWALL_ALLOWED_HOSTS`.
- `FIREWALL_ALLOWED_HOSTS` are reachable through the proxy on ports 80 and 443. Entries are exact names or wildcards
  such as `*.npmjs.org`.
- The proxy checks the host a connection really reaches, so an allowed host cannot front for another one sharing its
  addresses: it sets the `Host` header of plain HTTP requests, and only tunnels TLS whose server name is the requested
  host. It cannot see the requests inside TLS, so a CDN that routes them by their `Host` header alone can still be
  fronted: allow only the hosts you need.
- Everything else is blocked, including DNS and the other loopback addresses than `127.0.0.1`, such as Docker's
  resolver, so no query can carry data out. Blocked hosts are reported in the session
  log, telling which entry is missing.

The setup steps run before, with the full network, as the same unprivileged user, see
[Repository setup](#repository-setup). Connections they leave open are cut when the firewall starts. It needs
`CONTAINER_IMAGE` with `iptables`, `useradd` and `runuser`, and the job container gets the `NET_ADMIN` capability.

Codex runs the repository's code, tests and scripts in the same container as the run's tokens. The firewall keeps
that code from sending them anywhere else, and the job refuses to commit any change containing them as they are, which
does not stop code that encodes them first: the model token is only accepted for model requests and dies with the run. The tokens die
with the run, and the API key itself is never in the container. Without the firewall, only use agents on repositories
whose code you trust.

## Runner configuration

Register a runner with a token from **Site Administration → Actions → Runners**, in docker mode with the label
of `RUNS_ON`. The runner's `capacity` is the number of jobs it runs at once, so it bounds how many sessions
run in parallel across the instance:

```yaml
runner:
  capacity: 4
```

Each session runs at most one job at a time. Instructions sent while it runs are queued and continue
the same branch and pull request.

## Defining agents

In **Site Administration → Agents**, add an agent: its mention name (e.g. `codex`), display name, provider, base URL,
model and reasoning effort. Codex CLI only speaks the OpenAI Responses API, so a model of another vendor needs a gateway
exposing it with that API, such as LiteLLM. You can let users pick their own model, disable an agent or delete it.

**MCP servers** give the agent extra tools. Define them as YAML, each with either a `command` (with `args` and `env`)
or a `url`, plus optional `cwd`, `startup_timeout_sec` and `tool_timeout_sec`:

```yaml
docs:
  url: https://mcp.example.com/mcp
browser:
  command: npx
  args: [-y, "@playwright/mcp"]
```

Everyone using the agent gets these servers, do not put personal credentials in them.

## Repository settings

In **Repository Settings → Agents**, the repository's administrators enable or disable agents, and can restrict them
to some of the instance's agents. Repositories without settings follow `ENABLE_BY_DEFAULT`.

They can also pick an agent reviewing the new pull requests, see [Reviews](#reviews).

## Reviews

An agent reviews a pull request when someone clicks **Review** in its sidebar, with that user's key, or on its own
for the new pull requests of a repository whose administrators chose a review agent. Automatic reviews start when a
pull request opens, or when it stops being a work in progress, and use the key and the permissions of the
administrator who chose the agent, while they administer the repository; another administrator can take it over with
their own key, and does so when changing whose pull requests are reviewed. By default, only the pull requests of users
who can write to the repository are reviewed, so strangers cannot spend that key; the repository can also review
everyone's. Each head commit is reviewed once, and a repository gets at most 50 automatic reviews a day: once its
reviewer triggered that many reviews there in the last 24 hours, new pull requests are not reviewed until the count
drops. When automatic reviews cannot start, because their reviewer lost their key or the administration of the
repository, their agent was disabled, or the daily limit is reached, the repository's agents settings say why.

The run is read-only. The agent answers with a pull request review: its assessment, and comments on the changed lines.
Comments on lines outside the changes go to the review's body. When someone pushes while the agent reviews, its review
is attached to the commit the job checked out. The review continues the agent's conversation on the
pull request, so mentioning the agent afterwards discusses or applies its suggestions.

## Merging with an agent

Users who can merge a pull request get **Merge with an agent** in its sidebar. The agent merges the target branch
into the pull request with a real merge commit, resolves the conflicts by keeping the intent of both sides, and fixes
what the merge broke. Once its run succeeds, the pull request is scheduled to merge with the chosen style when its
checks pass, as the user who asked: branch protections, required reviews and checks still apply. Until it merges, failing checks
are fixed on the pull request's branch like on any agent pull request, up to `MAX_CI_FIX_ATTEMPTS` times.

With **Review the pull request first**, the agent also reviews the whole pull request and ends with a verdict: when it
blocks, or gives none, the merge is not scheduled and its reply tells why. Only the agent's own commits are covered by
its review: when someone else pushes to the pull request while the agent reviews it, the merge is not scheduled, and
when they push after, the scheduled merge is cancelled. A new merge with an agent replaces the earlier ones.

The verdict is advisory. It is read from the agent's answer, which the code of the pull request, run in the job, or
text the agent reads could steer or forge. Branch protections with required approvals and checks remain the real gate:
keep them on branches where an approving verdict must not be enough to merge. Pull requests from forks cannot be merged this way: the agent cannot push to the fork, and it
never brings the fork's commits into the repository. Nor can users who cannot push to the pull request's branch, since
the agent pushes the merge with their permissions: the button is only offered when the merge can work.

## Notifications

When a run of a session started from the Agents tab ends, the user who triggered it gets a notification, until the
session opens a pull request. Sessions of issues and pull requests notify their participants through the agent's reply
instead. The Agents tab updates the
status of its sessions live, and so do issues and pull requests with the agent's replies while it works, including
sessions started after the page was opened for users who can read the code.

## Usage and audit

The tokens Codex reports are counted per run. **Site Administration → Agents** shows the usage of the last 30 days per
user and agent, and each user sees their own in **Settings → AI / Codex**. When the audit log is enabled, it records the
changes of agents, of users' keys and of repositories' agents settings, and every queued run.

## Repository setup

A repository can prepare the agent's environment with `.gitea/agents/setup.yml` on its default branch. Its steps
are Gitea Actions steps, run after the checkout and before Codex, and it can add MCP servers next to the agent's:

```yaml
steps:
  - uses: shivammathur/setup-php@v2
    with:
      php-version: "8.3"
  - run: composer install --no-interaction
mcp_servers:
  docs:
    url: https://docs.example.com/mcp
```

The file is always read from the default branch, so a pull request cannot change what runs before the agent. Steps
accept `name`, `id`, `uses`, `with`, `run`, `shell`, `env`, `working-directory`, `timeout-minutes` and
`continue-on-error`, and `${{ }}` expressions are refused: the steps never see the job's secrets. Files they leave in
the working copy are never committed by the agent, nor are the dependencies Codex installs next to their manifest:
`node_modules/` next to a `package.json`, `vendor/` next to a `composer.json`, `.venv/` next to a `pyproject.toml` or a
`requirements.txt`, unless the repository tracks files in them. The agent's answer lists those it left out. Go's
`vendor/` is committed like any other directory. On a name clash, the administrator's MCP server wins.

Behind the [firewall](#network-firewall), the setup steps run as the unprivileged user Codex later runs as, with the
full network but without the job's tokens, and with `bash` or `sh` only. What they export with `GITHUB_ENV` and
`GITHUB_PATH` reaches Codex, not the job's own steps, so they get no more power than the code Codex runs. A step's
`env` is exported by its own script, and its `timeout-minutes`, a whole number, is enforced inside the job: the shell
that starts the step as root never gets them, nor variables like `LD_PRELOAD` or `PATH` that would run the user's code
as root. Processes a step starts are killed when it ends or times out, so a step cannot leave a service running for
Codex: Codex starts what it needs. They cannot
use actions, which run as root: install the toolchains in the [image](#the-job-image), or in the user's home directory
from a `run` step:

```yaml
steps:
  - run: |
      curl -fsSL https://go.dev/dl/go1.25.0.linux-amd64.tar.gz | tar -xz -C "$HOME"
      echo "$HOME/go/bin" >> "$GITHUB_PATH"
  - run: go mod download
```

`SETUP_AS_ROOT = true` runs them as root instead, which allows actions, but code they leave running could then use the
run's tokens while it lasts: call the model through Gitea, or push to the session branch. Without the firewall, the
setup steps and Codex run as root anyway.

## Using agents

1. In **Settings → AI / Codex**, add your API key for an agent, and your model if the agent allows it.
   **Test connection** checks that the endpoint streams the Responses API with your key and model. Deleting your key
   cancels the runs you queued with it. Agents the administrator disabled stay listed while you have a key for them,
   so you can remove it.
2. Start work in any of these ways:
   - in a repository, open **Agents**, describe a task and pick an agent,
   - assign an issue to an agent,
   - mention an agent in an issue or a pull request, e.g. `@codex explain this change`.
     The agents you have a key for are suggested when you type `@`. Editing a comment to mention an agent works too,
     only the newly mentioned agents get the request.
3. The agent reacts with 👀, opens a draft pull request as soon as it changes something and asks for your review when done.
   If you already worked with it on that pull request, reviewing it, approvals aside, sends your feedback to the agent
   without mentioning it. Other reviewers mention the agent, so nobody spends their key by merely reviewing. A review
   mentioning the agent in its body or in any of its comments on lines starts a single run with all of them.
4. When a CI check fails on a pull request the agent maintains, or whose merge it scheduled, it fixes it on its own with the key and permissions of
   the user who triggered its latest run, once per failing commit and at most `MAX_CI_FIX_ATTEMPTS` times in a row.
   Once it reaches that limit, it says so once on the pull request. Any request from a user resets the count.
5. The session page updates live while the agent works. Closing or merging the agent's pull request cancels its
   running and queued work on it, and the agent then answers mentions there without pushing. Unassigning the agent
   from an issue also cancels its work there.
6. **Your agent sessions** in the user menu lists the sessions you took part in across repositories, and the sidebar of
   an issue or a pull request links to the sessions working on it. The Agents tab filters its sessions by status, kind
   and agent, and to those you created.

When starting a session from the Agents tab, you can override the agent's reasoning effort.

## API

The API v1 lists the agents with `GET /api/v1/agents`, and manages the sessions of a repository under
`/api/v1/repos/{owner}/{repo}/agents/sessions`: list them, filtered with `status`, `kind`, `agent` and `created_by_me`,
start them, get one, list its runs and its Codex events
(`/events`, paginated), send instructions, cancel it, resume it (`/resume`) and retry one of your runs
(`/runs/{run}/retry`). `POST /api/v1/repos/{owner}/{repo}/pulls/{index}/agent-review` and `/agent-merge` ask an agent
to review or to merge a pull request, the latter with a `style` and `review` like in the sidebar. Everything that
starts a run uses your own key and permissions, like in the web UI.

## Troubleshooting

The Actions tab of the repository shows the raw logs of each agent job.

| Symptom | Cause |
| :--- | :--- |
| `node: not found` | The job image has no Node.js, set `CONTAINER_IMAGE`. |
| `invalid .gitea/agents/setup.yml` | The repository's setup file is not valid, the error says why. |
| `iptables: command not found` | `FIREWALL` is on but the image has no `iptables`. |
| `refusing to publish changes containing a secret of the job` | Code run in the job wrote the model token or the bot's token to the working copy, nothing was pushed. |
| The job cannot download the driver or clone | `ROOT_URL` is not reachable from the job containers. |
| The run stays "Waiting for a runner" | No online runner has the `RUNS_ON` label, or all are at capacity. |
| The runner fails to pull the image | The image only exists locally and `force_pull` is enabled, or the registry needs credentials. |
| `no API key was provided to this job` | The trigger user removed their key, the agent was deleted, or the job is a rerun of an agent run, which never gets a model token again. |
| Test connection is denied by the egress policy | The endpoint is on a private network, allow it in `[agents] ALLOWED_HOST_LIST`. |
