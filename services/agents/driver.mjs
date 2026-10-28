// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Runs one Codex CLI agent run inside a Gitea Actions job, in two steps around the repository's setup steps:
// `prepare` checks out the session branch in the workspace, `run` streams the `codex exec --json` events to Gitea,
// commits and pushes checkpoints while Codex works, and keeps the Codex conversation between ephemeral jobs.
import {spawn, execFileSync} from 'node:child_process';
import dns from 'node:dns/promises';
import fs from 'node:fs';
import http from 'node:http';
import net from 'node:net';
import os from 'node:os';
import path from 'node:path';
import readline from 'node:readline';

const api = process.env.GITEA_AGENT_API;
const token = process.env.GITEA_AGENT_TOKEN;
const apiKey = process.env.GITEA_AGENT_API_KEY; // a model token of the run, Gitea adds the trigger user's key to the model requests
const gitToken = process.env.GITEA_AGENT_GIT_TOKEN;
const maxOutput = 16 * 1024;
const agentUser = 'gitea-agent';

const repoDir = process.env.GITHUB_WORKSPACE || process.cwd();
// shared by the steps of the job, readable by the unprivileged user Codex runs as behind the firewall;
// outside /tmp, where Codex refuses to install its helper commands such as apply_patch
const stateDir = (() => {
  for (const dir of ['/var/lib/gitea-agent', path.join(os.homedir(), '.gitea-agent')]) {
    try {
      fs.mkdirSync(dir, {recursive: true, mode: 0o755});
      fs.accessSync(dir, fs.constants.W_OK);
      return dir;
    } catch {}
  }
  throw new Error('no writable directory for the agent state');
})();
// the driver's repository lives outside the workspace, so Codex cannot set the config or hooks of the git commands the driver runs as root;
// Codex gets a copy of it in the workspace for its own use
const gitDir = path.join(stateDir, 'git');
const codexHome = path.join(stateDir, 'codex-home');
// what the unprivileged setup steps export for the next steps, it only reaches Codex
const setupDir = path.join(stateDir, 'setup');
const setupShellPath = '/var/lib/gitea-agent/setup-shell'; // see setupShell in workflow.go

// a setup step runs unprivileged without the job's tokens, and cannot change the environment of the steps run as root:
// the files Actions reads after the step are locked and the step writes its exports to setupDir instead
// it runs as root: only system commands, and none of the loader or shell variables that would run the user's code
const setupShell = `#!/bin/sh
user_path="\${PATH:-/usr/local/bin:/usr/bin:/bin}"
PATH=/usr/sbin:/usr/bin:/sbin:/bin
IFS=' \t\n'
unset LD_PRELOAD LD_LIBRARY_PATH LD_AUDIT GLIBC_TUNABLES BASH_ENV ENV CDPATH SHELLOPTS BASHOPTS PS4 GCONV_PATH LOCPATH NLSPATH HOSTALIASES
export PATH
set -eu
shell="$1"
minutes="$2"
script="$3"
kill_leftovers() {
  /sbin/runuser -u ${agentUser} -- /bin/sh -c 'kill -KILL -1' 2>/dev/null || true
}
kill_leftovers # a process a previous step left could write this step's GITHUB_ENV before it is locked
for file in "\${GITHUB_ENV:-}" "\${GITHUB_PATH:-}" "\${GITHUB_OUTPUT:-}" "\${GITHUB_STATE:-}" "\${GITHUB_STEP_SUMMARY:-}"; do
  if [ -f "$file" ]; then /bin/chmod 600 "$file"; fi
done
for name in $(/usr/bin/env | /bin/sed -n 's/^\\([A-Za-z_][A-Za-z0-9_]*\\)=.*/\\1/p' | /bin/grep -E '^ACTIONS_|TOKEN|^LD_'); do unset "$name"; done
export GITHUB_ENV=${setupDir}/env GITHUB_PATH=${setupDir}/path GITHUB_OUTPUT=/dev/null GITHUB_STATE=/dev/null GITHUB_STEP_SUMMARY=/dev/null
limit=""
if [ "$minutes" -gt 0 ]; then limit="/usr/bin/timeout --kill-after=10 \${minutes}m"; fi
status=0
if [ "$shell" = sh ]; then $limit /sbin/runuser -u ${agentUser} -- /usr/bin/env PATH="$user_path" sh -e "$script" || status=$?
else $limit /sbin/runuser -u ${agentUser} -- /usr/bin/env PATH="$user_path" bash --noprofile --norc -eo pipefail "$script" || status=$?; fi
kill_leftovers
exit "$status"
`;

async function call(method, route, body, headers = {}) {
  for (let attempt = 1; ; attempt++) {
    try {
      const resp = await fetch(api + route, {method, body, headers: {Authorization: `Bearer ${token}`, ...headers}});
      if (resp.ok || resp.status < 500 || attempt >= 3) {
        if (!resp.ok) throw new Error(`${method} ${route}: HTTP ${resp.status} ${await resp.text()}`);
        return resp;
      }
    } catch (err) {
      if (attempt >= 3) throw err;
    }
    await new Promise((resolve) => setTimeout(resolve, attempt * 1000));
  }
}

