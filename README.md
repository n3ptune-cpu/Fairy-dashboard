# Fairy

Homelab panel for Proxmox clusters, agent-monitored hosts, wall power (Shelly, Tasmota, Tuya 3.3/3.4/3.5, NUT,
Redfish) and UPSes, behind Pocket ID. Go rewrite of the Python prototype with the same JSON API; the web UI is
embedded in the binary.

> Status: the `fairy` panel is complete (feature parity with the prototype), and `fairy-agent`, the metrics agent
> for the machines it watches, is written (Windows and Linux; see [AGENT.md](AGENT.md)). The agent speaks the
> panel's existing agent contract, so the panel needed no change; `internal/agent/contract_test.go` runs the
> panel's real poller and web API against it. The agent's Windows code is compiled and unit-tested but not yet
> run on a real Windows PC (list in AGENT.md). A full README (nginx / MikroTik deployment, load testing) follows.

## Build

```sh
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(git describe --always)" -o fairy ./cmd/fairy
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o fairy-linux-arm64 ./cmd/fairy
go test ./... && go vet ./...

# the agent (one static binary per platform, no dependencies)
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.version=$(git describe --always)" -o fairy-agent-windows-amd64.exe ./cmd/fairy-agent
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "-s -w -X main.version=$(git describe --always)" -o fairy-agent-linux-arm64 ./cmd/fairy-agent
```

Static binary, no runtime dependencies besides `/etc/fairy/config.yaml` and `/etc/fairy/fairy.env`.

## Run

```sh
fairy serve [--config /etc/fairy/config.yaml] [--env-file /etc/fairy/fairy.env] [--listen 127.0.0.1:8080]
                              # listen: --listen, else $FAIRY_LISTEN, else `listen:` in config.yaml, else
                              # 127.0.0.1:8080. The env file is read first, so FAIRY_* settings in it count.
fairy serve --demo            # simulated data, NO login, refuses anything but 127.0.0.1 / ::1 (port 8090)
fairy serve --demo --demo-as viewer:Straw   # ...as a viewer of the Straw site sees it (or: pending, admin);
                              # the demo menu (top right) has a "Preview as" switcher too
fairy admin help              # break-glass CLI: users and sites, tokens, sessions, control kill switch, host
                              # power flags, polling, audit tail, config check, health (all audited as "cli")
fairy admin users set-role you@example.com admin   # locked out? make yourself admin again
fairy version
```

See `deploy/config.example.yaml`, `deploy/fairy.env.example` and `deploy/fairy.service` (hardened systemd unit,
no capabilities needed: the panel never runs local power or IPMI commands).

### Agents

Each watched machine runs `fairy-agent` (Windows service or systemd unit), reachable only over Tailscale on port
9101. [AGENT.md](AGENT.md) explains installing it on Windows (PowerShell, `deploy/install-fairy-agent.ps1`) and
Linux (`deploy/fairy-agent.service`), its settings (`deploy/fairy-agent.config.example.yaml`), how to add it to
the panel's `agents:`, and what it never touches on a gaming PC (no driver, no access to other programs'
memory). The contract, in short: `GET /metrics`, `GET /processes?limit=&sort=`, `GET /fan/status` with the read
token, `POST /fan/mode`, `/fan/duty`, `/power`, `/wol` with the separate control token, both as
`Authorization: Bearer`; every POST feature is off on the agent until its owner enables it.

### Custom logo

Copy your image to `/etc/fairy/logo.png` (PNG, JPEG, WebP or GIF, at most 1 MiB; SVG is refused because it can
carry script; make it readable by the service, e.g. `0644`), set `branding: {logo: /etc/fairy/logo.png}` in
config.yaml and restart Fairy. It replaces the built-in logo in the sidebar and on the sign-in and waiting
pages, and becomes the favicon. A missing or invalid file only logs a warning. To try one first:
`fairy serve --demo --logo C:\path\to\logo.png` (Windows) or `fairy serve --demo --logo ./logo.png`.
The file is read once at start (path from the config only) and served at `/branding/logo` without sign-in.

