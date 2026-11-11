# Trying the panel on your own PC: the test build

A **test build** is a panel you build yourself with the Go build tag `fairytest`. It has **no sign-in**: opening
the page makes you the one built-in admin, `test-admin`. It is for clicking through new pages and trying Add
host and control actions against throwaway test machines before anything is pushed to GitHub.

It is not a release and can never become one: a release is built without the tag and has none of this code
(a test checks that, and the release workflow stops if a built program says it is a test build).

Because it has no sign-in, a test build refuses to start unless everything says "a test, on this PC":

* it listens on `127.0.0.1` (or `::1`, `localhost`) only, and answers no request that came through a proxy or
  for another host name;
* its config has no `oidc:` block, and neither `FAIRY_SESSION_SECRET` nor `FAIRY_OIDC_CLIENT_SECRET` is set;
* its data folder is new, empty, or one a test build made, with nobody in it but `test-admin`;
* none of its files is in an installed panel's folders (`/etc/fairy`, `/var/lib/fairy`, `C:\ProgramData\Fairy`).

So putting the test program in the place of an installed panel does not switch that panel's sign-in off. It
does not start, and says why.

Every page has a red bar at the top: **TEST BUILD: sign-in is off. Never expose this.** `fairy version`, the
Updates page and the start-up log say "TEST BUILD" too.

## A. Build and start (dev VM, `cmd.exe`, Go installed)

1. Unpack the zip into your test folder. You get a folder like `Fairy-dashboard-part1-r1`.
2. Open `cmd.exe` and go into that folder (use your own path):

   ```
   cd /d E:\Projects\Fairy-dashboard-TEST\Fairy-dashboard-part1-r1
   ```

3. Build. The first time, Go downloads the packages it needs (a minute or so):

   ```
   go build -tags fairytest -o fairy-test.exe .\cmd\fairy
   ```

4. Check what you built:

   ```
   fairy-test.exe version
   ```

   It answers `fairy dev TEST BUILD (...) - release key: built in`, and below that a line that starts with
   `TEST BUILD`.
5. Start it:

   ```
   fairy-test.exe serve
   ```

   The first start makes the folder `fairy-test` next to `fairy-test.exe`: `fairy-test\config.yaml` (a copy of
   `cmd\fairy\fairytest-config.yaml`, no secrets in it) and `fairy-test\data` (its state). The window shows
   three lines with `TEST BUILD: sign-in is off` and then `fairy listening ... addr=127.0.0.1:8080`.
6. In the browser **on the dev VM**, open this address:

   ```
   http://127.0.0.1:8080
   ```

   You see the panel with the red bar, signed in as `test-admin`. No sign-in page.

   `http://localhost:8080` works too: the test build sends the browser on to `http://127.0.0.1:8080`, and
   the address bar shows that. (The page and its requests have to use one address. Under the other name the
   page used to load and then refuse the first change, for example Add host, with "cross-origin request
   refused".)
7. To stop it: `Ctrl+C` in the `cmd.exe` window.

If it prints `Refusing to start: ...`, the line says what is in the way (see the list above).

Port 8080 already in use? Start with another one, and open that one: `fairy-test.exe serve --listen 127.0.0.1:8099`.

## B. Add a test VM as a host

The agent is **never** installed on the dev VM. Use a throwaway Windows test VM on the same VMware network,
with a snapshot from before anything Fairy, and with internet access (the command downloads the released
Fairy-agent from GitHub and checks its signature, exactly like the real panel).

1. On the **test VM**, in `cmd.exe`: `ipconfig`. Note its `IPv4 Address`.
2. On the **dev VM**, look at the lines `fairy-test.exe serve` printed. One has `Add host: panel address`
   and `address=...`. That must be the dev VM's own `IPv4 Address` on the VMware network (the test VM answers
   only that address). If it is another address, or a line says the PC has no private address or more than
   one: stop the panel, open `fairy-test\config.yaml` in Notepad, remove the `#` in front of
   `panel_address:` and put the dev VM's `IPv4 Address` there, save, start again.
3. In the panel: **Overview**, **Add host** (next to "Hosts"). Name: the test VM's computer name (letters,
   digits, `-`). System: Windows. Address: the test VM's IPv4 address from step 1. Tick what you want to try
   (power control, self-update). Press **Make the code**.
4. "Confirm it's you" appears and names the action. Press **Confirm (test build)**. Nothing goes to Pocket ID.
5. The dialog now shows one PowerShell line. Copy it.
6. On the **test VM**: open PowerShell **as Administrator**, paste the line, Enter. It installs the agent and
   then waits for the panel. Without Tailscale the agent does not know which address to listen on, so the
   wait ends after about two minutes with `not paired yet`. That is expected here.
