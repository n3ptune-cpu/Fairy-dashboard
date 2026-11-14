package admin

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `fairy admin config check` and the machines themselves in "Edit host": a name and Wake-on-LAN details
// changed in the panel are not in config.yaml, so the command lists them with what config.yaml says, and
// has a WARN for every decision the panel does not use.
func TestConfigCheckAndTheMachinesOfEditHost(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	t.Setenv("TOK_R", "r")
	t.Setenv("CTL_R", "c")
	t.Setenv("FAIRY_SESSION_SECRET", strings.Repeat("no-secret-", 5)) // made here, for this test only
	t.Setenv("FAIRY_OIDC_CLIENT_SECRET", "")
	y := "public_url: https://p.example\ndata_dir: " + dir + "/data\noidc: {issuer: \"https://id.example\", client_id: c, allowed_groups: [users], control_groups: [admins]}\n" +
		"agents:\n" +
		"  - {id: relay-a, url: \"http://relay-a:9101\", token_env: TOK_R, control_token_env: CTL_R, wol_relay: true}\n" +
		"  - {id: relay-b, url: \"http://relay-b:9101\", token_env: TOK_R, control_token_env: CTL_R, wol_relay: true}\n" +
		"  - {id: pc-a, name: PC-A, url: \"http://pc-a:9101\", token_env: TOK_R}\n" +
		"  - {id: pc-b, url: \"http://pc-b:9101\", token_env: TOK_R}\n" +
		"  - {id: box-c, url: \"http://box-c:9101\", token_env: TOK_R}\n" +
		"systems:\n" +
		"  - {id: pc-a, wol: {mac: \"02:00:00:00:00:a1\", relay: relay-a}}\n" +
		"  - {id: box-c, wol: {mac: \"02:00:00:00:00:c1\", relay: relay-a, broadcast: 10.20.30.255}}\n"
	if err := os.WriteFile(cfg, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir+"/data", 0o700); err != nil {
		t.Fatal(err)
	}
	run := func(hosts string) (string, int) {
		t.Helper()
		if err := os.WriteFile(dir+"/data/state.json", []byte(`{"host_options": {"rev": 3, "hosts": `+hosts+`}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		var out, errb bytes.Buffer
		code := Main([]string{"--config", cfg, "--env-file", dir + "/none.env", "config", "check"}, &out, &errb)
		if !strings.Contains(out.String(), "config "+cfg+": valid") {
			t.Fatalf("the fixture is not a valid config:\n%s%s", out.String(), errb.String())
		}
		return out.String(), code
	}
	_, base := run(`{}`)
	out, code := run(`{"pc-a": {"name": "Gaming PC", "wol": {"macs": ["02:00:00:00:00:ee"], "relay": "relay-b", "port": 7}, "by": "admin", "at": "2026-11-14T10:00:00Z"},
		"pc-b": {"name": "Den", "wol": {"macs": ["02:00:00:00:00:b1"], "relay": "relay-a"}}, "relay-b": {"wol": {"off": true}}}`)
	for _, want := range []string{
		"ok    host pc-a: changed in the panel (\"Edit host\") by admin, 2026-11-14 10:00 UTC, used INSTEAD of what config.yaml says about the same thing until Reset: " +
			"name \"Gaming PC\" (config.yaml: \"PC-A\"); Wake-on-LAN to 02:00:00:00:00:ee through relay-b, port 7 (config.yaml: to 02:00:00:00:00:a1 through relay-a, port 9)\n",
		"name \"Den\" (config.yaml: the name the machine or its node reports); Wake-on-LAN to 02:00:00:00:00:b1 through relay-a, port 9 (config.yaml: none)\n",
		"ok    host relay-b: changed in the panel (\"Edit host\"), used INSTEAD of what config.yaml says about the same thing until Reset: Wake-on-LAN off (config.yaml: none)\n",
	} {
		if strings.Count(out, want) != 1 {
			t.Errorf("the check lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "WARN  Edit host") || code != base {
		t.Errorf("decisions that are used: exit %d (without them %d)\n%s", code, base, out)
	}
	// what the panel does not use: each with a WARN, and the command says so with its exit code
	out, code = run(`{"pc-a": {"wol": {"relay": "pc-b"}}, "pc-b": {"wol": {"macs": ["02:00:00:00:00:b1"]}}, "box-c": {"wol": {"relay": "relay-b"}},
		"relay-a": {"wol": {"macs": ["02:00:00:00:00:b2"], "relay": "relay-a"}}, "old-pc": {"name": "Old"}, "pc-a-x": {"wol": {"broadcast": "10.9.8.7"}}}`)
	for _, want := range []string{
		"WARN  Edit host: the Wake-on-LAN details changed in the panel for pc-a are not used (the relay pc-b has no wol_relay: true): config.yaml's count (to 02:00:00:00:00:a1 through relay-a, port 9)\n",
		"WARN  Edit host: the Wake-on-LAN details changed in the panel for pc-b are not used (no relay is set for it): config.yaml's count (none)\n",
		"WARN  Edit host: the Wake-on-LAN details changed in the panel for box-c are not used (config.yaml sets a target address for this machine's wake packet: it belongs to the relay written there)",
		"WARN  Edit host: the Wake-on-LAN details changed in the panel for relay-a are not used (a machine cannot send its own wake packet): config.yaml's count (none)\n",
		"WARN  Edit host: state.json has changes about old-pc itself (name, Wake-on-LAN), which is no agent's id and not the id of a node of a proxmox: entry: they are ignored (a host that was removed?)\n",
		"WARN  Edit host: an override in state.json is ignored: host pc-a-x: \"wol\": \"broadcast\" cannot be set in the panel: the configuration's Wake-on-LAN counts\n",
	} {
		if strings.Count(out, want) != 1 {
			t.Errorf("the check lacks %q:\n%s", want, out)
		}
	}
	if code == 0 {
		t.Errorf("decisions that are not used and exit 0:\n%s", out)
	}
	if strings.Contains(out, "10.9.8.7") {
		t.Errorf("the check repeats a value of a key it does not read:\n%s", out)
	}
}
