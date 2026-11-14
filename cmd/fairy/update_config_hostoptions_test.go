package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `fairy check-config FILE` is what a `fairy update` asks the version it is about to install, before anything
// is replaced: "would you start with this config.yaml?". "Edit host" keeps what an admin changed in the panel
// in state.json ("host_options"), next to config.yaml's data folder. Whatever is under that key, the answer
// is about config.yaml alone and stays "ok": a broken override is ignored by the panel with a warning, it
// never stops a start, so it must never stop an update either. And config.yaml's own link fields, however
// odd, are not a reason to refuse the file (they never were).
func TestCheckConfigAnswersOKWhateverHostOptionsHolds(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TOK_R", strings.Repeat("r", 32))
	data := filepath.Join(dir, "data")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "config.yaml")
	body := cfgHead + "data_dir: " + data + "\nagents:\n  - {id: relay, url: \"http://relay:9101\", token_env: TOK_R,\n" +
		"     ups_powers: {\"bad name\": [PC, pc, pc]}, power_links: {Bad_ID: \"No Row\"}, power_exclude_from_total: [x, x]}\n"
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var agents []string
	for i := 0; i < 400; i++ {
		agents = append(agents, fmt.Sprintf(`"a%d": {"power_exclude_from_total": {"x": true}}`, i))
	}
	manyAgents := strings.Join(agents, ", ")
	for name, state := range map[string]string{
		"no state.json":               "",
		"overrides in order":          `{"host_options": {"rev": 3, "agents": {"relay": {"ups_powers": {"rack": {"relay": true}}}}}}`,
		"a text":                      `{"host_options": "x"}`,
		"a list":                      `{"host_options": [1, 2]}`,
		"null":                        `{"host_options": null}`,
		"an agent there is not":       `{"host_options": {"agents": {"gone": {"power_exclude_from_total": {"x": true}}}}}`,
		"fields that cannot be set":   `{"host_options": {"agents": {"relay": {"url": "http://198.51.100.7:9101", "power_control": true}}}}`,
		"fields of the wrong form":    `{"host_options": {"agents": {"relay": {"ups_powers": 7, "power_links": [], "power_exclude_from_total": {}}}}}`,
		"far too many agents":         `{"host_options": {"agents": {` + manyAgents + `}, "rev": "x"}}`,
		"a state.json that is cut":    `{"host_options": {"agents": {`,
		"a state.json that is empty":  " ",
		"a state.json that is a list": `[]`,
	} {
		if state == "" {
			_ = os.Remove(filepath.Join(data, "state.json"))
		} else if err := os.WriteFile(filepath.Join(data, "state.json"), []byte(state), 0o600); err != nil {
			t.Fatal(err)
		}
		before, _ := os.ReadFile(filepath.Join(data, "state.json"))
		var out bytes.Buffer
		if code := checkConfig([]string{file}, &out); code != 0 || out.String() != "fairy check-config 1 ok\n" {
			t.Errorf("%s: exit %d: %s", name, code, out.String())
		}
		if why := configFileProblem(file); why != "" { // what the running panel asks before it starts its own update
			t.Errorf("%s: configFileProblem: %q", name, why)
		}
		if after, _ := os.ReadFile(filepath.Join(data, "state.json")); !bytes.Equal(before, after) {
			t.Errorf("%s: check-config changed state.json", name)
		}
	}
}
