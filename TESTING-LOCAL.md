# Trying the panel on your own PC: the test build

A **test build** is a panel you build yourself with the Go build tag `fairytest`. It has **no sign-in**: opening
the page makes you the one built-in admin, `test-admin`. It is for clicking through new pages and trying Add
host and control actions against throwaway test machines before anything is pushed to GitHub.

It is not a release and can never become one: a release is built without the tag and has none of this code
(a test checks that, and the release workflow stops if a built program says it is a test build).

Because it has no sign-in, a test build refuses to start unless everything says "a test, on this PC":

* it listens on `127.0.0.1` (or `::1`, `localhost`) only, and answers no request for another host name or with
  one of the headers a reverse proxy usually adds;
* its config has no `oidc:` block, and neither `FAIRY_SESSION_SECRET` nor `FAIRY_OIDC_CLIENT_SECRET` is set
  (an `approval:` block is allowed: part I);
* its data folder is new, empty, or one a test build made, with nobody in it but `test-admin`;
* none of its files is in an installed panel's folders (`/etc/fairy`, `/var/lib/fairy`, `C:\ProgramData\Fairy`).

So putting the test program in the place of an installed panel does not switch that panel's sign-in off. It
does not start, and says why.

**It signs in only a browser that opens the address it prints.** At start the program prints one line:

```
Open this address in a browser on this PC (it holds the start key: keep it to yourself): http://127.0.0.1:8080/?key=...
```

The part after `key=` is the **start key**, a long random word. Open that address once: the browser gets a
cookie, the address bar shows the plain `http://127.0.0.1:8080`, and from then on that browser may open the
plain address. Every other request gets one line: "Test build: open the address printed in its console
window. Paste it into the address bar once; a link on another web page does not open the panel." The address
with the key also works when you click it in a chat or a notes page (it answers with a small page that goes
on by itself). The plain address does not: opened from a link on another web page it gets the line above,
also later; type or paste it, or use a bookmark. The key is kept in `fairy-test\data\start-key` and stays the
same at every start, so the address can be bookmarked. Delete that file and the next start makes a new key.

**A test build trusts every program on this PC. Run it only on a machine you trust, never on a server.** A
browser sends the test panel's cookies to every web server on `127.0.0.1`, whatever its port (cookies know no
port; the port in the cookies' names only keeps two test panels apart). So another program on this PC that
listens on a loopback port, and that this browser opens once, can read both cookies and is then admin of
the test panel. On Windows the "mode 0600" of `start-key` and of the data folder does nothing: the files
take the rights of the folder the zip was unpacked into, so unpack it where other accounts of the PC cannot
read (your own profile folder, not a shared drive).

**Never put a test build behind a reverse proxy, a port forward or a tunnel** (nginx, `ssh -L`, `netsh
interface portproxy`, a VMware port rule). The checks in the list above cannot see every one of them: a proxy
that rewrites the host name and adds no header looks like a browser on this PC. If the port is forwarded all the
same, the start key is the last line of defence: whoever reaches the port without it gets the one line above
and nothing else. Whoever has the address with the key is admin.

Every page has a red bar at the top: **TEST BUILD: sign-in is off. Never expose this.** `fairy version`, the
Updates page and the start-up log say "TEST BUILD" too.

## What to click in zip 7

Unpack the zip (folder `Fairy-dashboard-part1-r7`), open `cmd.exe` in it, then:

```
go build -tags fairytest -o fairy-test.exe .\cmd\fairy
fairy-test.exe serve
```

On Linux or macOS give the program another name than its state folder, which is `fairy-test` next to the
program: `go build -tags fairytest -o fairy-t ./cmd/fairy`, then `./fairy-t serve`. (A program named
`fairy-test` stops with "Refusing to start: config .../fairy-test/config.yaml: ... not a directory".)

Open the address the console prints (`http://127.0.0.1:8080/?key=...`, see above). To keep the test VM you
added before, copy the folder `fairy-test` from the folder `Fairy-dashboard-part1-r6` into the folder
`Fairy-dashboard-part1-r7` **before** the first start.

1. **Overview**: one row per host. Under the name, small: the PC model. In the tile in front: the mark of its
   system (Windows, Debian, ...). Behind the name: small signs for the helper, the battery and the UPS, where
   the host has one. Point at a sign, then click it: a small box opens with the same words.