### Behind nginx in another container (and Cloudflare)

`deploy/nginx.example.conf` is a complete server block. In short:

* Fairy: `listen: <this LXC's LAN IP>:8080` and `trusted_proxies: ["<nginx IP>/32"]` in config.yaml; firewall
  port 8080 so only nginx reaches it (nftables in the LXC, or `IPAddressAllow=` in the unit). Fairy warns at
  start when it listens beyond loopback but trusts only loopback proxies.
* nginx: `set_real_ip_from <Cloudflare ranges>; real_ip_header CF-Connecting-IP;` and
  `proxy_set_header X-Forwarded-For $remote_addr;` (overwrite, not `$proxy_add_x_forwarded_for`). Fairy then
  takes the right-most X-Forwarded-For entry that is not a trusted proxy: the real visitor.
* `limit_req` on `/auth/` and `/api/`, buffering off for `/api/stream` (SSE), `client_max_body_size 128k`
  (9m for `/api/settings/background`). Security headers are Fairy's: do not add them in nginx.
* The secrets file stays `0600 root:root`: systemd loads it (`EnvironmentFile=`), the unit passes
  `--env-file ""`. Run the CLI as the service user: `sudo -u fairy fairy admin ...`.

## Security model

* Pocket ID only (OIDC code flow + PKCE S256, nonce, state). No passwords anywhere. Pocket ID (allowed_groups /
  allowed_emails, matched only for a verified email) decides who may sign in; nothing else.
* Roles, managed in the panel (`users.json`, Users page or `fairy admin users|sites`), keyed by the OIDC subject:
  **admin** sees everything, may use control actions, the Audit and Users pages; **viewer** is read-only and sees
  only the systems, guests, power sources, UPSes (and their processes, sensors, history) of their sites;
  **pending** (first sign-in of anyone else) sees a "waiting for access" page and gets 403 from every data API.
  Filtering is server-side for every endpoint and the SSE stream (`internal/web/view.go`); hidden ids answer the
  same 404 as unknown ones; a viewer's summary counts and cluster totals only add up what they can see.
* Sites: a host's site is the Users page's assignment, else `site:` in config.yaml (proxmox / agents / systems),
  else (guests) its node's site. Power sources and UPSes follow the hosts they are attached to unless assigned.
  No site = admins only.
* Bootstrap / upgrade: only while `users.json` has no admin at all, a user without a `users.json` entry who is in
  `oidc.control_groups` or whose **verified** email (`email_verified: true`) is in `oidc.bootstrap_admins` becomes
  admin at their first real sign-in (a fresh Pocket ID login; never from the
  groups cached in a session cookie, and never for someone an admin removed). After that the panel decides.
  Removing a user also ends their sessions and revokes their API tokens. The last admin cannot be demoted or removed (server-enforced; the CLI needs `--force`).
* Step-up: control actions need a sign-in from the last `step_up_window_s` (default 5 min); the re-login sends
  `max_age=0` (optionally `prompt=login`) and the callback requires a fresh `auth_time` (at most 2 min old).
  A missing `auth_time` never counts as fresh: the identity provider must send it (OIDC Core requires it when
  `max_age` is requested); otherwise step-up fails with a page that says so and control actions stay locked.
* Sessions: HMAC-signed `fairy_session` cookie (HttpOnly, Secure, SameSite=Lax) that is valid only while its id is
  in `sessions.json`, within the idle timeout and absolute lifetime; revocable with `fairy admin sessions`.
* API tokens: `fy_…`, CLI-created only, stored as SHA-256, scope `read` or `control`, optional expiry, never
  accepted on `/auth/*`, the settings endpoints or the Users page. `control` acts as an admin; `read` sees every
  site, or only `--sites A,B`. A token owned by a panel user is capped to that user's current role and sites; a token whose owner is not
  (or no longer) in `users.json` has no access. Only `--owner system` tokens keep their own scope.
