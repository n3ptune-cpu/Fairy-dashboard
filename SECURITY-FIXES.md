# Security fixes (November 2026 review)

This file explains, in plain words, what the security review of Fairy found, what could have gone wrong, what
was changed, and what **you** need to do on the server, in Pocket ID and in nginx. Each fix is its own commit
with a test that proves it.

A later review led to a second set of fixes (sign-in and sessions, pairing with agents, limits on what one
agent can send). What that release changes for you, and the order to install it in, is in the `README.md` of
that release, "Upgrading to this release". Three points of the list "Not changed" below were changed by it; they are marked
there.

**Short version of your to-do list** (details further down):

1. In `/etc/fairy/config.yaml`: set `listen:` to the panel LXC's LAN address, set `trusted_proxies` to nginx's
   address, set `step_up_prompt_login: true`, and empty `bootstrap_admins` once you are admin.
2. Replace `/etc/systemd/system/fairy.service` with the new `deploy/fairy.service` (keep a copy of yours
   first), then `sudo systemctl daemon-reload && sudo systemctl restart fairy`. Check:
   `systemctl show fairy -p MemoryMax -p NoNewPrivileges` must show `MemoryMax=536870912` and
   `NoNewPrivileges=yes`. `fairy update` never does this step for you: it replaces the program, not the unit
   file. If you only want the memory limit, use the four steps in README.md, "One step by hand: the memory
   limit".
3. Use `deploy/nginx.example.conf` for nginx (real visitor IP from Cloudflare, rate limits, streaming).
4. Run the admin tool only as `sudo -u fairy fairy admin ...`, never as root. One exception:
   `sudo fairy admin config check` (as root). It only reads, and only root can read `fairy.env`.
5. In Pocket ID: run the latest version, turn off "users can edit their own account", and check that a fresh
   sign-in works for control actions (see M3).
6. If you ever ran `fairy admin` as root: `chown -R fairy:fairy /var/lib/fairy` before starting the new version.

---

## Medium findings (all fixed)

### M1. A removed user could get their access back

**What could happen.** Pressing **Remove** on the Users page made things *worse*: the person's API tokens
were no longer limited to their role (a "read" token suddenly saw every site, a "control" token acted as an
admin). And someone who had been admin through the `control_groups` setting got admin back on their very next
click, because the panel looked at the group list stored in their (up to 7 days old) login cookie.

**What changed.**
- Removing someone now signs them out everywhere and revokes all their API tokens.
- Fairy remembers who was removed. If that person signs in again, they wait for access like a stranger and
  are never made admin automatically.
- "Automatic admin" (`control_groups` / `bootstrap_admins`) is only checked at a real, fresh Pocket ID
  sign-in, never from the cookie.
- An API token whose owner is not on the Users list works for nothing. The admin tool refuses to create one.
  Only tokens with `--owner system` keep their own scope.
- A login cookie for someone who is not on the Users list is ended (they must sign in again).

**What you need to do.** Nothing. After upgrading, everyone signs in once more (old cookies from before the
Users list existed are asked to sign in again).

### M2. Email addresses were trusted without being verified

**What could happen.** `allowed_emails` and `bootstrap_admins` matched the email address in the Pocket ID
token, even if Pocket ID had not verified it. A Pocket ID user who could put your address (or an old/typo
address from the list) on their own account could sign in, or even become admin. Automatic admin also worked
for *any* new person at any time, not just at first setup.

**What changed.**
- Email lists only count when Pocket ID says the email is verified (`email_verified: true`).
- Automatic admin (`control_groups` / `bootstrap_admins`) only happens while there is **no admin at all** in
  the panel (a fresh install or the first upgrade). Once you are admin, everyone new waits for you.

**What you need to do.**
- After you are admin, set `bootstrap_admins: []` in config.yaml (it is harmless now, but tidy).
- In Pocket ID: run the latest version (older ones had a "verified email spoofing" bug,
  GHSA-rm8c-2cv8-jwf6) and turn off "users can edit their own account" if you don't need it.
- If you rely on `allowed_emails`, make sure the addresses are verified in Pocket ID; otherwise use groups.

### M3. "Confirm it's you" could pass without a real re-login

**What could happen.** Before power, fan and user changes, Fairy asks you to sign in again at Pocket ID
(step-up). It checked the sign-in time (`auth_time`) that Pocket ID reports. If Pocket ID did not send that
time, Fairy assumed "just now", so a hijacked or unattended browser could pass the check silently.

**What changed.**
- A missing sign-in time is now treated as "long ago": control actions ask for a step-up, and if Pocket ID
  still does not send `auth_time`, Fairy shows a clear page ("Pocket ID did not say when you signed in...")
  instead of passing, and logs it. It never loops.
- The example config now sets `step_up_prompt_login: true`, which also asks Pocket ID to show the login
  screen.