2. **Updates**: each ⓘ opens the longer explanation. The button is named **Update unpowered hosts** (no "…").
   Its dialog lists the test VM under "On now: update only, never switched off".
3. Tick the VM and start the run: it still ends with "It was on and stays on.", and the VM stays on.
4. A host that does not answer is "not sure" for 15 minutes before it can count as off. The count starts
   again at every start of the panel, so right after a start no machine is offered as off. It also starts
   again each time the host answers or refuses the connection (it runs, only its agent is stopped), or
   answers with anything else (an error, a connection it takes and then leaves without an answer).
5. **Sessions**: make the window narrower and wider. Nothing scrolls sideways.
6. **Settings**: the ⓘ buttons; the sign-in picture you set before is still there (with the copied folder).
7. If the VM has fans: change a fan. "Confirm it's you" names the change in words, not in braces. Else skip.
8. The VM's page, **⋯**, **Set model name**: type a name, **Save**. It is under the host's name on the
   Overview; the ⓘ in the "Model" line of its page says what the host reports. **Clear** brings that back.
9. **Settings**, "Pop-ups": press **Show a test pop-up**. It shows in the lower right corner for 5 seconds.
   A real one comes when you shut the VM down **inside Windows** (Start, Shut down) and about two minutes
   pass: "offline", with the VM's name. Shut down from the panel it is "off" and shows no pop-up.
10. **Buttons**: buttons in one row are the same height. Look at the foot of a dialog (**Cancel** beside
    **Save** in "Set model name", the run's dialog, "Confirm it's you"), the row at the top of the VM's page,
    and the rows of buttons on the Updates page. (New in zip 5, like the last sentence of step 4.)

Every page with made-up data, without any machine: build the plain program and start its demo.

```
go build -o fairy-demo.exe .\cmd\fairy
fairy-demo.exe serve --demo
```

It needs no config, listens on `http://127.0.0.1:8090` (this PC only, no sign-in, a yellow bar says DEMO) and
keeps its files in a folder `.fairy-demo` where you started it. Stop either program with `Ctrl+C`.

## A. Build and start (dev VM, `cmd.exe`, Go installed)

1. Unpack the zip into your test folder. You get a folder like `Fairy-dashboard-part1-r7`.
2. Open `cmd.exe` and go into that folder (use your own path):

   ```
   cd /d E:\Projects\Fairy-dashboard-TEST\Fairy-dashboard-part1-r7
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
   three lines with `TEST BUILD: sign-in is off` (a line about this PC's address can come before them, and
   one that starts with `Add host:` comes after them: see part B), then
   `fairy listening ... addr=127.0.0.1:8080`, and last the line with the address to open:

   ```
   Open this address in a browser on this PC (it holds the start key: keep it to yourself): http://127.0.0.1:8080/?key=...
   ```
6. In the browser **on the dev VM**, open the address the console prints, with everything after `key=`
   (mark it in the `cmd.exe` window, Enter copies it; paste it into the address bar).

   You see the panel with the red bar, signed in as `test-admin`. No sign-in page. The address bar now shows
   `http://127.0.0.1:8080` without the key, and this browser may open that plain address from now on (a
   bookmark of the address with the key works for ever, in any browser on this PC).

   Without the key the answer is one line: "Test build: open the address printed in its console window."

   `http://localhost:8080` works too: the test build sends the browser on to `http://127.0.0.1:8080`, and
   the address bar shows that. (The page and its requests have to use one address. Under the other name the
   page used to load and then refuse the first change, for example Add host, with "cross-origin request
   refused".)
7. To stop it: `Ctrl+C` in the `cmd.exe` window.

If it prints `Refusing to start: ...`, the line says what is in the way (see the list above).

Port 8080 already in use? Start with another one, and open the address it prints: `fairy-test.exe serve --listen 127.0.0.1:8099`.

For a script or a test (curl, Playwright): read the key from `fairy-test\data\start-key` (the file holds the
key and a line end, nothing else) and open `/?key=<key>` first, with a cookie jar or in the browser context
that is used afterwards. For example `curl -c jar -b jar -L "http://127.0.0.1:8080/?key=<key>"`, then
`curl -b jar http://127.0.0.1:8080/api/me`. The cookie is named `fairytest_key_<port>`, so two test builds on
two ports do not take each other's cookie away.

## B. Add a test VM as a host

The agent is **never** installed on the dev VM. Use a throwaway Windows test VM on the same VMware network,
with a snapshot from before anything Fairy, and with internet access (the command downloads the released
Fairy-agent from GitHub and checks its signature, exactly like the real panel). The dev VM needs internet
access for this part too: before it shows the command, the panel fetches the release's signed `SHA256SUMS`
from GitHub and checks the install script with it. Without it the Add host dialog says "cannot check the
Fairy-agent install script" and shows no command.

1. On the **test VM**, in `cmd.exe`: `ipconfig`. Note its `IPv4 Address`.
2. On the **dev VM**, look at the lines `fairy-test.exe serve` printed. One has `Add host: panel address`
   and `address=...`. That must be the dev VM's own `IPv4 Address` on the VMware network (the test VM answers
   only that address). If it is another address, or a line says the PC has no private address or more than
   one: stop the panel, open `fairy-test\config.yaml` in Notepad, remove the `#` in front of
   `panel_address:` and put the dev VM's `IPv4 Address` there, save, start again.