* Writes: admin role (or control-scope token), CSRF header `X-Requested-With: fairy` + Origin /
  Sec-Fetch-Site checks for browsers, kill switch, per-host power flags, rate limits, typed confirmation for
  destructive power actions, append-only JSON-lines audit log (+ Audit page), optional ntfy/Discord notification.
  The audit log is bounded: lines are capped at 8 KiB, the file rotates by size (`audit_max_mb`, `audit_keep`;
  the Audit page shows the current file), and entries anyone can cause without signing in (rejected API tokens,
  failed or rate-limited sign-ins) are limited to 10 per client IP per minute and 300 in all, the rest summed
  up in one `audit.suppressed` entry a minute. An audit write failure never blocks a sign-in but is logged loudly.
  Changes to users and sites: admin browser session only, CSRF checks, step-up (same window), audited + notified;
  the kill switch does not block them.
* `trusted_proxies` is used only to find the client IP for rate limits and the audit log. The login limiter
  (`login_rate_per_min`) counts per client IP, and per user for a signed-in step-up, so nobody else's attempts
  can block an admin from confirming a control action.
* Strict CSP, HSTS (https), frame/referrer/permissions policies, generic 500s, no secrets in logs or errors.
* Sign-out is a POST with the CSRF header (no cross-site sign-out); visiting `/auth/login` keeps a valid session
  until a new sign-in completes.
* State files fail closed: an unreadable or corrupt `users.json`, `sessions.json`, `tokens.json` or `state.json`
  stops the panel at start, and at run time grants nothing (no sessions, control off) instead of being read as
  empty; such a file is never overwritten. Files are written `0600` by the service user, and `fairy admin`
  commands that change something refuse to run as another user (use `sudo -u fairy fairy admin ...`).
* Defence in depth: process command lines are redacted by the panel too; fan duty is never set below 30 %;
  background uploads need a role and are rate limited; revoking a token ends its live streams.
* See `SECURITY-FIXES.md` for the 2026-11 review fixes and what to configure.

## Map of the code

How a request flows: `cmd/fairy` starts the **poller** (which fills an in-memory cache from Proxmox, agents,
power sources and UPSes on a schedule) and the **web** server (which answers the API and the UI from that
cache). Each folder below is one Go package; inside a package, files are named after what they contain.

