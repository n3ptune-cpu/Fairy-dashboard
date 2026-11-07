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

## Notes

* Frontend changes vs. the prototype: CSRF header value `fairy`, localStorage keys `fairy-*`, CLI hint text.
* Tuya 3.3/3.4/3.5 framing, encryption and session-key derivation are implemented from tinytuya's source and
  checked against vectors generated with tinytuya; 3.4/3.5 have not been tested against real devices yet.
