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

## What to click in zip 4

Unpack the zip (folder `Fairy-dashboard-part1-r4`), open `cmd.exe` in it, then:

```
go build -tags fairytest -o fairy-test.exe .\cmd\fairy
fairy-test.exe serve
```

Open `http://127.0.0.1:8080`. To keep the test VM you added before, copy the folder `fairy-test` from the r3
folder into the r4 folder **before** the first start.

1. **Overview**: one row per host. Under the name, small: the PC model. In the tile in front: the mark of its
   system (Windows, Debian, ...). Behind the name: small signs for the helper, the battery and the UPS, where
   the host has one. Point at a sign, then click it: a small box opens with the same words.
2. **Updates**: each ⓘ opens the longer explanation. The button is named **Update unpowered hosts** (no "…").
   Its dialog lists the test VM under "On now: update only, never switched off".
3. Tick the VM and start the run: it still ends with "It was on and stays on.", and the VM stays on.
4. A host that does not answer is "not sure" for 15 minutes before it can count as off. The count starts
   again at every start of the panel, so right after a start no machine is offered as off.
5. **Sessions**: make the window narrower and wider. Nothing scrolls sideways.
6. **Settings**: the ⓘ buttons; the sign-in picture you set in r3 is still there (with the copied folder).
7. If the VM has fans: change a fan. "Confirm it's you" names the change in words, not in braces. Else skip.

Every page with made-up data, without any machine: build the plain program and start its demo.

```
go build -o fairy-demo.exe .\cmd\fairy
fairy-demo.exe serve --demo
```

It needs no config, listens on `http://127.0.0.1:8090` (this PC only, no sign-in, a yellow bar says DEMO) and
keeps its files in a folder `.fairy-demo` where you started it. Stop either program with `Ctrl+C`.

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

## E. Trying the sign-in wallpaper

1. Menu, **Settings**, "Sign-in page": **Upload picture…**, a PNG, JPEG or WebP of at most 8 MiB. The preview shows it.
2. Menu, **Sign out**: the sign-in page shows the picture over the whole window, the button in a strip at the bottom.
3. Narrow the window, then make it very wide: the strip stays at the bottom. The page stays until you press the button.
4. Press **Continue as test-admin (test build)**: you are back in the panel.
5. Rename `notes.txt` to `notes.png` and upload it: refused ("not a PNG, JPEG or WebP picture"), the old picture stays.
6. **Remove picture**, Sign out again: the same page on black. Menu, **Audit**, "Settings": both changes are listed.

## F. Trying wake, update, shut down

"Update unpowered hosts" (README, the part with that name) wakes machines that are off, updates their
agent and switches them off again. A dev VM and one test VM that is already on cannot show all of that: a
VMware test VM cannot be woken with Wake-on-LAN the way a real PC is. So this part has three steps: what you
can try now, what you can try with a second test VM, and what only the real machines can show.

### F1. With one test VM that is on (about five minutes)

The test VM is added as in part B, with **self-update** ticked.

1. Menu, **Updates**. Below the panel's own box there is **Update unpowered hosts**, and in it a
   table "Machines" with the test VM: "Now: on", "In these runs: off".
