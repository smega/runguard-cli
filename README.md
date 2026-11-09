# runguard-cli

[![CI](https://github.com/smega/runguard-cli/actions/workflows/ci.yml/badge.svg)](https://github.com/smega/runguard-cli/actions/workflows/ci.yml)
[![Release](https://github.com/smega/runguard-cli/actions/workflows/release.yml/badge.svg)](https://github.com/smega/runguard-cli/actions/workflows/release.yml)

The open-source client for RunGuard — cron/heartbeat job monitoring, point
it at wherever your own RunGuard instance is hosted. This repository is the
entire client: two small Go files,
zero third-party dependencies, nothing hidden. If you're about to point a
binary at your cron jobs, you should be able to read exactly what it does
before you trust it. That's what this repo is for.

The server it talks to is closed-source, but the agent doesn't need to be —
this is the part that runs on your machines, so this is the part you get to
audit.

## Install

**Build from source** — it's stdlib-only, so all you need is Go 1.24+:

```sh
git clone https://github.com/smega/runguard-cli.git
cd runguard-cli
go build -o runguard .
```

**Or grab a prebuilt binary from a [GitHub Release](https://github.com/smega/runguard-cli/releases)**.
Every tagged release is built directly from this source by the `release.yml`
GitHub Actions workflow in this repo (not on a private machine), with a
`.sha256` checksum published alongside each binary — so the build is
reproducible and independently verifiable, not just something you have to
take our word for:

```sh
curl -fsSL https://github.com/smega/runguard-cli/releases/latest/download/runguard-linux-amd64 -o runguard
curl -fsSL https://github.com/smega/runguard-cli/releases/latest/download/runguard-linux-amd64.sha256 -o runguard.sha256
sha256sum -c runguard.sha256
chmod +x runguard
sudo mv runguard /usr/local/bin/runguard
```

(Swap `linux-amd64` for `linux-arm64`, `darwin-amd64`, or `darwin-arm64` as needed.)

Once installed, `runguard self-update` keeps it current by pulling new
releases from this same GitHub repo — no separate download host involved.

## Usage

```sh
# Wrap any command. Exit code and last output are heartbeated back to RunGuard.
runguard wrap --key=YOUR_HEARTBEAT_KEY -- /opt/scripts/nightly_backup.sh

# Retry a flaky job up to 2 extra times before reporting failure.
runguard wrap --key=YOUR_HEARTBEAT_KEY --retry=2 -- /opt/scripts/flaky_sync.sh

# Register a new job from a script/provisioning tool instead of the UI.
runguard register --token=$RUNGUARD_AGENT_TOKEN --name="orders sync" --interval=60

# Update in place.
runguard self-update
```

Run `runguard` with no arguments for the full flag reference.

## What data this sends

This is a complete accounting, not a marketing summary — if something's
missing here, consider it a bug in this doc.

There are two commands that talk to the network: `wrap` and `register`.
`self-update` also makes network calls (to fetch a new binary); that's
covered separately at the bottom.

### `runguard wrap -- your-command`

Every time a wrapped command finishes, the CLI sends one HTTP POST to your
job's heartbeat URL (`https://<your-runguard-host>/hb/<key>`, or wherever
`--endpoint`/`RUNGUARD_ENDPOINT` points). No authentication beyond the
heartbeat key itself is required for this request — that's unchanged from
plain `curl`-based heartbeats.

The request body contains:

- `status` — `"ok"` or `"fail"`, derived from the command's exit code.
- `exit_code` — the wrapped command's actual exit code.
- `duration_ms` — how long the command (including retries, if any) took to run.
- `env` — only if you passed `--env=`. An arbitrary label you chose (e.g. `"prod"`); not auto-detected.
- `tags` — only if you passed `--tag=`, plus an automatic `retried:N` tag if `--retry` caused extra attempts.
- `agent_version` — the CLI's own version string (e.g. `"0.1.4"`), so RunGuard can flag jobs on old/unsupported agent versions.
- `stdout_tail` / `stderr_tail` — the **last `--truncate` bytes** (default 4000) of the command's stdout and stderr, captured separately. This is genuinely the tail end of whatever your command printed — nothing is added, redacted, or filtered by the CLI itself. If your command prints secrets, those secrets end up in this field. The server additionally caps how much of this it stores; the CLI-side `--truncate` is a client-side cap on top of that.
- `ts` — a UTC timestamp taken right after the command finishes.

What it does **not** send: no hostname, no environment variables (other than
the one string you explicitly pass via `--env`), no file paths, no command
line/arguments, no network configuration, no OS/user info. The server does
see your request's source IP as part of ordinary HTTP, the same as any web
request — that's not something the CLI adds, it's just how HTTP works.

If the heartbeat POST itself fails (network error, non-2xx response), the
CLI logs that failure to its own stderr and exits with the wrapped command's
real exit code regardless — a broken connection to RunGuard never changes
whether your job is considered to have succeeded or failed for the purposes
of its own exit status.

### `runguard register`

Sends one HTTP POST to `<api-base>/api/agent/register`, authenticated with
`Authorization: Bearer <your-agent-token>` (the token from your account
page). Body:

- `name` — the `--name` you passed.
- `interval_minutes`, `grace_minutes`, `severity` — the values you passed (or their defaults).

That's the entire payload. It does not send the command you intend to wrap,
your token is never echoed back in the response, and repeated calls with the
same `--name` return the existing job instead of creating a new one (so
re-running a setup script is safe).

### Agent tokens

The agent token (visible/regenerable from your account page) is a bearer
credential scoped to one thing: creating/looking-up jobs via `register`. It
is not your login password and doesn't grant access to the web UI.
Regenerating it immediately invalidates the old value — any script still
using the old token will start getting 401s from `register` (existing
heartbeats using job keys are unaffected, since those don't use the agent
token at all).

### `runguard self-update`

Fetches release metadata and a binary from GitHub's public Releases API
(`api.github.com/repos/<repo>/releases/...`) over plain HTTPS GET requests
— no data about your machine is sent as part of this beyond what's
inherent in making an HTTP request (IP, User-Agent). The downloaded
binary's SHA256 checksum is verified before it replaces the currently
running binary; if no checksum is available or it doesn't match, the
update is refused rather than installed anyway.

## Why this is a separate, open repo

The RunGuard server (dashboards, alert routing, storage) stays
closed-source — that's the product. This CLI is just an HTTP client that
runs on your infrastructure, so there's no reason it should be a black box.
Keeping it in its own dependency-free module also means it's easy to read
end-to-end in one sitting, which is the actual goal here, more than
"open source" as a label.

Issues and PRs against the CLI itself (flags, retry behavior, platform
support) are welcome. This repo doesn't include the server, so
account/billing/dashboard questions belong on the main site instead.

## Releasing (maintainers)

Push a tag matching `v*.*.*` (e.g. `v0.1.5`) and the `release.yml` workflow
builds linux/darwin amd64/arm64 binaries with that version baked in, computes
a `.sha256` for each, and publishes them as a GitHub Release:

```sh
git tag v0.1.5
git push origin v0.1.5
```

`workflow_dispatch` (manual run from the Actions tab) builds the same
binaries as downloadable artifacts without creating a release — useful for
testing the build itself.

## License

MIT — see [LICENSE](LICENSE).