**What you need to do.**
- Set `step_up_prompt_login: true` in config.yaml.
- Test once: open a host, try a harmless control action (for example change its polling interval), click
  "Continue to Pocket ID", sign in, and check it works. If you get "Could not confirm it's you ... no
  auth_time", Pocket ID is not sending `auth_time` (the OpenID standard requires it when Fairy asks for a
  fresh sign-in); update Pocket ID. Until then control actions stay locked (safe, not open).

### M4. Anyone could flood the audit log until the disk was full

**What could happen.** Every request with a fake API token wrote one line to the audit log, with no limit,
and the log never rotated. An anonymous attacker could fill the disk; then nobody could sign in (including
for an emergency shutdown).

**What changed.**
- Entries that anyone can cause without signing in (bad tokens, failed or rate-limited sign-ins) are limited
  to 10 per visitor address per minute and 300 in total; the rest are counted in one summary line per minute
  ("audit.suppressed").
- Every audit line is at most 8 KiB.
- The audit log rotates by size: `audit.log` -> `audit.log.1` ... keeping 5 old files of 100 MiB by default
  (`audit_max_mb`, `audit_keep` in config.yaml; `audit_max_mb: -1` turns it off if you prefer logrotate).
- If the audit log cannot be written, sign-in still works and the server log says so loudly. If the session
  file cannot be written (disk full), sign-in fails with a clear message instead of a vague error.

**What you need to do.** Nothing required. Optionally add a Cloudflare rate-limit rule on `/auth/*`.

### M5. One visitor could block everyone's sign-in (nginx in its own container)

**What could happen.** With nginx in a separate container, Fairy saw every visitor as nginx's address, so the
login rate limit (20 per minute) was shared by everyone. One anonymous visitor could block sign-in and step-up
for all users. The audit log also showed nginx's address instead of the real one. The shipped service file
also listened on 127.0.0.1 only, which nginx in another container cannot reach.

**What changed.**
- Fairy reads the real visitor address from `X-Forwarded-For` (only when the request comes from a trusted
  proxy), including forms with port numbers and IPv6.
- The login rate limit is counted per visitor address, and for a signed-in user doing a step-up per user, so
  attempts from other addresses do not use up your budget for confirming a control action. Two things still
  can (found in the fourth review, not changed in code): someone who holds a copy of your session cookie,
  and a page on another site that makes your own browser open `/auth/login` again and again. About 20 such
  requests a minute use up your budget, and each one makes a waiting "Confirm it's you" forget its action.
  "Continue" then answers "too many login attempts" or "The action is no longer waiting". Nothing runs
  because of it: wait a minute and press the button again.
- New setting `listen:` in config.yaml (the service file no longer hardcodes an address). Fairy warns at
  start if it listens on the LAN but only trusts 127.0.0.1 as proxy.