// whatever the code Codex ran printed, the session log and the summaries never show a secret of the job
const secrets = [apiKey, gitToken, token].filter((secret) => secret?.length >= 8);
const redact = (text) => secrets.reduce((result, secret) => result.split(secret).join('[secret]'), text);

const postJSON = (route, data) => call('POST', route, redact(JSON.stringify(data)), {'Content-Type': 'application/json'});

// the bot's token only reaches git through the environment of the driver's own git commands, never Codex's
let gitEnv = {...process.env, GIT_TERMINAL_PROMPT: '0'};

function setGitCredentials(user) {
  const basic = Buffer.from(`${user}:${gitToken}`).toString('base64');
  secrets.push(basic);
  gitEnv = {...gitEnv, GIT_CONFIG_COUNT: '1', GIT_CONFIG_KEY_0: 'http.extraHeader', GIT_CONFIG_VALUE_0: `Authorization: Basic ${basic}`};
}

function git(...args) {
  return execFileSync('git', ['--git-dir', gitDir, '--work-tree', repoDir, ...args], {cwd: repoDir, env: gitEnv, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'], maxBuffer: 64 * 1024 * 1024}).trim();
}

function tryGit(...args) {
  try {
    return git(...args);
  } catch {
    return null;
  }
}

function fetchRef(ref) {
  const full = ref.startsWith('refs/') ? ref : `refs/heads/${ref}`;
  if (tryGit('fetch', '--no-tags', 'origin', full) === null) return null;
  return git('rev-parse', 'FETCH_HEAD');
}

function prepare(ctx) {
  if (fs.existsSync(gitDir)) return;
  fs.mkdirSync(gitDir, {mode: 0o700});
  git('init', '--quiet');
  git('remote', 'add', 'origin', ctx.clone_url);
  git('config', 'user.name', ctx.commit_name);
  git('config', 'user.email', ctx.commit_email);
  const baseSha = fetchRef(ctx.base_ref);
  if (!baseSha) throw new Error(`cannot fetch ${ctx.base_ref}`);
  git('config', 'gitea-agent.base', baseSha);
  if (ctx.diff_base) tryGit('fetch', '--no-tags', 'origin', `+refs/heads/${ctx.diff_base}:refs/remotes/origin/${ctx.diff_base}`);
  if (!ctx.push_branch) {
    git('checkout', '--quiet', '--detach', baseSha);
  } else {
    const remoteSha = tryGit('fetch', '--no-tags', 'origin', `+refs/heads/${ctx.push_branch}:refs/remotes/origin/${ctx.push_branch}`) === null ? null : git('rev-parse', `refs/remotes/origin/${ctx.push_branch}`);
    git('checkout', '--quiet', '-B', ctx.push_branch, remoteSha ?? baseSha);
    if (remoteSha) git('config', 'gitea-agent.pushed', remoteSha);
    if (ctx.merge_from) startMerge(ctx);
  }
  fs.cpSync(gitDir, path.join(repoDir, '.git'), {recursive: true});
}

const withCoAuthor = (ctx, title) => (ctx.co_author ? `${title}\n\nCo-authored-by: ${ctx.co_author}` : title);
const isMerging = () => fs.existsSync(path.join(gitDir, 'MERGE_HEAD'));

// a merge run starts the merge itself so the result is a real merge commit, Codex only resolves the conflicts it leaves
function startMerge(ctx) {
  const from = `refs/remotes/origin/${ctx.merge_from}`;
  if (tryGit('rev-parse', '--verify', '--quiet', from) === null || tryGit('merge-base', '--is-ancestor', from, 'HEAD') !== null) return;
  const title = `Merge branch '${ctx.merge_from}' into ${ctx.push_branch}`;
  git('config', 'gitea-agent.merge-title', title);
  if (tryGit('merge', '--quiet', '--no-ff', '--no-commit', from) !== null) {
    git('commit', '--quiet', '--no-verify', '-m', withCoAuthor(ctx, title));
  }
}

// the merge is only committed once no conflict marker is left in the files that were in conflict
function assertConflictsResolved(conflicted) {
  const left = conflicted.filter((p) => {
    try {
      return /^(<{7}|>{7})( |$)/m.test(fs.readFileSync(path.join(repoDir, p), 'utf8'));
    } catch {
      return false; // deleted
    }
  });
  if (left.length) throw new Error(`conflicts are left unresolved in ${left.join(', ')}`);
}

// files the setup steps created, like dependencies, must never be committed by the agent
function excludeSetupLeftovers() {
  const untracked = (tryGit('ls-files', '--others', '--exclude-standard', '--directory') ?? '').split('\n').filter(Boolean);
  if (!untracked.length) return;
  const escaped = untracked.map((p) => `/${p.replace(/([*?[\\!#])/g, '\\$1')}`);
  fs.appendFileSync(path.join(gitDir, 'info', 'exclude'), `\n# setup steps of the agent job\n${escaped.join('\n')}\n`);
}

// the dependencies Codex installs are not committed either: only next to the manifest of their package manager and
// unless the repository tracks files in them, Go's vendor directory is deliberate
const dependencyManifests = {'package.json': 'node_modules', 'composer.json': 'vendor', 'pyproject.toml': '.venv', 'requirements.txt': '.venv'};
const excludedDependencies = [];
function excludeDependencies() {
  const dependencyDirs = new Set(Object.values(dependencyManifests));
  const manifests = (tryGit('ls-files', '--cached', '--others', '--exclude-standard', '--', ...Object.keys(dependencyManifests).map((m) => `:(glob)**/${m}`)) ?? '').split('\n');
  const fresh = [];
  for (const manifest of manifests) {
    const parts = manifest.split('/');
    const deps = dependencyManifests[parts.at(-1)];
    if (!deps || parts.slice(0, -1).some((part) => dependencyDirs.has(part))) continue;
    const dir = [...parts.slice(0, -1), deps].join('/');
    if (fresh.includes(dir) || !fs.lstatSync(path.join(repoDir, dir), {throwIfNoEntry: false})?.isDirectory()) continue;
    if (tryGit('check-ignore', '-q', '--', dir) !== null || tryGit('ls-files', '--', `${dir}/`)) continue; // ignored already, or tracked
    fresh.push(dir);
  }
  if (!fresh.length) return;
  excludedDependencies.push(...fresh);
  console.log(`Not committing the dependencies in ${fresh.join(', ')}`);
  fs.appendFileSync(path.join(gitDir, 'info', 'exclude'), `\n# dependencies\n${fresh.map((dir) => `/${dir.replace(/([*?[\\!#])/g, '\\$1')}/`).join('\n')}\n`);
}

const dependenciesNote = () => (excludedDependencies.length ? `\n\n_Dependencies left out of the commits: ${excludedDependencies.map((dir) => `\`${dir}/\``).join(', ')}._` : '');

function codexEnv(user) {
  const env = {...process.env, ...user?.exports, CODEX_HOME: codexHome};
  if (user) {
    env.HOME = user.home;
    const proxy = `http://127.0.0.1:${proxyPort}`;
    Object.assign(env, {HTTP_PROXY: proxy, HTTPS_PROXY: proxy, http_proxy: proxy, https_proxy: proxy});
    env.NO_PROXY = env.no_proxy = 'localhost,127.0.0.1'; // Gitea and the model too go through the proxy, the only way out
  }
  for (const name of Object.keys(env)) {
    if (/^(GITEA_AGENT_TOKEN|GITEA_AGENT_GIT_TOKEN|GITEA_TOKEN|GITHUB_TOKEN|ACTIONS_.*TOKEN.*|ACTIONS_ID_TOKEN_REQUEST_URL)$/.test(name)) delete env[name];
  }
  return env;
}

function ensureCodex(version) {
  const prefix = path.join(stateDir, `codex-${version}`);
  process.env.PATH = `${path.join(prefix, 'bin')}${path.delimiter}${process.env.PATH}`;
  try {
    execFileSync('codex', ['--version'], {stdio: 'ignore'});
    return;
  } catch {}
  console.log(`Installing @openai/codex@${version}`);
  execFileSync('npm', ['install', '--global', '--prefix', prefix, '--no-audit', '--no-fund', `@openai/codex@${version}`], {cwd: stateDir, stdio: 'inherit'});
}

function writeCodexConfig(ctx) {
  const toml = (v) => JSON.stringify(v); // JSON strings, numbers and string arrays are valid TOML values
  const lines = [
    `model = ${toml(ctx.model)}`,
    ...(ctx.reasoning_effort ? [`model_reasoning_effort = ${toml(ctx.reasoning_effort)}`] : []),
    'model_provider = "gitea"',
    'approval_policy = "never"',
    'sandbox_mode = "danger-full-access"', // the Actions job container is the sandbox
    'web_search = "disabled"', // a hosted search would reach the network past the firewall, Gitea drops it
    '',
    '[model_providers.gitea]',
    `name = ${toml(ctx.provider_name)}`,
    `base_url = ${toml(ctx.base_url)}`,
    'env_key = "GITEA_AGENT_API_KEY"',
    'wire_api = "responses"',
    '',
    '[shell_environment_policy]',
    'inherit = "all"',
    'exclude = ["GITEA_AGENT_*", "GITEA_TOKEN", "GITHUB_TOKEN", "ACTIONS_*"]',
    '',
    // nothing of a run leaves the job but the model requests: no usage analytics, no feedback, no OpenTelemetry export
    '[analytics]',
    'enabled = false',
    '',
    '[feedback]',
    'enabled = false',
    '',
    '[otel]',
    'exporter = "none"',
    'metrics_exporter = "none"',
    'trace_exporter = "none"',
  ];
  for (const [name, server] of Object.entries(ctx.mcp_servers ?? {})) {
    lines.push('', `[mcp_servers.${toml(name)}]`);
    for (const [key, value] of Object.entries(server)) {
      if (key === 'env') {
        lines.push(`env = { ${Object.entries(value).map(([k, v]) => `${toml(k)} = ${toml(v)}`).join(', ')} }`);
      } else {
        lines.push(`${key} = ${toml(value)}`);
      }
    }
  }
  fs.writeFileSync(path.join(codexHome, 'config.toml'), `${lines.join('\n')}\n`);
}

// system tools like iptables and useradd live in sbin directories some job environments leave out of PATH
const systemPath = `${process.env.PATH}:/usr/local/sbin:/usr/sbin:/sbin`;

function run(cmd, args) {
  return execFileSync(cmd, args, {encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'], env: {...process.env, PATH: systemPath}}).trim();
}

const proxyPort = 3128;

function hostAllowed(host, patterns) {
  host = host.toLowerCase().replace(/\.$/, '');
  return patterns.some((p) => (p.startsWith('*.') ? host.endsWith(p.slice(1)) : host === p));
}

// the server name of a TLS ClientHello split in any records: '' without one, null until it is complete, throws on anything else
function clientHelloServerName(data) {
  let handshake = Buffer.alloc(0);
  for (let offset = 0; ;) {
    if (data.length < offset + 5) return null;
    if (data[offset] !== 0x16) throw new Error('not a TLS handshake');
    const end = offset + 5 + data.readUInt16BE(offset + 3);
    if (data.length < end) return null;
    handshake = Buffer.concat([handshake, data.subarray(offset + 5, end)]);
    offset = end;
    if (handshake.length < 4) continue;
    if (handshake[0] !== 0x01) throw new Error('not a ClientHello');
    const length = handshake.readUIntBE(1, 3);
    if (handshake.length >= 4 + length) return helloServerName(handshake.subarray(4, 4 + length));
  }
}

function helloServerName(hello) {
  let p = 2 + 32; // version and random
  const vector = (lengthSize, limit = hello.length) => {
    if (p + lengthSize > limit) throw new Error('truncated ClientHello');
    const start = p + lengthSize;
    p = start + hello.readUIntBE(p, lengthSize);
    if (p > limit) throw new Error('truncated ClientHello');
    return [start, p];
  };
  vector(1); // session id
  vector(2); // cipher suites
  vector(1); // compression methods
  if (p === hello.length) return '';
  const [extensionsStart, extensionsEnd] = vector(2);
  for (p = extensionsStart; p < extensionsEnd;) {
    if (p + 2 > extensionsEnd) throw new Error('truncated ClientHello');
    const type = hello.readUInt16BE(p);
    p += 2;
    const [start, end] = vector(2, extensionsEnd);
    if (type !== 0) continue; // server_name
    p = start;
    const [namesStart, namesEnd] = vector(2, end);
    for (p = namesStart; p < namesEnd;) {
      const nameType = hello[p++];
      const [nameStart, nameEnd] = vector(2, namesEnd);
      if (nameType === 0) return hello.toString('latin1', nameStart, nameEnd);
    }
    throw new Error('no host name in the server name extension');
  }
  return '';
}

const normalizeHost = (host) => host.toLowerCase().replace(/\.$/, '');

// the proxy lets Codex reach the allowed hosts by name, whatever their addresses, so CDNs keep working;
// the endpoints only on their own ports. It checks the host a request really reaches, so an allowed host sharing a CDN
// address with another cannot front for it.
function startProxy(allowed, endpoints, onBlocked) {
  const portAllowed = (host, port) => endpoints.get(host.toLowerCase())?.has(port) || (['80', '443'].includes(port) && hostAllowed(host, allowed));
  const deny = (host, message) => {
    onBlocked(host, message);
    return `the agent firewall blocks ${host}`;
  };
  const server = http.createServer((req, res) => {
    req.socket.on('error', () => req.socket.destroy());
    let target;
    try {
      target = new URL(req.url);
    } catch {
      res.writeHead(400).end();
      return;
    }
    if (target.protocol !== 'http:' || !portAllowed(target.hostname, target.port || '80')) {
      res.writeHead(403).end(`${deny(target.hostname)}\n`);
      return;
    }
    const headers = {...req.headers, host: target.host};
    const upstream = http.request({host: target.hostname, port: target.port || 80, path: target.pathname + target.search, method: req.method, headers}, (up) => {
      res.writeHead(up.statusCode, up.headers);
      up.pipe(res);
    });
    upstream.on('error', () => res.destroy());
    req.pipe(upstream);
  });
  // a client or an upstream hanging up must never take the driver down
  server.on('clientError', (_err, socket) => socket.destroy());
  server.on('connect', (req, socket, head) => {
    socket.on('error', () => socket.destroy());
    const [host, port = '443'] = req.url.split(':');
    if (!portAllowed(host, port)) {
      socket.end(`HTTP/1.1 403 Forbidden\r\n\r\n${deny(host)}\n`);
      return;
    }
    // tunnels only carry TLS, to the server named in the CONNECT: the client's first bytes are held until its ClientHello is complete
    socket.write('HTTP/1.1 200 Connection Established\r\n\r\n');
    let received = head;
    const timer = setTimeout(() => socket.destroy(), 10 * 1000);
    const check = () => {
      let serverName;
      try {
        serverName = clientHelloServerName(received);
      } catch {
        serverName = undefined;
      }
      if (serverName === null && received.length <= 64 * 1024) return;
      socket.pause();
      socket.off('data', onData);
      clearTimeout(timer);
      const expected = net.isIP(host) ? '' : normalizeHost(host);
      if (typeof serverName !== 'string' || normalizeHost(serverName) !== expected) {
        deny(host, `The agent firewall closed a connection to ${host} that named another server, ${serverName || 'none'}, or was not TLS`);
        socket.destroy();
        return;
      }
      const upstream = net.connect(Number(port), host, () => {
        upstream.write(received);
        upstream.pipe(socket);
        socket.pipe(upstream);
      });
      upstream.on('error', () => socket.destroy());
      socket.on('close', () => upstream.destroy());
    };
    const onData = (chunk) => {
      received = Buffer.concat([received, chunk]);
      check();
    };
    socket.on('data', onData);
    socket.on('close', () => clearTimeout(timer));
    if (received.length) check();
  });
  return new Promise((resolve) => server.listen(proxyPort, '127.0.0.1', resolve));
}

// behind the firewall, Codex and the code it runs only reach the proxy: Gitea, the model endpoint and the MCP servers
// on their ports, FIREWALL_ALLOWED_HOSTS on 80 and 443; they cannot resolve names, so no DNS query can carry a token out
async function applyFirewall(ctx, onBlocked) {
  const endpoints = new Map(); // host -> ports
  const addURL = (raw) => {
    try {
      const u = new URL(raw);
      const port = u.port || (u.protocol === 'http:' ? '80' : '443');
      endpoints.set(u.hostname, new Set([...(endpoints.get(u.hostname) ?? []), port]));
    } catch {}
  };
  addURL(api);
  addURL(ctx.clone_url);
  addURL(ctx.base_url);
  // a repository's MCP servers only reach FIREWALL_ALLOWED_HOSTS through the proxy, its writers must not open the firewall to their own hosts
  for (const [name, server] of Object.entries(ctx.mcp_servers ?? {})) {
    if (server.url && (ctx.trusted_mcp_servers ?? []).includes(name)) addURL(server.url);
  }

  const notRoot = ['-m', 'owner', '!', '--uid-owner', '0'];
  const base = (ipv4) => [
    ['-F', 'OUTPUT'],
    ['-A', 'OUTPUT', '-o', 'lo', '-p', 'udp', '--dport', '53', ...notRoot, '-j', 'DROP'],
    ['-A', 'OUTPUT', '-o', 'lo', '-p', 'tcp', '--dport', '53', ...notRoot, '-j', 'DROP'],
    // Docker's resolver 127.0.0.11 listens on a random port its NAT rules redirect port 53 to
    ...(ipv4 ? [['-A', 'OUTPUT', '-o', 'lo', '-d', '127.0.0.1', '-j', 'ACCEPT'], ['-A', 'OUTPUT', '-o', 'lo', '-d', '127.0.0.0/8', ...notRoot, '-j', 'DROP']] : []),
    ['-A', 'OUTPUT', '-o', 'lo', '-j', 'ACCEPT'],
    ['-A', 'OUTPUT', '-m', 'owner', '--uid-owner', '0', '-j', 'ACCEPT'], // the driver and its proxy
  ];
  const pinned = [];
  for (const host of endpoints.keys()) {
    if (/^[\d.]+$/.test(host)) continue;
    try {
      pinned.push(...(await dns.lookup(host, {family: 4, all: true})).map((a) => `${a.address} ${host}`));
    } catch {
      console.warn(`Firewall: cannot resolve ${host}`);
    }
  }
  // the proxy keeps reaching the endpoints at the addresses they had when the run started
  fs.appendFileSync('/etc/hosts', `\n# gitea agent firewall\n${pinned.join('\n')}\n`);
  await startProxy(ctx.firewall_allowed_hosts ?? [], endpoints, onBlocked);
  for (const rule of [...base(true), ['-P', 'OUTPUT', 'DROP']]) run('iptables', rule);
  try {
    for (const rule of [...base(false), ['-P', 'OUTPUT', 'DROP']]) run('ip6tables', rule);
  } catch (err) {
    const ipv6 = fs.existsSync('/proc/net/if_inet6') && fs.readFileSync('/proc/net/if_inet6', 'utf8').split('\n').some((line) => line && !line.trim().endsWith(' lo'));
    if (ipv6) throw new Error(`the agent firewall cannot filter IPv6: ${err.message}`);
  }
  console.log(`Firewall: through the proxy to ${[...endpoints.keys(), ...(ctx.firewall_allowed_hosts ?? [])].join(', ')}`);
}

// behind the firewall Codex runs unprivileged, so it cannot lift the firewall nor read the driver's tokens
function prepareAgentUser() {
  try {
    run('id', ['-u', agentUser]);
  } catch {
    run('useradd', ['--create-home', '--shell', '/bin/bash', agentUser]);
  }
  const uid = Number(run('id', ['-u', agentUser]));
  const gid = Number(run('id', ['-g', agentUser]));
  run('chown', ['-R', `${uid}:${gid}`, ...[repoDir, codexHome].filter((dir) => fs.existsSync(dir))]);
  return {uid, gid, home: run('sh', ['-c', `getent passwd ${agentUser} | cut -d: -f6`])};
}

function prepareSetup() {
  if (path.join(stateDir, 'setup-shell') !== setupShellPath) throw new Error(`isolated setup steps need a writable ${path.dirname(setupShellPath)}`);
  const user = prepareAgentUser();
  fs.mkdirSync(setupDir, {mode: 0o700});
  fs.chownSync(setupDir, user.uid, user.gid);
  fs.writeFileSync(setupShellPath, setupShell, {mode: 0o755});
}

// a process the setup steps left must not use the network before the firewall, nor read the restored conversation
function killSetupLeftovers() {
  try {
    run('runuser', ['-u', agentUser, '--', 'sh', '-c', 'kill -KILL -1']);
  } catch {} // kill reports an error as it kills itself too
}

// read without following the links nor opening the pipes the unprivileged user may have left there
function readSetupFile(name, uid) {
  let fd;
  try {
    fd = fs.openSync(path.join(setupDir, name), fs.constants.O_RDONLY | fs.constants.O_NOFOLLOW | fs.constants.O_NONBLOCK);
  } catch {
    return '';
  }
  try {
    const stat = fs.fstatSync(fd);
    return stat.isFile() && stat.uid === uid && stat.size <= 1024 * 1024 ? fs.readFileSync(fd, 'utf8') : '';
  } finally {
    fs.closeSync(fd);
  }
}

// the variables and PATH entries the setup steps exported with GITHUB_ENV and GITHUB_PATH
function readSetupExports(uid) {
  const env = {};
  const lines = readSetupFile('env', uid).split('\n');
  for (let i = 0; i < lines.length; i++) {
    const heredoc = /^([A-Za-z_]\w*)<<(.+)$/.exec(lines[i]);
    if (heredoc) {
      const end = lines.indexOf(heredoc[2], i + 1);
      if (end < 0) break;
      env[heredoc[1]] = lines.slice(i + 1, end).join('\n');
      i = end;
    } else {
      const pair = /^([A-Za-z_]\w*)=(.*)$/.exec(lines[i]);
      if (pair) env[pair[1]] = pair[2];
    }
  }
  for (const name of Object.keys(env)) {
    if (name.startsWith('GITEA_AGENT_') || name === 'CODEX_HOME') delete env[name];
  }
  const paths = readSetupFile('path', uid).split('\n').map((p) => p.trim()).filter(Boolean).reverse();
  if (paths.length) env.PATH = [...paths, env.PATH ?? process.env.PATH].join(path.delimiter);
  return env;
}

// only the sessions directory is restored: the previous run may be another user's, who must not set this run's config
async function restoreState() {
  const resp = await call('GET', '/run/state');
  const archive = path.join(stateDir, 'state.tar.gz');
  fs.writeFileSync(archive, Buffer.from(await resp.arrayBuffer()));
  execFileSync('tar', ['-xzf', archive, '-C', codexHome, '--no-same-owner', 'sessions']);
}

async function saveState() {
  if (!fs.existsSync(path.join(codexHome, 'sessions'))) return;
  const archive = path.join(stateDir, 'state-out.tar.gz');
  execFileSync('tar', ['-czf', archive, '-C', codexHome, 'sessions']);
  await call('PUT', '/run/state', fs.readFileSync(archive), {'Content-Type': 'application/gzip'});
}

// Codex only reports the paths it changed, the diff makes the session log reviewable
function attachDiff(event) {
  const item = event.item;
  if (event.type !== 'item.completed' || item?.type !== 'file_change' || !Array.isArray(item.changes)) return;
  const paths = item.changes.map((change) => change.path).filter(Boolean);
  for (const p of paths) tryGit('add', '--intent-to-add', '--', p); // shows new files in the diff
  const diff = tryGit('diff', '--no-color', 'HEAD', '--', ...paths) ?? '';
  item.diff = diff.length > maxOutput * 2 ? `${diff.slice(0, maxOutput * 2)}\n…` : diff;
}

// Codex often edits files through shell commands, which report no file change: the driver reports them itself
let lastWorkingDiff = '';
function workingTreeChange(event) {
  if (event.type !== 'item.completed' || event.item?.type !== 'command_execution') return null;
  tryGit('add', '--intent-to-add', '--all');
  const diff = tryGit('diff', '--no-color', 'HEAD') ?? '';
  if (!diff || diff === lastWorkingDiff) return null;
  lastWorkingDiff = diff;
  const changes = (tryGit('diff', '--name-status', 'HEAD') ?? '').split('\n').filter(Boolean).map((line) => {
    const [status, p] = line.split('\t');
    return {path: p, kind: {A: 'add', D: 'delete'}[status[0]] ?? 'update'};
  });
  return {
    type: 'item.completed',
    item: {id: `${event.item.id}-changes`, type: 'file_change', status: 'completed', changes, diff: diff.length > maxOutput * 2 ? `${diff.slice(0, maxOutput * 2)}\n…` : diff},
  };
}

function trimItem(event) {
  const item = event.item;
  if (item && typeof item.aggregated_output === 'string' && item.aggregated_output.length > maxOutput) {
    item.aggregated_output = `…${item.aggregated_output.slice(-maxOutput)}`;
  }
  return event;
}

async function codexRun(ctx) {
  prepare(ctx);
  excludeSetupLeftovers();
  const baseSha = git('config', '--get', 'gitea-agent.base');
  let pushedSha = tryGit('config', '--get', 'gitea-agent.pushed');
  const startSha = pushedSha ?? baseSha; // what Codex reads, before a merge run's own merge commit

  if (ctx.isolate_setup) killSetupLeftovers();
  fs.mkdirSync(codexHome, {recursive: true, mode: 0o700});
  ensureCodex(ctx.codex_version);
  let resume = ctx.resume;
  if (resume) {
    try {
      await restoreState();
    } catch (err) {
      console.warn(`Cannot restore the Codex conversation, starting a new one: ${err.message}`);
      resume = false;
    }
  }
  writeCodexConfig(ctx);
  let progress = '';
  let todos = [];
  let lastMessage = '';
  let lastError = '';
  let turnFailed = false;
  let dirty = false;
  let lastCheckpoint = Date.now();
  let lastPublishedProgress = '';
  const pending = [];

  const flush = async () => {
    if (!pending.length) return;
    const lines = redact(pending.splice(0).map((e) => JSON.stringify(e)).join('\n'));
    await call('POST', '/run/events', lines, {'Content-Type': 'application/x-ndjson'});
  };
  const flushTimer = setInterval(() => flush().catch((err) => console.warn(`events: ${err.message}`)), 1000);

  let user = null;
  if (ctx.firewall) {
    const blocked = new Set();
    // a blocked host is reported once in the session log, so a failing install is easy to explain
    await applyFirewall(ctx, (host, message) => {
      if (blocked.has(host)) return;
      blocked.add(host);
      pending.push({type: 'error', message: message ?? `The agent firewall blocked ${host}, an administrator can add it to [agents] FIREWALL_ALLOWED_HOSTS`});
    });
    user = prepareAgentUser();
    if (ctx.isolate_setup) user.exports = readSetupExports(user.uid);
  }

  const hasChanges = () => (tryGit('status', '--porcelain') ?? '') !== '';
  // uncommitted changes, or commits Codex made itself
  const hasUnpushed = () => hasChanges() || tryGit('rev-parse', 'HEAD') !== (pushedSha ?? baseSha);
  let pushedThisRun = false;

  // a commit must never publish the API key or the bot's token, whatever the code Codex ran wrote to the workspace
  const assertNoSecret = (range) => {
    const diff = tryGit('diff', '--no-color', ...range) ?? '';
    if ([apiKey, gitToken].some((secret) => secret?.length >= 8 && diff.includes(secret))) {
      throw new Error('refusing to publish changes containing a secret of the job');
    }
  };

  // checkpoints are serialized so a slow push never overlaps the next one
  let chain = Promise.resolve();
  const checkpoint = (force, final = false) => {
    chain = chain.then(async () => {
      // the first changes of a run are published at once, later ones at most every interval
      const firstChanges = ctx.push_branch && !pushedThisRun && hasUnpushed();
      if (!force && !firstChanges && Date.now() - lastCheckpoint < ctx.checkpoint_interval * 1000) return;
      lastCheckpoint = Date.now();
      const merging = isMerging();
      if (ctx.push_branch && (!merging || final)) { // a merge in progress is committed and pushed at the end only
        excludeDependencies();
        if (hasChanges() || merging) {
          const conflicted = merging ? (tryGit('diff', '--name-only', '--diff-filter=U') ?? '').split('\n').filter(Boolean) : [];
          git('add', '--all');
          try {
            assertNoSecret(['--cached']);
            if (merging) assertConflictsResolved(conflicted);
          } catch (err) {
            if (!merging) git('reset', '--quiet'); // a reset would abort the merge
            throw err;
          }
          // a checkpoint is named after the step of Codex's plan it completes, the last commit after the request
          const done = todos.filter((todo) => todo.completed).at(-1)?.text?.trim();
          let title = !final && done ? (done.length > 72 ? `${done.slice(0, 71)}…` : done) : ctx.commit_title;
          if (merging) title = tryGit('config', '--get', 'gitea-agent.merge-title') ?? title;
          git('commit', '--quiet', '--no-verify', '-m', withCoAuthor(ctx, title));
        }
        dirty = false;
        const head = git('rev-parse', 'HEAD');
        if (head !== pushedSha && head !== baseSha) {
          assertNoSecret([`${pushedSha ?? baseSha}..${head}`]); // Codex may have committed by itself
          if (tryGit('push', '--quiet', 'origin', `HEAD:refs/heads/${ctx.push_branch}`) === null) {
            // someone else pushed to the branch meanwhile, replay our commits on top of it
            git('pull', '--quiet', ctx.merge_from ? '--no-rebase' : '--rebase', 'origin', ctx.push_branch); // a rebase would flatten the merge
            git('push', '--quiet', 'origin', `HEAD:refs/heads/${ctx.push_branch}`);
          }
          pushedSha = git('rev-parse', 'HEAD');
          pushedThisRun = true;
        }
      }
      if (final || progress !== lastPublishedProgress || ctx.push_branch) {
        lastPublishedProgress = progress;
        await postJSON(final ? '/run/publish?final=1' : '/run/publish', {progress, summary: lastMessage + dependenciesNote(), head: startSha});
      }
    }).catch((err) => {
      console.warn(`checkpoint: ${err.message}`);
      lastError = err.message;
    });
    return chain;
  };

  // Codex may think for minutes after editing, push what is pending without waiting for its next event
  const checkpointTimer = ctx.push_branch ? setInterval(() => {
    if (hasUnpushed()) checkpoint(false);
  }, ctx.checkpoint_interval * 1000) : null;

  const optionArgs = ['--json', '--skip-git-repo-check', '-C', repoDir];
  const runCodex = (args) => new Promise((resolve) => {
    const child = spawn('codex', args, {
      cwd: repoDir,
      env: codexEnv(user),
      stdio: ['ignore', 'pipe', 'pipe'],
      ...(user ? {uid: user.uid, gid: user.gid} : {}),
    });
    const stop = () => child.kill('SIGTERM');
    process.once('SIGTERM', stop);
    process.once('SIGINT', stop);
    let stderr = '';
    let events = 0;
    child.stderr.on('data', (chunk) => {
      process.stderr.write(chunk);
      stderr = (stderr + chunk).slice(-4096);
    });
    readline.createInterface({input: child.stdout}).on('line', (line) => {
      let event;
      try {
        event = JSON.parse(line);
      } catch {
        return;
      }
      events++;
      attachDiff(event);
      pending.push(trimItem(event));
      const change = workingTreeChange(event);
      if (change) pending.push(change);
      const item = event.item;
      if (event.type === 'turn.failed') {
        turnFailed = true;
        lastError = event.error?.message ?? 'the Codex turn failed';
      } else if (event.type === 'error') {
        lastError = event.message ?? 'Codex reported an error';
      }
      if (!item) return;
      if (item.type === 'todo_list' && Array.isArray(item.items)) {
        todos = item.items;
        progress = todos.map((todo) => `- [${todo.completed ? 'x' : ' '}] ${todo.text}`).join('\n');
      }
      if (event.type !== 'item.completed') return;
      if (item.type === 'agent_message') lastMessage = item.text ?? lastMessage;
      if (item.type === 'file_change') dirty = true;
      if (item.type === 'file_change' || item.type === 'command_execution' || item.type === 'todo_list') {
        if (ctx.push_branch && hasUnpushed()) dirty = true;
        if (dirty || progress !== lastPublishedProgress) checkpoint(false);
      }
    });
    child.on('close', (code) => {
      process.off('SIGTERM', stop);
      process.off('SIGINT', stop);
      resolve({code, stderr, events});
    });
  });

  let result;
  if (resume) {
    result = await runCodex(['exec', ...optionArgs, 'resume', ctx.thread_id, ctx.prompt]);
    if (result.code !== 0 && result.events === 0) {
      console.warn('Cannot resume the Codex conversation, starting a new one');
      result = await runCodex(['exec', ...optionArgs, ctx.fresh_prompt]);
    }
  } else {
    result = await runCodex(['exec', ...optionArgs, ctx.prompt]);
  }

  let success = result.code === 0 && !turnFailed;
  clearInterval(checkpointTimer);
  lastError = success ? '' : lastError;
  await checkpoint(true, true);
  let summary = lastMessage + dependenciesNote();
  if (!ctx.push_branch && hasChanges()) {
    summary += `\n\n_The changes made in the working copy were discarded: ${ctx.read_only_reason}._`;
  }
  if (success && lastError) success = false; // the final changes could not be published
  clearInterval(flushTimer);
  await flush();
  try {
    await saveState();
  } catch (err) {
    console.warn(`Cannot save the Codex conversation: ${err.message}`);
  }
  const error = success ? '' : (lastError || result.stderr.trim().split('\n').slice(-5).join('\n') || `codex exited with code ${result.code}`);
  await postJSON('/run/finish', {status: success ? 'success' : 'failure', summary, error});
  return success;
}

async function main(mode) {
  if (!api || !token) throw new Error('GITEA_AGENT_API and GITEA_AGENT_TOKEN are required');
  const ctx = await (await call('GET', '/run')).json();
  if (!gitToken || (mode === 'run' && !apiKey)) {
    throw new Error('no API key was provided to this job: the trigger user removed their key, the agent was deleted, or this is a rerun of an agent run');
  }
  setGitCredentials(ctx.commit_name);
  if (mode === 'prepare') {
    prepare(ctx);
    if (ctx.isolate_setup) prepareSetup();
    return true;
  }
  return codexRun(ctx);
}

main(process.argv[2] ?? 'run').then((ok) => process.exit(ok ? 0 : 1), async (err) => {
  console.error(err);
  try {
    await postJSON('/run/finish', {status: 'failure', error: String(err.message ?? err)});
  } catch {}
  process.exit(1);
});
