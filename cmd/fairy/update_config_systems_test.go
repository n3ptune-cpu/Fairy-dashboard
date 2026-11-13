package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Build 14 refuses a config.yaml with two systems: entries of one id (until then the last one counted, whole,
// and the first one's wol: was gone without a word). A file that an older panel runs with can have such a
// pair. What happens to it on `fairy update`: the new version is asked about the file BEFORE anything is
// replaced (check-config, the same check its start makes), it answers "refused" with the id, and the update
// stops there with the old panel still running (TestTheUpdateStopsBeforeTheSwapOnAConfigTheNewVersionRefuses
// is that run). It is not the new panel that fails at its start.
func TestCheckConfigRefusesTwoSystemsEntriesWithOneID(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TOK_R", strings.Repeat("r", 32))
	t.Setenv("CTL_R", strings.Repeat("c", 32))
	file := filepath.Join(dir, "config.yaml")
	body := cfgHead + "agents:\n  - {id: relay, url: \"http://relay:9101\", token_env: TOK_R, control_token_env: CTL_R, wol_relay: true}\n" +
		"systems:\n  - {id: pc-a, wol: {mac: \"00:00:5e:00:53:01\", relay: relay}}\n  - {id: pc-a, wake_update: {off_after_s: 360}}\n"
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	code := checkConfig([]string{file}, &out)
	if code != 1 || !strings.HasPrefix(out.String(), "fairy check-config 1 refused ") || strings.Count(out.String(), "\n") != 1 ||
		!strings.Contains(out.String(), "systems: two entries with the id pc-a: write everything for that machine into one entry") {
		t.Fatalf("exit %d: %s", code, out.String())
	}
	if v, why := readCheckAnswer(out.String()); v != newRefuses || !strings.Contains(why, "two entries with the id pc-a") {
		t.Fatalf("what the update reads: %d %q", v, why)
	}
	if why := configFileProblem(file); !strings.Contains(why, "two entries with the id pc-a") { // what the running panel says before it starts its own update
		t.Fatalf("configFileProblem: %q", why)
	}
	// one entry with both: accepted
	one := cfgHead + "agents:\n  - {id: relay, url: \"http://relay:9101\", token_env: TOK_R, control_token_env: CTL_R, wol_relay: true}\n" +
		"systems:\n  - {id: pc-a, wol: {mac: \"00:00:5e:00:53:01\", relay: relay}, wake_update: {off_after_s: 360}}\n"
	if err := os.WriteFile(file, []byte(one), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := checkConfig([]string{file}, &out); code != 0 || out.String() != "fairy check-config 1 ok\n" {
		t.Fatalf("one entry with both: exit %d: %s", code, out.String())
	}
}