- New `deploy/nginx.example.conf`: takes the real visitor address from Cloudflare (`CF-Connecting-IP`, only
  from Cloudflare's address ranges), overwrites `X-Forwarded-For`, rate limits `/auth/` and `/api/`, keeps
  the live stream unbuffered, sets body size limits and timeouts. Security headers stay Fairy's job.

**What you need to do.**
- config.yaml: `listen: <panel LXC LAN IP>:8080` (e.g. `10.0.0.20:8080`) and
  `trusted_proxies: ["<nginx container IP>/32"]`.
- Firewall port 8080 in the panel LXC so that only nginx can reach it (nftables, or `IPAddressAllow=` in the
  service file if your LXC supports it).
- nginx: start from `deploy/nginx.example.conf`; replace the names, addresses and certificate paths, and keep
  the Cloudflare address list up to date. Use nginx 1.30.6 / 1.31.7 or newer. The example also has
  Cloudflare "Authenticated Origin Pulls" (only Cloudflare may connect) - set it up in Cloudflare or remove
  those two lines.

### M6. A broken or unreadable users file was treated as empty

**What could happen.** If `users.json` (or the session, token or state file) could not be read - for example
because `fairy admin` was run as root and the file became root-only - Fairy treated it as empty and then
rewrote it. All roles and site limits were lost, and anyone in `control_groups` became admin again.

**What changed.**
- A missing file is still "empty", but an unreadable or damaged file is an error. Fairy refuses to start
  with a clear message, and if a file breaks while running, nobody gets access (sessions end, control
  actions are switched off) until it is fixed. The damaged file is never overwritten.
- Fairy refuses to change a state file, or write into the data folder, owned by another user. The message
  says to use `sudo -u fairy fairy admin ...`.
- `fairy admin` commands that change something refuse to run as the wrong user (e.g. root). Commands that
  only look (`list`, `audit tail`, `config check`, `health`) still work.
- Files are written with mode 0600 by the service user.

**What you need to do.**
- Always run `sudo -u fairy fairy admin ...`.
- If you ran it as root before: `chown -R fairy:fairy /var/lib/fairy` (Fairy tells you if this is needed).
- If Fairy says a file is "corrupt": restore it from a backup, or move it aside (that file then starts over;
  for `users.json`, sign in again: you become admin through `control_groups` / `bootstrap_admins`, or run
  `sudo -u fairy fairy admin users set-role <your email> admin` after signing in once; it asks before making
  an admin, `--yes` skips the question, and an address that `fairy admin users list` marks "(unverified)" needs
  the SUBJECT from that list instead of the e-mail).

---

## Other fixes

| Item | What could happen | What changed |
|---|---|---|
| **go-jose library** (L2) | Three published bugs in a library used to check Pocket ID's tokens (crash / CPU use; only reachable through a compromised Pocket ID or connection) | Updated to v4.1.9. |
| **Service would not start** (I1) | The service file told Fairy to read `/etc/fairy/fairy.env`, which only root can read, so Fairy (running as user `fairy`) stopped at start | systemd reads the file as root (`EnvironmentFile=`) and the service passes `--env-file ""`. The file stays `0600 root:root`; the folder `/etc/fairy` is `0750 root:fairy` and `config.yaml` `0640 root:fairy`. Fairy also reads `FAIRY_LISTEN` etc. from the env file correctly now. |
| **Cross-site sign-out** (L9) | Another website could sign you out (a hidden form, or a link to `/auth/login`) | Sign-out needs Fairy's own security header (the menu does this for you); visiting the sign-in page keeps your session until a new sign-in finishes. |
| **Uploads** (L7) | Someone still waiting for access could upload 8 MiB images without limit | Uploads need a role, max 6 per minute per user (settings changes 30 per minute). Size limit stays 8 MiB. |
| **Fan floor** (L8) | A wrong click plus an old agent could set a server's fans to 0 % | The panel refuses anything below 30 %; the slider starts at 30 %. |
| **Live stream after token revoke** (L1) | A revoked or expired API token kept receiving live data until it disconnected | The stream checks the token every second and ends. |
| **Process command lines** (L3) | Passwords in a program's command line could be shown if an agent did not hide them | The panel hides them too. |
| **BMC connections** (L5) | Each power reading opened a new encrypted connection to the BMC; the example turned certificate checks off | One connection is reused; the example keeps checks on and uses `ca_file` and a read-only BMC account. |
| **/static/ listing** (I2) | The file list of the web UI was visible | Now "not found". |

## Not changed (and why)

- **L4 - limits on live streams per user.** Bigger change to the streaming code; the nginx example limits
  connections per visitor (`limit_conn`) in the meantime.
  *Changed since:* at most 8 live-update connections per user or API token, 200 for the whole panel.
- **L6 - API docs page loads Swagger from a CDN.** It is off by default (`enable_docs: false`); keep it off.
  *Changed since:* the page loads the two Swagger files only when they match the hashes built into the panel,
  and it needs a sign-in and a role. It is still off by default; keep it off unless you use it.
- **I3 - cookie name prefix `__Host-` / shorter session.** Would sign everyone out and breaks local http
  testing; consider `session_max_age: 172800` (2 days) yourself.
  *Changed since:* the cookie is `__Host-fairy_session` (the plain name only with `FAIRY_INSECURE_COOKIES`).
  Everyone signs in once after that update. A tab nobody touches signs out after `session_idle_timeout`
  (4 hours by default), and a sign-in ends after `session_max_age` (1 day by default; these were 12 hours and
  7 days until the release with the Sessions page).
- **I4 - HSTS for subdomains.** Better set at Cloudflare for the whole domain.
- **I5 - demo mode.** Already loopback-only; never run `--demo` on the panel LXC.
- **I6 / I8 - userinfo claims, multi-audience tokens.** Depend on Pocket ID; run its latest version.
- Server-side items from the review (MikroTik, Tailscale, BMC firmware, NUT, Proxmox token rights) are outside
  Fairy; see the review's section 4.

## Check of the new menu (hamburger) commit

Every name, email, site and demo label in the new menu is escaped before it is shown, the menu uses no
inline script or style (the strict Content-Security-Policy still applies), and Sign out now uses the
protected request described above. Nothing to fix.

## Custom logo

You can show your own logo instead of the purple square (sidebar, sign-in and waiting pages, browser tab icon):

1. Copy your image to `/etc/fairy/logo.png` - PNG, JPEG, WebP or GIF, at most 1 MiB (SVG is not accepted,
   because it can contain script). Make it readable: `chmod 644 /etc/fairy/logo.png`.
2. Add to config.yaml:

   ```yaml
   branding:
     logo: /etc/fairy/logo.png
   ```
3. Restart Fairy (`systemctl restart fairy`). If the file is missing or not a valid image, Fairy keeps the
   normal logo and writes a warning to the log.

Try it on Windows first with the demo: `fairy.exe serve --demo --logo C:\Users\you\Pictures\logo.png`, then
open http://127.0.0.1:8090. The logo is public (no sign-in needed to load it), so don't use a picture you want
to keep private. Fairy reads only the file named in the config, once at start.

## Checks run

gofmt, go vet (Linux and Windows), staticcheck, `go test -race ./...`, govulncheck (no vulnerabilities),
osv-scanner 2.7.0 (no issues), and builds for windows/amd64, linux/arm64 and linux/amd64 with CGO_ENABLED=0
all pass.
