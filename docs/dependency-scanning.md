# Dependency scanning

Dependency scanning checks the packages a repository depends on against an [OSV](https://osv.dev) vulnerability
database and raises an alert for each vulnerable one, like Dependabot alerts. Alerts are listed in the repository's
Security tab.

## Setup

The site administrator turns the feature on in `app.ini`:

```ini
[dependency_scanning]
ENABLED = true
; Base URL of the OSV API, e.g. a self-hosted mirror
OSV_URL = https://api.osv.dev
; Whether repositories are scanned until their administrators change it in their settings
ENABLE_BY_DEFAULT = false
```

Gitea reaches `OSV_URL` through the outgoing request policy of `[security] EGRESS_MODE`. A mirror on a private or
loopback address needs an entry in `[dependency_scanning] ALLOWED_HOST_LIST`, for example `private` or a CIDR.

Repository administrators then enable scanning in Settings → Security, unless `ENABLE_BY_DEFAULT` already
scans every repository.

## What is scanned

Alerts only cover the default branch, pull requests get a [review](#pull-request-review) instead. Only the files below are read. Files under `node_modules` and `vendor` are skipped.

| Ecosystem | Files                                                         |
|-----------|---------------------------------------------------------------|
| Go        | `go.mod`                                                      |
| npm       | `package-lock.json`, `yarn.lock`, `pnpm-lock.yaml`            |
| Packagist | `composer.lock`                                               |
| PyPI      | `requirements.txt` (pinned `==` versions), `Pipfile.lock`, `poetry.lock` |
| crates.io | `Cargo.lock`, crates from a registry                          |
| RubyGems  | `Gemfile.lock`, gems from a `GEM` source                      |

Lockfiles are read rather than manifests such as `package.json`, because they pin the versions a build really uses.
Up to `MAX_MANIFESTS` files are read per repository, and files larger than `MAX_MANIFEST_SIZE` are skipped. A scan or a
review fails when the files pin more than 20000 dependencies, or when it would fetch more than 5000 vulnerability
records for a scan, 500 for a review. Records are cached for an hour, as many repositories share them. Pull request
reviews run in their own `dependency_review` queue, so they never delay the scans of the `dependency_scanning` queue.

A repository is scanned:

- after each push to its default branch, and when its default branch changes;
- when scanning is enabled, or with Scan now in the Security tab;
- every night by the `[cron.dependency_scanning]` task, as new vulnerabilities are published.

An open Security tab reloads when a scan of its repository ends.

## Alerts

Each alert is one vulnerability in one package of one file. Its severity comes from the vulnerability database,
or else from its CVSS v3 score. Its fixed versions are those ending the affected range the version is in, so a
vulnerability fixed on several release lines only suggests the fix of the line in use. When a lockfile pins several
affected versions of the package, the alert names the lowest.

- **Open**: the dependency is affected.
- **Fixed**: a later scan no longer finds the affected version. If the version comes back, the alert reopens.
- **Dismissed**: a user with write access judged it not a risk and gave a reason. Scans keep it dismissed until
  someone reopens it.

The Security tab and its dependency list are visible to users with write access to the code. The Security tab of an
organization lists the open alerts of all its repositories whose code the member can write.

When scanning is disabled for a repository, its alerts are no longer updated: they are left out of the organization's
Security tab and of the API's alert list until scanning is enabled again, and the next scan brings them up to date.

When a scan opens an alert, or reopens one, the administrators of the repository get a notification if it reaches
the severity chosen in Settings → Security: critical by default, or high, moderate, low, every severity or none.

## Pull request review

When a pull request of a scanned repository is opened, pushed to or retargeted, Gitea compares the packages pinned by
its head with those of its merge base. Each added package version is checked against the vulnerability database, and
a `Dependency review` commit status is set on the head, pending while the review runs: it fails when the pull request
adds a vulnerable version and errors when the review fails. It is skipped when the pull request was closed or scanning
disabled meanwhile.
Versions already used by the merge base are not reported, they already have alerts.

In Settings → Security, administrators choose the least severe vulnerability failing the status, any by default: the
others are listed but the status succeeds. They can also list advisory IDs or aliases, such as `GHSA-…` or `CVE-…`,
that the reviews accept and leave out.

The status links to the list of the vulnerable versions found, visible to anyone who can read the pull request. Why a
review failed is only shown to users with write access to the code.
Branch protection can require the status to block merging.

## Fixing with an agent

When [agents](agents.md) are enabled for the repository, an open alert offers Fix with an agent. It starts an agent
session that upgrades the package to a fixed version, regenerates the lockfile with the package manager and opens a
pull request. The session runs with the API key and the permissions of the user who clicked it.
The alert closes on the next scan once the pull request is merged into the default branch.

## Dependency updates

Like Dependabot, an agent can keep the dependencies up to date. In **Settings → Security**, a repository
administrator picks the agent, the interval (daily, weekly or monthly), whether major versions are included, and
an ignore list, one entry per line, where `*` matches anything in a package name:

| Entry                          | Effect                                                                      |
|--------------------------------|-----------------------------------------------------------------------------|
| `@types/*`                     | The packages stay as they are.                                              |
| `react@no-major`               | The package stays within its major version, even when major versions are included. |
| `lodash@>=5`, `django@4.2.x`   | The updates never upgrade the package to these versions.                   |

Versions accept `<`, `<=`, `>`, `>=`, `=`, prefixes such as `5`, `5.2.x` or `5.*`, constraints joined by commas, and
alternatives joined by `||`. The updates run with the API key and permissions of the
administrator who last changed them: another administrator who changes them, or clicks **Update now**, takes them over
with their own key. They stop when that user no longer administers the repository, has no key for the agent anymore,
or when the agent is disabled or no longer allowed in the repository: Settings → Security then shows why the latest
updates failed, until they succeed or the settings change, and the updater gets a notification once per cause. They
pause while scanning is disabled.
**Update now** starts them at once, it is offered while scanning is enabled.

At each interval, checked hourly by the `dependency_updates` cron task, the agent gets one session per package
ecosystem the last scan found. It lists the outdated direct dependencies with the package manager, upgrades them,
regenerates the lockfiles, runs the tests and fixes what breaks, then opens a pull request listing the upgrades. The
vulnerable versions of the open alerts are upgraded first, even across a major version, but only to the first major
version fixing all their vulnerabilities when major versions are off. Ignored packages stay as they are, vulnerable or
not. While the
pull request is open, the next updates continue it: the agent brings it up to date with the default branch and adds the
new upgrades, and is told which packages its branch upgraded beyond what the settings allow. Only direct dependencies
are named, as declared by `go.mod` (without `// indirect`), `package.json`, `composer.json`, `requirements.txt`,
`Pipfile`, `pyproject.toml`, `Cargo.toml` or the `DEPENDENCIES` of `Gemfile.lock`, and only when no version of the
package is left on its allowed line: an indirect one follows the packages requiring it. Direct dependencies brought to
an ignored or refused version are named too. So that the agent does
not run for nothing, an open pull request is only continued when the default branch, the open alerts or the settings
changed since its last update, when that update failed or was cancelled, or when it is four weeks old, to catch new
releases; **Update now** always continues it. Once it is merged or closed, or the agent changes, the next updates open a new one. An update still
running when the next is due is not started twice, the following one catches up with what changed meanwhile.

The **Upgrades** setting can restrict the updates to security: the agent then only upgrades the packages of the open
alerts, to the lowest version fixing their vulnerabilities, and an ecosystem without such an alert gets no session.
With **Start the updates … as soon as a scan opens a high or critical alert**, a scan opening or reopening such an
alert starts the updates of its ecosystem at once, without waiting for the interval nor moving it. They go through the
same session as the scheduled updates, so the open update pull request is continued rather than duplicated, and an
alert already handled by a running update waits for it.

Closing an update pull request without merging it refuses its upgrades: the versions its branch brought the direct
dependencies to, compared with its merge base, are listed under **Refused upgrades** in Settings → Security and the
next updates never propose them again. Removing one there allows it again.

In an update pull request, administrators can add entries to the ignore list with comments such as
`@agent ignore lodash@>=5` or `@agent ignore react@no-major`, one per line, where `agent` is the update agent: the agent
replies instead of starting a run. When the updater sends the command, the update of the pull request starts at once
to bring these packages back, otherwise the next update does.

Behind the agents' [firewall](agents.md#network-firewall), the package registries the agent queries must be in
`[agents] FIREWALL_ALLOWED_HOSTS`, for example `registry.npmjs.org, proxy.golang.org, sum.golang.org, pypi.org,
files.pythonhosted.org, repo.packagist.org, index.crates.io, rubygems.org`.

## API

The API v1 follows the permissions of the web UI. Users who can write the code of a repository can:

- list its alerts with `GET /repos/{owner}/{repo}/dependency-alerts`, filtered by `state` and `severity`;
- dismiss an alert with a reason, or reopen it, with `PATCH /repos/{owner}/{repo}/dependency-alerts/{id}` and
  `{"state": "dismissed", "reason": "…"}` or `{"state": "open"}`;
- list the dependencies of the last scan with `GET /repos/{owner}/{repo}/dependencies`, filtered by `q`.

When agents are enabled, its administrators can read the [dependency updates](#dependency-updates) with
`GET /repos/{owner}/{repo}/dependency-updates`, change them with `PUT` on the same path, where omitted fields are kept,
and start them with `POST /repos/{owner}/{repo}/dependency-updates/run`. As in the web UI, changing them or starting
them makes the doer the updater, with their own API key; `take_over` does it without changing anything else, and
`allow_refused` lists refused versions to propose again.

## Limitations

- No comment summarizes the dependency review of a pull request, only its commit status links to the list, as there is
  no bot to post one.
- Each ecosystem gets one pull request with all its upgrades, they cannot be split into groups like Dependabot's.

## Audit

Enabling or disabling scanning, dismissing or reopening an alert, changing the dependency updates and taking them over
with one's own key are recorded in the audit log.
