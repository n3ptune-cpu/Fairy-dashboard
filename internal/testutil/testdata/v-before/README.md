# v-before: a panel's files as the release before the security fixes wrote them

Used by the upgrade-in-place tests (`internal/web/upgrade_test.go`, `internal/admin/upgrade_test.go`): the new
code must start from these files, the existing admin must stay admin, and the break-glass CLI must still work.

Nothing here was written by hand. `generator_test.go.txt` was copied into `internal/web` of a checkout of commit
`05b1019` (as a `_test.go` file) and run there with `FAIRY_GEN_FIXTURE=<dir>`: that release's own code wrote
every file, through its HTTP handler, a sign-in round trip against a fake identity provider, and its admin CLI.

The files were made a second time when the example names in this repository were replaced by neutral ones
(host names, people, sites): the same generator, with only those names changed, run at `05b1019` again. The
test helpers the generator calls there (`internal/testutil/testutil.go` and the `_test.go` files of
`internal/web` of that commit) had the same names replaced first; no file of the release itself was changed.
Apart from the names, times, random ids and tokens, the files are the same as the first time (compared).

- `config.yaml`: the session values are plain numbers of seconds; none of the keys added later is in it. The
  tests replace `/var/lib/fairy` by a temporary directory and change nothing else.
- `config.example.yaml`: `deploy/config.example.yaml` of that release (`git show 05b1019:deploy/config.example.yaml`),
  with its example host names, people, sites and MAC addresses replaced by neutral ones. Every key and every
  other value is as that release shipped it.
- `var-lib-fairy/`: the data directory. Each file has `.old` added to its name, only because the repository's
  `.gitignore` ignores `data/`, `*.log`, `tokens.json`, `sessions.json` and `state.json`; the tests copy them
  back to their real names.
- `users.json.old`: no `email_verified` field. An admin (the owner, admin through `oidc.bootstrap_admins`), a
  viewer of the site Bob, someone waiting for access, and someone the owner removed.
- `sessions.json.old`, `state.json.old`, `settings.json.old`, `notifications.json.old`, `tokens.json.old`,
  `ups-events.jsonl.old`, `audit.log.old`: as that release left them.
- `generated.json`: when it was made, and the session cookies and API tokens that belong to these files.

Every name, address, token and cookie is made up; the cookies are signed with the test secret in `generated.json`.
