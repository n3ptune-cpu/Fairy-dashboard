# Fairy

Homelab panel for Proxmox clusters, agent-monitored hosts, wall power (Shelly, Tasmota, Tuya 3.3/3.4/3.5, NUT,
Redfish) and UPSes, behind Pocket ID. Go rewrite of the Python prototype with the same JSON API; the web UI is
embedded in the binary.

> Status: the `fairy` panel is complete (feature parity with the prototype). `fairy-agent` (the Go port of the
> metrics agent) is not written yet: the panel works with the existing prototype agent, whose `/metrics` JSON it
> reads unchanged. A full README (nginx / MikroTik deployment, load testing) follows in the next phase.

## Build

```sh
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(git describe --always)" -o fairy ./cmd/fairy
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o fairy-linux-arm64 ./cmd/fairy
go test ./... && go vet ./...
```

Static binary, no runtime dependencies besides `/etc/fairy/config.yaml` and `/etc/fairy/fairy.env`.

## Run

```sh
fairy serve [--config /etc/fairy/config.yaml] [--env-file /etc/fairy/fairy.env] [--listen 127.0.0.1:8080]
fairy serve --demo            # simulated data, NO login, refuses anything but 127.0.0.1 / ::1 (port 8090)
fairy admin help              # break-glass CLI: tokens, sessions, control kill switch, host power flags,
                              # polling, audit tail, config check, health (all audited as source "cli")
fairy version
```

See `deploy/config.example.yaml`, `deploy/fairy.env.example` and `deploy/fairy.service` (hardened systemd unit,
no capabilities needed: the panel never runs local power or IPMI commands).

## Security model

* Pocket ID only (OIDC code flow + PKCE S256, nonce, state). No passwords anywhere.
* Step-up: control actions need a sign-in from the last `step_up_window_s` (default 5 min); the re-login sends
  `max_age=0` (optionally `prompt=login`) and the callback checks `auth_time`.
* Sessions: HMAC-signed `fairy_session` cookie (HttpOnly, Secure, SameSite=Lax) that is valid only while its id is
  in `sessions.json`, within the idle timeout and absolute lifetime; revocable with `fairy admin sessions`.
* API tokens: `fy_…`, CLI-created only, stored as SHA-256, scope `read` or `control`, optional expiry, never
  accepted on `/auth/*` or the settings endpoints.
* Writes: control group (or control-scope token), CSRF header `X-Requested-With: fairy` + Origin /
  Sec-Fetch-Site checks for browsers, kill switch, per-host power flags, rate limits, typed confirmation for
  destructive power actions, append-only JSON-lines audit log (+ Audit page), optional ntfy/Discord notification.
* `trusted_proxies` is used only to find the client IP for rate limits and the audit log.
* Strict CSP, HSTS (https), frame/referrer/permissions policies, generic 500s, no secrets in logs or errors.

## Map of the code

How a request flows: `cmd/fairy` starts the **poller** (which fills an in-memory cache from Proxmox, agents,
power sources and UPSes on a schedule) and the **web** server (which answers the API and the UI from that
cache). Each folder below is one Go package; inside a package, files are named after what they contain.

| Folder | What it does |
| --- | --- |
| `cmd/fairy` | The `fairy` program: `main.go` picks the sub-command, `serve.go` starts the panel (or demo mode). |
| `internal/web` | HTTP server: security headers, Pocket ID login (`auth.go`, `oidc.go`, `policy.go`), signed session cookies (`session.go`), API endpoints (`api_*.go`), the checks in front of every control action (`guards.go`), live updates (`stream.go`), API docs (`docs.go`) and the embedded UI (`static/`). |
| `internal/poller` | Polls everything in the background and builds what the API shows: Proxmox (`proxmox.go`), agents (`agents.go`), the merged system list (`snapshot.go`), cluster totals (`summary.go`), per-host polling rate (`pollctl.go`), `health.json` (`health.go`), and the real network calls (`httpbackend.go`). |
| `internal/scheduler` | Runs each source as a job at its own interval: overrides, pause, boost while viewing, never overlapping. |
| `internal/normalise` | Turns raw Proxmox and agent data into the one "system" shape the UI uses; turns errors into short safe messages. |
| `internal/power` | Wall power: one file per device type (`shelly.go`, `tasmota.go`, `tuya.go`, `nut.go`, `redfish.go`, `agentpower.go`), retry/backoff (`runner.go`), totals and history (`manager.go`), devices reported by agents (`remote.go`). |
| `internal/ups` | UPSes via NUT: sources (`source.go`), state and history (`ups.go`), on-battery / restored events (`events.go`). |
| `internal/devices` | Device protocols shared with the future `fairy-agent`: Shelly/Tasmota/Redfish/Tuya parsing, NUT client, HTTP helpers, digest auth. |
| `internal/tuya` | The Tuya 3.3/3.4/3.5 local protocol (encryption, frames, one persistent connection per plug). |
| `internal/powerctl` | Which power actions a system offers (and why not) and carrying them out: agent, Proxmox API or Wake-on-LAN. |
| `internal/config` | Reads and checks `config.yaml` (`config.go`, `validate.go`) and secrets from `fairy.env` / the environment (`env.go`). |
| `internal/admin` | The break-glass CLI `fairy admin`: one file per command (tokens, sessions, control, audit, config check, health). |
| `internal/adminstate` | `state.json` and `sessions.json`: kill switch, per-host power flags, polling overrides, the session list. |
| `internal/statefile` | Safe shared JSON files: lock, re-read, write to a temp file, rename (so CLI and server never lose changes). |
| `internal/tokens` | Personal API tokens (`fy_...`), stored only as hashes in `tokens.json`. |
| `internal/audit` | The append-only audit log and its reader (Audit page, `fairy admin audit tail`). |
| `internal/health` | `health.json`, the status file the panel writes for `fairy admin health`. |
| `internal/settings` | Per-user UI settings and uploaded background images. |
| `internal/notify` | Optional ntfy / Discord message on control actions. |
| `internal/security` | Finds the real client IP behind nginx (only for rate limits and the audit log). |
| `internal/model` | The JSON shapes of the API (field names match the Python prototype). |
| `internal/jsonx` | Helpers for reading loosely-typed JSON from Proxmox, agents and devices. |
| `internal/apierr` | The error type that becomes an HTTP status + `{"detail": ...}`. |
| `internal/redact` | Removes passwords/tokens from process command lines. |
| `internal/demo` | `fairy serve --demo`: a simulated homelab (fake Proxmox, agents, plugs, UPS, BMC). |
| `internal/testutil` | Shared test fixtures. |
| `deploy/` | Example `config.yaml`, `fairy.env` and the systemd unit. |

## Notes

* Frontend changes vs. the prototype: CSRF header value `fairy`, localStorage keys `fairy-*`, CLI hint text.
* Tuya 3.3/3.4/3.5 framing, encryption and session-key derivation are implemented from tinytuya's source and
  checked against vectors generated with tinytuya; 3.4/3.5 have not been tested against real devices yet.
