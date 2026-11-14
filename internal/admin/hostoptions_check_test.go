package admin

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `fairy admin config check` and "Edit host": the overrides an admin made in the panel are not in
// config.yaml, so the command lists them: one ok line per agent, which says for every overridden field that
// config.yaml's value of it is not used, a WARN for everything the panel ignores, and notes about config.yaml's
// own UPS links (which nothing else looks at). config.yaml is never refused for its link fields.
func TestConfigCheckAndTheOverridesOfEditHost(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	t.Setenv("TOK_R", "r")
	t.Setenv("PVE_S", "s")
	t.Setenv("FAIRY_SESSION_SECRET", strings.Repeat("no-secret-", 5)) // made here, for this test only
	t.Setenv("FAIRY_OIDC_CLIENT_SECRET", "")
	y := "public_url: https://p.example\ndata_dir: " + dir + "/data\noidc: {issuer: \"https://id.example\", client_id: c, allowed_groups: [users], control_groups: [admins]}\n" +
		"proxmox:\n" +
		"  - {id: lab, id_prefix: lab-, urls: [\"https://pve:8006\"], token_id: \"fairy@pve!panel\", token_secret_env: PVE_S}\n" +
		"agents:\n" +
		"  - {id: nas-agent, url: \"http://nas:9101\", token_env: TOK_R,\n" +
		"     ups_powers: {rack: [lab-node-pve-a], \"bad name\": [pc-b]}, power_links: {a-plug: pc-a}, power_exclude_from_total: [corner]}\n" +
		"  - {id: pc-a, url: \"http://pc-a:9101\", token_env: TOK_R}\n" +
		"  - {id: pc-b, url: \"http://pc-b:9101\", token_env: TOK_R, power_exclude_from_total: [x, x]}\n"
	if err := os.WriteFile(cfg, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir+"/data", 0o700); err != nil {
		t.Fatal(err)
	}
	run := func(state string) (string, int) {
		t.Helper()
		if state == "" {
			_ = os.Remove(dir + "/data/state.json")
		} else if err := os.WriteFile(dir+"/data/state.json", []byte(state), 0o600); err != nil {
			t.Fatal(err)
		}
		var out, errb bytes.Buffer
		code := Main([]string{"--config", cfg, "--env-file", dir + "/none.env", "config", "check"}, &out, &errb)
		if !strings.Contains(out.String(), "config "+cfg+": valid") {
			t.Fatalf("the fixture is not a valid config:\n%s%s", out.String(), errb.String())
		}
		return out.String(), code
	}
	// no overrides: nothing about them, but the notes about config.yaml's own link fields
	plain, base := run("")
	if strings.Contains(plain, "Edit host\")") || strings.Contains(plain, "WARN  Edit host") {
		t.Fatalf("without overrides:\n%s", plain)
	}
	for _, want := range []string{
		"note  agent nas-agent: ups_powers: \"bad name\" is not a name a UPS can have (letters, digits and _ . : -, at most 64): nothing is refused for it, and \"Edit host\" cannot change this field until it is put right\n",
		"note  agent pc-b: power_exclude_from_total: x is there twice: nothing is refused for it, and \"Edit host\" cannot change this field until it is put right\n",
	} {
		if strings.Count(plain, want) != 1 {
			t.Errorf("the output lacks (or has twice) %q:\n%s", want, plain)
		}
	}
	// overrides in order: an ok line that names who, each field, and what of config.yaml is not used
	got, code := run(`{"host_options": {"rev": 7, "agents": {
		"nas-agent": {"ups_powers": {"rack": ["pc-b", "lab-node-pve-a"]}, "power_exclude_from_total": [], "by": "nora@example.com", "at": "2026-11-13T23:40:00Z"},
		"pc-a": {"power_links": {"own": "lab-qemu-100"}}}}}`)
	for _, want := range []string{
		"ok    agent nas-agent: changed in the panel (\"Edit host\") by nora@example.com, 2026-11-13 23:40 UTC: " +
			"ups_powers: rack -> pc-b, lab-node-pve-a (config.yaml's ups_powers of this agent is NOT used until Reset: bad name -> pc-b; rack -> lab-node-pve-a); " +
			"power_exclude_from_total: nothing (config.yaml's power_exclude_from_total of this agent is NOT used until Reset: corner)\n",
		"ok    agent pc-a: changed in the panel (\"Edit host\"): power_links: own -> lab-qemu-100 (config.yaml has none)\n",
	} {
		if strings.Count(got, want) != 1 {
			t.Errorf("the output lacks (or has twice) %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "WARN  Edit host") || code != base {
		t.Errorf("overrides in order: exit code %d (without them %d), or a WARN:\n%s", code, base, got)
	}
	// what the panel ignores: one WARN each, and the exit code says that something is to be looked at
	got, code = run(`{"host_options": {"agents": {
		"nas-agent": {"url": "http://198.51.100.7:9101", "power_links": 7, "ups_powers": {"rack": ["pc-b", "old-pc"]}},
		"gone-agent": {"power_exclude_from_total": ["x"]},
		"pc-a": {"power_links": {"own": "lab-node-"}}}}}`)
	for _, want := range []string{
		"WARN  Edit host: an override in state.json is ignored: agent nas-agent: \"url\" cannot be set in the panel: it is not used\n",
		"WARN  Edit host: an override in state.json is ignored: agent nas-agent: power_links is not of the form Fairy writes (found a number): config.yaml's value counts\n",
		"WARN  Edit host: state.json has overrides for gone-agent, which is no agent's id (config.yaml and the hosts added in the panel): they are ignored\n",
		"WARN  Edit host: an override of agent nas-agent names the row old-pc, which is no agent's id and not the id of a node or a guest of a proxmox: entry: it is ignored (a host that was removed?)\n",
		"WARN  Edit host: an override of agent pc-a names the row lab-node-, which is no agent's id and not the id of a node or a guest of a proxmox: entry: it is ignored (a host that was removed?)\n",
		"ok    agent nas-agent: changed in the panel (\"Edit host\"): ups_powers: rack -> pc-b, old-pc (config.yaml's ups_powers of this agent is NOT used until Reset: bad name -> pc-b; rack -> lab-node-pve-a)\n",
	} {
		if strings.Count(got, want) != 1 {
			t.Errorf("the output lacks (or has twice) %q:\n%s", want, got)
		}
	}
	if n := strings.Count(got, "WARN  Edit host"); n != 5 || code != 1 {
		t.Errorf("%d WARN lines about Edit host (want 5), exit code %d (want 1):\n%s", n, code, got)
	}
	if strings.Contains(got, "198.51.100.7") {
		t.Errorf("a value of the file is printed:\n%s", got)
	}
	// host_options of another kind altogether: one WARN, never a refusal of config.yaml
	for _, overlay := range []string{`"x"`, `[]`, `null`, `{"agents": []}`} {
		got, _ := run(`{"host_options": ` + overlay + `}`)
		if n := strings.Count(got, "WARN  Edit host: an override in state.json is ignored:"); n != 1 {
			t.Errorf("host_options %s: %d WARN lines, want 1:\n%s", overlay, n, got)
		}
	}
	// a state.json that cannot be read is said, and config.yaml is still "valid"
	got, _ = run(`{"host_options": {`)
	if !strings.Contains(got, "note  Edit host: state.json cannot be read, so the overrides made in the panel were not looked at\n") {
		t.Errorf("a state.json that is cut off:\n%s", got)
	}
}