3. In the panel: **Overview**, **Add host** (next to "Hosts"). **Name**: the test VM's computer name (letters,
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

### B2. Without internet: a host written into the config by hand

Part B needs GitHub on both VMs. Without it Add host shows no command, and the panel can still be tried
with a machine that **already runs fairy-agent** (a test VM you added earlier, for example): write it into
the test folder's config yourself. This is how a host is set up in a real panel's `config.yaml` too.

1. On the machine with the agent, as Administrator, read its two tokens: the one line in each of
   `C:\ProgramData\fairy-agent\token` and `C:\ProgramData\fairy-agent\control-token` (Linux:
   `/etc/fairy-agent/token` and `/etc/fairy-agent/control-token`). The second one is needed only for power
   actions, updates, fans and the Wake-on-LAN relay. Note the agent's address: the line `listen:` of
   `config.yaml` in the same folder (without that line it is the machine's Tailscale address, port 9101).
2. On the dev VM, stop the panel (`Ctrl+C`). Make a file `fairy-test\hosts.env` with the tokens (a name of
   your choice in front of each, capitals, digits and `_`):

   ```
   AGENT_TOKEN_TESTVM=<the line from token>
   AGENT_CONTROL_TOKEN_TESTVM=<the line from control-token>
   ```
3. Open `fairy-test\config.yaml` in Notepad and add at the end (put the agent's own address in place of
   `192.168.50.20`; `id` is small letters, digits and `-`, and not the id of a host the panel already has):

   ```
   updates: {check_latest: false}    # no GitHub: the Updates page then says "release check is off"
   agents:
     - id: testvm
       name: TESTVM
       url: "http://192.168.50.20:9101"
       token_env: AGENT_TOKEN_TESTVM
       control_token_env: AGENT_CONTROL_TOKEN_TESTVM   # leave this line and the next two out without a control token
       power_control: true
       self_update: true
   ```

   Power actions and Update work only where the agent's own `config.yaml` has them switched on as well.
4. Start the panel with the file: `fairy-test.exe serve --env-file fairy-test\hosts.env`. The host is on the
   Overview after the first poll. A wrong token shows it as offline with "http 401"; a name in `token_env`
   that is not in the file stops the start, and the line says which one.

A host written this way has no **Remove** and no **Re-pair** on its page (those belong to Add host): to take
it out, delete its lines from `fairy-test\config.yaml` and start again. The tokens stay in `hosts.env`; they
are the agent's real tokens, so delete the file when you are done.

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

1. Start the panel (part A). Open the address the console prints in the first window.
2. Open the same address, with the key, in the second window too (another browser or a private window has no
   cookie of the first one).
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

1. Menu, **Settings**, "Sign-in page": **Upload picture**, a PNG, JPEG or WebP of at most 8 MiB. The preview shows it.
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
   have been running all that time: if you start it again, the 15 minutes start again. They count from B's
   last sign of life: while Windows is still shutting down and the agent has already stopped, B refuses the
   connection, and the table says "Now: on"; the 15 minutes start when B gives nothing at all any more.
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
* **That a PC that is off counts as off.** The panel must get no answer at all from it for 15 minutes, and
  no refused connection or other answer either: each of those shows that the machine runs and starts the 15
  minutes again. A PC that is shut down gives none. If the report says "It is on (the machine refused the
  connection)" or "It is on (its address answered: ...)" for a PC that is off, something else answers at its
  address (a router, a firewall): tell us.