| Folder | What it does |
| --- | --- |
| `cmd/fairy` | The `fairy` program: `main.go` picks the sub-command, `serve.go` starts the panel (or demo mode). |
| `cmd/fairy-agent` | The `fairy-agent` program: `main.go` (run, check-config, gen-token, version), `service_windows.go` (the Windows service: install, uninstall, run under the service manager). |
| `internal/agent` | The agent's HTTP server and the panel contract (`server.go`), settings and tokens (`config.go`), BMC sensors (`bmc.go`), Supermicro fan control and its watchdog (`fan.go`), shutdown / reboot (`power*.go`), Wake-on-LAN (`wol.go`), plugs and UPSes it relays (`relay.go`), Tailscale address (`listen.go`), start-up (`run.go`), and `contract_test.go`, which runs the panel against it. |
| `internal/agent/collect` | What the agent measures: Linux `/proc` and `/sys` (`procfs.go`, `linux.go`), Windows APIs, registry and PDH (`windows.go`, `pdh_windows.go`), NVIDIA GPUs via nvidia-smi (`nvidia.go`). |
| `internal/agent/ipmi` | Raw IPMI to the BMC: sensor list and readings (`sdr.go`), Supermicro fan commands (`ipmi.go`), through ipmitool or Windows' IPMI WMI class (`ipmitool.go`, `wmi_windows.go`); `fakebmc/` for tests. |
| `internal/web` | HTTP server: security headers, Pocket ID login (`auth.go`, `oidc.go`, `policy.go`), signed session cookies (`session.go`), who may see and do what (`grant.go`), what each user sees (`view.go`: every response and the stream are filtered here), API endpoints (`api_*.go`, the Users page's in `api_users.go`), the checks in front of every control action and admin change (`guards.go`), live updates (`stream.go`), API docs (`docs.go`), demo preview (`demo.go`) and the embedded UI (`static/`). |
| `internal/poller` | Polls everything in the background and builds what the API shows: Proxmox (`proxmox.go`), agents (`agents.go`), the merged system list (`snapshot.go`), cluster totals (`summary.go`), per-host polling rate (`pollctl.go`), `health.json` (`health.go`), and the real network calls (`httpbackend.go`). |
| `internal/scheduler` | Runs each source as a job at its own interval: overrides, pause, boost while viewing, never overlapping. |
| `internal/normalise` | Turns raw Proxmox and agent data into the one "system" shape the UI uses; turns errors into short safe messages. |
| `internal/power` | Wall power: one file per device type (`shelly.go`, `tasmota.go`, `tuya.go`, `nut.go`, `redfish.go`, `agentpower.go`), retry/backoff (`runner.go`), totals and history (`manager.go`), devices reported by agents (`remote.go`). |
| `internal/ups` | UPSes via NUT: sources (`source.go`), state and history (`ups.go`), on-battery / restored events (`events.go`). |
| `internal/devices` | Device protocols shared by the panel and `fairy-agent` (which relays plugs and UPSes on its own LAN): Shelly/Tasmota/Redfish/Tuya parsing, NUT client, HTTP helpers, digest auth. |
| `internal/tuya` | The Tuya 3.3/3.4/3.5 local protocol (encryption, frames, one persistent connection per plug). |
| `internal/powerctl` | Which power actions a system offers (and why not) and carrying them out: agent, Proxmox API or Wake-on-LAN. |
| `internal/config` | Reads and checks `config.yaml` (`config.go`, `validate.go`) and secrets from `fairy.env` / the environment (`env.go`). |
| `internal/admin` | The break-glass CLI `fairy admin`: one file per command (users and sites, tokens, sessions, control, audit, config check, health). |
| `internal/access` | `users.json`: users (by OIDC subject) with a role and sites, the list of sites, and which site each host / power source / UPS is assigned to. |
| `internal/adminstate` | `state.json` and `sessions.json`: kill switch, per-host power flags, polling overrides, the session list. |
| `internal/statefile` | Safe shared JSON files (state, sessions, tokens, users): lock, re-read, write to a temp file, rename (so CLI and server never lose changes). |
| `internal/tokens` | Personal API tokens (`fy_...`), stored only as hashes in `tokens.json`. |
| `internal/audit` | The append-only audit log and its reader (Audit page, `fairy admin audit tail`). |
| `internal/health` | `health.json`, the status file the panel writes for `fairy admin health`. |
| `internal/settings` | Per-user UI settings and uploaded background images. |
| `internal/notify` | Optional ntfy / Discord message on control actions. |
| `internal/security` | Finds the real client IP behind nginx (only for rate limits and the audit log). |
| `internal/model` | The JSON shapes of the API (field names match the Python prototype). |
| `internal/jsonx` | Helpers for reading loosely-typed JSON from Proxmox, agents and devices. |
| `internal/apierr` | The error type that becomes an HTTP status + `{"detail": ...}`. |
| `internal/redact` | Removes passwords/tokens from process command lines (in the agent, and again in the panel). |
| `internal/demo` | `fairy serve --demo`: a simulated homelab (fake Proxmox, agents, plugs, UPS, BMC). |
| `internal/testutil` | Shared test fixtures. |
| `deploy/` | Panel: example `config.yaml`, `fairy.env`, the systemd unit, nginx. Agent: `fairy-agent.service`, `fairy-agent.config.example.yaml`, `install-fairy-agent.ps1`, and the optional polkit / tmpfiles / udev rules. |

## Notes

* Frontend changes vs. the prototype: CSRF header value `fairy`, localStorage keys `fairy-*`, CLI hint text.
* Tuya 3.3/3.4/3.5 framing, encryption and session-key derivation are implemented from tinytuya's source and
  checked against vectors generated with tinytuya; 3.4/3.5 have not been tested against real devices yet.
