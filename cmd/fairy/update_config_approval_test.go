package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
)

// The approval: block (the Pocket ID client for PC actions) is checked by the one check of a config file,
// config.LoadFull: at the start, by `fairy admin config check`, and by the question `fairy update` asks the
// new version before it replaces the program. A block the panel would not start with, such as the sign-in
// client's own Client ID as approval.client_id, stops the update before the swap, in the same plain sentence
// the start gives.
func TestTheOneConfigCheckKnowsTheApprovalBlock(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(cfgHead+body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ask := func(path string) (int, string) { // the update's question to the new version
		var out bytes.Buffer
		code := checkConfig([]string{path}, &out)
		return code, out.String()
	}
	const sentence = "approval.client_id is the sign-in client's Client ID (oidc.client_id). Approvals need a separate client in Pocket ID"
	same := write("same.yaml", "approval: {client_id: fairy}\n") // cfgHead's oidc.client_id
	if code, out := ask(same); code != 1 || !strings.HasPrefix(out, checkConfigLead+"refused ") || !strings.Contains(out, sentence) || strings.Count(out, "\n") != 1 {
		t.Errorf("the update's check of a config with the sign-in client as approval client: %d %q", code, out)
	}
	if why := configFileProblem(same); !strings.HasPrefix(why, same+": ") || !strings.Contains(why, sentence) {
		t.Errorf("what the running panel says before it starts its own update: %q", why)
	}
	if _, err := config.LoadFull(same); err == nil || !strings.Contains(err.Error(), sentence) {
		t.Errorf("the start's check (config.LoadFull): %v", err)
	}
	for _, body := range []string{
		"approval: {client_id: \"has space\"}\n",
		"approval: {issuer: \"https://id.example.com\"}\n", // a block without client_id
		"approval: {client_id: pc-actions, users: [a, a]}\n",
		"approval: {client_id: pc-actions, run_approval_wait_s: 30}\n",
	} {
		if code, out := ask(write("bad.yaml", body)); code != 1 || !strings.Contains(out, "refused ") || !strings.Contains(out, "approval") {
			t.Errorf("%q: the update's check says %d %q, want a refusal that names the block", body, code, out)
		}
	}
	// a good block passes, and a mistake in a device list is still found, with its line, when the block is there
	if code, out := ask(write("good.yaml", "approval: {client_id: pc-actions, users: [user-1]}\nups:\n  - {id: rack, type: nut, host: 10.0.0.2}\n")); code != 0 || out != checkConfigLead+"ok\n" {
		t.Errorf("a good approval block: %d %q", code, out)
	}
	if code, out := ask(write("ups.yaml", "approval: {client_id: pc-actions}\nups:\n  - {id: rack-ups, type: nutt, host: 127.0.0.1}\n")); code != 1 || !strings.Contains(out, "line 5 (ups, entry 1)") {
		t.Errorf("a mistake in a ups entry under a good approval block: %d %q", code, out)
	}
}