* **That a PC the run woke counts as woken.** The run compares the uptime the PC reports with the time since
  the wake. Windows with Fast Startup keeps counting its uptime through "Shut down"; such a PC is taken for
  "already on" and left on. Switch Fast Startup off on the PCs that take part, then look at the first
  report: it must say "Came online after the wake, updated ..., and switched off again".
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

## H. Trying the pop-ups

1. Menu, **Settings**, "Pop-ups": press **Show a test pop-up**. A pop-up shows in the lower right corner for
   5 seconds. Press the button ten times quickly: three show, the rest become "N more critical events".
2. Move "Stays for" to 3 s and press the button again: it stays 3 seconds. Untick "Show pop-ups": the button
   is off. Tick it again.
3. A real one: with the test VM added (part B) and online, shut the test VM down **in the VM itself** (Start,
   Shut down), not from the panel. Keep any page of the panel open. Within about two minutes (the panel
   waits before it calls a host offline) a pop-up "offline" with the VM's name shows, and the bell counts
   one more. (Shut down from the panel it is "off",
   not an alert, and shows no pop-up.)

## I. Trying the check on the machine itself

With the check on, the test VM itself verifies your passkey approval before a shutdown, a fan change or an
update (README, "The check on the machine itself"). The sign-in of the test build stays off, but **this part
is real**: the approval is asked at your real Pocket ID, with your real passkey, and the helper on the test VM
verifies it. Only one check is left out in a test build: the panel does not compare the approver with the
signed-in person (nobody is signed in), and says so in its window every time. The machine's own list of
who may approve is checked as always.

Not run by us: no step of this part was tried against a real Pocket ID or a real Windows helper. On Linux
the same build was run in a browser against a stand-in Pocket ID and the real agent (the report says what
was simulated). The stand-in is on another site than the panel and shows a page that needs a click before
it sends the browser back, as Pocket ID does with its passkey prompt. If a step goes differently, stop there
and send what I8 lists.

You need: the dev VM with this zip and agent zip 3 (`Fairy-agent-part1-r3`) unpacked **next to each other**,
the test VM `NSX-TEST-VM`, and your Pocket ID, reachable over https from the dev VM.

This is the second version of this part (zip 2). New since zip 1: every approval names its machine (I4,
step 7 and I10), Cancel needs no passkey (I5), Update all says what happened on each machine, and the agent
must be the one of agent zip 3: an agent of zip 2 on the VM shows "Agent too old" in its Approval box and
gets no approval.

### I0. The short path

The same as I1 to I5 in few words, for somebody who has done it before. Every step has its section below.

1. **Pocket ID**, as administrator: OIDC Clients, add a client. Name `Fairy PC actions`. Callback URL
   `http://127.0.0.1:8080/auth/approval/callback` for the test panel (and, for the real panel, a second one:
   `https://<public_url>/auth/approval/callback`). Public Client **on** and PKCE **on**; or a client that is
   not public, with its secret given to the panel through the environment only (step 3; never type the secret
   into a chat). Requires Re-Authentication **on**. Save.
2. **Build and start**, dev VM, `cmd.exe`. The folder of this panel and the folder `Fairy-agent-part1-r3`
   must be next to each other:

   ```
   cd /d E:\Projects\Fairy-agent-TEST\Fairy-dashboard-passkey-r2
   set GOOS=
   set GOARCH=
   go build -tags fairytest -o fairy-test.exe .\cmd\fairy
   fairy-test.exe serve
   ```

   The window prints `Open this address in a browser on this PC (it holds the start key: keep it to
   yourself): http://127.0.0.1:8080/?key=...`. Paste **that address** into the browser's address bar, once.
   A test build opens for no other address: `http://127.0.0.1:8080` alone answers "Test build: open the
   address printed in its console window." until this browser has opened the printed one.
