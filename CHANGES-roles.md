# Users, roles and sites

## What's new

Pocket ID now only decides **who can sign in** to Fairy. **What each person can see and do** is set inside
Fairy, on a new **Users** page (admins only, in the menu under Audit).

Every person has one of three roles:

| Role | What they get |
| --- | --- |
| **Admin** | Everything: all hosts, control actions (power, fans, Wake-on-LAN, polling), the Audit page and the Users page. |
| **Viewer** | Read-only. Only the hosts, VMs/containers, power plugs, UPSes, processes and sensors of the **sites** you tick for them. |
| **No access** (pending) | Signed in, but sees only a "Waiting for access, ask an admin" page. Everyone new starts here. |

A **site** is a group of hosts, for example "Neptune" (your machines) and "Straw" (Straw's machines). You can
make as many as you like. Each thing belongs to one site:

* **Hosts** (Proxmox nodes and agent machines): the site you pick on the Users page. If you didn't pick one,
  the `site:` written in `config.yaml` (optional). Otherwise none.
* **VMs and containers** follow the node they run on, unless you pick a site for one of them.
* **Power plugs and UPSes** follow the hosts they are attached to (a UPS is shown to a viewer if it powers at
  least one host they can see, and only those hosts are listed). One that isn't attached to a host gets its own
  site on the Users page.
* **Anything without a site is visible to admins only.** So a new host never shows up for a viewer by surprise.

For viewers the panel only ever sends what they may see: the host list, the Overview totals (cluster CPU and
memory, counts, wall power are added up over their hosts only), power history, UPS alarms, the live updates.
A hidden host looks exactly like one that doesn't exist.

Every change on the Users page asks you to confirm it's you with Pocket ID (the same "Confirm it's you" screen
as for power actions, same 5-minute window), and is written to the Audit log (and sent to ntfy/Discord if you
set that up). Fairy refuses to demote or remove the **last admin**, so you can't lock yourself out by mistake.

## How to set it up

1. **Pocket ID:** keep (or make) a group for everyone who may sign in at all, e.g. `homelab`, and put it in
   `config.yaml` under `oidc: allowed_groups: [homelab]`. Add Straw (and anyone else) to that group.
2. **Make sure you become admin.** Either keep your admin group in `oidc: control_groups: [homelab-admins]`
   (anyone in it becomes **admin the first time they sign in**), or list your email:
   `oidc: bootstrap_admins: [you@example.com]`. If you already used `control_groups` before this update, you
   don't have to change anything: you'll be admin on your next visit.
3. Optional: write default sites into `config.yaml`, e.g. `site: Neptune` on your Proxmox cluster and agents,
   `site: Straw` on Straw's. Those sites appear on the Users page automatically. (Sites from `config.yaml` can
   only be renamed or removed in `config.yaml`.)
4. Restart Fairy, sign in, open **Users**.
5. Straw signs in once. He shows up under People with **No access (waiting)**. Set him to **Viewer** and tick
   **Straw**. He now sees only his own machines, read-only.
6. Check the **Sites** part of the page: every host, VM, plug and UPS shows which site it is in and who can see
   it. Pick a site for anything that says "admins only" and should be shared.

Good to know:

* After the first sign-in, the Users page is in charge: removing someone from `control_groups` in Pocket ID
  does **not** take their admin role away. Change it on the Users page.
* To lock someone out completely, set them to **No access** (and remove them from the Pocket ID group).
  "Remove" only clears them from the list; if they sign in again they start as waiting (or admin, if they are
  still in `control_groups` / `bootstrap_admins`).
* The control kill switch (`fairy admin control disable`) stops power/fan/polling actions, not the Users page.

## API tokens

* Tokens made before this update keep working exactly as before: a **read** token sees every site, a
  **control** token can do what an admin can.
* New: `fairy admin tokens create --name ha --owner system --scope read --sites Straw` makes a read token that
  only sees the Straw site. Control tokens cannot be limited (they act as an admin).
* A token whose `--owner` is a person who has signed in (their subject id from `fairy admin users list`) is
  never allowed more than that person: if you make them a viewer of Straw, their tokens see only Straw; if you
  set them to No access, their tokens stop working. Tokens owned by `system` keep their own scope.
* Why "read without sites = every site": only someone with a shell on the panel's server can create tokens, and
  existing scripts (Home Assistant etc.) must not break. Use `--sites` for anything you hand to someone else.

## If you lock yourself out

On the server running Fairy (as root or the service user):

```sh
fairy admin users list                                  # who is there, with which role
fairy admin users set-role you@example.com admin        # make yourself admin again
fairy admin users sites straw@example.com Straw         # make Straw a viewer of the Straw site
fairy admin users set-role straw@example.com pending    # take Straw's access away
fairy admin users remove someone@example.com            # forget someone (--force even for the last admin)
fairy admin sites list                                  # sites, who sees them, what is assigned
fairy admin sites assign straw-pc Straw                 # put a host in a site (creates the site if needed)
fairy admin sites assign power:desk-plug Straw          # a power plug (power:<id>) or UPS (ups:<id>)
fairy admin sites assign straw-pc -                     # back to its default site
```

The running panel picks these changes up within a second. They are written to the Audit log as source "cli".
Everything is stored in one file, `users.json`, in Fairy's data folder (`/var/lib/fairy` or
`C:\ProgramData\Fairy\data`). Deleting that file starts over: the next sign-in of someone in `control_groups` /
`bootstrap_admins` makes them admin again.

## Trying it in the demo

```sh
fairy serve --demo                          # as admin; Users page shows sites Neptune and Straw
fairy serve --demo --demo-as viewer:Straw   # exactly what a Straw-only viewer sees
```

The demo menu also has a **Preview as** switcher (Admin, Viewer of each site, Waiting for access). The demo
still has no login and only listens on 127.0.0.1.

## Menu

The gear in the top-right corner is now a **menu** button (three lines). It holds who you are signed in as (name,
email and role; a viewer also sees which sites), **Settings**, **Audit** (admins), **Users** (admins), the demo's
**Preview as** choices, and **Sign out**. The page you are on is marked. These entries are no longer in the sidebar,
which now only lists hosts, guests, Power and UPS. On phones the sidebar drawer button is a panel icon, so the two
buttons in the top bar don't look alike.

## Demo fix

The **Network** row on a host's page no longer flickers in the demo (the simulated network counters sometimes
went backwards, which the panel correctly treats as a counter reset).