2. Press **Switch on** in its line. The browser asks whether you mean it; then "Confirm it's you" names the
   action; press **Confirm (test build)**. The line now says "switched on". (The same switch is in the ⋯
   menu on the test VM's page.)
3. Press **Update unpowered hosts**. The dialog says what a run does, and lists the test VM under
   "On now: update only, never switched off", unticked. Tick it. The button reads "Start for 1 machine".
4. Press it. "Confirm it's you" shows the list: one machine, "update only; it is on and is never switched
   off", and a short number for the list. Press **Confirm (test build)**.
5. The page shows the run: **Is on**, **Update**, **Stays on**. The test VM's agent really asks GitHub for
   the newest release and installs it if it is newer, exactly as the Update button does. If the agent is
   already on the newest release, the report says "already runs the newest release". Either way the test VM
   **stays on**: look at it.
6. Reload the page: the report is still there under "Last run". "Show its steps" lists what was sent and
   when. Menu, **Notifications**: one entry for the finished run. Menu, **Audit**, filter "Update unpowered
   hosts": who started it, the machine, each step.
7. Press **Switch off** in the "Machines" table when you are done: the test VM no longer takes part.

A run with one machine that is on is over in seconds, so **Cancel run** and "one run at a time" are hard to
see here. They are in F2.

### F2. With a second test VM (optional, about fifteen minutes)

This shows the countdown, Cancel, and a real Windows shutdown at the end of a run, without real
Wake-on-LAN: **you** start the VM by hand at the moment the run sends the wake. The run cannot tell the
difference, and that is the point of trying it.

You need test VM **A** (stays on) and test VM **B** (the one that is switched off and on), both added as in
part B. In Add host, tick **Wake-on-LAN relay** for A. For B tick **power control** and **self-update**, put
B's network card address (`ipconfig /all`, "Physical Address") in **Wake-on-LAN MAC(s)** and choose A as
the relay. In B, switch Fast Startup off (Control Panel, Power Options, "Choose what the power buttons do",
untick "Turn on fast startup"), or the run will take B for a machine that was on all along (see below).

1. Switch B on for these runs (F1, step 2). Shut B down in Windows. **Wait 15 minutes.** The panel shows B
   as offline after about a minute, but a run takes a machine for off only after 15 minutes without an
   answer (a PC that restarts is also silent for a few minutes). Until then the "Machines" table says "Now:
   unclear" for B and how long it has had no answer; after 15 minutes it says "Now: off". The panel must
   have been running all that time: if you start it again, the 15 minutes start again.
2. **Update unpowered hosts**: B is listed under "Off (no answer for 15 minutes or more): wake,
   update, switch off again", ticked. Start, **Confirm (test build)**.
3. The run shows B as "waking". **Now power B on in VMware.** You have 5 minutes.
4. When B's agent answers, the run goes on by itself: "updating", then "pause before shutdown" (a minute),
   then "switching off" with a countdown of 2 minutes. B's page shows the same countdown with **Cancel**.
5. First time: let it run out. B shuts down. The report says "done · off again".
6. Second time (steps 1 to 4 again): press **Cancel** on B's page during the countdown. B stays on, and the
   report says "LEFT ON" and that its shutdown was cancelled.
7. Third time: press **Cancel run** while B is "waking" or "updating". The dialog asks what happens to the
   machines the run has woken; leave "stay on". B stays on and the report lists it as ON. While a run is
   going, the **Update unpowered hosts** button is grey ("one run at a time").
8. To see the rule "a machine that is already on is never switched off": shut B down, wait until the table
   says "Now: off" (15 minutes), power B on in VMware **before** you start the run, and start the run for it
   within the first seconds (the panel still shows it as off). The run asks B directly before any wake. If
   B answers, or refuses the connection (Windows runs, the agent has not started yet), the report says that
   it is on and that nothing was sent to it. If B does not answer yet, the run sends its wake, and when B
   answers it has been up for longer than since the wake: "It was already on". Either way B stays on.
9. To see "not sure": shut B down and open **Update unpowered hosts** two or three minutes later. B
   is listed under "Not sure that they are off: left alone", with how long it has had no answer, and has
   no tick box. Nothing can be sent to it until the 15 minutes are over.

The times a run shows come from `wake_update:` in config.yaml. With the defaults it waits up to **5 minutes**
for a machine to come online (and goes on as soon as its agent answers), **1 minute** between the update's
result and the shutdown request, and counts down **2 minutes**. A page or a screenshot that shows other
numbers (60 s, 10 s, 20 s, say) runs with a shortened test config. To make a try shorter yourself, put this in
`fairy-test\config.yaml` and start the panel again (a shorter wait and pause; the countdown stays long enough
to press Cancel):

```
wake_update: {online_timeout_s: 60, settle_s: 10, shutdown_delay_s: 30}
```

The 15 minutes in step 1 are not one of these settings and cannot be shortened.

F2 was not run by us on real Windows VMs. The same steps were run in a browser against stand-in agents on
one PC (machines that are "off" do not answer, "boot" after the wake, "update" and "shut down"), with the
waits shortened to 60 s, 10 s and 20 s; screenshots of that run are labelled so. That run was made before
the 15-minute rule; the rule itself is covered by the panel's own tests, with a clock. If a step
goes differently for you, send what the page shows and the lines of the `fairy-test.exe serve` window.

### F3. What needs the real machines

* **That Wake-on-LAN really starts the PC** from "shut down": firmware and network card settings, the relay
  in the same network. Try **Power on** in the machine's Power menu first; a run sends the same request.
* **That a PC that is off counts as off.** The panel must get no answer at all from it for 15 minutes. A PC
  that is shut down gives none. If the report says "It is on (the machine refused the connection)" for a PC
  that is off, something else answers at its address (a router, a firewall): tell us.
* **That a PC the run woke counts as woken.** The run compares the uptime the PC reports with the time since
  the wake. Windows with Fast Startup keeps counting its uptime through "Shut down"; such a PC is taken for
  "already on" and left on. Switch Fast Startup off on the PCs that take part, then look at the first
  report: it must say "Woken, updated ..., and switched off again".
* **A PC that signs in by itself after the start**: the run asks nobody and shows nothing on the screen
  before the agent's shutdown notice. That the update and the shutdown go through while Windows is still
  signing in is to be seen on the real PC.
* **A Proxmox node.** A node that the panel reads through the Proxmox API is switched off only when the
  panel sees no guest that is not stopped; one that the panel knows only through its agent is always left
  on. Neither was tried against a real Proxmox.

## G. Trying the model name

A host's page, **⋯**, **Set model name**: type a name, **Save**. It is under the host's name on the Overview
and in the "Model" line of its page; the ⓘ there (or pointing at it on the Overview) says what the host
reports. **Clear** in the same dialog brings the reported model back. Menu, **Audit**, "Host admin" lists both.

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
* **Update unpowered hosts, on real machines.** Part F says what a test VM can show. Real
  Wake-on-LAN, the shutdown of a PC the run really woke, and a Proxmox node are not part of it.
* **The Sessions page with real addresses and a real confirmation.** Here every session is `127.0.0.1` and
  "Confirm it's you" is the test build's own button. That the address is the visitor's own behind nginx and
  Cloudflare, that Pocket ID asks for the passkey before a session is signed out, and what a second person
  (a viewer) sees are not part of it.