3. Stop it (`Ctrl+C`). Open `fairy-test\config.yaml` in Notepad and add at the end, with your three values
   in place of the three `<...>` (the angle brackets go too):

   ```
   approval:
     issuer: <issuer>
     client_id: <client id>
     users: [<your Pocket ID user id>]
   ```

   * `<issuer>`: the address of your Pocket ID as its own address bar shows it, `https://` and the host
     name, nothing behind it. It is the `issuer:` under `oidc:` in the real panel's `config.yaml`.
   * `<client id>`: Pocket ID, OIDC Clients, `Fairy PC actions`: the field **Client ID**. Not the Client ID
     of the client the real panel signs in with ("Fairy"): the two are different clients.
   * `<your Pocket ID user id>`: Pocket ID, Users, open your own user: the last part of the address in the
     address bar. It is an id, not your name and not your e-mail address.

   Only for a client that is not public: `set FAIRY_APPROVAL_CLIENT_SECRET=` followed by the secret, in the
   same `cmd.exe` window, before the next line. Then `fairy-test.exe serve` again. The window must show a line
   with `!!! TEST BUILD: approvals for the machines are asked at` your Pocket ID's address and `callback
   http://127.0.0.1:8080/auth/approval/callback`.
4. The test VM with the agent of agent zip 3 (I3): its page has an **Approval** box that says "off".
5. **Turn on** in that box, **Turn on for 1 machine (passkey)**, **Continue to Pocket ID**, your passkey at
   Pocket ID, whose page must name the client `Fairy PC actions`. The browser comes back to the panel (see
   the note below), a message says "1 of 1 machines now check approvals themselves", and the Approval box
   says **required**.
6. Power menu, **Shut down**, 60 seconds, type the name. "Confirm it's you" says that NSX-TEST-VM will check
   this itself. **Continue to Pocket ID**, passkey. Back in the panel a bar on the VM's page counts down:
   "Shut down in 0:57 requested by test-admin", with **Cancel** at its right end.
7. **Cancel**: no dialog and no passkey. The message says "The shutdown of NSX-TEST-VM is cancelled." and
   the VM stays on.

**The way back from Pocket ID.** When Pocket ID sends the browser back, a white page with the one line "Fairy
test build: back from Pocket ID. Continue" can show for a moment. It goes on by itself; if it stays, press
**Continue**. (A test build's cookies are not sent with a jump that another site's page starts, so the panel
asks the browser to come again from its own page. A real panel does not need this.) If you see "Test build:
open the address printed in its console window." instead, this browser is not the one that opened the printed
address of step 2: open that address here and press the button again.

**"redirect_uri ... is not registered for this client"** on Pocket ID's page (step 5 or 6) means that the
`client_id` in `config.yaml` is another client's, usually the Client ID of the client the real panel signs in
with, or that the Callback URL line of step 1 is missing in `Fairy PC actions`. The panel shows the
**Client ID** it uses in the Turn on dialog and in "Confirm it's you", without a name: compare it with the
Client ID of `Fairy PC actions` in Pocket ID. Correct step 3 and start the panel again.

### I1. Pocket ID: one new client

In Pocket ID, as administrator: OIDC Clients, add a client.

| Field | Value |
| --- | --- |
| Name | `Fairy PC actions` |
| Callback URL | `http://127.0.0.1:8080/auth/approval/callback` (exactly this, for the test build) |
| Public Client | on |
| PKCE | on |
| Requires Re-Authentication | **on** |

Save, and copy its **Client ID** (of this client, not of the client the real panel signs in with: Pocket ID
lists both, and the panel cannot tell their ids apart by a name). Later, for the live panel, add a second Callback URL to the same client:
`https://<your panel>/auth/approval/callback` (the panel's `public_url` with `/auth/approval/callback`; a
client that is not public works too, with its secret as `FAIRY_APPROVAL_CLIENT_SECRET` in `fairy.env`).

Your **user id**: Pocket ID, Users, open your own user; the id is the last part of the address in the
browser's address bar. (If that is not how your Pocket ID shows it: I4, step 4 shows the id.)

### I2. Dev VM: build and start

Both folders must be in `E:\Projects\Fairy-agent-TEST`: `Fairy-agent-part1-r3` (agent zip 3) and
`Fairy-dashboard-passkey-r2` (this zip). This panel is built against that agent folder: with another folder
name, or without it, `go build` stops with "replacement directory ../Fairy-agent-part1-r3 does not exist".
In `cmd.exe`:

```
cd /d E:\Projects\Fairy-agent-TEST
tar -xf fairy-dashboard-passkey-testpanel-src-2.zip
cd Fairy-dashboard-passkey-r2
set GOOS=
set GOARCH=
go build -tags fairytest -o fairy-test.exe .\cmd\fairy
fairy-test.exe version
fairy-test.exe serve
```

`version` must say `TEST BUILD`. `serve` prints the address to open, with the start key (see the top of this
file): open it once. To keep the test VM and the settings of zip 1, copy the folder `fairy-test` from
`Fairy-dashboard-passkey-r1` into this folder **before** the first start; the start key and the address
stay the same then.

