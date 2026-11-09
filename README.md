# Fairy-dashboard

> [!WARNING]
> Heads up: this whole thing was written by AI. We're not programmers,
> so we can't help you set it up or fix your install. If you want a
> feature or found a bug, open an issue and we'll have the AI look at it.
> It might not work on your hardware.
>
> It's MIT, so no warranty, use it at your own risk. Nobody has
> professionally security-tested it. It can shut down and reboot PCs
> and change fan speeds if you turn those on, and on Windows part of
> it runs as SYSTEM. Only connect to the agents over Tailscale, never
> straight from the internet, and only install from our Releases page.

The Fairy panel: a homelab panel for Proxmox clusters, agent-monitored hosts, wall power (Shelly, Tasmota, Tuya 3.3/3.4/3.5, NUT,
Redfish) and UPSes, behind Pocket ID. Go rewrite of the Python prototype with the same JSON API; the web UI is
embedded in the binary.

> Status: the `fairy` panel is complete (feature parity with the prototype). `fairy-agent`, the metrics agent
> for the machines it watches (Windows and Linux), lives in its own repository,
> [github.com/n3ptune-cpu/Fairy-agent](https://github.com/n3ptune-cpu/Fairy-agent), together with the device code
> both programs share (plugs, UPSes, BMCs, Tuya). The panel imports that code from there instead of keeping a
> copy, and `internal/agentcontract` runs the panel's real poller and web API against the real agent handler.
> A full README (nginx / MikroTik deployment, load testing) follows.

Two repositories:

| Repository | What is in it |
| --- | --- |
| [Fairy-dashboard](https://github.com/n3ptune-cpu/Fairy-dashboard) (this one) | the `fairy` panel: web UI, API, poller, login, admin CLI, panel deployment files |
| [Fairy-agent](https://github.com/n3ptune-cpu/Fairy-agent) | `fairy-agent`, its install files and [AGENT.md](https://github.com/n3ptune-cpu/Fairy-agent/blob/main/AGENT.md), plus the shared Go packages under `pkg/` |

## Build

```sh
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(git describe --always)" -o fairy ./cmd/fairy
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o fairy-linux-arm64 ./cmd/fairy
go test ./... && go vet ./...
```

Static binary, no runtime dependencies besides `/etc/fairy/config.yaml` and `/etc/fairy/fairy.env`.

The build needs Go 1.27.3 or newer. `go build` downloads the shared packages from
`github.com/n3ptune-cpu/Fairy-agent` (the version pinned in `go.mod`, checked against `go.sum`) like any other
dependency. To change shared code, change it in Fairy-agent, tag a new version there, then here run
`go get github.com/n3ptune-cpu/Fairy-agent@vX.Y.Z && go mod tidy`. The agent itself is built in its own
repository (see its README).

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
fairy update --check          # installed vs the latest signed Fairy-dashboard release
fairy update [--yes]          # as root on the panel host: install it, restart fairy.service, roll back on failure
fairy version
```

See `deploy/config.example.yaml`, `deploy/fairy.env.example` and `deploy/fairy.service` (hardened systemd unit,
no capabilities needed: the panel never runs local power or IPMI commands).

### Agents

Each watched machine runs `fairy-agent` (Windows service or systemd unit), reachable only over Tailscale on port
9101. It is built and documented in [Fairy-agent](https://github.com/n3ptune-cpu/Fairy-agent):
[AGENT.md](https://github.com/n3ptune-cpu/Fairy-agent/blob/main/AGENT.md) explains installing it on Windows
(PowerShell, `deploy/install-fairy-agent.ps1` in that repository) and Linux (`deploy/fairy-agent.service`), its
settings (`deploy/fairy-agent.config.example.yaml`), how to add it to the panel's `agents:`, and what it never touches on a gaming PC (no driver, no access to other programs'
memory). The contract, in short: `GET /metrics`, `GET /processes?limit=&sort=`, `GET /fan/status` with the read
token, `POST /fan/mode`, `/fan/duty`, `/power`, `/wol` with the separate control token, both as
`Authorization: Bearer`; every POST feature is off on the agent until its owner enables it.

Laptops: newer agents add an optional `battery` object to `/metrics` (`percent`, `state` charging / discharging /
full / idle, `ac_online`, `power_w` positive while discharging, `time_remaining_s`, `health_pct`). The panel
shows it as a Battery box on the host page and the charge in the host list; agents without it keep working.

**Elevated helper** (Fairy-agent v0.5.0+, Windows): the agent runs as `NT SERVICE\fairy-agent` and a separate
LocalSystem service without network access, `fairy-agent-helper`, reads Core Temp and the BMC's sensors, drives
the fans (with the agent's temperature watchdog), runs power actions and installs updates. `/metrics` then carries a
`helper` block (state, needed, version, since, watchdog, features, error) and, while it is not running, an
`unavailable` list of what it would read (`power_control.available` false when it does the power actions). The host page shows
"Elevated helper: running / stopped / error" next to the name (only on hosts that need it), with Stop / Start in its
menu: admins only, typed host name, step-up sign-in, rate limit, audited as `helper_stop` / `helper_start`
(`POST /api/systems/{id}/helper/stop|start`, sent to the agent's `/helper/stop|start` with its control token). While it
is stopped the readings it provides collapse into one "paused" line with a Start button, the Fans tab's controls are
disabled (the panel also refuses fan writes), shut down / reboot / sleep say "start the helper first", and the host list
flags the host. Linux and older agents send no helper.

**Sleep** (Fairy-agent v0.4.2+): the Power menu of a host whose agent has power control and reports
`power_control.sleep: true` offers Sleep next to Shut down / Reboot, with the same typed name, step-up sign-in,
audit, delay and Cancel. If the agent says the machine cannot sleep (e.g. a Modern-Standby-only laptop) the menu
shows why; older agents show "needs v0.4.2". Proxmox nodes are not offered Sleep. While it sleeps the host shows
"asleep (from panel)" (and an informational notification, not an alarm) until it is seen again; Power on
(Wake-on-LAN) wakes it, which from sleep needs the network card's "Allow this device to wake the computer" /
"Wake on Magic Packet".

**Power on via BMC** (Fairy-agent v0.4.2 `bmc_relay`): for a machine Wake-on-LAN cannot wake (e.g. its BMC
shares the LAN port), `systems: [{id: nsx-cyrene, bmc_power: {relay: nsx-furina-agent, target: cyrene-bmc}}]`
(or "Or powered on via BMC by" in Add host) names an always-on agent that can reach the BMC and its
`bmc_relay` target (see AGENT.md for the agent side: `fairy-agent bmc-relay add`, `fairy-agent secret set`). The
relay agent's entry needs `control_token_env`. While the machine is offline and the relay is online and reports
the target, the host page and Power menu offer "Power on (via BMC, through NSX-FURINA)", with typed name,
step-up sign-in and audit like the other power actions; if the relay cannot reach the BMC (laptop not cabled)
the error says so. Radeon cards on Windows whose agent reads AMD's ADL (v0.4.2) show their temperature and
watts "via AMD".

### Updates

Nothing updates on its own. Every push to `main` makes a signed GitHub release (`.github/workflows/release.yml`:
vet, test, build `fairy-linux-amd64` / `fairy-linux-arm64` with the version `<run number>-<short commit>` and the
public key from `release/signing.pub`, sign `SHA256SUMS` with the `FAIRY_RELEASE_KEY` secret, release
`build-<run number>` with the commits since the last one as notes). The job runs in the GitHub Environment
`release`: `FAIRY_RELEASE_KEY` is a secret of that environment (not of the repository), and the environment can
require the owner's approval before each release is built and signed. The one-time setup (key, secrets, first
manual install per machine) is in Fairy-agent's
[SETUP-UPDATES.md](https://github.com/n3ptune-cpu/Fairy-agent/blob/main/SETUP-UPDATES.md).

* **The panel**: `fairy update` on its host as root (or the wrapper `deploy/fairy-update` as
  `/usr/local/sbin/fairy-update`). It checks the release signature with the key built into the running binary,
  keeps `/usr/local/bin/fairy.old`, restarts `fairy.service` and rolls back if the service does not stay up and
  answer `/healthz`. Or **Update panel** on the Updates page (admins): the panel (user `fairy`, no root) may only
  *start* the root oneshot unit `fairy-panel-update.service` (`deploy/fairy-panel-update.service`), allowed by one
  polkit rule (`deploy/fairy-panel.polkit.rules`: user `fairy`, that unit, verb `start`, nothing else). The unit
  runs `fairy update --yes --from-panel`, takes nothing from the web request, never installs an older or the
  same version, and writes its steps to `/var/lib/fairy-panel-update/status.json`, so the page shows
  downloading, verified, restarting and "running X" / rolled back, also after the panel's restart. The button
  needs a Pocket ID sign-in from the last **2 minutes** (always, also within the usual step-up window), the
  CSRF header and the kill switch off; it is audited as `panel.update`.
  **One-time setup**: every `fairy update` run by hand as root installs (or refreshes) the unit and the polkit
  rule (`apt install polkitd` if it is missing). The release running today predates this, so the first time run
  `fairy update` twice on the panel host: the first run installs the new panel, the second (now the new
  `fairy`) sets up the button. Until then the button says what is missing.
* **Agents**: admins see, in the panel, a banner when a newer Fairy-agent or Fairy-dashboard release exists
  (dismissed per release), an arrow next to each host's agent version when it is behind (click: release notes
  and **Start update**), a dot on the menu's **Updates** entry, and the **Updates** page (every agent's version
  and state, **Update** per host, **Update all**, the panel's own version vs the latest panel release). Update is
  a control action: admin, CSRF header, kill switch, step-up sign-in, rate limit, audit (`update.agent`), and only
  for agents with `self_update: true` (needs `control_token_env`) whose own config enables it; the agent then
  downloads, verifies and installs the release itself (see Fairy-agent's AGENT.md). Viewers see none of it.
* The latest releases are looked up on GitHub at most every 10 minutes, in the background and only while an
  admin uses the panel; `updates: {check_latest: false}` switches that off on a panel without internet access.
  **Check for updates** on the Updates page asks GitHub now (both the agent and the panel release) and shows
  "Up to date", "N updates available" or the error, and when GitHub was last checked. It is read-only (no
  step-up, works with the kill switch on) but admins only, browser session, CSRF header, at most once every
  30 seconds for the whole panel, and audited as `update_check`.

### Add host

Admins: **Add host** on the Overview. Give the machine's Tailscale name (the panel reaches it at
`http://<name>:9101`; an address can be given instead), its system (Windows / Linux), site, options (power
control, self-update, Wake-on-LAN relay, a local UPS through NUT, Core Temp on Windows, IPMI on Linux), whether
it is a Proxmox node (`node:`), and optionally its Wake-on-LAN MAC(s) and the relay that wakes it. The panel
makes a one-time join code (160 random bits, shown once, grouped as `ABCD-EFGH-...`, valid 15 minutes, single
use, only its hash is kept) and one command:

```text
Windows (PowerShell as Administrator):
$u='https://github.com/n3ptune-cpu/Fairy-agent/releases/download/build-13'; $f="$env:TEMP\fairy-install.ps1"; iwr -useb "$u/install.ps1" -OutFile $f; if ((Get-FileHash $f -Algorithm SHA256).Hash -ne '<SHA-256>') { throw '...' }; powershell -ExecutionPolicy Bypass -File $f -ReleaseUrl $u -Join <CODE> -PanelAddress <panel IP> [-PowerControl -SelfUpdate -CoreTemp ...]
Linux:
f=$(mktemp) && curl -fsSL --proto '=https' -o "$f" https://github.com/n3ptune-cpu/Fairy-agent/releases/download/build-13/install.sh && echo "<SHA-256>  $f" | sha256sum -c - && sudo env FAIRY_RELEASE_URL=https://github.com/n3ptune-cpu/Fairy-agent/releases/download/build-13 sh "$f" --join <CODE> --panel <panel IP> [--power-control --self-update ...]
```

The command never runs "the latest" script. It names one Fairy-agent release: the one this panel was built
against (`build-13` = Fairy-agent v0.5.2; `enroll: {agent_release: build-N}` picks another). Before it shows a
command the panel downloads that release's `SHA256SUMS`, `SHA256SUMS.sig`, `VERSION`, `install.sh` and
`install.ps1` and checks them with the release key built into the panel (`release/signing.pub`); a panel build
without the key, an unreachable GitHub or a bad signature refuses Add host (HTTP 503) instead. The command runs
the script only if its SHA-256 is the one the panel checked, and points it at the same release. When the checked
installer takes it (Fairy-agent v0.4.3 and later), the command also passes the SHA-256 of the `SHA256SUMS` the
panel verified (`--sums-sha256 <hash>` / `-SumsSha256 <hash>`): the installer refuses any other `SHA256SUMS`
before it trusts a line of it, so even a first install rests on the panel's key. The installer
checks every file it downloads against `SHA256SUMS` and its signature, installs the agent like the manual install, makes its tokens on
the machine and waits for the panel. The panel tries to pair every few seconds; the dialog follows it
(waiting -> paired -> online) and Cancel stops it. Pairing is `POST /pair` on the agent with a proof of the code,
accepted once, only from the panel's address, at most 5 tries a minute; then the agent switches pairing off for
good. Creating a code is a control action (admin, CSRF, kill switch, step-up sign-in, rate limit, audit
`host.add` / `host.pair`).

Hosts added this way are kept in `<data_dir>/managed-hosts.yaml` and their tokens in
`<data_dir>/secrets/agent-<id>.json` (0600), not in config.yaml; both are merged at start (an id in config.yaml
wins and the managed one is skipped with a log line) and checked by `fairy admin config check`. Their page has
**Re-pair** (a new code, e.g. after the panel lost the tokens) and **Remove** (stops polling, deletes the tokens;
the agent stays installed until uninstalled on the machine); both ask for the host's name to be typed and a
step-up sign-in, and are audited. Hosts in config.yaml are not changed by the panel.

`enroll: {panel_address: 100.x.y.z}` in config.yaml is the address the command gives the agent. It is optional:
without it the panel finds its own Tailscale address at start (logged once as `Add host: panel address`, and
shown in the Add host dialog as "Panel address: 100.x.y.z (auto)"): the 100.64.0.0/10 address of an interface
named `tailscale*` (else the only such address of any interface); when there is no such interface (tailscaled
with `--tun=userspace-networking`, e.g. in an unprivileged LXC) it asks tailscaled itself over its LocalAPI
socket `/var/run/tailscale/tailscaled.sock` (`GET /localapi/v0/status`, 2 s timeout). On Linux that socket is
0666 and tailscaled gives every local user read-only access, so the `fairy` user needs no
`tailscale set --operator=fairy`. A Tailscale IPv6 (fd7a:115c:a1e0::/48) is only used when there is no IPv4,
and only shown: agents pair over IPv4, so set `panel_address` then. Set it too when the panel has several
Tailscale-range addresses, or the agents reach it at another address. The panel must resolve MagicDNS names (or
give the address).

**A new Proxmox VE node** (panel and Fairy-agent v0.4.2): "Is it a Proxmox node? → Yes: a new node". The Linux
command gets `--proxmox`; run it as root on the node. There `fairy-agent join` makes the panel a read-only API
token (user `fairy@pve`, role `PVEAuditor` on `/`, token `fairy@pve!panel-<node>` without privilege
separation; a re-run replaces only that token) and the agent hands it to the panel once, with its own tokens.
The panel then keeps the token secret with the host's tokens in `<data_dir>/secrets/agent-<id>.json` (0600) and
the node's CA (`/etc/pve/pve-root-ca.pem`) in `<data_dir>/secrets/pve-<id>-ca.pem`, adds a `proxmox:` entry for
the node at once (standalone, `id_prefix` `<site>-<node>-`, which must not clash with another entry's; URL
`https://<the host's address>:8006`, certificate verified against that CA for the node's name, which Proxmox's
own certificate lists: `tls_server_name`), and attaches the agent to the node. If the certificate is not issued
for that name the host page and the Add host dialog say so (`pvecm updatecerts --force`, then `systemctl restart
pveproxy`). **Remove** takes the node and its guests away too and shows the one command that deletes the token
on the node (`pveum user token remove fairy@pve panel-<node>`). Standalone nodes only: for a node in a cluster
the command says so and the host is added without the token (write the cluster under `proxmox:` by hand). A
hand-written `proxmox:` entry keeps working and wins on the same id. `tls_server_name` can also be set on
hand-written entries.

### Notifications

The bell (top bar) and **Notifications** (sidebar): what changed, newest first. Events are made on state
*changes* only: a host offline (after 30 s) and back, unstable (6 changes in 10 minutes: one warning, then quiet),
"off (shut down from the panel)" or "asleep (sent to sleep from the panel)" instead of an alert when the panel
did it, a temperature over 85 °C (for
10 s) and back under 80 °C, UPS on battery / low battery / back on mains / connection lost, power actions,
agent and panel updates, Add host results. Filters (severity, type, host, unread), mark read, mark all read,
dismiss, per user; each person sees the hosts and UPSes of their sites (events without a host are for admins).
Kept in `<data_dir>/notifications.json` for 30 days (at most 2000). The Overview shows one line, "N active
alerts", for alerts whose condition still holds. Warnings and critical events also go to `FAIRY_NOTIFY_URL`
(ntfy or Discord) when it is set.

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
  `--env-file ""`. `/etc/fairy` itself is `0750 root:fairy` and `config.yaml` `0640 root:fairy`. Run the CLI as the service user: `sudo -u fairy fairy admin ...`.

## Security model

* Pocket ID only (OIDC code flow + PKCE S256, nonce, state). No passwords anywhere. Pocket ID (allowed_groups /
  allowed_emails, matched only for a verified email) decides who may sign in; nothing else.
* Roles, managed in the panel (`users.json`, Users page or `fairy admin users|sites`), keyed by the OIDC subject:
  **admin** sees everything, may use control actions, the Audit and Users pages; **viewer** is read-only and sees
  only the systems, guests, power sources, UPSes (and their processes, sensors, history) of their sites;
  **pending** (first sign-in of anyone else) sees a "waiting for access" page and gets 403 from every data API.
  Filtering is server-side for every endpoint and the SSE stream (`internal/web/view.go`); hidden ids answer the
  same 404 as unknown ones; a viewer's summary counts, cluster totals and the Overview's all-hosts CPU / memory totals (`totals` in `/api/summary`) only add up what they can see.
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
| `cmd/fairy` | The `fairy` program: `main.go` picks the sub-command, `serve.go` starts the panel (or demo mode), `update.go` is `fairy update`. |
| `internal/web` | HTTP server: security headers, Pocket ID login (`auth.go`, `oidc.go`, `policy.go`), signed session cookies (`session.go`), who may see and do what (`grant.go`), what each user sees (`view.go`: every response and the stream are filtered here), API endpoints (`api_*.go`, the Users page's in `api_users.go`), the checks in front of every control action and admin change (`guards.go`), live updates (`stream.go`), API docs (`docs.go`), demo preview (`demo.go`) and the embedded UI (`static/`). |
| `internal/poller` | Polls everything in the background and builds what the API shows: Proxmox (`proxmox.go`), agents (`agents.go`), the merged system list (`snapshot.go`), cluster and all-hosts totals (`summary.go`), per-host polling rate (`pollctl.go`), `health.json` (`health.go`), and the real network calls (`httpbackend.go`). |
| `internal/scheduler` | Runs each source as a job at its own interval: overrides, pause, boost while viewing, never overlapping. |
| `internal/normalise` | Turns raw Proxmox and agent data into the one "system" shape the UI uses. |
| `internal/power` | Wall power on the panel: retry/backoff per source (`runner.go`), totals and history (`manager.go`), devices reported by agents (`remote.go`), and the `type: agent` component-power metrics (`agentpower.go`). The device types themselves are Fairy-agent's `pkg/power`. |
| `internal/ups` | UPSes on the panel: state and history (`ups.go`), on-battery / restored events (`events.go`). The NUT sources are Fairy-agent's `pkg/ups`. |
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
| `internal/updates` | The latest Fairy-agent / Fairy-dashboard release for the Updates page (GitHub, cached, never blocking). |
| `internal/notify` | Optional ntfy / Discord message on control actions and notifications. |
| `internal/notifications` | The Notifications page: events and per-user read / dismissed marks (`notifications.go`), made from the poller's snapshots on state changes only (`watcher.go`). |
| `internal/managed` | Hosts added with Add host: `managed-hosts.yaml` and `secrets/`, merged with config.yaml. |
| `internal/enrollment` | Add host: join codes, the command per OS, the pairing loop (waiting -> paired -> online). |
| `internal/panelupdate` | Update panel: starting `fairy-panel-update.service` and reading its status file. |
| `internal/security` | Finds the real client IP behind nginx (only for rate limits and the audit log). |
| `internal/model` | The JSON shapes of the API (field names match the Python prototype). |
| `internal/jsonx` | Helpers for reading loosely-typed JSON from Proxmox, agents and devices. |
| `internal/apierr` | The error type that becomes an HTTP status + `{"detail": ...}`. |
| `internal/demo` | `fairy serve --demo`: a simulated homelab (fake Proxmox, agents, plugs, UPS, BMC). |
| `internal/agentcontract` | The panel <-> agent contract test: the panel's real poller, normaliser, power/UPS managers and web API against the real `fairy-agent` handler (fake collectors, fake BMC, fake devices). |
| `internal/testutil` | Shared test fixtures. |
| `deploy/` | Example `config.yaml`, `fairy.env`, the systemd unit `fairy.service`, nginx, the `fairy-update` wrapper, the Update panel unit and its polkit rule. (The agent's install files are in Fairy-agent.) |
| `.github/workflows` | `release.yml` (signed release on every push to main), `ci.yml` (tests on pull requests). |

Shared code imported from [Fairy-agent](https://github.com/n3ptune-cpu/Fairy-agent) (`github.com/n3ptune-cpu/Fairy-agent/...`):

| Package | What the panel uses it for |
| --- | --- |
| `pkg/devices` | Device protocols: Shelly/Tasmota/Redfish/Tuya parsing, NUT client, HTTP helpers, digest auth, and `Describe`, which turns errors into short safe messages. |
| `pkg/power` | One power source per device type (`shelly`, `tasmota`, `tuya`, `nut`, `redfish`/`ipmi`, `agent`), built from a `power_sources:` entry. |
| `pkg/ups` | NUT UPS sources built from an `ups:` entry, and which systems each UPS powers. |
| `pkg/tuya` | The Tuya 3.3/3.4/3.5 local protocol (encryption, frames, one persistent connection per plug). |
| `pkg/redact` | Removes passwords/tokens from process command lines (in the agent, and again in the panel). |
| `pkg/configutil` | The id rule (`ValidID`) and secrets referenced by environment variable name (`EnvSecret`). |
| `pkg/enroll` | Add host: join codes, the proof sent to the agent's `/pair`, and its answer. |
| `pkg/release` | Signed releases: signature checks, the GitHub download, the swap / restart / rollback installer and the `update` command shared with `fairy-agent update`. |
| `pkg/agent`, `pkg/agent/...` | Only in `internal/agentcontract`'s test: the agent's HTTP handler, fake collector and fake BMC. |

## Notes

* Frontend changes vs. the prototype: CSRF header value `fairy`, localStorage keys `fairy-*`, CLI hint text.
* Tuya 3.3/3.4/3.5 framing, encryption and session-key derivation are implemented from tinytuya's source and
  checked against vectors generated with tinytuya; 3.4/3.5 have not been tested against real devices yet.

## Licence

MIT, see [LICENSE](LICENSE). Fairy-agent is MIT-licensed too.