7. Still in that PowerShell window on the test VM, give the agent its address (put the test VM's IPv4 address
   from step 1 in place of `192.168.50.20`), then restart it:

   ```
   Add-Content C:\ProgramData\fairy-agent\config.yaml "`nlisten: 192.168.50.20:9101"
   Restart-Service fairy-agent
   ```

   Do this within 15 minutes of step 5 (that is how long the code works).
8. Back in the panel: the Add host dialog goes from "waiting" to "paired" to "online", and the host is on the
   Overview. Now try its page, the Updates page, power actions (use a delay and Cancel first).
9. When you are done: remove the host on its page (**Remove**), and put the test VM back to its snapshot.

These steps were run on 2026-11-11 with a Windows 10 dev VM and a Windows 11 test VM without Tailscale, and
work as written: step 6 ended with `not paired yet`, and after step 7 the host was paired and online within
seconds. If step 6 ends differently for you, send what the window shows.

## C. Start again from nothing

Stop the panel (`Ctrl+C`), then in the folder of `fairy-test.exe`:

```
rmdir /s /q fairy-test\data
```

That removes users, sessions, added hosts, settings and the audit log of the test build; `config.yaml` stays.
To remove the config as well: `rmdir /s /q fairy-test`. Test machines that were added still have their agent:
put them back to their snapshot.

A new zip goes into a new folder; build and start there. The old folder can be deleted whole.

## D. Trying the Sessions page

Every browser that opens the panel is its own sign-in, a **session**. To have two, use two browsers on the dev
VM (Edge and Firefox, say), or one browser with a normal window and a private (InPrivate, incognito) window.

1. Start the panel (part A). Open `http://127.0.0.1:8080` in the first window.
2. Open `http://127.0.0.1:8080` in the second window too.
3. In the first window: the menu button at the top right (three lines), then **Sessions**.
4. Under "Your sessions" there are two lines. The one marked **This session** is the window you are looking
   at; the other one is the second window. Both show `127.0.0.1`, because both are on this PC. The times
   ("12s ago") count by themselves. Below, "Everyone's sessions" shows the same two with the user's name:
   that table is for admins.
5. Press **Sign out** on the other line. "Confirm it's you" opens and says what will happen: "Fairy will run:
   sign out another session of yours (...)". Press **Cancel** once: nothing happens, both lines stay.
6. Press **Sign out** again, then **Confirm (test build)**. The page loads again, says "that session is signed
   out", and one line is left.
7. Look at the second window. Within a few seconds it shows "You were signed out." by itself.
8. In the second window press **Continue as test-admin**. It is signed in again, as a new session.
9. First window: wait up to 15 seconds (or reload). Two lines again. Press **Sign out everywhere else**, then
   **Confirm (test build)**. One line is left, and the second window is signed out again.
10. With one line left, **Sign out everywhere else** is grey: there is nowhere else.
11. Menu, **Audit**, choose "Sessions" in the filter: the sign-outs are listed, with who did it and from which
    address.
12. Make the window narrow (or press F12 and choose a phone): each session becomes a block of its own.

The line marked **This session** has no button. To end it, use **Sign out** in the menu, as before.

Stop the panel and start it again: the list starts empty and your open window signs in anew. (A test build
forgets its sessions at every start, see below.)

## What the test build does NOT prove

* **Real sign-in.** Nothing goes to Pocket ID: not the sign-in, not the groups and allowed addresses, not a
  first sign-in that waits for access, not what happens when Pocket ID is down.
* **"Confirm it's you" at Pocket ID.** The dialog appears and the action runs after it, but the passkey check
  and what Pocket ID answers are not part of it.
* **The Tailscale-only rule.** The test config switches it off (`allow_non_tailscale: true`), and the panel's
  own Tailscale address is not what the test VM pairs with.
* **The panel's release.** Its signature, `fairy update`, the Update panel button and its roll-back are off in a
  test build. (The agent's release is checked for real: Add host and the agents' Update button use the real
  signed release.)
* **The real panel host.** No nginx or Cloudflare in front, no https, no Secure cookie, no systemd unit, no
  Linux file owners. A test build runs as you, on Windows, on plain http.
* **`fairy admin`.** The command line for an installed panel (users, sessions, kill switch) is off in a test
  build: it works on an installed panel's files.
* **Several people.** There is one admin. What a viewer or a waiting user sees: `fairy serve --demo` of a
  normal build.
* **Sessions across a restart.** Every start of a test build makes a new session secret and empties its
  session list, so a restart signs the page out; "Continue as test-admin" or a reload signs it in again.
* **The Sessions page with real addresses and a real confirmation.** Here every session is `127.0.0.1` and
  "Confirm it's you" is the test build's own button. That the address is the visitor's own behind nginx and
  Cloudflare, that Pocket ID asks for the passkey before a session is signed out, and what a second person
  (a viewer) sees are not part of it.
