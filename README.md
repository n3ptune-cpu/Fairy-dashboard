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

The Fairy panel: a homelab panel for Proxmox clusters, agent-monitored hosts, wall power (Shelly, Tasmota, NUT, Redfish) and UPSes, behind Pocket ID. Go rewrite of the Python prototype with the same JSON API; the web UI is
embedded in the binary.

> Status: the `fairy` panel is complete (feature parity with the prototype). `fairy-agent`, the metrics agent
> for the machines it watches (Windows and Linux), lives in its own repository,
> [github.com/n3ptune-cpu/Fairy-agent](https://github.com/n3ptune-cpu/Fairy-agent), together with the device code
> both programs share (plugs, UPSes, BMCs). The panel imports that code from there instead of keeping a
> copy, and `internal/agentcontract` runs the panel's real poller and web API against the real agent handler.

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

The build needs Go 1.27.4 or newer. `go build` downloads the shared packages from
`github.com/n3ptune-cpu/Fairy-agent` (the version pinned in `go.mod`, checked against `go.sum`) like any other
dependency. To change shared code, change it in Fairy-agent, tag a new version there, then here run
`go get github.com/n3ptune-cpu/Fairy-agent@vX.Y.Z && go mod tidy`. The agent itself is built in its own
repository (see its README).

`go.mod` also has the line `godebug multipathtcp=0`: the panel does not use Multipath TCP, which Go otherwise
switches on for listeners on Linux. `go version -m fairy` shows `DefaultGODEBUG=multipathtcp=0`. Fairy-agent's
`go.mod` has the same line for the agent's programs; neither reaches the other's.

## Run

```sh
fairy serve [--config /etc/fairy/config.yaml] [--env-file /etc/fairy/fairy.env] [--listen 127.0.0.1:8080]
                              # listen: --listen, else $FAIRY_LISTEN, else `listen:` in config.yaml, else
                              # 127.0.0.1:8080. The env file is read first, so FAIRY_* settings in it count.
fairy serve --demo            # simulated data, NO login, refuses anything but 127.0.0.1 / ::1 / localhost (port 8090)
fairy serve --demo --demo-as viewer:Bob   # ...as a viewer of the Bob site sees it (or: pending, admin);
                              # the demo menu (top right) has a "Preview as" switcher too
fairy admin help              # break-glass CLI: users and sites, tokens, sessions, control kill switch, host
                              # power flags, polling, audit tail, config check, health (all audited as "cli")
fairy admin users set-role you@example.com admin   # locked out? make yourself admin again. It shows the account
                              # it found and asks before making an admin: answer yes, or add --yes (needed
                              # when there is no terminal: a script, cron, ssh without -t). An address that
                              # `fairy admin users list` marks "(unverified)" does not find an account: use the
                              # SUBJECT from that list instead: fairy admin users set-role <subject> admin --yes
fairy update --check          # installed vs the latest signed Fairy-dashboard release
fairy update [--yes]          # as root on the panel host: install it, restart fairy.service, roll back on failure
fairy version
```

See `deploy/config.example.yaml`, `deploy/fairy.env.example` and `deploy/fairy.service` (hardened systemd unit,
no capabilities needed: the panel never runs local power or IPMI commands).

`FAIRY_SESSION_SECRET` in `fairy.env` signs the session cookies. Make it with `openssl rand -base64 48`. The
panel does not start with the example value from `deploy/fairy.env.example` or with one character repeated.
In `fairy.env` nothing may follow a value: `FAIRY_INSECURE_COOKIES=1   # testing` is read, comment and all,
as one value (by systemd and by Fairy), and the setting is then off. Comments go on lines of their own. The
panel's log at start and `fairy admin config check` name a variable that looks like this.

**Trying changes on your own PC, without Pocket ID?** That is a separate *test build*
(`go build -tags fairytest`), see [TESTING-LOCAL.md](TESTING-LOCAL.md). It has no sign-in, so it only runs on
`127.0.0.1` with its own empty state, and no release contains it.

**Updating a panel that already runs?** Read [Upgrading to this release](#upgrading-to-this-release) first.
It is short, and it names what to look at in `config.yaml` before the update: the two session settings (one
combination of them does not start any more) and the `wake_update:` block (write it only after the update).

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

**Model name.** The Overview shows a host's model under its name when its agent reports one. An admin can set
a name of their own instead: the host's page, the ⋯ menu, **Set model name**; **Clear** brings the reported
model back. The reported model stays one step behind the name (its tooltip on the Overview, the ⓘ on the
host's page). The name is kept by the panel (`state.json`, `host_models`), is 1 to 64 characters on one line,
and goes when the host is removed. Admins only, from a browser session, no "Confirm it's you"; audited as
`host.model`. API: `PUT /api/systems/{id}/custom-model`.

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
shares the LAN port), `systems: [{id: agent-1, bmc_power: {relay: agent-2, target: server-bmc}}]`
(or "Or powered on via BMC by" in Add host) names an always-on agent that can reach the BMC and its
`bmc_relay` target (see AGENT.md for the agent side: `fairy-agent bmc-relay add`, `fairy-agent secret set`). The
relay agent's entry needs `control_token_env`. While the machine is offline and the relay is online and reports
the target, the host page and Power menu offer "Power on (via BMC, through AGENT-2)", with typed name,
step-up sign-in and audit like the other power actions; if the relay cannot reach the BMC (laptop not cabled)
the error says so. (`agent-1`, `agent-2` and `server-bmc` are example names.) Radeon cards on Windows whose
agent reads AMD's ADL (v0.4.2) show their temperature and watts "via AMD".

**What a newer agent refuses, and what the panel shows then** (Fairy-agent 0.6.0). The agent's side of each
point is in AGENT.md. Its Windows parts are listed there under "Not yet tested on a real Windows PC".

* **Fans.** The agent answers a fan speed below 100 % with HTTP 409 and a sentence when it is not safe. The
  panel shows that sentence as it is. The answer also carries one of three codes:

  | Code | When |
  | --- | --- |
  | `fan_too_hot` | the processor is at or over the agent's temperature limit |
  | `fan_watchdog_hold` | the agent's watchdog set the fans to 100 % less than 5 minutes ago |
  | `fan_no_temperature` | the agent has no temperature reading yet (normal right after it started) |

  In all three cases only 100 % is accepted. On Linux the fans go back to the BMC's automatic mode whenever
  the agent stops (restart, update, reboot). So after an agent restart or update the Fans tab shows the
  automatic mode, and a fixed speed has to be set again.
* **Shut down / reboot on Windows.** The agent remembers a shutdown that was started from the panel, so it
  is still shown as pending after the agent restarted, and Cancel still works. While the elevated helper is
  stopped the agent refuses Cancel ("helper stopped: cannot cancel"): start the helper first. Cancel only
  stops a shutdown that Fairy started, never one that Windows or somebody at the PC started.
* **`only_from_panel: true`** in an agent's own config.yaml (off by default): that agent answers only the
  address of the panel that paired with it. If the panel's Tailscale address changes, the agent refuses the
  panel until you run **Re-pair** for that host.
* **Processes tab.** A Linux agent installed with this release sends no command lines (`processes: {cmdline:
  false}` in its config.yaml). Agents installed earlier keep what their config.yaml says. Set it to `true` on
  the machine to see command lines again.
* **Plugs and UPSes that report nonsense.** A value that cannot be a reading (not a number, a charge of
  250 %) is left out. The device stays in the list with its other values.

### Plugs through Home Assistant

A plug that neither the panel nor an agent can ask itself (a Tuya plug, a Zigbee plug) can still be read when
a Home Assistant on the plug's network knows it. An **agent** on that network asks that Home Assistant for the
plug's power sensor and relays the watts to the panel like every other plug it reads (Fairy-agent v0.8.0,
`type: homeassistant`; the watts are read, nothing is switched). The whole of it, with the steps in Home
Assistant, is in the agent's
[AGENT.md](https://github.com/n3ptune-cpu/Fairy-agent/blob/main/AGENT.md#plugs-that-are-in-home-assistant-type-homeassistant).
In short:

1. **Update that agent to v0.8.0 first** (build 18; the Updates page, or "Add host" from this panel). An older
   agent does not know the type and does not start with such an entry.
2. **On the agent's machine**, the token goes into the agent's secrets file, never into config.yaml:
   `sudo fairy-agent secret set HA_TOKEN` (Windows: `& "C:\Program Files\Fairy\fairy-agent.exe" secret set
   HA_TOKEN` in an Administrator PowerShell). Then the entry in the agent's own config.yaml:

   ```yaml
   power_sources:
     - id: corner-plug                      # the id the panel's links name
       type: homeassistant
       url: "http://192.168.1.10:8123"      # Home Assistant itself: http(s)://host[:port]
       token_env: HA_TOKEN                  # the NAME of the variable, never the token
       entity: sensor.corner_plug_power     # the plug's power sensor
   ```

   `fairy-agent check-config`, then restart the agent.
3. **In the panel's config.yaml**, in the entry of the agent that reads the plug, say which machine the plug
   feeds, and whether it counts:

   ```yaml
   agents:
     - id: nas-agent
       # ...
       power_links: {pc-plug: lab-b-agent, rack-plug: lab-node-pve-a}   # <device id on the agent>: <system id>
       power_exclude_from_total: [corner-plug]                          # shown, and in no total
   ```

   * `power_links` names the machine by its id, and the machine can be any machine of the panel, not only the
     one the agent runs on (an agent on one site that reads the plugs of machines on another). For a Proxmox
     node with an agent **either id works** (build 14): the node's id (`<id_prefix>node-<name>`) or its
     agent's. The watts are on the row the machine is shown as, also after a restart of the panel with the
     machine off, when it is shown under its agent's id. `system_hint: self` in the agent's entry of a plug
     (the plug feeds the agent's own machine) now works for the agent of a node as well. Where config.yaml does
     not decide which node is an agent's (see [Update unpowered hosts](#update-unpowered-hosts): `links_to:`),
     a link written with the node's id shows only while the node's row is there; `fairy admin config check`
     has a `note` line for such a link, and one for a link to an id that is no machine's.
   * `power_exclude_from_total` is for a plug whose watts are already counted another way. The usual case is
     **a plug at the input of a UPS**: the machines behind the UPS are counted through the UPS (its load, or
     their own plugs), and the plug in front of it would count them a second time. Such a plug has its own
     line on the Power page with its watts and the chip **not in total**, and is in neither the Power page's
     total nor the Overview's.
   * The device ids are the agent's own. When the agent reports no device with an id that is written here (a
     typing mistake), nothing is linked or left out for it: the panel says so once in its log ("... names
     devices the agent does not report"), and `fairy admin config check` lists what is set for each agent.
   * Hosts added with **Add host** have no `power_links` (their entry is not in config.yaml): give such an
     agent's plug `system_hint: self` in the agent's own config.yaml, or let an agent that is in config.yaml
     read the plug and link it.
4. A plug that cannot be read stays on the Power page and on its machine's page with the agent's words for
   why (`home assistant refused the token`, `entity not found`, `the sensor is unavailable`), also after it
   has had no reading for longer than `offline_after`.

> **A Home Assistant token is never read-only.** Home Assistant's long-lived tokens carry no limits: the
> token that reads one sensor can also switch every plug, light and lock of that Home Assistant. It is kept
> on the agent's machine only (the panel never has it), readable by root / administrators and the agent. Use
> a Home Assistant user made for this, and delete the token there when the agent no longer needs it.

`type: homeassistant` is an agent's only: the panel's own `power_sources:` refuse it ("... is read by an
agent, not by the panel"). After a refused token an agent waits before it asks Home Assistant again (5
minutes, 15, then an hour), because Home Assistant counts such a request as a failed login; the panel's own
poller has no such wait.

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
  answer `/healthz`. It updates the program `fairy.service` starts: started from another file (a copy, an old
  download) it stops at once and names the installed one. Or **Update panel** on the Updates page (admins): the panel (user `fairy`, no root) may only
  *start* the root oneshot unit `fairy-panel-update.service` (`deploy/fairy-panel-update.service`), allowed by one
  polkit rule (`deploy/fairy-panel.polkit.rules`: user `fairy`, that unit, verb `start`, nothing else). The unit
  runs `fairy update --yes --from-panel`, takes nothing from the web request, never installs an older or the
  same version, and writes its steps to `/var/lib/fairy-panel-update/status.json`, so the page shows
  downloading, verified, restarting and "running X" / rolled back, also after the panel's restart. The button
  needs a Pocket ID sign-in from the last **2 minutes** (always, also within the usual step-up window), the
  CSRF header and the kill switch off; it is audited as `panel.update`.
  **One-time setup**: `fairy update` run by hand as root installs (or refreshes) the unit and the polkit
  rule (`apt-get install polkitd` if polkit is missing). It writes nothing before you have said yes: it
  first says what it would write, and does it after an update that went through, or, when there is nothing
  to update, after a question of its own ("Set up the Update panel button now?"; `--yes` answers both).
  After "N", an error or a rollback nothing was written. If the button says what is missing, run
  `fairy update` once more.
  **A run that was stopped** (the host restarted during the update, or systemd ended the unit at its time
  limit of 18 minutes; 10 minutes while the unit is still the one an older release wrote, see
  [Upgrading to this release](#upgrading-to-this-release), step 3): the Updates page shows a red "stopped" with "stopped before it finished (while
  downloading, <time>)" for an hour, and the button works again. Nothing has to be deleted by hand. What the
  stopped download left next to the program (`/usr/local/bin/.download-*`) is removed by the next
  `fairy update` or button run once it is older than 15 minutes. Details: `journalctl -u fairy-panel-update`.
  **One update at a time**: a second `fairy update` (or the button) while one runs ends with "an update is
  already running on this host" and changes nothing (the lock is `/usr/local/bin/.fairy-update.lock`; it
  stays there, empty).
  **Before you update to this version**: one combination of the two session settings does not start any
  more, and `fairy update` does not check that for you. Look at
  [Upgrading to this release](#upgrading-to-this-release), step 2, first.
  **What an update does not touch**: `fairy update` and the button never replace
  `/etc/systemd/system/fairy.service`. They replace the program; run by hand as root, `fairy update` also
  writes the update unit and the polkit rule named above. What `deploy/fairy.service` has gained since your
  install (the memory limit, the sandbox lines) is added once by hand: the memory limit as in
  [Upgrading to this release](#upgrading-to-this-release), step 5; the whole unit as in SECURITY-FIXES.md,
  to-do list, item 2.
* **Agents**: admins see, in the panel, a banner when a newer Fairy-agent or Fairy-dashboard release exists
  (dismissed per release), an arrow next to each host's agent version when it is behind (click: release notes
  and **Start update**), a dot on the menu's **Updates** entry, and the **Updates** page (every agent's version
  and state, **Update** per host, **Update all**, the panel's own version vs the latest panel release). Update is
  a control action: admin, CSRF header, kill switch, step-up sign-in, rate limit, audit (`update.agent`), and only
  for agents with `self_update: true` (needs `control_token_env`) whose own config enables it; the agent then
  downloads, verifies and installs the release itself (see Fairy-agent's AGENT.md). Viewers see none of it.
  The panel only asks; it sends no "force". What an agent of Fairy-agent 0.6.0 answers, and how to read it:
  * "an update was installed on this machine less than 10 minutes ago; refused": the agent takes one update
    from the panel in 10 minutes. The Updates page shows the host as "failed" with this reason. Wait, then
    press Update again.
  * A release whose version does not start with a build number (a local build) is refused.
  * "rolled back to the previous version": the new version did not come up, and the old one runs again. The
    machine is fine.
  * "the previous version was put back and started again, but ...": the old program was put back and started,
    but its check could not be completed. Look at the machine; it is very likely running the old version.
  * "rolling back FAILED too": the agent may be down. Look at the machine.
* Machines that are normally off: [Update unpowered hosts](#update-unpowered-hosts) wakes them,
  updates their agent and switches them off again.
* The latest releases are looked up on GitHub at most every 10 minutes, in the background and only while an
  admin uses the panel; `updates: {check_latest: false}` switches that off on a panel without internet access.
  **Check for updates** on the Updates page asks GitHub now (both the agent and the panel release) and shows
  "Up to date", "N updates available" or the error, and when GitHub was last checked. It is read-only (no
  step-up, works with the kill switch on) but admins only, browser session, CSRF header, at most once every
  30 seconds for the whole panel, and audited as `update_check`.
* **One agent release per panel.** Add host and Re-pair install the Fairy-agent release this panel was built
  against (the Add host dialog names it), not the newest one. A panel older than its agents would put the
  older agent back on a machine at Re-pair; the Re-pair dialog warns when the machine runs a newer agent than
  the command installs. So: update the agents, then the panel, then Re-pair.

### Update unpowered hosts

For machines that are normally switched off: a PC that is only on while someone uses it, a Proxmox node
whose guests do not start by themselves. **Update unpowered hosts** on the Updates page (admins; the
section under the table of machines, folded until a run is going or ended less than 10 minutes ago by the
panel's clock: **Show the machines** opens it, and what you have opened stays open until you fold it)
starts a **run**: you tick the machines, confirm once, and the panel does by itself what you would do by hand.
For each ticked machine that is off:

1. It sends Wake-on-LAN through the machine's relay (the same request as **Power on** in the Power menu).
2. It waits for the machine to come online: up to 5 minutes, and it goes on as soon as the machine's agent
   answers. (The machine has to start, get its network and bring Tailscale up first.) A machine that does
   not come online in that time is given up on and reported; nothing more is sent to it. It may then still
   be starting, or be on without its network: look at it.
3. It asks the agent to update itself, exactly as the **Update** button does: the agent downloads the newest
   signed release, checks it and installs it. The panel sends no version and no "force".
4. It waits for the result of the update, and then one minute more.
5. It asks the agent for the usual delayed shutdown with a countdown of 2 minutes. The machine's page shows
   the countdown with **Cancel**, as for every shutdown from the panel.

The page shows each machine's step while the run works, and which wait it is in ("waiting for it to come
online, up to 5 min"). The report stays there afterwards (the last 5 runs). A finished run also makes one notification: the
machines that need a look first (left on, given up on, not confirmed, not updated), each with its sentence,
then the others. When a sentence for every machine does not fit (a notification holds 1500 characters), the
others are counted and the machines that need a look are all still named; no sentence is cut. The full list
is always in "Last run" (in the section; while it is folded, one line says how the last run ended and how
many machines need a look). A run that went well is no alert, so no pop-up shows that notification; the page
that watched the run says once that it is over, in the line at the bottom ("Update unpowered hosts: run
finished · 3 machines", with each machine's sentence behind **Details**), and it stays until you close it.
A machine the report calls "is off" is shown as offline at the same moment (its dot is grey, not amber for
another minute): the run saw it go off. While a run is going, the line under "Machines" says how many are
switched on, not how many could be chosen: none can until the run is over.

The rules, in plain words:

* **Off by default, per machine.** A machine takes part only after an admin switched it on for this: in the
  "Machines" table on the Updates page (**Show the machines**), or in the ⋯ menu on the machine's page. Switching it on asks "Confirm
  it's you". It is kept in the panel (`state.json`, `wake_update_hosts`), not in config.yaml. The machine
  needs an agent with `self_update: true`; to be woken and switched off again it also needs
  `power_control: true` and `wol` with a relay ([Agents](#agents)). The machine the panel runs on cannot take
  part. **Remove** (for a host added with Add host) switches the machine off for this as well: a host that
  is added again later under the same name starts off, like every new host.
* **A run only switches off a machine that was off before the run and that the run woke itself.** For
  that, all of this must be true: the panel had asked the machine for 15 minutes and got nothing at all (no
  answer of any kind: also no refused connection, no error answer, no connection that was taken and then left
  without an answer) when the run started; right before the wake the run asked the machine directly and got
  nothing at all either; the relay
  accepted a wake request for it (one whose answer was lost does not count: the packet may have gone out, so
  the run waits for the machine all the same, counting from before that request left, and never reports "it
  was not woken" about it, but unless a later wake request for it is accepted it leaves the machine on, with
  "the run's wake request for it got no answer, so Fairy cannot tell that the run woke it"); the machine
  first answered after it; and the uptime it then
  reported is shorter than the time since the wake, so it started after it. (That time is measured with a clock that does not move
  when the panel's clock is set, so a clock that jumps forward during the wait cannot make an old uptime
  look fresh.) If one of these is missing, the machine is left on.
* **Off, on, or not sure.** For a run a machine is one of three things, and the dialog lists them apart:
  * **Off**: nothing at all from it for 15 minutes or more. It is listed under "Off (no answer for 15 minutes or
    more)", ticked. The run wakes it, updates it and switches it off again.
  * **On**: its agent answers. It is listed under "On now: update only", unticked. The report says "It was
    on and stays on." A machine also counts as on when it refuses the connection (the machine runs, only
    its agent does not answer), when anything else than its agent's answer comes from its address (an HTTP
    error such as 401 or 500, a page that is not the agent's, a connection that is taken and then gives
    nothing), and a Proxmox node when the Proxmox API shows it as online. Nothing is sent to those; the
    report says "It is on (...), and its agent does not answer."
  * **Not sure**: its last sign of life is less than 15 minutes old. It may be restarting (a Windows Update
    restart looks like this for some minutes) or be on and out of reach. It is listed under "Not sure that
    they are off: left alone" and cannot be ticked. If a run meets such a machine, it sends it nothing, and
    the report says: "Not sure that it is off, so nothing was sent to it: Fairy takes a machine for off
    only after 15 min without an answer (it may be restarting, or be on and out of reach)."

  The 15 minutes are the default of a setting (build 14; they were fixed before): `wake_update.off_after_s`
  in config.yaml, 900, allowed 300 to 3600, and one machine can have its own (the timings below). Wherever
  this section says 15 minutes, read the value in force; the dialog and the report name it. 15 minutes is
  the safe value: a machine that restarts is silent too and comes back by itself, and with a lower value a
  restart that takes longer than it can be taken for "off" by a run, which then wakes the machine, updates
  it and switches it off as a machine it woke (`fairy admin config check` says so in a `note` line for
  every value below 900). The time counts from the machine's last sign of life: an
  answer from its agent, a refused connection, any other answer from its address (an error, a page that is
  not the agent's, an accepted connection that then gives nothing), or, for a Proxmox node, the Proxmox API
  showing it as online. Only "no route", or no answer at all to the attempt to connect, is silence.
  Each of these shows that the machine runs, so each starts the count again. A machine whose agent was
  stopped for 14 minutes (it refused the connection, and counted as on) and that then gives nothing at all
  for a minute is "not sure"; it counts as off 15 minutes after its last refusal, not before. The panel
  counts the 15 minutes itself, poll after poll: after a restart of the panel, after polling was paused for
  a machine, and for a machine that was just added, the count starts again. So for the first 15 minutes
  after a restart of the panel no machine counts as off. The silence is measured with a clock that does not
  move when the system's clock is set, and time the panel did not watch is not counted: a pause between two
  polls that is longer than the machine's poll interval allows (about half a minute at the default interval;
  the panel's host slept, the clock was set by more than that) starts the count again.
  What this cannot see: a machine that was on and out of reach for more than 15 minutes and then restarts
  by itself during the run's wait is treated as a machine the run woke.
  **A second limit: a machine switched on by a person while the run waits for it is treated as woken by
  the run.** The run cannot tell a power button from its wake packet. The window is the whole wait, not a
  moment: from shortly before the wake (a machine that is still in its firmware or boot loader gives nothing
  at all, like a machine that is off: seconds on a desktop, minutes on a server, without limit at a boot menu
  or a disk password prompt) until the run gives up on the machine (`online_timeout_s`: 5 minutes by
  default, up to 30; the wake is sent again every minute). Such a machine was off before the run, starts its
  system after the wake and reports a fresh uptime: every part of the rule is true, and it is updated and
  switched off, whoever switched it on. The same holds for a machine that starts by a firmware schedule or
  when the power returns. The panel cannot tell these apart: it would need the agent to say why the machine
  started (planned for a later agent release). That is why the report says "Came online after the wake",
  not "Woken", and why the machine's steps in the report say that it is treated as woken. Only one corner is
  cut off: a machine whose agent answers within 8 seconds of the wake cannot have been off, and is left on
  ("it was already starting"). So start a run when nobody is about to switch on a machine in its list, and
  keep `online_timeout_s` short.
* **A machine that is already on is never switched off.** When you open the dialog it is offered as "update
  only" (unticked). A machine you ticked as off that turns out to be on when the run gets to it is skipped
  and nothing is sent to it, unless you ticked "If one of these turns out to be on after all: update it
  too"; then it is updated and stays on.
* **A woken machine is switched off again also when its update failed** or was rolled back. The report says
  that the update did not work, and why (the agent's own words). The one exception: an update that never
  reports a result (15 minutes). That machine is left on and reported, because a shutdown in the middle of an
  install could leave it without a working agent. For the same reason an update request whose answer is lost
  (a timeout) is not taken for a failure: the agent may be installing. The run then goes by what the agent
  itself reports: an install that runs is waited for and its real result reported; an agent that reports no
  install after a minute was "NOT updated (the update request got no answer ...)"; an agent that does not
  say is "not confirmed" and the machine is left on. And whatever the run believes, it never asks for a
  shutdown while the agent says that it is downloading or installing an update. If such a request arrives
  late, during the shutdown countdown, the run calls the countdown off and leaves the machine on.
* **A woken machine that could not be switched off is reported as ON**: its agent did not answer again after
  the update, the agent refused the shutdown, someone cancelled the countdown, or the machine is still on 5
  minutes after the countdown ended (the run counts the countdown itself, from the moment its request was
  accepted: a machine whose clock is wrong, or a panel clock that is set meanwhile, changes nothing). The run never tries another way to switch a machine off: its shutdown
  is the agent's own, with the countdown that can be cancelled. If the agent can no longer take it (power
  control was switched off for the machine during the run), nothing is sent, also not a node shutdown through
  the Proxmox API (`node_power: true`), and the machine is left on and reported.
  When someone called the run's shutdown off on the machine's page and asked for another action there (a
  reboot, sleep, or a shutdown of their own), the run lets go of the machine as soon as its agent shows that
  action: it is reported as ON, with the action and its time, and a restart that makes it silent for a while
  is never reported as "switched off again". (An agent does not say whose a shutdown is. A shutdown of
  someone's own is told from the run's when it shows after the run's countdown is over and the agent has shown
  nothing pending in between; one that is asked for in place of the run's while that still counts down is
  taken for the run's.)
* **The report says what the run saw, not what it assumed.** Before a machine's result is written the run
  looks at it once more. An answer from its agent, or a refused connection, is "is ON". A machine that gives
  nothing at all after a shutdown was asked for, once its countdown is over and for 20 seconds, is "off (not
  answering)". That silence must be seen by the panel's own polls after the countdown's end, at least two of
  them in a row; silence from before the end does not count. A machine that was already out of reach while
  its countdown was still running, or whose polling is paused, is "not confirmed that it is off", with the
  power "on or off: not known": the run cannot tell a machine that went off from a connection that dropped.
  A machine whose polling is paused when its shutdown would be asked for is not asked: it is left on.
  (For a Proxmox node "out of reach" begins with the first poll of its agent that got nothing, also while the
  Proxmox API still listed the node as online: that list is a little behind when a node drops out. While it
  says "online" the node counts as on, as everywhere else.)
  When the run cannot see it either way within half a minute, the result says "Asked to switch
  off, not confirmed" (or "not confirmed that it is on"), with the power "on or off: not known". So: a
  **Cancel run** that comes after a countdown has run out reports that machine as off, not as "it is ON"; a
  shutdown request whose answer was lost (a timeout) is watched like one that was accepted, and the report
  says how it ended (if the run never saw its countdown and the machine then gives nothing, that is "not
  confirmed that it is off", never "off": the request may not have arrived; the same after a **Cancel run**
  whose own request got no answer). An agent that answers and shows no countdown after such a request has not
  got it so far, and no more than that: a request can reach the agent after the panel stopped waiting for its
  answer. So the run goes on watching. A countdown that shows late is the run's own, counted from when it
  showed, and its earliest end from the last answer that showed none: a machine that goes out of reach
  during that countdown is "not confirmed that it is off", as with a request that was answered. (Where no
  answer came between the request and the countdown, the run cannot know when it began: there a link that
  drops in the countdown's last part, with a shutdown that then does not happen, can still read as off.) If none has shown 5 minutes after the time it would have ended, the report says "The shutdown request
  got no answer ... The request may still arrive: the machine then switches off ... later unless someone
  cancels that on its page", with the power "on or off: not known". And a machine that refuses the connection after its countdown (its agent stopped, the
  system hangs in its shutdown) is never "switched off": after 5 minutes it is reported as ON.
* **Nobody is asked.** The run does not look whether someone is signed in or working on a machine it woke,
  and shows nothing on its screen before the agent's own shutdown notice. Use it for machines that are off
  because nobody is using them.
* **A Proxmox node with running guests is left on.** For a node the panel reads through the Proxmox API it
  counts the guests that are not stopped; one is enough. Only a guest the API reports as "stopped" counts as
  stopped: a paused or suspended guest, one whose state the API gives as "unknown" (or not at all), and one
  whose polling is paused in the panel keep the node on as well. If the panel cannot know (the API has not
  answered since the node came online, or the machine is a Proxmox host that the panel knows only through
  its agent), the node is left on, and the report says why. "Since the node came online" is decided by the
  time of day and by the panel's steady clock, and both must agree: a list from before the node was up does
  not count as new because the panel's clock was set back. And the list must be from now: before the shutdown
  request, and at every look during the countdown, the run asks the Proxmox API anew, however seldom the panel
  polls it otherwise (`intervals.proxmox`, the poll menu), and takes only a list that was asked for after its
  own question and is not older than 20 seconds. If none comes within 30 seconds, the node is left on. The
  panel counts the guests the API lists for
  its token: **the token must be allowed to see every guest** (role `PVEAuditor` on `/`, as Add host makes it;
  a token of your own in `proxmox:` needs `VM.Audit` on the guests). A guest the API does not list for the
  token does not exist for the panel, and a node whose guests are not listed is treated as a node without
  guests: it is switched off. If the cluster lists more than the panel takes (64 nodes, 2000 guests), the node
  is left on, because the panel then does not see all of them. While the node's shutdown countdown runs, the run
  asks again every few seconds: a guest that starts or arrives on the node then (until 15 seconds before the
  countdown ends) calls the countdown off, and the node is left on and reported.
* **One run at a time**, at most 16 machines in it. A second start is refused with a message. **Update
  panel** is refused while a run is going.
* **Cancel run** is never refused because of other control requests: it has a request budget of its own (60
  a minute for one admin), and so has the **Cancel** of a shutdown countdown.
* **The limit on control requests** is 20 a minute for one admin (`control_rate_per_min` in config.yaml; it
  was 6, which switching seven machines on for these runs in a row already met). Over it the answer is "too
  many control requests; try again in N s". An agent has its own limit (12 a minute, in its own config), and
  a run sends one agent at most 8 a minute: both are unchanged.
  It starts nothing new: no further wake, no further update. The dialog asks what happens to
  the machines the run has already woken: they stay on (the default; the report lists them as ON), or they
  are still switched off again (an update that is running is waited for first).
* **A restart of the panel ends a run.** It is never continued. After the restart the page and a notification
  say which machines the interrupted run had woken and how far each one was; look at those machines and
  switch them off yourself. (The run keeps a small journal for this, `wake-update.json` in the data folder:
  machine ids, steps and times, no secrets. If that file cannot be read, no run can be started until you have
  looked at it and moved it away. The panel starts all the same, also when what is there is not a plain file
  (a link, a named pipe): the panel reads its state files only as plain files of a sane size. An empty machine entry (`null`) in the file
  is left out, with one line in the log. Every request of a run is entered in the journal before it goes
  out. If the file cannot be written (a full disk), the run sends nothing further, stops and says so, the
  machines it has woken stay on and are reported, and no run starts until the file can be written again.)
* **A machine the run lets go of during its countdown** (it was paired again, or switched off for these
  runs, while the shutdown the run had asked for was counting down) has that countdown called off first, by
  the same rule as below: the machine stays on and is reported so, and if the cancel does not get through,
  the report says that the machine may switch off, and when. (Up to build 13 the run left the countdown
  running with nobody watching the machine.)
  The run calls off only its own countdown. A machine's cancel stops whatever is pending on it, so the run
  looks first at what the agent shows: a shutdown with another countdown length than the run asked for was
  asked for by someone in its place (on the machine's page: 10 s, 1, 5 or 15 minutes; the run's is
  `shutdown_delay_s`, 2 minutes unless you set it) and is left alone; the report says so and when the machine
  switches off. And after a **Re-pair** the id may be another machine: the run sends nothing until the agent
  that has the id now shows the run's own countdown (the same end, the same length). If it never does, the
  report says that the run does not know whether the machine it woke switches off, and when it would (power
  "on or off: not known"); look at that machine. If the run had already sent a cancel before the Re-pair, the
  report says so too: one that got no answer may have arrived, and then that machine stays on. (A shutdown
  you ask for with exactly the run's countdown length in the place of the run's cannot be told apart and is
  called off like the run's.)
* **The kill switch stops a run**: nothing further is started, and what was woken stays on and is reported.
  A shutdown countdown the run had already started is called off (the one request a stopped run still sends,
  and only while that countdown can still be running: after its end the run sends nothing, so that an action
  someone asked for on the machine's page since is not called off; the same holds for **Cancel run**). The
  cancel is also sent when the run's shutdown request had got no answer: it may have arrived. When the agent
  answers "nothing is pending" to it, the request may still arrive later: the run asks again every 10 seconds
  for as long as it would itself wait for that request (until 5 minutes after the countdown's end, also when
  the stop comes after that end), and calls off the countdown that shows. If none shows, the machine's line
  says that the request may still arrive, with the power "on or off: not known".
  While the agent shows another action as pending in that time (a reboot somebody asked for on the machine's
  page, or a shutdown with another countdown length than the run asked for), no cancel is sent, and the line
  names that action. (A shutdown that somebody else asked for in that time with exactly the run's countdown
  length cannot be told from the run's own: the cancel calls it off.)
  One case inside the countdown in which it is not sent: the machine's agent already shows another action
  than the run's shutdown as pending (someone called the shutdown off on the machine's page and asked for a
  reboot, say). The agent's cancel would call that one off, so the run sends none, and the machine's line
  says which action is pending.
  A cancel that does not get through is tried again every 10 seconds until the countdown's end, and if none
  gets through, the machine's line says so and names the time it may switch off.
  Calling a countdown off is the one control action that works while the kill switch, or the power switch of
  a host, is on: the page's **Cancel**, the Power menu's **Cancel pending** and **Cancel run** only ever stop
  an action. The audit entry of such a cancel says that control was off.
* Everything is in the audit log: who started the run, with which machines, and every wake, update,
  shutdown and result (`wakeupdate.*`, `power.wake`, `update.agent`, `power.shutdown`, each with the run's
  id).

What to know before the first real run:

* **Windows: switch Fast Startup off** on the PCs that take part (Control Panel, Power Options, "Choose what
  the power buttons do", untick "Turn on fast startup"; or `powercfg /h off`). With Fast Startup, "Shut
  down" is a kind of hibernate and Windows keeps counting its uptime, as it does through sleep and
  hibernate. The run then cannot tell such a PC from one that was on all along, and leaves it on. That is
  the safe side (a PC that was asleep may have someone's open work), but it is not what you want for a PC
  that was shut down. The report says so as a warning: "It answered after the wake but did not look freshly
  started (up for ...): it may have been asleep, or use Fast Startup. It was left on." A machine you sent
  to sleep from the panel is shown as "asleep or off" and is listed apart in the dialog, not ticked.
* The wake needs what **Power on** needs: Wake-on-LAN enabled in the firmware and on the network card, and
  a relay agent in the same network that is on.
  For a Proxmox node with an agent (`node:`), write its `wol:` in `systems:` under the node's id,
  `<id_prefix>node-<node name in small letters>` (for example `lab-b-node-pve-b`), not under the agent's id.
  The panel also finds that entry after it was restarted while the machine is off, when the machine is shown
  under its agent's id until its Proxmox API answers again: **Power on** and a run can wake it then too (up
  to build 12 they said that no Wake-on-LAN was set up). For that the panel must know from config.yaml alone
  which node is the agent's, and it only takes what leaves no doubt:
  * `links_to: <the node's id>` in the agent's entry says it outright, and always counts first.
  * With one `proxmox:` entry, `node:` is enough: the node is `<that entry's id_prefix>node-<name>`.
  * With several `proxmox:` entries, the entry is the one whose `id_prefix` the agent's own id begins with
    (agent `lab-b-agent`, `id_prefix: lab-b-`); an empty `id_prefix` never counts. When the id begins with
    the `id_prefix` of more than one entry (`lab-` and `lab-b-`), the longest one counts, so a host whose
    `id_prefix` begins with its site's needs no `links_to:` (up to build 13 it did). If the id begins with
    none of them, give the agent `links_to:`.
    A node worked out this way is said, so that you can see a wrong one: one line per agent in the panel's
    log at start and a `note` line of `fairy admin config check`, "agent lab-b-agent (node: PVE-B) -> node
    lab-b-node-pve-b: inferred from id_prefix lab-b- ...; set links_to in the agent's entry to override".
    It is wrong for an agent of the entry with the shorter prefix whose id happens to begin with the longer
    one (an agent on the cluster `lab-` that is called `lab-b-2-agent`): give that agent `links_to:`.
  * No other agent's entry may mean the same node (the same `node:` name, or a `links_to` to it): then
    each of these agents needs its own `links_to:`. With several `proxmox:` entries a `node:` name can
    mean the node of that name of every entry: two hosts whose node is called `pve`, each with an agent
    `node: pve`, need `links_to:` on both agents.

  The rows follow the same decision (build 14): an agent is shown as a part of the node the configuration
  names, and of no other. Up to build 13 the rows took the first node of that name that was there, in the
  order of the `proxmox:` entries, so with two entries that both have a node `pve` an agent could be shown
  inside the other host's row, and what was pressed on that row (shut down, restart, fans, update) went to
  that agent's machine. Where config.yaml does not decide (the cases above that need `links_to:`) the rows
  go by the node's name, but never against the agent's id: an agent whose id begins with the `id_prefix`
  of a `proxmox:` entry is a part of that entry's node (the longest `id_prefix` counts) or has a row of its
  own, and is never shown inside a node of the same name of another entry. Only an agent whose id begins
  with no `id_prefix` is put into the first node of that name that is there, which can be another
  machine's: give it `links_to:`. The row of a node belongs to one agent, the first in config.yaml that
  names it; a second agent that names the same node keeps a row of its own.

  Where the panel cannot tell, nothing is guessed for Wake-on-LAN: the machine has none until its Proxmox
  API has answered once. Every agent whose node config.yaml does not decide is named once in the panel's
  log at start and by `fairy admin config check`, with the row it is shown in and what to add: a WARN line
  when a `systems:` entry is written for such a node (that entry is not found for the agent), a `note`
  line otherwise.
* The run paces itself: at most 8 control requests a minute to one agent (an agent accepts 12 a minute; the
  rest stays free for what you do in the page). With many machines behind one relay the wakes follow each
  other more slowly; nothing is lost.
* The timings can be changed in config.yaml (`wake_update:`, see `deploy/config.example.yaml`): how long to
  wait for a machine (default 300 s), the pause before the shutdown (60 s), the countdown (120 s), the
  pacing, and after how long without an answer a machine counts as off (`off_after_s`, 900, 300-3600). A
  value outside its range stops the panel at start with the key and the line; that includes 0, which does
  not mean "the default" here. For the default leave the key out.
* One machine can have its own `off_after_s` and `shutdown_delay_s` (build 14): a `wake_update:` block in
  its `systems:` entry, same ranges (300-3600 and 10-3600), any other key in the block is refused:

  ```yaml
  systems:
    - {id: pc-a, wake_update: {off_after_s: 360, shutdown_delay_s: 30}}
  ```

  A key that is left out is the panel's. The entry is found under both ids of a machine (the Proxmox node's
  row `<id_prefix>node-<name>` and its agent's id); written under both with different values, the larger
  one counts. The run uses the machine's values for everything about that machine: when it counts as
  off (still the unbroken silence the panel counted itself, see above), the countdown its agent is given,
  how long the run waits for it to go off, and the sentences of the report. The dialog says them in the
  machine's line when they are not the panel's. One machine has one `systems:` entry: two entries
  with one id are refused when config.yaml is read ("systems: two entries with the id ..."; `fairy update`
  asks the new version about the file first and stops before it replaces anything). Write the block into
  the entry that has the machine's `wol:`. The one exception is a machine added with "Add host" with
  Wake-on-LAN, whose `wol:` entry the panel makes: for it, write an entry in config.yaml with its id and
  nothing but `wake_update:`; the panel puts the two together. An entry whose id is no machine's (a typing
  mistake) is a `note` line of `fairy admin config check`.

### The check on the machine itself

Normally the panel decides: you confirm an action in the panel ("Confirm it's you"), and the panel sends it
to the machine with its control token. Whoever has that token, or the panel itself, can then do the same
without you. With **the check on the machine itself** the machine decides: before it changes its fans, shuts
down, restarts, sleeps or updates its agent, it verifies a Pocket ID approval (your passkey) that was made
for exactly that action. It checks offline, with Pocket ID's signing key that it has pinned. The panel only
carries the approval; it cannot make one and cannot skip the check.

It is off for every machine until you turn it on, and machines with an older agent work as before.

**1. Create a second client in Pocket ID**, next to the one the panel signs in with:

| Field in Pocket ID | Value |
| --- | --- |
| Name | `Fairy PC actions` (this is the name Pocket ID shows when it asks for the passkey) |
| Callback URL | `<public_url>/auth/approval/callback`, for example `https://panel.example.com/auth/approval/callback` |
| Public Client | **off**, and the client secret as `FAIRY_APPROVAL_CLIENT_SECRET` in `fairy.env`. On works too (nothing to store), but then a person who is on a machine's list and has that machine's control token can get an approval from Pocket ID without the panel: no entry in the panel's Audit log, and the kill switch is not asked. |
| PKCE | on |
| Requires Re-Authentication | **on**. This is what makes Pocket ID ask for the passkey every time. Without it Pocket ID answers from its own session, and the machine refuses the approval as too old (after Pocket ID's 60 seconds of grace). |

Do not reuse the sign-in client: the panel refuses the same Client ID for both (it does not start, and
`fairy admin config check` says "approval.client_id is the sign-in client's Client ID ..."), and the machines
accept only approvals made for this one.

**2. Tell the panel** (config.yaml), then restart it:

```yaml
approval:
  client_id: "<the Client ID of Fairy PC actions in Pocket ID; not the sign-in client's>"
  # issuer: https://id.example.org     # only if it is another Pocket ID than oidc.issuer
  # users: ["<Pocket ID user id>"]     # may approve too, besides the panel's admins and a host's owner
  # owners: {Straw: "<Pocket ID user id>"}   # who the owner label "Straw" is in Pocket ID
```

`sudo fairy admin config check` shows the client, the callback address to enter in Pocket ID and whether the
secret is set (present / not set, never its value). The Pocket ID of the approvals must be at an `https://`
address: a machine takes no other. With an `http://` one the panel starts, says so in its log and in the
config check, and every machine is "Not ready: the issuer must be https". nginx needs no change: the callback is under `/auth/`
like the sign-in's. With `deploy/nginx.example.conf` that is at most 10 round trips a minute per address
(burst 10), sign-ins included.

**3. Turn it on.** The **Approvals** page (admins; in the menu, next to Updates) lists every machine with how
its check stands (on, off, not supported, offline, needs accepting) and says "N machines are
ready for the check on the machine itself", with the button **Turn on the check for N machines** (when no
machine is ready there is no button, and one line says so). A row has one button, **Approval**, which opens that
machine's own dialog (it reads **Review**, in red, when the machine waits for an accept: nothing is accepted in
the row). A row has no button while the panel has nothing of the check to open for its machine: an agent that
says nothing about the check and has no record in the panel ("not supported", or one the panel has not heard
from since it started), or a machine that has no row on the Overview right now (its dialog is its row's; when
it waits for an accept, the row says "needs accepting", and the button comes with the machine's row). A machine's own page has the button **Approval** in
its header, and the dialog behind it has **Turn on** for that machine. The Updates page keeps one line with a
link where an update meets the check. Turn on is deliberate and proven:

* The dialog shows exactly what each machine will trust: the Pocket ID address, the client id, the signing
  key(s) Pocket ID publishes right now (key id and thumbprint) and the list of people who may approve there,
  by name and user id. By default these are the panel's admins and the host's owner, if `approval.owners`
  says who that is in Pocket ID; `approval.users` adds people. The list is never empty.
* You confirm **once**, with your passkey at Pocket ID, for all chosen machines (at most 64). Pocket ID's
  page names the client you made (`Fairy PC actions`); the panel shows its **Client ID** only, because a
  client's name is Pocket ID's to know. If Pocket ID answers "redirect_uri ... is not registered for this
  client", `approval.client_id` is another client's (usually the sign-in client's), or the Callback URL is
  missing in that client.
* Each machine accepts its new trust only together with that approval, and only if the approval verifies
  with the very values it is asked to trust. So a wrong address, client id, key or user id is found now, not
  at the first shutdown. Nothing is written otherwise.
* The result is shown per machine and written to the Audit log: accepted, the proof was refused, already
  set, offline, or "not known": the machine was sent its trust and gave no answer, so it may have been
  written. Look at the machine's **Approval** then; if it shows the check on and "Not on record", accept its
  id there.

**Afterwards it is locked.** The panel cannot change or remove a machine's trust, and neither can anybody
with the panel's tokens. It is changed only on the machine, as root / Administrator:

```
fairy-agent helper-trust show                  # what this machine trusts, and whether it can be used
fairy-agent helper-trust set --fetch-keys      # after a key rotation at Pocket ID: pin the new key(s)
fairy-agent helper-trust set --user <id> ...   # another list of people (the users given replace the list)
fairy-agent helper-trust set --new-machine-id  # on a machine made from a copy of another one's disk
fairy-agent helper-trust clear                 # off again: nothing needs an approval; the panel can Turn on once more
```

**Which machine an approval is for.** Every approval names the machine it is for, by a **machine id** in that
machine's trust file, and a machine accepts only an approval with its own id. A machine cannot tell whether
another machine has the same id, so the panel keeps the id it has **on record** for each host
(`machines.json` in the data folder, written like the other state files, mode 0600) and makes every approval
with that one, never with what a host answers at that moment. An id comes on record in two ways only: when you Turn on from the panel (the
panel makes it up), or when an admin accepts the id a host shows (below). Never because a host shows it: a
machine id is no secret, and a host that answers in another machine's name would show that machine's id.
The record belongs to the host's entry: a host you remove in the panel loses it, and a host you add in the
panel starts with nothing on record, whatever was kept for its id before. The record of a host you take out
of `config.yaml` or rename there does not count from the next start on (a line in the log says so): under its
new name the machine is "Not on record" and needs one accept. Such a record is not deleted from
`machines.json` for that. A host that is only missing at one start (an added host whose tokens file could not
be read, a list of added hosts that was moved away, a typing mistake in an id) has its record again when it
is back under the same id.
In the panel, admins see it in the host's Approval dialog (the **Approval** button on the host's page; hover
"Machine id"); other users and API tokens get no id from the panel. It is a name, not a secret: a machine shows its own id to whoever may read its `/metrics`,
and the machines of one batch see each other's (Known limits, below).

From then on the record counts. The Overview shows a red key sign behind the host's name; on the host's page
the **Approval** button is red, with a red line under the header; and the dialog behind that button and the
Approvals page say which of these it is:

* **the host has the check on and no id is on record for it** ("Not on record": the check was set on the
  machine itself or from another panel; or its id came on record "at first sight" in an earlier build of
  this panel, which counts as none now);
* **the host shows another id** than the one on record ("Machine id changed": its trust was set anew on the
  machine, or something else answers in its name);
* **two hosts have the same id** (a machine made from a copy of another one's disk, or a host that hands the
  panel's questions on to another machine): both are flagged, because each would accept an approval made
  for the other;
* **a host that had the check shows none** any more.

While a host is flagged, the panel asks it for no approval and sends it no fan, power or update request,
and it is in no run. An admin resolves it in the host's dialog: press **Review** in its row on the Approvals page, or **Approval** on the host's page, and in the dialog **Accept
this machine's id** (not on record), **Accept this machine's new id** (changed) or "Accept that its check is
off", in a browser, behind "Confirm it's you", with an entry in the Audit log. Before you accept an id,
compare it with the machine's own: on the machine `fairy-agent helper-status`, line `machine id:`. A
machine that answers in another's name shows the other's id. "Confirm it's you" says the same and shows the
id **in full**, on a line of its own that can be selected (and under it the id on record, where there is
one): what comes on record is exactly the id that dialog showed. The comparison catches something that sits
between the panel and the machine, and an agent that answers in another's name. It does not catch a host
whose own administrator is hostile: only the real machine being on record in this panel stops that.
For two hosts with one id, one id is accepted for one host only. The host that has the id on record has the
button **This host keeps its machine id**; the other one has no button: its dialog names the host that keeps
the id and says what to do (on that machine `fairy-agent helper-trust set --new-machine-id`, then accept the
new id on its page). A host that shows an id which is on record for another host never gets it, however it
came to show it, and the panel refuses an accept for it, whoever sends the request. Two hosts that are both
not on record and show one id each have the button, and the first accept decides. Every accept is for the
id the dialog showed: a machine that shows another one by the time you have confirmed is refused ("it
changed while you were confirming"). Accept only when you know why it changed.

**What you see afterwards.** A fan, power or update action on such a machine opens the usual dialog, which
now says that the machine checks this itself and, in one line, what your passkey signs ("shutdown in 60 s",
"fan zone 1 to 30%"), and then Pocket ID asks for your passkey under the name of that client ("Fairy PC
actions", if you named it so). One passkey per action: the panel's own 3-minute window does not count for these machines.
Two exceptions, both decided by the machine and shown in its Approval dialog:

* **Fan window** (default 30 s, at most 60): after one approved fan change, further fan changes on that
  machine need no new approval for that long, so a slider can be moved.
* **Shutdown after boot** (default off): one shutdown without approval shortly after the machine started.
  It is set on the machine (`helper-trust set --boot-shutdown N`).

**Update all** asks for one approval that covers every machine with the check on; each machine verifies its
own entry. The dialog lists every machine the button will update: the ones that check it, the ones that are
not sent to (and why), and the ones without the check, which are updated on the same confirmation. What is
sent afterwards is that list: a machine that was not in it (it did not answer when you pressed the button)
gets "not sent", and the next press is for it. The
result is a count ("2 started, 1 not") with one line per machine behind **Details** (started, refused, not
sent, offline, no answer), and it stays until you close it. A machine whose request got no answer of its
agent (a timeout, 502, 504) may have started all the same: it is "no answer", counted apart ("1 started, not
known for 1 (no answer)") and never under "not". The button's number, the dialog's list and that count are
the same machines; one that was not answering when you pressed the button is "skipped", not counted.

**Cancel** of a pending shutdown, restart or sleep needs no passkey and no "Confirm it's you", with or
without the check: an admin's session in a browser is enough, so a 10-second countdown can be stopped in
time. If the machine had already begun to switch off when the cancel reached it, the page says "Too late"
and not "cancelled".

A shutdown, restart or sleep request that got no answer of the machine's agent ("no answer (http 504): it may
have arrived") may be counting down although the page shows no countdown yet. For the time such a request
can still arrive (its delay plus 5 minutes) the Power menu offers **Cancel pending** for that machine, and
the cancel is sent to its agent whatever the page shows. The agent's own answer is shown as it is ("... may
still be scheduled: send the cancel again"); when it answers that nothing is pending, the page says exactly
that and not "cancelled". Without such a request a cancel is sent only while a countdown shows, as before.
The panel keeps this in memory: after a restart of the panel Cancel is there again once the countdown shows.

**Update unpowered hosts** does not ask for approvals yet. A machine with the check on is not taken
into a run: its row says "needs approval: not supported in runs yet", before and after "Switch on", also
while the machine is off and after a restart of the panel (the panel has it on record). Update it and
switch it off on its own page. "Switch on" for such a machine asks one question, and it says both things: no
run can take it for now, and once that is over a run you start may wake, update and switch it off. Nothing
is switched on before that question was answered (the API refuses the switch with `ask_first` until the
request says `"asked": true`, or comes back from "Confirm it's you").

**When something is wrong** the page says which thing, not "failed":

* **broken**: the machine has a trust file it cannot use and refuses fan, power and update requests until it
  is repaired on the machine (`fairy-agent helper-trust show` names the reason). The Overview and the Updates
  page flag such a machine.
* "approval refused (…)": the machine did not accept the approval; the reason is in brackets (`user`: you
  are not on its list; `token_key`: Pocket ID has a new signing key, run `helper-trust set --fetch-keys` on
  the machine; `clock` / `auth_time`: the machine's clock or the age of the passkey use).
  `nonce`: what answered is not the machine the panel has on record for this host; do not try again, look
  at the host.
* "The helper that runs on ... is older than its agent" (Windows, after an update of the agent): press
  **Stop helper**, then **Start helper**, next to the host's name. Until then the machine refuses fan, power
  and update requests.
* "Try again in N s": the machine has had its requests for this minute (it counts questions, actions and
  cancels apart). The panel does not try again by itself.
* A long refusal shows its first sentence; the machine's own words are behind **Details**, and the message
  stays until you close it.
* API tokens cannot give an approval: a control token gets 403 for these actions on such a machine.

**Known limits.**

* One approval that covers several machines (Turn on for several, Update all) is sent to each of them with
  the whole list of entries, so every machine of that batch receives the machine ids of the others. Each
  machine needs the list to work out what the passkey signed. An id alone opens nothing: it names a machine,
  and it comes on record for a host only by Turn on or an admin's accept. But what a machine of the batch
  receives is the whole approval, so it also holds a usable approval for the other machines' entries: whoever
  has that copy and another machine's control token can start the action that was approved for that machine
  (exactly that one, for example its update, and nothing else) once, as long as that machine has not used
  its entry itself and its challenge lives (15 minutes for an update). What would close it: sending the
  other machines' entries as hashes, which is a change of the protocol and needs a later agent release.

**Windows and Linux** are treated the same by the panel. On Windows the elevated helper
(`fairy-agent-helper`) checks what goes through it and the agent checks the rest with the same trust file;
on Linux the agent checks everything. The Approval dialog says who checks ("Checked by").

### Add host

Admins: **Add host** on the Overview. Give the machine's Tailscale name (the panel reaches it at
`http://<name>:9101`; an address can be given instead), its system (Windows / Linux), site, options (power
control, self-update, Wake-on-LAN relay, a local UPS through NUT, Core Temp on Windows, IPMI on Linux), whether
it is a Proxmox node (`node:`), and optionally its Wake-on-LAN MAC(s) and the relay that wakes it. The panel
makes a one-time join code (160 random bits, shown once, grouped as `ABCD-EFGH-...`, valid 15 minutes, single
use; the panel keeps only a key made from it, in memory) and one command:

Linux:

```
f=$(mktemp) && curl -fsSL --proto '=https' -o "$f" BASE/install.sh && echo "SH_HASH  $f" | sha256sum -c - && printf '%s' 'CODE' | sudo env FAIRY_RELEASE_URL=BASE sh "$f" --sums-sha256 SUMS --join-stdin --pair-v2 --panel PANEL [options]
```

Windows (PowerShell as Administrator, one line):

```
$u='BASE'; $b=(New-Object Net.WebClient).DownloadData("$u/install.ps1"); if ((([Security.Cryptography.SHA256]::Create().ComputeHash($b) | ForEach-Object { $_.ToString('x2') }) -join '') -eq 'PS1_HASH') { $env:FAIRY_JOIN_CODE='CODE'; try { & ([scriptblock]::Create([Text.Encoding]::UTF8.GetString($b))) -ReleaseUrl $u -SumsSha256 SUMS -PanelAddress PANEL -PairV2 [options] } finally { Remove-Item Env:FAIRY_JOIN_CODE -ErrorAction SilentlyContinue } } else { throw 'install.ps1 is not the release the panel checked: not run' }
```

The panel fills in every value. The words in capitals only show where its values are (the two lines are the
same as in Fairy-agent's AGENT.md, "Add a host from the panel"):

| In the line | What the panel puts there |
| --- | --- |
| `BASE` | the address of one release, `https://github.com/n3ptune-cpu/Fairy-agent/releases/download/build-<number>`. Never `latest`. |
| `SH_HASH`, `PS1_HASH` | the SHA-256 of that release's `install.sh` / `install.ps1`. A script with any other content is not run. |
| `SUMS` | the SHA-256 of that release's `SHA256SUMS` file, which the panel checked with its own release key. |
| `CODE` | the one-time join code. |
| `PANEL` | the panel's Tailscale address (an IPv4 address such as `100.64.0.10`, an example value). Only this address may pair. |
| `[options]` | what you ticked. Linux: `--power-control`, `--self-update`, `--wol`, `--ipmi`, `--proxmox`, `--ups` or `--ups-name NAME`. Windows: `-PowerControl`, `-SelfUpdate`, `-Wol`, `-CoreTemp`, `-Ipmi`, `-Ups` or `-UpsName NAME`. |

On a Proxmox node the Linux line has no `sudo` (run it as root).

The command never runs "the latest" script. It names one Fairy-agent release: the one this panel was built
against (the Add host dialog shows it: "Installs Fairy-agent release build-N"; `enroll: {agent_release:
build-N}` in config.yaml picks another). Before it shows a
command the panel downloads that release's `SHA256SUMS`, `SHA256SUMS.sig`, `VERSION`, `install.sh` and
`install.ps1` and checks them with the release key built into the panel (`release/signing.pub`); a panel build
without the key, an unreachable GitHub or a bad signature refuses Add host (HTTP 503) instead. The command runs
the script only if its SHA-256 is the one the panel checked, and points it at the same release. It also passes
the SHA-256 of the `SHA256SUMS` the panel verified (`--sums-sha256 <hash>` / `-SumsSha256 <hash>`): the
installer refuses any other `SHA256SUMS` before it trusts a line of it, so even a first install rests on the
panel's key.

The join code is on no program's command line, where every user of the machine could read it while the
installer runs. On Linux the shell's own `printf` writes it to the installer's standard input
(`--join-stdin`); on Windows the line sets it as an environment variable of that PowerShell window
(`FAIRY_JOIN_CODE`), which the installer reads and removes. On Windows the script is never a file before it
runs: the line downloads it into memory, checks the SHA-256 of exactly those bytes and runs them, so nothing
can be swapped between the check and the run. (The line stays in the shell's history with the code, which is
single-use and valid 15 minutes.) There is one form of the command per system; a release whose install script
cannot take it (anything before Fairy-agent v0.6.0) is refused, see below. The installer
checks every file it downloads against `SHA256SUMS` and its signature, installs the agent like the manual install, makes its tokens on
the machine and waits for the panel. The panel asks the machine about every 15 seconds (13 to 17), which stays
inside the agent's limit of 5 pairing requests a minute, so pairing follows the command within about 17 seconds.
If the agent still answers that its limit is reached (something else is asking it too), the panel waits 30 and
then 60 seconds between requests and says so in the dialog; the code stays valid. The dialog follows the pairing
(waiting -> paired -> online) and Cancel stops it. Creating a code is a control action (admin, CSRF, kill switch,
step-up sign-in, rate limit, audit `host.add` / `host.pair`); API tokens cannot do it.
When the code was made right after "Confirm it's you", the page fetches it once. The panel gives it only to the
browser and address that confirmed, and only while that confirmation still counts; anyone else who holds the
session cookie gets 403 and the code stays where it is.

Pairing is `POST /pair` on the agent, answered once, only to the panel's address, at most 5 tries a minute; then
the agent switches pairing off for good. The panel speaks **pairing v2** only. The command carries `--pair-v2`
(`-PairV2` on Windows), so the installer stores a key made from the join code instead of the code's hash. Every
request of the panel carries a new random value and a check value made with that key; the agent answers only
such a request, with its tokens encrypted and authenticated under a key bound to both the join code and the
request. So the panel knows the answer comes from the machine the code was given to (anything else that answers
at that address is not added, and the audit log says so: `host.pair unverified`), and nobody who reads the
exchange learns the tokens. Hosts paired this way show "Pairing: verified" on their page; hosts added by an
older panel show "not verified" until they are re-paired. The join code proves that the answer comes from a
machine the command was run on; which machine that is, the panel cannot know. The dialog and the audit entry
therefore show what the machine calls itself next to the address you typed ("it calls itself AGENT-2, at
agent-2", example names): look at it when you add a host.

There is no fallback to the old exchange, in which the panel sent the same proof every time and the agent
answered with its tokens in clear:
- A Fairy-agent **release** whose install script does not know `--pair-v2`, or takes the join code only on
  its command line (older than Fairy-agent v0.6.0), is refused by Add host and Re-pair (HTTP 409) before any
  code is made; the message names what the script lacks. Choose a newer one with
  `enroll: {agent_release: build-N}` or update the panel.
- An **agent** from before pairing v2 that already runs on the machine answers the panel's request with an
  error. The dialog then says the agent is too old and must be updated first. Nothing is lost: the command
  shown in the dialog installs the newer agent, and the panel pairs with the same code as soon as that agent
  is up (the code stays valid for its 15 minutes).
- Such agents stay monitored and controlled as before; only pairing them again needs the newer agent.

**What the dialog says while it waits.** The panel asks the machine until it pairs, the code runs out (15
minutes) or you press Cancel. None of the answers below ends the Add host or uses up the code. The dialog
shows a sentence; the API (`GET /api/admin/enrollments/{id}`) also has a `hint` for some of them.

| What answers at the address | The dialog says (start of the sentence) | `hint` |
| --- | --- | --- |
| nothing, or the machine is off | "waiting for the machine (...)" | |
| a new agent that has no join code yet (404 `pairing_inactive`) | "the agent answers but has no join code yet: run the command" | |
| an agent from before pairing v2 (400 `bad_request`, or no `/pair` at all) | "the Fairy-agent on that machine is too old for this panel" | `agent_too_old` |
| a new agent that holds a code for the old pairing (400 `wrong_version`) | "the agent on the machine holds a join code for the old pairing" | `legacy_join_code` |
| an agent that holds another code (403 `wrong_code`) | "the agent has a different join code (an older command?): run this one" | |
| an agent that was given another panel address (403 `wrong_address`) | "the agent refuses: the panel address in the command (...) is not where the panel connects from" | |
| an agent whose limit of 5 pairing requests a minute is reached (429) | "the agent has answered all the pairing requests it takes in one minute" | `rate_limited` |
| something that says "paired" but cannot prove it has the code | "not added: something answers at ..., but it is not a Fairy agent that has this join code" | `unverified_answer` |
| a name that leads to an address outside Tailscale | "not contacted: ... is not a Tailscale address" | `not_tailscale` |
| any other answer (for example 415 or 500) | "the agent answered 415" | |

For `agent_too_old` and `legacy_join_code`: run the command shown in the dialog on the machine. For
`unverified_answer` the dialog also shows a yellow box, "Not a Fairy agent". Nothing that such an answer
contained is used or stored. If the command has not run on the machine yet, another program or another machine
answers at that address: check the name and the address. If it has run: Cancel, make a new code, run the new
command. When the code runs out in that state, the Add host ends as "expired" and still says what answered.

Hosts added this way are kept in `<data_dir>/managed-hosts.yaml` and their tokens in
`<data_dir>/secrets/agent-<id>.json` (0600), not in config.yaml; both are merged at start (an id in config.yaml
wins and the managed one is skipped with a log line) and checked by `fairy admin config check`. A list that
holds no host while tokens of added hosts are still in `secrets/` (the file was emptied, cut off or lost) does
not stop the start, but it is said: one WARN line at the start, on the Add host dialog and in the config check.
Their page has
**Re-pair** (a new code, e.g. after the panel lost the tokens) and **Remove** (stops polling, deletes the tokens;
the agent stays installed until uninstalled on the machine); both ask for the host's name to be typed and a
step-up sign-in, and are audited. Hosts in config.yaml are not changed by the panel. What a machine answers on
Re-pair is checked like a new host's, against everything else the panel has: an answer that would collide with
another host or Proxmox node is refused and the host stays as it was. A host that is its own Proxmox node must
answer with the node name it was added as, unless **The node was renamed** is ticked in the Re-pair dialog.
If it answers without a Proxmox API token (the node has joined a cluster, or the command could not make a
token), it is kept as a plain host and the dialog says so: its own node and guests are no longer shown, and
its agent is not attached to any node, also not to a node of another cluster that has the same name. To show
it as a node again, Re-pair once more when the node can make its token, or add its cluster under `proxmox:`
in config.yaml.
When the machine runs a newer Fairy-agent than the release the command installs, the Re-pair dialog says so
before the command is run; when the agent that answers afterwards is an older build than before, the dialog
and the audit entry (`host.repair`) say that an older build was installed.

Add host and Re-pair only pair over Tailscale. An address outside 100.64.0.0/10 and fd7a:115c:a1e0::/48 is
refused in the dialog, and a name is checked every time the panel connects to it: when it leads anywhere else
(the local network, a stale record), no connection is opened and the dialog says where the name leads. Over
Tailscale the path is encrypted and addresses are authenticated, so nobody outside the machine can listen to
the pairing or answer in its place. `enroll: {allow_non_tailscale: true}` in config.yaml switches the check
off for machines that are only reachable over another network (default: `false`).

The check covers pairing and nothing after it. Once a host is paired, the panel polls it at the same name
with its tokens, like every agent in config.yaml, and those requests are not checked against the Tailscale
ranges: they are plain HTTP to wherever the name leads at that moment, the read token goes with every poll
and the control token with every action, and an answer is not signed. This is the same as for every agent in
config.yaml and it has not changed in this release. Keep the names of your hosts on the tailnet (MagicDNS
names or 100.x addresses); a name that a local DNS server can answer is only as safe as that network.

The two refusals that are on by default, and how each is changed:

| The panel refuses | Why | To change it |
| --- | --- | --- |
| a Fairy-agent release or agent that is too old for pairing v2 | the old pairing cannot prove which machine answered | no setting: there is no way back to the old pairing. Use a newer release (`enroll: {agent_release: build-N}`) |
| an address outside Tailscale | only there the path is encrypted and the addresses are authenticated | `enroll: {allow_non_tailscale: true}` in config.yaml, then restart the panel |

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
dismiss, per user. "Critical" in the list and its filter is what it is for the pop-ups below: an event that
starts an alert, or one of severity critical; each person sees the hosts and UPSes of their sites (events without a host are for admins).
Kept in `<data_dir>/notifications.json` for 30 days (at most 2000). The Overview shows one line, "N active
alerts", for alerts whose condition still holds. Warnings and critical events also go to `FAIRY_NOTIFY_URL`
(ntfy or Discord) when it is set.

**Pop-ups.** A critical event that is made while a page of the panel is open also shows in the lower right
corner (a strip at the bottom on a phone) for 5 seconds and then goes away. It stays in the list and the bell;
nothing is marked as read. "Critical" here is: a host goes offline, a temperature passes its limit, a UPS goes
on battery, runs low or cannot be reached (the events the Overview counts as active alerts, and severity
"critical"). Nothing from before the page was opened is shown. At most three show at a time; more wait, and
more than five waiting become one "N more" pop-up that leads to Notifications. Pointing at a pop-up or having
the keyboard focus in it stops its time, × closes it, Escape closes the newest, a click opens the host's page.
Per user, in **Settings**, "Pop-ups": on or off, 3 to 30 seconds, "Critical only" or "Critical and warnings"
(also a failed update, an unstable host, a failed Add host, a UPS overload), and **Show a test pop-up**, which
shows a sample in that browser and makes no notification. The stream carries them as `notes_new`.

What one machine reports again and again is counted, not repeated: a temperature that goes over the limit and
back, an agent that reports another version, a failed update. Per host there is one such event in 10 minutes,
its text ends in "(x7)", and it is pushed once. The list still ends in the state the host is in: a temperature
alarm that came back holds again. One source (a host, a Proxmox API with all its nodes, a UPS source) holds at
most a quarter of the list and gets at most 5 push messages a minute; the sixth is one line that says so, and
the next message of that source says how many were left out. Everything is on the page either way. When the
list is full, the source with the most entries gives up its own, so a standing alarm of another host stays.

### Sessions

**Sessions** in the menu (top right) shows where you are signed in: one line per browser, with the browser
and system ("Firefox on Windows"), the address, when it signed in and when it was last used. The line marked
**This session** is the window you are looking at. **Sign out** ends one of the other sessions, **Sign out
everywhere else** all of them; the browser that was signed out shows the sign-in page within a few seconds.
Admins also see "Everyone's sessions" and can sign any of them out. Viewers have the page for their own
sessions. A person who is waiting for access does not have it.

* Signing another session out asks "Confirm it's you" every time, also seconds after an earlier
  confirmation. So somebody who got hold of a copy of your session cookie cannot sign you out and stay in.
  The session you are using is ended with **Sign out** in the menu.
* A copy of your cookie is not a line of its own: it is the same session as the browser it was taken from.
  It shows under the address of that line as "also used from ...". A phone or laptop that changes networks
  shows the same. If such an address is not yours: confirm any action (that gives your session a new id, and
  every older copy stops working), or sign out and in again.
* Sessions end by themselves: 4 hours after the last use and 1 day after the sign-in, unless config.yaml
  says otherwise (`session_idle_timeout`, `session_max_age`).
* The address is the one in the audit log: the visitor's own when nginx is set up as below
  (`trusted_proxies`), nginx's otherwise. No location is looked up. "Unknown browser" and "unknown" stand for
  what was not recorded: a session from before this page, until it is used again.
* Every sign-out from this page is in the audit log (`sessions.revoke`, `sessions.revoke_others`).
  `fairy admin sessions list | revoke | revoke-all` on the panel host work as before. The list shows the
  sign-in and the last use the way this page does, and marks an entry whose session has ended by age or
  idleness as "ended" (the file keeps such an entry for up to 30 days; this page leaves it out).

### Custom logo

Copy your image to `/etc/fairy/logo.png` (PNG, JPEG, WebP or GIF, at most 1 MiB; SVG is refused because it can
carry script; make it readable by the service, e.g. `0644`), set `branding: {logo: /etc/fairy/logo.png}` in
config.yaml and restart Fairy. It replaces the built-in logo in the sidebar and on the sign-in and waiting
pages, and becomes the favicon. The logo is checked by its first bytes only (they must be those of a PNG,
JPEG, WebP or GIF): it is your own file, and it is not read to its end like the sign-in wallpaper below. A
missing or invalid file only logs a warning. To try one first:
`fairy serve --demo --logo C:\path\to\logo.png` (Windows) or `fairy serve --demo --logo ./logo.png`.
The file is read once at start (path from the config only) and served at `/branding/logo` without sign-in (204, no content, when no logo is set).

### Sign-in wallpaper

An admin can put a picture behind the sign-in page: **Settings**, "Sign-in page", **Upload picture** (PNG,
JPEG or WebP, at most 8 MiB) and **Remove picture**. The sign-in page and "Waiting for access" show it over the
whole window, with the name, the message and the button in a strip along the bottom; without a picture the
page is black, with the same strip.

* The picture is **public**. The sign-in page comes before any sign-in, so everyone who can open the panel's
  address can load it (`GET /branding/wallpaper`, like the logo). Use one that may be seen.
* It is always a file the panel keeps (`<data_dir>/backgrounds/signin-wallpaper`, mode `0600`), never an
  address on another site: the sign-in page loads nothing from elsewhere. What the file is called does not
  matter; its content decides, and SVG, GIF, HTML and everything else is refused. The file has to be a whole
  picture of its type (its parts are read up to its end mark and its checksums checked, without unpacking
  it), not only a file that begins like one; the same holds for a background a user uploads. A JPEG may have
  more data after its end mark (cameras write some there): the panel stores and serves that as it came.
* A picture may have at most 8192 pixels a side and 40 megapixels, whatever its file size: a browser needs
  four bytes of memory for every pixel, and a 76 kB file can state 20000 x 20000. A larger one is refused
  with its size in the message; the same holds for a background a user uploads (GIF and AVIF there too: for
  a GIF every image in the file counts, not only its header, and for an AVIF the size its items or tracks
  state; a file that states none where a decoder reads it is refused). A picture that was stored
  before this limit is kept and shown, and the panel's log says so once at start: replace it with a smaller
  one.
* Your own background (the block above it in Settings) is separate: per account, shown only to you after
  the sign-in.
* Only admins can change it, from a browser session, and a change asks for "Confirm it's you" like the
  other admin actions (the same window, `step_up_window_s`; with `step_up_per_action: true` every time): the
  picture is what everyone sees before any sign-in. An upload outside the window is checked first (a file
  the panel will not take is refused at once), then the picture waits in the panel's memory for up to 3
  minutes and is stored right after the sign-in. Your own background never asks. Setting and removing the
  sign-in picture are in the audit log (`settings.signin_wallpaper_upload`,
  `settings.signin_wallpaper_delete`; the Audit page's "Settings" filter).
* Behind nginx the upload needs the exception the background upload has: `client_max_body_size 9m` for
  `/api/admin/signin-wallpaper` (`deploy/nginx.example.conf`). Without it nginx refuses a file over 128 KiB,
  and the Settings page says so.

### Behind nginx in another container (and Cloudflare)

`deploy/nginx.example.conf` is a complete server block. In short:

* Fairy: `listen: <this LXC's LAN IP>:8080` and `trusted_proxies: ["<nginx IP>/32"]` in config.yaml; firewall
  port 8080 so only nginx reaches it (nftables in the LXC, or `IPAddressAllow=` in the unit). Fairy warns at
  start when it listens beyond loopback but trusts only loopback proxies.
* nginx: `set_real_ip_from <Cloudflare ranges>; real_ip_header CF-Connecting-IP;` and
  `proxy_set_header X-Forwarded-For $remote_addr;` (overwrite, not `$proxy_add_x_forwarded_for`). Fairy then
  takes the right-most X-Forwarded-For entry that is not a trusted proxy: the real visitor.
* `limit_req` on `/auth/` and `/api/`, buffering off for `/api/stream` (SSE), `client_max_body_size 128k`
  (9m for `/api/settings/background` and `/api/admin/signin-wallpaper`). Security headers are Fairy's: do not
  add them in nginx.
* The secrets file stays `0600 root:root`: systemd loads it (`EnvironmentFile=`), the unit passes
  `--env-file ""`. `/etc/fairy` itself is `0750 root:fairy` and `config.yaml` `0640 root:fairy`. Run the CLI as the service user: `sudo -u fairy fairy admin ...`. One exception: `sudo fairy admin config check` (as root; it only reads, and user `fairy` cannot read `fairy.env`).

## Upgrading to this release

From the release before it. This one adds pages and settings (Sessions, "Update unpowered hosts", pop-ups, the
sign-in picture, model names) and shortens how long a sign-in lasts when config.yaml does not say. Nothing has
to be converted and no file has to be deleted. The update itself signs nobody out.

### 1. The order

1. **Look at the two session settings**: section 2. One combination does not start any more.
2. **Then the panel.** On the panel host, as root: `fairy update`.
   Before it, `find /var/lib/fairy -maxdepth 1 -name '*.json' -size 0` (your data folder) must print nothing:
   a `users.json` or `state.json` of 0 bytes, left by an old power cut, makes the update roll back until the
   file is restored from a backup or moved away.
   The state files must be plain files: `find /var/lib/fairy -maxdepth 2 -type l` must not list `users.json`,
   `state.json`, `sessions.json`, `tokens.json`, `managed-hosts.yaml` or a file in `secrets/`. This release
   does not read a state file through a symbolic link: it refuses to start, and the update rolls back. Put the
   file itself there (`cp --remove-destination <target> <link>`). A data folder that is itself a link is fine.
   **Update first, then add new config keys, then restart.** The one new key of this release is the
   `wake_update:` block. The program you run today does the update, and it refuses a config.yaml that has the
   block ("field wake_update not found"). So write the block only after the update, then
   `systemctl restart fairy`. If the block is already in the file: take it out (or put `#` in front of its
   lines), update, put it back, restart. What happens otherwise is in the paragraph after this list.

3. **Then `fairy update` once more**, as root, and answer yes to "Set up the Update panel button now".
   The first run was done by the program you had, and it wrote its own, older
   `fairy-panel-update.service`: the one with a time limit of 10 minutes (`TimeoutStartSec=600`). This
   release's unit has 18 minutes, which the **Update panel** button's run is planned for (15 minutes and a
   rollback). The second run says "its files are older than this version" and writes the new unit after the
   yes; nothing else is changed. Until then the button works with the older limit. The same holds after any
   later release that changes the unit: the button never writes it, a run by hand does.

If config.yaml already has the `wake_update:` block when you update: on a panel that listens on the default
`127.0.0.1:8080` the update goes through. On a panel with another `listen:` address (the nginx layout) the
old `fairy update` asks the wrong address after the restart, puts the old program back, and the old program
does not start with the block: the panel is down until you take the block out and run
`systemctl restart fairy`. From this release on `fairy update` asks the address the service really listens on
(the unit's `--listen`, `FAIRY_LISTEN`, `listen:`, also from a config.yaml it cannot use otherwise), says
"update first, then add new config keys, then restart" when the installed version refuses the file, and
stops before it replaces anything when the running panel does not answer at that address. The **Update
panel** button goes one step further: when the installed version refuses config.yaml it stops before it
replaces anything ("config.yaml is not usable by the installed version: ..."), because a file that the new
version refuses too (a typing mistake) would end with the panel down and the page gone. `fairy update` by
hand warns and goes on.
The file all of this is about is the one the service starts with: the unit's `--config`, else
`FAIRY_CONFIG_FILE` from the unit's `EnvironmentFile=` / `Environment=` or from the env file the service
reads itself, else `/etc/fairy/config.yaml` (before, `fairy update` always read the last one). `fairy update
--config FILE` names another file. When systemd does not say how `fairy.service` is started, or shows a
command line with a space or a quote in an argument (systemd prints the arguments without quotes, so where
a path with a space ends is not certain), the update stops ("cannot tell which config file fairy.service
starts with ...") and asks for `--config`. The unit's `Environment=` is read the way systemd prints it (an
assignment with a space or a bracket in it stands in quotes: `"FAIRY_CONFIG_FILE=/etc/fairy/my dir/c.yaml"`,
`"FAIRY_LISTEN=[::1]:8080"`); a line that is not written like that stops the update in the same way. A flag
that stands twice in the unit counts where it stands last, as for `fairy serve`. The **Update panel** button also looks at the file the running
panel was started with before it starts anything, and says on the page what is wrong with it.
The next check is made by both, after the download and before anything is replaced: the new version, once its
signature and hash are verified, checks the config file itself, with the check `fairy serve` starts with
and `fairy admin config check` reports (the entries of `ups:` and `power_sources:` included, which the
update did not look at before). A file it refuses stops the update with the line: "the new version (...)
refuses /etc/fairy/config.yaml: config: line 131 (ups, entry 1): ... Nothing was changed." Correct the file
and update again. (A semantic mistake, such as an address that is not one, is named without a line.)

**The limit on control requests** is now 20 a minute for one admin when config.yaml does not say (it was 6).
A config.yaml copied from the older example file has the line `control_rate_per_min: 6` and keeps 6: change
the number or take the line out, then `systemctl restart fairy`. `fairy admin config check` shows the number
in use.

Nothing has to be done on the agents for this panel: every agent the panel before it worked with works with
this one, and it asks nothing new of them.

### 2. Before `fairy update`: the two session settings

When config.yaml sets neither `session_max_age` nor `session_idle_timeout`, a sign-in now ends after **one
day**, and after **four hours** in which nobody used the page. Until this release that was 7 days and 12
hours. Open `/etc/fairy/config.yaml` and look for the two keys:

| In config.yaml | After the update |
| --- | --- |
| neither key | 1 day and 4 hours |
| both keys | your values, as before. The example file used to set `session_max_age: 604800` and `session_idle_timeout: 43200`: a config.yaml copied from it keeps those long values until you change or remove the two lines |
| only `session_max_age` | your value; the idle timeout is 4 hours, or the whole session when that is shorter |
| only `session_idle_timeout`, one day or less | your value; the session is one day |
| only `session_idle_timeout`, **more than one day** | **the new panel does not start**: `session_idle_timeout (2d) must not be longer than session_max_age (1d)` |

In the last case, before the update: add a `session_max_age` that is at least as long, or make the idle
timeout shorter.

### 3. Behind nginx: one more location

Only when nginx is in front of the panel and an admin wants to upload a picture for the sign-in page: that
upload needs the exception the background upload already has, `client_max_body_size 9m` for
`/api/admin/signin-wallpaper`. The block is in `deploy/nginx.example.conf`. Without it nginx refuses a
picture over 128 KiB and the Settings page says so. See [Sign-in wallpaper](#sign-in-wallpaper).

### 4. What you will notice afterwards

1. **Nobody is signed out by the update.** The session cookie and its signing key are the same as before.
   What ends a sign-in is its age: with the new defaults (section 2) one that is older than a day, or a tab
   nobody used for four hours, signs in again.
2. **Sessions** is new in the menu: where you are signed in, and Sign out for the other places. Signing
   another session out asks "Confirm it's you" every time. A session that was open before the update is
   listed with "Unknown browser" until that browser signs in again or confirms an action ("Confirm it's
   you"). See [Sessions](#sessions).
3. **Update unpowered hosts** is new on the Updates page: it wakes machines that are normally off,
   updates their agent and switches them off again. Nothing happens until an admin switches a machine on
   for it and starts a run; a script with an API token can do neither. A machine counts as off for a run
   only after 15 minutes without any sign of life (no answer, and no refused connection either), counted
   by the panel since its own start: for the first 15
   minutes after this update's restart no machine is offered as off. See
   [Update unpowered hosts](#update-unpowered-hosts).
4. **Pop-ups.** A critical event that is made while a page of the panel is open (a host goes offline, a
   temperature passes its limit, a UPS goes on battery) also shows in the lower right corner, for 5
   seconds. They are on for everyone; each person changes that in **Settings**, "Pop-ups": on or off, 3 to
   30 seconds, critical only or warnings too. See [Notifications](#notifications).
5. **Settings, "Sign-in page"** (admins): a picture behind the sign-in page. It is public: everyone who can
   open the panel's address sees it before signing in. See [Sign-in wallpaper](#sign-in-wallpaper).
6. **Set model name** (admins): a host's page, the ⋯ menu. The name is shown in place of the model the
   machine reports; **Clear** brings that back.
7. **The Overview is one line per host.** Under the name, small, the machine's model; in front the mark of
   its system; behind the name small signs for the elevated helper, the battery and the UPS, where the host
   has one (point at a sign, or click it). Threads, memory and the system are on the host's page.
8. **"Panel not reachable".** When a page hears nothing from the panel for ten seconds and a knock gets no
   answer, a strip says so on every page and the status dots turn grey, until the panel answers again.
9. **Scripts that read `/api/notifications`**: the filter `severity=critical` now returns what the page
   calls critical (every event that starts an alert, such as a host that goes offline, and every event
   whose severity is critical), and `severity=warning` the other warnings. Before, both went by the stored
   severity alone, where "host offline" is a warning. The `severity` field of each event is unchanged.

### 5. One step by hand: the memory limit (only an install older than the release before this one)

`fairy update` replaces the program, never `fairy.service`. An install made before the release before this
one has no memory limit, unless it was added then. Check: `systemctl show fairy -p MemoryMax` (536870912
means 512M: nothing to do). Otherwise add it once:

1. `sudo systemctl edit fairy`
2. Write these two lines, then save:

   ```
   [Service]
   MemoryMax=512M
   ```
3. `sudo systemctl restart fairy`
4. Check again with the command above.

What else the release before this one asked for (its order with the agents, three settings and the session
secret) is in that release's own README.

Going back to the older panel: the update kept the program it replaced as `/usr/local/bin/fairy.old`, until
the next update replaces that too. As root: `systemctl stop fairy`, `cp /usr/local/bin/fairy
/usr/local/bin/fairy.new` (to keep this one), `mv /usr/local/bin/fairy.old /usr/local/bin/fairy`,
`systemctl start fairy`. (`fairy update --force` is not the way: it installs the latest release again, not
the older one.) The older panel reads the files this one writes (`users.json`, `notifications.json`,
`managed-hosts.yaml`, `state.json`, `settings.json`, and `sessions.json`, where it ignores what this one adds).
It does not start when config.yaml has a `wake_update:` block: take the block out first. The same goes for
an `approval:` block ("field approval not found"). Build 13 and older know the `wake_update:` block but not
its `off_after_s`, and no `wake_update:` block in a `systems:` entry: take those out before going back to one. (They are the two keys that are new in this release; do
it before you go back.) The older panel does not know `machines.json` and leaves it alone; a machine with
the check on still refuses what comes without an approval, so the older panel cannot change its fans, switch
it off or update it until the check is cleared on the machine (`fairy-agent helper-trust clear`). A panel older than
the last release also does not start with `enroll: {allow_non_tailscale: ...}`, `step_up_per_action`, or a
text such as `4h` in the two session settings: take those out as well (write the session settings as whole
seconds). The older panel does not know the pop-up settings: when someone saves a setting there, that
person's pop-up settings (on or off, seconds, events) are the defaults again after the next update.
Everything else is kept in both directions: custom model names, the machines switched on for "Update
unpowered hosts", users, tokens, sessions and the sign-in picture.

## Security model

* Pocket ID only (OIDC code flow + PKCE S256, nonce, state). No passwords anywhere. Pocket ID (allowed_groups /
  allowed_emails, matched only for a verified email) decides who may sign in; nothing else. That is looked at
  at every sign-in and every "Confirm it's you". When Pocket ID then says the account is not allowed (any
  more), all sessions of that account end on the panel at once (audit: `auth.login`, "denied: not in an
  allowed group", `sessions_revoked`). When the groups are not in the ID token the panel reads them from
  Pocket ID's userinfo; if that request fails, the sign-in fails with a page that says so and no session is
  ended (nothing was said about the account). From userinfo the panel takes the groups, and a name or e-mail
  address to show where the ID token has none; when a sign-in happened, whether an e-mail address is verified
  and everything else come from the signed ID token alone, and a userinfo answer about another account
  refuses the sign-in. Between two sign-ins the panel does not ask Pocket ID.
* No release has a way around that sign-in. The test build (`go build -tags fairytest`,
  [TESTING-LOCAL.md](TESTING-LOCAL.md)) has no sign-in, and so it is separate code that a build without the tag
  does not contain: a test of the normal build checks that, and the release workflow stops before signing if a
  built program says it is a test build. `fairy update` does not install one either: it reads the build tag
  out of the downloaded, verified file and stops before anything is replaced ("... was not installed: it is a
  test build ..."). A test build refuses to start unless it listens on loopback only, has
  no `oidc:` block and no session or OIDC secret, and its data folder is new or its own. It signs in only a
  browser that opens the address it prints at start, which carries a random start key (kept in its data
  folder, mode 0600); every other request gets 403. It also refuses a request for another host name or with a
  reverse proxy's usual headers, but those checks cannot see every proxy or port forward: a test build must
  never be put behind one, and the start key is the last line of defence if it is. (`fairy serve --demo`,
  simulated data on loopback only and only under the names 127.0.0.1, ::1 or localhost, is in every build.)
* The check on the machine itself (optional, per machine; "The check on the machine itself" above): a machine
  that has it on verifies a Pocket ID ID token itself before a fan, power or update action: signature against
  the key(s) it pinned, issuer, audience (the approval client, `approval.client_id`), a user on its own list, a passkey
  use that is seconds old, and a nonce that is a hash of its own one-time challenge and the exact request.
  The panel asks for that token in a round trip of its own (`/auth/approval/callback`, PKCE, the kept
  action's id as state) and checks it too, for the audit log and to fail early; the token is kept in memory
  with the one kept action, passed to the machines it was asked for, and never logged, stored or sent to the
  browser. An approval is not a sign-in: it opens no window and changes no session. Opening the address of
  an accepted approval a second time (a reload, the Back button) runs nothing and sets no cookie: for 2
  minutes the page says what came of the action (already sent; still on its way; or refused or ended with an
  error, with the details in the message on the panel's page and in the audit log), and never "nothing was
  done" for an action that was sent. Over the limit on sign-in attempts this address answers the same small
  page as the sign-in's, with the seconds to wait. The trust is set from
  the panel once, with a proof, and after that only on the machine. An approval names the machine it is for
  (the machine id in its trust file), and the panel makes it with the id it has on record for the host
  (`machines.json`), never with one a host answers with a challenge: a host that hands the panel's question
  on to another machine gets an approval that machine refuses. An id comes on record only when the panel
  issues it at Turn on or when an admin accepts it, never because a host shows it. A check that is on with
  no id on record, a changed id, one id on two hosts and a check that is gone stop every approval for the
  host until an admin accepts it (browser session only, "Confirm it's you", audited). In the panel, machine
  ids are shown to admins in a browser only, and the audit log has their first six characters. They are
  names, not secrets: the machines of one batch receive each other's ("Known limits" under "The check on
  the machine itself").
* Roles, managed in the panel (`users.json`, Users page or `fairy admin users|sites`), keyed by the OIDC subject:
  **admin** sees everything, may use control actions, the Audit and Users pages; **viewer** is read-only and sees
  only the systems, guests, power sources, UPSes (and their processes, sensors, history) of their sites;
  **pending** (first sign-in of anyone else) sees a "waiting for access" page and gets 403 from every data API.
  Filtering is server-side for every endpoint and the SSE stream (`internal/web/view.go`); hidden ids answer the
  same 404 as unknown ones; a viewer's summary counts, cluster totals and the Overview's all-hosts CPU / memory totals (`totals` in `/api/summary`) only add up what they can see.
* An account is its OIDC subject; the e-mail address is only what the account says about itself. The Users page
  and `fairy admin users list` mark an address "unverified" unless Pocket ID sent `email_verified: true` at that
  person's last sign-in (Pocket ID sends `false` until an administrator there marks the address as verified, the
  person confirms it by e-mail, or Pocket ID is set to treat every address as verified). An unverified address
  never finds an account (`fairy admin users ...`, the role API), and never matches `allowed_emails` or
  `bootstrap_admins`. Signing in and the role of an existing account do not depend on it.
* Sites: a host's site is the Users page's assignment, else `site:` in config.yaml (proxmox / agents / systems),
  else (guests) its node's site. Power sources and UPSes follow the hosts they are attached to unless assigned.
  No site = admins only.
* Bootstrap / upgrade: only while `users.json` has no admin at all, a user without a `users.json` entry who is in
  `oidc.control_groups` or whose **verified** email (`email_verified: true`) is in `oidc.bootstrap_admins` becomes
  admin at their first real sign-in (a fresh Pocket ID login; never from the
  groups cached in a session cookie, and never for someone an admin removed). After that the panel decides.
  Removing a user also ends their sessions and revokes their API tokens. The last admin cannot be demoted or removed (server-enforced; the CLI needs `--force`).
* Step-up: control actions need a sign-in from the last `step_up_window_s` (default 3 min); the re-login sends
  `max_age=0` (optionally `prompt=login`) and the callback requires a fresh `auth_time` (at most 2 min old).
  A missing `auth_time` never counts as fresh: the identity provider must send it (OIDC Core requires it when
  `max_age` is requested); otherwise step-up fails with a page that says so and control actions stay locked.
  The sign-in time is kept on the server (memory only, never in the cookie) and counts only for the browser and
  address that confirmed it: a copy of the session cookie used from anywhere else is asked to confirm, every
  step-up gives the session a new id (an older copy of the cookie is signed out), and after a panel restart the
  first control action asks again. `step_up_per_action: true` removes the window: every control action needs
  its own sign-in, which confirms exactly that one action.
* The action that waits for "Confirm it's you" is kept on the server (memory only, 3 minutes) and belongs to
  the browser and address that asked for it. It is kept only for the client the session was signed in or last
  confirmed from: the same cookie used from anywhere else is refused and nothing of it is kept. Only "Continue"
  in the dialog (a write with the CSRF header, from that same client) can have it run, for the one sign-in that
  follows, and only when that same client comes back from Pocket ID. No link can do that: opening
  `/auth/login` in any form runs nothing and throws the waiting action away. The dialog and the audit log
  (`auth.step_up`, fields `for` and `runs`) name the action in the panel's own words. What runs is the request
  as it was first routed (same route, same parameters, same body). It runs to its end also when the browser
  leaves in the middle (a reload, a broken connection), and it runs once. Opening the address of an accepted
  confirmation a second time (a reload, a lost answer) never hands out a session or a confirmation: a browser
  that already holds the new session is sent to its page again, and one that still holds the cookie from
  before gets a page that says what came of the action (already sent; refused and not carried out; ended with
  an error; or still on its way), and signs in again. The panel recognises such a
  repeat for 2 minutes (memory only) by the confirmation's state and code, the client's address and its
  browser; any other repeat is refused as before.
  A refusal at the address Pocket ID sends the browser back to (cancelled there, started more than 10 minutes
  ago, an answer that does not check, the limit, Pocket ID not reachable, an account that is not allowed) is
  a small page without script: what happened in one sentence and one link back to the panel. Which check of
  Pocket ID's answer failed is in the server log and the audit log only.
* Sessions: HMAC-signed `__Host-fairy_session` cookie (HttpOnly, Secure, SameSite=Lax, host-only; the plain name
  `fairy_session` only with `FAIRY_INSECURE_COOKIES`) that is valid only while its id is
  in `sessions.json`, within the idle timeout and absolute lifetime; revocable with `fairy admin sessions`.
  The idle timeout counts real use only: writes the panel accepts (a request it refuses, for example a form
  post from another site, never counts), and requests the page marks `X-Fairy-Active: 1` because there
  was pointer / key / touch input in the last minute (`POST /api/me/active` is its explicit signal). The live
  stream and the page's timers never count, so a tab nobody touches signs out, and its stream ends.
* The Sessions page (`/api/me/sessions`, `/api/admin/sessions`): a person sees and ends their own sessions, an
  admin everyone's. The browser never gets a session id: a session is named by a keyed hash of its id (not a
  cookie, not guessable, looked up only among the sessions the caller may end). Ending a session other than
  one's own current one always needs its own "Confirm it's you": a confirmation made for another action does
  not count, also not inside `step_up_window_s`. Reading the list needs the session only, like every other
  read, so whoever holds a copy of a cookie can see that person's sessions; they cannot end them. What the
  page shows of a browser is the client's own text (User-Agent, cut to 256 bytes, control characters removed)
  and is escaped; the address is taken as everywhere else (`trusted_proxies`). The audit log has every
  sign-out with who, whose session and from which address, and never a session id or cookie. That holds for
  `fairy admin sessions revoke <id from the list>` too: its entry names the account and the address of the
  session it ended, not the characters that were typed.
* "Update unpowered hosts" (`/api/admin/wake-update`, `internal/wakerun`): a run sends only what an
  admin can already send by hand, through the same code and the same checks (wake through a relay, the
  agent's own update, the delayed shutdown, its cancel); there is no new agent request and nothing is run on
  a machine. Starting a run always needs its own "Confirm it's you", also inside `step_up_window_s`, and the
  confirmation is for exactly the machines and options in the request: the dialog and the audit log show the
  list and a short number made from it, and what runs after the sign-in is that same request. Admins only,
  browser session only (no API token), CSRF header, kill switch. A machine takes part only after an admin
  switched it on for this (a control action of its own). One place in the code decides whether a machine
  may be switched off (`mayShutDown` in `internal/wakerun/shutdown.go`), from what the run itself saw; a
  machine that was on is never switched off. The run's requests go through the per-host power switch, the
  kill switch and the audit log like every control action (the cancel of a countdown passes both switches,
  for a run as for a click), and are paced below the agents' own limit. Text
  from an agent (an update's error) is shown as text. The journal has no secrets and is never read as an
  instruction: after a restart the panel reports and does nothing.
* API tokens: `fy_…`, CLI-created only, stored as SHA-256, scope `read` or `control`, optional expiry, never
  accepted on `/auth/*`, the settings endpoints or the Users page, nor (they cannot "confirm it's you") for
  stopping / starting the elevated helper, Add host, Re-pair, Remove, cancelling a waiting Add host, and for
  "Update unpowered hosts" (starting a run, cancelling it, switching a machine on or off for it), which need a browser sign-in. Otherwise `control` acts as an admin; `read` sees every
  site, or only `--sites A,B`. A token owned by a panel user is capped to that user's current role and sites; a token whose owner is not
  (or no longer) in `users.json` has no access. Only `--owner system` tokens keep their own scope.
* Writes: admin role (or control-scope token), CSRF header `X-Requested-With: fairy` + Origin /
  Sec-Fetch-Site checks for browsers, kill switch, per-host power flags, rate limits, typed confirmation for
  destructive power actions, append-only JSON-lines audit log (+ Audit page), optional ntfy/Discord notification.
  One write passes the kill switch and the per-host power flag: the cancel of a pending shutdown, reboot or
  sleep countdown (it only stops an action; everything else in this list applies to it). Past the kill switch
  it goes for a signed-in admin only: an API token's cancel is refused while the kill switch is on (423), like
  every other write of a token.
  The audit log is bounded: lines are capped at 8 KiB, the file rotates by size (`audit_max_mb`, `audit_keep`;
  the Audit page shows the current file), and entries anyone can cause without signing in (rejected API tokens,
  failed or rate-limited sign-ins) are limited to 10 per client IP per minute and 300 in all, the rest summed
  up in one `audit.suppressed` entry a minute. An audit write failure never blocks a sign-in but is logged loudly.
  Changes to users and sites: admin browser session only, CSRF checks, step-up (same window), audited + notified;
  the kill switch does not block them.
* `trusted_proxies` is used to find the client IP: for rate limits, the audit log, and as the address a
  "Confirm it's you" is bound to (a wrong list makes every client look like the proxy). By default the list
  is the panel's own host (127.0.0.1 and ::1), for a reverse proxy that runs there: so every program on that
  host can connect to the panel and name any client address in `X-Forwarded-For`. A local account there that
  also holds a copy of an admin's session cookie can pass for the address that confirmed; keep other accounts
  off the panel's host, or the panel in a container of its own. The login limiter
  (`login_rate_per_min`) counts per client IP, and per user for a signed-in step-up, so attempts from other
  addresses do not use up an admin's budget for confirming a control action. A copy of the admin's own cookie,
  or another site that makes the admin's browser open `/auth/login` about 20 times a minute, still does, and
  each such request makes a waiting "Confirm it's you" forget its action: "Continue" then answers "too many
  sign-in attempts" (with the seconds to wait) or "no longer waiting" until the minute is over. Nothing runs because of it.
* Strict CSP, HSTS (https), frame/referrer/permissions policies, generic 500s, no secrets in logs or errors.
* The sign-in wallpaper is, like the logo, handed out without a sign-in. Its route (`/branding/wallpaper`)
  reads no file name or other parameter from the request: it sends the one stored picture or 404, as
  `image/png`, `image/jpeg` or `image/webp` (what the content was found to be at the upload, checked again at
  every start), with `nosniff` and its own `Content-Security-Policy: default-src 'none'`, at most 4 downloads
  at a time per address. The page's Content-Security-Policy did not change for it.
* Sign-out is a POST with the CSRF header (no cross-site sign-out); visiting `/auth/login` keeps a valid session
  until a new sign-in completes (a sign-in that fails because `users.json` cannot be written ends it too).
* A sign-out that cannot be written to `sessions.json` (disk full, read-only folder, wrong owner) answers 503
  and says so. The session is then ended in the panel's memory only: a copy of its cookie is refused until
  the panel restarts, and **after a restart that session is valid again** until it idles out or reaches
  `session_max_age`. So when you see that message or its line in the log: make the file writable again, then
  end the session for good on the panel host with `fairy admin sessions revoke-all` (everyone signs in once
  more), or with `fairy admin sessions list` and `fairy admin sessions revoke <id from the list>` for that one
  session. The browser that signed out no longer has the cookie, so signing out there once more is not
  possible.
* `last-seen.json` in the data folder is the one file that decides nothing: the time each agent last
  answered, written about once a minute while machines answer and when one stops answering, and read at
  start so that a machine that is off is shown with "last seen 3 h ago" instead of "never". Nothing but
  that line reads it ("Update unpowered hosts" still takes a machine for off only after 15 minutes without
  an answer since the panel started). A damaged or empty `last-seen.json` is not used: the panel
  starts all the same, moves the file to `last-seen.json.bad` (one older copy is replaced), starts a new
  one and says so once in its log. A symbolic link or a folder at that name is never touched: nothing is
  read or kept until it is taken away.
* State files fail closed: an unreadable `users.json`, `sessions.json`, `tokens.json` or `state.json`, and a
  corrupt `users.json` or `state.json`, stops the panel at start, and at run time such a file grants nothing
  (no sessions, control off) instead of being read as empty; it is never overwritten. A file that exists and
  is empty (0 bytes) counts as corrupt: the panel never writes one, and read as "nothing yet" it would switch
  the kill switch off and make the next sign-in the first admin again. Only a file that does not exist is a
  fresh start. The message names the file; restore it from a backup, or move it aside (for `users.json` that
  means: the first sign-in through `oidc.control_groups` / `oidc.bootstrap_admins` becomes admin again, and
  the list of removed people is gone; the message says so). A corrupt or empty `sessions.json` or
  `tokens.json` does not keep the panel down: at start it is moved aside as `sessions.json.corrupt-<UTC time>` /
  `tokens.json.corrupt-<UTC time>` (for example `sessions.json.corrupt-20261112T140501Z`; never over an older copy, and the five
  newest copies of a file are kept), one ERROR in the log names it, and the panel starts with nobody signed in (everyone
  signs in again) or with no API token valid (make new ones). If the damaged file cannot be moved, the panel
  starts all the same and the ERROR says that the file is still in place: move it away yourself. A damaged
  `settings.json` (everybody's page settings) is kept aside the same way, with one ERROR, and the settings
  start over with the defaults. A `settings.json` that cannot be read at all at some moment (an I/O error, too
  many open files) is left in place, with one ERROR: the settings show as the defaults and a save is refused
  until the file can be read again. In `state.json` a kill switch that is there in another shape than Fairy writes
  it (a hand edit such as `"control_disabled": true`) counts as ON, and so does a per-host power list that is
  not a list of host ids (power control is then off for every host, and `fairy admin host <id> power-control`
  changes nothing and says how to put the list right); one ERROR names the key. That command takes a system's
  id as the panel has it (a Proxmox node's is `node-<name>`, not the id of its agent) and refuses any other. Every file the panel replaces is first written to the disk in full and then renamed into place, so
  a power cut leaves the old or the new content. Files are written `0600` by the service user, and
  `fairy admin` commands that change something refuse to run as another user (use
  `sudo -u fairy fairy admin ...`).
* Defence in depth: process command lines are redacted by the panel too; fan duty is never set below 30 %;
  background uploads need a role and are rate limited; revoking a token ends its live streams.
* The API docs page (`/docs`, `enable_docs`, default `false`) needs a sign-in and a role. Keep it off unless
  you use it.
* Live updates: at most 8 open Fairy tabs (live-update connections) per user or API token, 200 for the whole
  panel. One more answers "too many live-update connections": close another tab.
* An agent's answer may be 1 MiB with 64 KiB of headers; a longer one is not read to its end and the host
  shows offline with "invalid response". At most two answers are unpacked at the same time.
* What one Proxmox API may put into the panel is limited like an agent's answer, because a node added with Add
  host supplies its own API address, token and CA: 4 MiB for the list of the cluster (256 KiB for every other
  answer, 64 KiB of headers), at most **64 nodes** and **2000 VMs and containers** per `proxmox:` entry or added
  node (Proxmox supports clusters of up to 32 nodes; a template that is not shown does not count), names and
  tags cut like an agent's, two CPU-model requests at a time per cluster. A cluster that lists more is shown up
  to the limit (the first rows) and the panel's log says so once: "the Proxmox API lists more than the panel
  takes from one source". Nothing is kept for a guest or node that is no longer listed.
* Texts from machines: every text an agent or a Proxmox API sends (host name, OS, sensor and disk names, a
  version, an error message, a guest's name and tags) is cleaned where the answer comes in, before the panel
  keeps, shows, logs or sends it on: escape sequences (colours, cursor moves, window titles), control
  characters and direction overrides (the character that makes `gpj.exe` read `exe.jpg`) are removed, a line
  break becomes a space, and every text has a longest length. From the answer of a newly paired machine the
  panel keeps `agent_version` only when it is a version and `hostname` only when it is a computer name
  (letters, digits, `.`, `-`, `_`, at most 64). `fairy admin` applies the same rule to everything it prints.
  The page escapes HTML wherever it inserts a text; that has not changed.
* Memory: `deploy/fairy.service` sets `MemoryMax=512M`. `fairy update` never replaces the unit file, so an
  older install gets this line by hand, once (see [Upgrading to this release](#upgrading-to-this-release)).
* See `SECURITY-FIXES.md` for the 2026-11 review fixes and what to configure.

Three rules for the people who use the panel:

1. **"Confirm it's you" that you did not ask for: do not approve it.** A passkey prompt belongs to the action
   you just started. If one appears on its own, close it. Closing the dialog (Cancel, Escape or clicking
   outside) also throws the waiting action away.
2. **Take people out on the Users page**, not only in Pocket ID. Leaving the allowed group in Pocket ID does
   not end a panel session that is already open. **Remove** on the Users page signs the person out everywhere
   and revokes their API tokens.
3. **A tab nobody touches signs out** after `session_idle_timeout` (4 hours unless you set it), and every
   sign-in ends after `session_max_age` (1 day unless you set it). That is meant. Sign in again.

Session and "Confirm it's you" settings in config.yaml (all optional):

| Key | Default | What it takes |
| --- | --- | --- |
| `session_max_age` | `86400` (1 day) | whole seconds (`86400`) or a text such as `36h`, `1d`; at least 5 minutes |
| `session_idle_timeout` | `14400` (4 hours; or `session_max_age`, when that is shorter) | whole seconds or a text such as `30m`, `4h`, `1d`; at least 1 minute, at most `session_max_age` |
| `step_up_window_s` | `180` | seconds, 30 to 86400: how long one "Confirm it's you" covers further control actions |
| `step_up_per_action` | `false` | `true`: every control action needs its own "Confirm it's you"; the window is not used |
| `step_up_prompt_login` | `false` (`true` in the example file) | `true`: Pocket ID is also asked to show its sign-in every time |

## Map of the code

How a request flows: `cmd/fairy` starts the **poller** (which fills an in-memory cache from Proxmox, agents,
power sources and UPSes on a schedule) and the **web** server (which answers the API and the UI from that
cache). Each folder below is one Go package; inside a package, files are named after what they contain.

| Folder | What it does |
| --- | --- |
| `cmd/fairy` | The `fairy` program: `main.go` picks the sub-command, `serve.go` starts the panel (or demo mode), `update.go` is `fairy update`. |
| `internal/web` | HTTP server: security headers, Pocket ID login (`auth.go`, `oidc.go`, `policy.go`), signed session cookies (`session.go`), who may see and do what (`grant.go`), what each user sees (`view.go`: every response and the stream are filtered here), API endpoints (`api_*.go`, the Users page's in `api_users.go`, the Sessions page's in `api_sessions.go`, "Update unpowered hosts" in `api_wakeupdate.go`), the checks in front of every control action and admin change (`guards.go`), live updates (`stream.go`), API docs (`docs.go`), demo preview (`demo.go`) and the embedded UI (`static/`). |
| `internal/poller` | Polls everything in the background and builds what the API shows: Proxmox (`proxmox.go`), agents (`agents.go`), the merged system list (`snapshot.go`), cluster and all-hosts totals (`summary.go`), per-host polling rate (`pollctl.go`), `health.json` (`health.go`), and the real network calls (`httpbackend.go`). |
| `internal/scheduler` | Runs each source as a job at its own interval: overrides, pause, boost while viewing, never overlapping. |
| `internal/normalise` | Turns raw Proxmox and agent data into the one "system" shape the UI uses. |
| `internal/power` | Wall power on the panel: retry/backoff per source (`runner.go`), totals and history (`manager.go`), devices reported by agents (`remote.go`), and the `type: agent` component-power metrics (`agentpower.go`). The device types themselves are Fairy-agent's `pkg/power`. |
| `internal/ups` | UPSes on the panel: state and history (`ups.go`), on-battery / restored events (`events.go`). The NUT sources are Fairy-agent's `pkg/ups`. |
| `internal/powerctl` | Which power actions a system offers (and why not) and carrying them out: agent, Proxmox API or Wake-on-LAN. |
| `internal/wakerun` | "Update unpowered hosts": the run as a state machine per machine (`wakerun.go`, `steps.go`), the one place that decides a shutdown (`shutdown.go`), pacing below the agents' limit (`pacer.go`) and the journal `wake-update.json` (`journal.go`). It knows no HTTP: the web package gives it the panel's wake, update and power calls. |
| `internal/config` | Reads and checks `config.yaml` (`config.go`, `validate.go`) and secrets from `fairy.env` / the environment (`env.go`). |
| `internal/admin` | The break-glass CLI `fairy admin`: one file per command (users and sites, tokens, sessions, control, audit, config check, health). |
| `internal/access` | `users.json`: users (by OIDC subject) with a role and sites, the list of sites, and which site each host / power source / UPS is assigned to. |
| `internal/adminstate` | `state.json` and `sessions.json`: kill switch, per-host power flags, polling overrides, the machines switched on for "Update unpowered hosts", the hosts' custom model names (`hostmodel.go`), the session list. Also `machines.json` (`machines.go`) and `last-seen.json` (`lastseen.go`: when each agent last answered, so that a machine that is off when the panel starts is shown with that time and not with "last seen never"). |
| `internal/statefile` | Safe shared JSON files (state, sessions, tokens, users): lock, re-read, write to a temp file, rename (so CLI and server never lose changes). |
| `internal/tokens` | Personal API tokens (`fy_...`), stored only as hashes in `tokens.json`. |
| `internal/audit` | The append-only audit log and its reader (Audit page, `fairy admin audit tail`). |
| `internal/health` | `health.json`, the status file the panel writes for `fairy admin health`. |
| `internal/settings` | Per-user UI settings and uploaded background images; the sign-in wallpaper (`signin.go`). |
| `internal/updates` | The latest Fairy-agent / Fairy-dashboard release for the Updates page (GitHub, cached, never blocking). |
| `internal/notify` | Optional ntfy / Discord message on control actions and notifications. |
| `internal/notifications` | The Notifications page: events and per-user read / dismissed marks (`notifications.go`), made from the poller's snapshots on state changes only (`watcher.go`). |
| `internal/managed` | Hosts added with Add host: `managed-hosts.yaml` and `secrets/`, merged with config.yaml. |
| `internal/enrollment` | Add host: join codes, the command per OS, the pairing loop (waiting -> paired -> online), pairing v2 (panel side), Tailscale-only pairing. |
| `internal/panelupdate` | Update panel: starting `fairy-panel-update.service` and reading its status file. |
| `internal/security` | Finds the real client IP behind nginx (rate limits, the audit log, and the address a "Confirm it's you" belongs to). |
| `internal/model` | The JSON shapes of the API (field names match the Python prototype). |
| `internal/jsonx` | Helpers for reading loosely-typed JSON from Proxmox, agents and devices. |
| `internal/apierr` | The error type that becomes an HTTP status + `{"detail": ...}`. |
| `internal/demo` | `fairy serve --demo`: a simulated homelab (fake Proxmox, agents, plugs, UPS, BMC). |
| test build | Only with `go build -tags fairytest`, never in a release ([TESTING-LOCAL.md](TESTING-LOCAL.md)): `cmd/fairy/testmode.go` (what it refuses to start with, its own folder), `internal/web/testmode.go` (sign-in and "Confirm it's you" without Pocket ID, the banner), `release/key.go` (the release key, for Add host). `cmd/fairy/testmode_off.go` is what every other build has instead. |
| `internal/approval` | The check on the machine itself, the panel's part without HTTP: the list of requests an approval covers and its nonce (`approval.go`), the panel's own checks of the ID token, the proposal a Turn on sends (`trust.go`), which machine an approval is for (`binding.go`; the record is `internal/adminstate/machines.go`, kept up by `internal/poller/machines.go`). `internal/web/approval.go` is the round trip, `approval_control.go` asks the machines and attaches the approval, `api_approval.go` is Turn on. |
| `internal/agentcontract` | The panel <-> agent contract test: the panel's real poller, normaliser, power/UPS managers and web API against the real `fairy-agent` handler (fake collectors, fake BMC, fake devices). `approval_test.go`: the check on the machine itself, with the agent's real gate and a fake Pocket ID; `binding_test.go`: which machine an approval is for (a relayed challenge, one id on two hosts, a changed id, a restart). |
| `internal/testutil` | Shared test fixtures. |
| `deploy/` | Example `config.yaml`, `fairy.env`, the systemd unit `fairy.service`, nginx, the `fairy-update` wrapper, the Update panel unit and its polkit rule. (The agent's install files are in Fairy-agent.) |
| `.github/workflows` | `release.yml` (signed release on every push to main), `ci.yml` (tests on pull requests). |

Shared code imported from [Fairy-agent](https://github.com/n3ptune-cpu/Fairy-agent) (`github.com/n3ptune-cpu/Fairy-agent/...`):

| Package | What the panel uses it for |
| --- | --- |
| `pkg/devices` | Device protocols: Shelly/Tasmota/Redfish parsing, NUT client, HTTP helpers, digest auth, and `Describe`, which turns errors into short safe messages. |
| `pkg/power` | One power source per device type (`shelly`, `tasmota`, `nut`, `redfish`/`ipmi`, `agent`, and `homeassistant`, which only an agent reads), built from a `power_sources:` entry. `DropRemoved` takes out entries of a type that is no longer read (`tuya`). |
| `pkg/ups` | NUT UPS sources built from an `ups:` entry, and which systems each UPS powers. |
| `pkg/redact` | Removes passwords/tokens from process command lines (in the agent, and again in the panel). |
| `pkg/configutil` | The id rule (`ValidID`) and secrets referenced by environment variable name (`EnvSecret`). |
| `pkg/enroll` | Add host: join codes, the pairing v2 request sent to the agent's `/pair`, and its sealed answer. |
| `pkg/release` | Signed releases: signature checks, the GitHub download, the swap / restart / rollback installer and the `update` command shared with `fairy-agent update`. |
| `pkg/agent/api` | The contract types of the check on the machine itself: the canonical request text, the nonce, the hash of a proposed trust, codes and limits. |
| `pkg/approval` | The machine's own check. The panel uses it to validate what a Turn on proposes exactly as the machine will; the tests use its gate as the machine. |
| `pkg/agent`, `pkg/agent/...` | Only in `internal/agentcontract`'s test: the agent's HTTP handler, fake collector and fake BMC. |

## Notes

* Frontend changes vs. the prototype: CSRF header value `fairy`, localStorage keys `fairy-*`, CLI hint text.
* Tuya plugs (the local protocol) are no longer read. An old `type: tuya` entry in `power_sources:` is
  ignored: `fairy serve` says so once at start (a warning), `fairy admin config check` prints a `note` line
  for it, and the panel starts as before. Remove the entry when you see the line.

## Licence

MIT, see [LICENSE](LICENSE). Fairy-agent is MIT-licensed too.

The marks the panel shows for a host's system (Windows, Proxmox VE, the Linux distributions) are listed with
their sources and licences in [THIRD-PARTY-ICONS.md](THIRD-PARTY-ICONS.md). They are trademarks of their
owners and are used only to show which system a host runs.