Stop it again (`Ctrl+C`), open `fairy-test\config.yaml` in Notepad and add these lines at the end (your
three values in place of the three `<...>`, without the angle brackets; with the copied folder they are there
already), save, start it again:

```
approval:
  issuer: <issuer>
  client_id: <client id>
  users: [<your Pocket ID user id>]
```

`<issuer>` is the address of your Pocket ID (`https://` and the host name, as in `oidc: issuer:` of the real
panel's config), `<client id>` the Client ID of I1, `<your Pocket ID user id>` the user id of I1.

The window must now show a line that starts with `time=... level=WARN msg="!!! TEST BUILD: approvals for the
machines are asked at` your Pocket ID's address and has `callback http://127.0.0.1:8080/auth/approval/callback`
in it. If it refuses to start, the line says which of the three values it does not accept.

### I3. The test VM

1. Add `NSX-TEST-VM` to this panel as in part B, with **power control** and **self-update** ticked.
   (Add host installs the released agent, build 16. It does not know the check yet: its page has no
   Approval box.)
2. Put the self-built agent of agent zip 3 on the VM over it: `TEST-ON-WINDOWS.md` in the agent folder,
   section 13, part A (steps 1 to 5). The script there installs it for both services and leaves the check
   **off**. If the VM still has the agent and the trust of zip 1 / agent zip 2: part H there (H1) says what
   to look at; the trust file gets a machine id by itself and stays as it was.
3. On the VM, PowerShell as Administrator:

   ```
   & "C:\Program Files\Fairy\fairy-agent.exe" version
   & "C:\Program Files\Fairy\fairy-agent.exe" helper-status
   ```

   `version` must say `16-selfbuilt`. `helper-status` must have `pipe protocol:   3` and the line
   `approval:        off (checked by the helper): no trust file, nothing needs an approval; the panel can set the trust once`.
4. In the panel, the VM's page now has an **Approval** box: "off", "Would be checked by: the elevated helper
   (LocalSystem)", and a **Turn on** button.

   If the box says "required" with the red sentence **Not on record** instead, the VM still has the check
   from an earlier test, and this panel did not turn it on (or it took the VM's id "at first sight" in an
   earlier build, which counts as none now). Either clear it on the VM (the command in I9; the box then says
   "Check gone", press **Accept that its check is off**, and go on with I4), or keep it and press **Accept
   this machine's id** (I10, steps 7 to 9) and go on with I5.

From here on do **not** press Re-pair for the VM, and do not press Update: both can put build 16 back.

### I4. Turn on, with your passkey

1. On the VM's page, Approval box: **Turn on**. (The same is on the Updates page: "1 machine is ready for
   the check on the machine itself", with the button **Turn on the check for 1 machine**.)
2. The dialog "Turn on the check on the machine itself" lists what the VM will trust: your Pocket ID's
   address, the **Client ID** (compare it with the Client ID of `Fairy PC actions` in Pocket ID: the panel
   shows the id only, it does not know a client's name), the signing key with its thumbprint, and "may approve:" with the user id from
   I2 and "(approval.users)" behind it: where the id comes from. Read the user id. Press **Turn on for 1 machine (passkey)**. "Confirm it's you" then shows the same
   once more, line by line; press **Continue to Pocket ID**.
3. The browser goes to your Pocket ID. Its page names the client, **Fairy PC actions**, and asks for your
   passkey. Use it.

   If Pocket ID shows an error instead, "redirect_uri ... is not registered for this client" (or its page
   names another client): the `client_id` in `fairy-test\config.yaml` is another client's, usually the
   Client ID of the client the real panel signs in with; or the Callback URL line of I1 is missing in
   `Fairy PC actions`. Nothing was changed on the VM. Correct `client_id` (I0, step 3), restart the panel
   and press **Turn on** again.
4. You are back in the panel, on the page you started from (a white page "Fairy test build: back from Pocket
   ID." can show for a moment on the way: I0, "The way back from Pocket ID"). The `fairy-test.exe serve` window has a new
   line that starts with `time=... level=WARN msg="!!! TEST BUILD: an approval was accepted without
   checking` and ends with `approver=<an id>`: that is your user id as Pocket ID sends it.
5. A message at the bottom right says "1 of 1 machines now check approvals themselves". The Approval box of
   the VM says **required**, with Pocket ID's address, the Client ID, the pinned key and "Allowed users: 1".
   The result line (**switched on · locked**) is on the **Updates** page, in the box "The check on the
   machine itself", under "Last Turn on".

   If it says **proof refused**: nothing was written on the VM and you can try again. The reason is in
   brackets in the line, and the ⓘ behind it shows the VM's own answer. `user`: the id in `users:` is not
   the one from step 4; correct it in `fairy-test\config.yaml`, restart the panel, do I4 again. `clock` or
   `auth_time`: compare the VM's clock with the real time.
6. On the VM: `helper-status` again. It must now say `approval:        required (checked by the helper)`,
   then `issuer:`, `client id:`, `pinned key:`, `allowed users:   1` and a line `machine id:`.
7. **The machine id.** In the Approval box, point at the row **Machine id**: the tooltip shows the whole id
   and "(turn-on, by test-admin)". It is the id of the `machine id:` line of step 6: the panel made it up
   and the VM took it with the trust. The row **Trust set** says "from a panel".

### I5. Shutdown with a delay, and cancel

1. VM's page, Power menu: **Shut down**, delay 60 seconds, type the name, confirm.
2. The dialog says that NSX-TEST-VM will check this itself, what your passkey signs ("shutdown in 60 s") and
   the Client ID. **Continue to Pocket ID**, passkey (Pocket ID's page names the client, "Fairy PC actions").
3. Back in the panel: the page shows the countdown. Windows on the VM shows its shutdown notice.
4. Press **Cancel** on the page within the minute. It needs no passkey and no confirmation of any kind:
   the countdown is gone at once, the message says "The shutdown of NSX-TEST-VM is cancelled." and the VM
   stays on.
5. Once more, to see that every action needs its own passkey: **Shut down** again asks again, also seconds
   after the last one. Cancel again.
6. Optional, the short countdown: **Shut down** with a delay of 10 seconds, passkey, then **Cancel** at
   once. The VM stays on. If you wait until it has begun to switch off, Cancel says "Too late" (and the VM
   switches off: start it again).

### I6. Fans, and what not to try

A VMware VM has no fans Fairy can set, so there is no Fans box and nothing to try here. Fan changes with the
30-second window need a real PC with fan control. Do not press **Update** for the VM (see I3).

### I7. What the VM must show

On the VM, PowerShell as Administrator:

```
& "C:\Program Files\Fairy\fairy-agent.exe" helper-status
Get-Content C:\ProgramData\fairy-agent\helper\helper.log -Tail 20
```

`helper.log` must have one line per request with `msg=request`: for I5 two with `verb=power`,
`args="action=shutdown delay_s=60"` and `status=200`, and two with `action=cancel`. A request the helper
refused has another status and an `error=` text that starts with `approval`. No line holds a token.

In the panel: Menu, **Audit**. For each approved action there is `auth.approval` twice ("redirected to
Pocket ID", then "ok" with the machine and the approver's id) and the action itself (`power.shutdown`,
`approval.turn_on`). A `power.cancel` has no `auth.approval` before it. `approval.machine_id` is the entry
for the id that came on record at Turn on (its first six characters, "how: turn-on", and who pressed Turn
on). A long line of details is cut: click it to see all of it.

### I8. What to send back

* the last 40 lines of the `fairy-test.exe serve` window (it never prints a token or a secret);
* the output of the two commands in I7;
* what the Approval box and the Turn on result say (a screenshot is fine), and for I10 the box with the
  red sentence;
* at which numbered step it went differently, if it did.

### I9. Afterwards

The check stays on until it is removed **on the VM** (the panel cannot):

```
& "C:\Program Files\Fairy\fairy-agent.exe" helper-trust clear --yes
```

Within half a minute the VM's Approval box has a red sentence: "Check gone." That is the alarm for a machine
that had the check and shows none (the panel cannot know that it was you). Press **Accept that its check
is off**, **Confirm (test build)**: the box says "off" again, with **Turn on**. The Notifications list says
"its check being off was accepted by test-admin".

Then put the VM back as `TEST-ON-WINDOWS.md` in the agent folder says under "Going back to build 16", or
back to its snapshot. In Pocket ID the client can stay: the live panel will use it with its own Callback
URL. To go back to the test panel of zip 1 or to an older panel: take the `approval:` block out of
`fairy-test\config.yaml` for a panel from before part I; the file `fairy-test\data\machines.json` can
stay, older panels do not read it.

### I10. The machine id: a changed id, an id that is not on record, and accepting it (about ten minutes)

Do this after I5, with the check on. It shows what the panel does when a machine is not the one it has on
record.

1. On the VM, PowerShell as Administrator:

   ```
   & "C:\Program Files\Fairy\fairy-agent.exe" helper-trust set --new-machine-id --yes
   ```

   It answers `the machine id:   NEW, it was <old>`.
2. Within half a minute the VM's page shows a red sentence in the Approval box: "Machine id changed. It
   shows another machine id than the one Fairy has on record. No approvals until an admin accepts the new
   id.", and the Overview shows a red key sign behind the VM's name (point at it, click it). The ⓘ in the
   box says what can cause it.
3. Power menu, **Shut down**, 60 seconds, type the name. No passkey is asked: the page answers that the VM
   shows another machine id and that nothing was sent. The VM stays on, and its `helper.log` has no new
   `msg=request` line.
4. In the Approval box press **Accept this machine's new id**. "Confirm it's you" says what would be
   accepted (the first six characters of the id it shows and of the one on record). **Confirm (test
   build)**. The red sentence is gone, and the tooltip of **Machine id** says "(accepted, by test-admin)".
5. **Shut down** again: now Pocket ID asks for the passkey, and the countdown starts. **Cancel**.
6. Menu, **Audit**: `approval.alarm` ("alarm: changed"), `approval.accept`, `approval.alarm` ("cleared").

A check the panel did not turn on ("Not on record"). The panel takes a machine id on record only when it
turned the check on itself (I4) or when you accept the id. To see that with the one VM, make the panel forget
what it has on record:

7. Stop the test panel (`Ctrl+C`), delete the file `fairy-test\data\machines.json`, start it again
   (`fairy-test.exe serve`). The VM still has the check on.
8. Within half a minute the VM's Approval box says "required" with the red sentence "Not on record. Fairy has
   no machine id on record for it. No approvals until an admin accepts its id.", and the Overview shows the
   red key sign. **Shut down** (60 seconds, the name): no passkey is asked; the page answers "NSX-TEST-VM
   checks approvals itself, and its machine id is not on record: an admin accepts it in the Approval box on
   its page first. Nothing was sent."
9. Press the ⓘ in the box: it shows the id the VM shows, in full. Compare it with the VM's own (on the VM
   `helper-status`, the line `machine id:`); a machine that answers in another's name would show the other's
   id. Then **Accept this machine's id**, **Confirm (test build)**. The red sentence is gone, the tooltip of
   **Machine id** says "(accepted, by test-admin)", and **Shut down** asks for the passkey again. **Cancel**.
   Audit: `approval.alarm` ("alarm: no_record"), `approval.accept`, `approval.alarm` ("cleared").

Not to be tried on one VM: two hosts with the same id (it needs a second machine made from a copy of the
first one's disk). The Linux run did it with two agents: both are flagged, neither gets an approval. The
host that has the id on record has the button "This host keeps its machine id"; the other has no button, its
box names the first and says to run `helper-trust set --new-machine-id` on that machine and then accept its
new id there. A host that shows an id which is on record for another host never gets it, also when the panel
sees that host for the first time. Two hosts that are both not on record and show one id each have "Accept
this machine's id", and the first accept decides.


## What the test build does NOT prove

* **Real sign-in.** Nothing goes to Pocket ID: not the sign-in, not the groups and allowed addresses, not a
  first sign-in that waits for access, not what happens when Pocket ID is down.
* **"Confirm it's you" at Pocket ID.** The dialog appears and the action runs after it, but the passkey check
  and what Pocket ID answers are not part of it.
* **Who approves.** Part I asks for a real passkey at your real Pocket ID and the machine checks it. But a
  test build has no signed-in person, so the panel's own check "the approver is the person who pressed the
  button" is off (the window says so each time). That check, the default list of approvers (the panel's
  admins and a host's owner) and the approval behind nginx and Cloudflare belong to the real panel.
* **Who accepts a machine's new id.** In a real panel "Accept this machine's new id" is for an admin in a
  browser, behind a passkey at Pocket ID ("Confirm it's you"). Here the confirmation is the test build's own
  button. Two hosts with one id, and a host that hands the panel's questions on to another machine, were run
  on Linux only (the report says how).
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
