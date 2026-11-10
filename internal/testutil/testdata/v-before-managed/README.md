# v-before-managed: two hosts added with Add host by the release before the security fixes

Used by `internal/web/upgrade_managed_test.go`: the new panel must load these hosts, keep polling them with
their old tokens, show them as "not verified" (they were paired with the old exchange), and Re-pair must
replace such a pairing with a verified one.

Nothing here was written by hand. `generator_test.go.txt` was copied into `internal/web` of a checkout of commit
`05b1019` (as a `_test.go` file) and run there with `FAIRY_GEN_FIXTURE=<dir>`: that release's own HTTP handler,
enrollment manager and managed store wrote every file, with a simulated machine answering the old pairing
(tokens in clear).

- `managed-hosts.yaml`: NSX-FURINA (Windows, power control, self-update, Core Temp) and nsx-kachina (a
  standalone Proxmox node with its own `proxmox:` entry). No `pairing:` field: that came with pairing v2.
- `secrets/agent-<id>.json`: their tokens; `secrets/pve-nsx-kachina-ca.pem`: the node's CA (valid 30 years).
- `generated.json`: when it was made.

It goes on top of the `v-before` fixture (the same panel, one data directory): `testutil.AddVBeforeManaged`.
Every name, address, token and certificate is made up.
