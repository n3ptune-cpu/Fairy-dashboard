package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const base = `
public_url: https://panel.example.com/
oidc: {issuer: "https://id.example.com", client_id: fairy}
`

func TestParseDefaultsAndProxmoxForms(t *testing.T) {
	c, err := Parse([]byte(base + `proxmox: {urls: ["https://pve:8006"], token_id: "fairy@pve!panel"}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseURL() != "https://panel.example.com" || c.Intervals.Agent != 1 || c.Intervals.Proxmox != 2 || c.StepUpWindowS != 180 ||
		len(c.Trusted()) != 2 || c.Proxmox[0].Prefix() != "" || !c.Boost() || c.LoginRatePerMin != 20 {
		t.Fatalf("%+v", c)
	}
	// the limit on control requests: 20 a minute per admin unless the file says otherwise (it was 6); a file that
	// has the old example's line keeps its 6
	if c.ControlRatePerMin != 20 || DefaultControlRatePerMin != 20 {
		t.Fatalf("control_rate_per_min defaults to %d, want 20", c.ControlRatePerMin)
	}
	if c6, err := Parse([]byte(base + "control_rate_per_min: 6\n")); err != nil || c6.ControlRatePerMin != 6 {
		t.Fatalf("control_rate_per_min: 6 in the file: %v %v", c6, err)
	}
	if example, err := os.ReadFile(filepath.Join("..", "..", "deploy", "config.example.yaml")); err != nil || !strings.Contains(string(example), "\ncontrol_rate_per_min: 20 ") {
		t.Fatalf("deploy/config.example.yaml must show the default of 20: %v", err)
	}
	if c, err := Parse([]byte(base + `enroll: {agent_release: build-9}`)); err != nil || c.Enroll.AgentRelease != "build-9" {
		t.Fatal(err)
	}
	if c.Enroll.AllowNonTailscale {
		t.Fatal("Add host outside Tailscale must be off unless config.yaml says so")
	}
	if c, err := Parse([]byte(base + `enroll: {allow_non_tailscale: true}`)); err != nil || !c.Enroll.AllowNonTailscale {
		t.Fatal(err)
	}
	if _, err := Parse([]byte(base + `enroll: {allow_non_tailscale: maybe}`)); err == nil {
		t.Fatal("allow_non_tailscale takes true or false only")
	}
	c, err = Parse([]byte(base + `proxmox: [{id: bob, urls: ["https://a:8006"], token_id: "x@pve!y"}]`))
	if err != nil || c.Proxmox[0].Prefix() != "bob-" {
		t.Fatal(err)
	}
}

func TestStrictValidation(t *testing.T) {
	bad := []string{
		base + "unknown_key: 1",
		base + `agents: [{id: "Bad ID", url: "http://a", token_env: T}]`,
		base + `proxmox: {urls: ["https://pve:8006"], token_id: "no-bang"}`,
		base + `systems: [{id: x, wol: {mac: "zz:zz", relay: nope}}]`,
		base + `trusted_proxies: ["not-a-cidr"]`,
		base + `enroll: {agent_release: latest}`,
		base + `enroll: {agent_release: "build-7; curl evil"}`,
	}
	for _, y := range bad {
		if _, err := Parse([]byte(y)); err == nil {
			t.Errorf("accepted:\n%s", y)
		}
	}
}

func TestEnvFileNeverOverrides(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fairy.env")
	_ = os.WriteFile(p, []byte("# c\nFAIRY_T1=\"from file\"\nexport FAIRY_T2=b\nbad line\n"), 0o600)
	t.Setenv("FAIRY_T2", "already")
	os.Unsetenv("FAIRY_T1")
	if err := LoadEnvFile(p); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("FAIRY_T1") != "from file" || os.Getenv("FAIRY_T2") != "already" {
		t.Fatal(os.Getenv("FAIRY_T1"), os.Getenv("FAIRY_T2"))
	}
	if err := LoadEnvFile(p + ".missing"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAIRY_SESSION_SECRET", "tiny-xyz")
	if _, err := LoadSecrets(); err == nil || strings.Contains(err.Error(), "tiny-xyz") {
		t.Fatal("short secret accepted or echoed")
	}
}

func TestBMCPowerValidation(t *testing.T) {
	t.Setenv("T", strings.Repeat("t", 32))
	t.Setenv("C", strings.Repeat("c", 32))
	agents := `agents: [{id: winpc, url: "http://100.64.0.11:9101", token_env: T, control_token_env: C}, {id: ro, url: "http://100.64.0.12:9101", token_env: T}]
`
	ok := base + agents + `systems: [{id: srv-tower, bmc_power: {relay: winpc, target: tower-bmc}}]`
	c, err := Parse([]byte(ok))
	if err != nil || c.Systems[0].BMCPower.Relay != "winpc" || c.Systems[0].BMCPower.Target != "tower-bmc" {
		t.Fatal(err)
	}
	for y, want := range map[string]string{
		base + agents + `systems: [{id: srv-tower, bmc_power: {relay: ro, target: tower-bmc}}]`:      "control_token_env",
		base + agents + `systems: [{id: srv-tower, bmc_power: {relay: nope, target: tower-bmc}}]`:    "control_token_env",
		base + agents + `systems: [{id: srv-tower, bmc_power: {relay: winpc, target: "Tower BMC"}}]`: "bmc_power.target",
		base + agents + `systems: [{id: winpc, bmc_power: {relay: winpc, target: x}}]`:               "itself",
	} {
		if _, err := Parse([]byte(y)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", y, err)
		}
	}
}

// step_up_per_action is off unless the config turns it on.
func TestStepUpPerActionKey(t *testing.T) {
	c, err := Parse([]byte(base))
	if err != nil || c.StepUpPerAction {
		t.Fatal("default must be off", err)
	}
	c, err = Parse([]byte(base + "step_up_per_action: true\nstep_up_prompt_login: true\n"))
	if err != nil || !c.StepUpPerAction || !c.StepUpPromptLogin {
		t.Fatal(err)
	}
	if _, err := Parse([]byte(base + "step_up_per_action: sometimes\n")); err == nil {
		t.Fatal("a non-boolean must be refused")
	}
}

// step_up_window_s: 3 minutes unless the config says otherwise; a value that is set is kept.
func TestStepUpWindowDefault(t *testing.T) {
	if c, err := Parse([]byte(base)); err != nil || c.StepUpWindowS != 180 {
		t.Fatal("default step_up_window_s must be 180", err)
	}
	for _, v := range []int{30, 300, 900, 86400} {
		c, err := Parse([]byte(base + "step_up_window_s: " + strconv.Itoa(v) + "\n"))
		if err != nil || c.StepUpWindowS != v {
			t.Fatalf("step_up_window_s: %d was not kept: %v", v, err)
		}
	}
	for _, v := range []string{"29", "86401", "-1"} {
		if _, err := Parse([]byte(base + "step_up_window_s: " + v + "\n")); err == nil {
			t.Errorf("step_up_window_s: %s accepted", v)
		}
	}
}

// L35 (panel side): "not a number" and infinity are not valid timings. A range check written as "below the
// minimum or above the maximum" lets .nan through, because every comparison with NaN is false.
func TestNaNAndInfTimingsRefused(t *testing.T) {
	for _, key := range []string{"request_timeout", "offline_after"} {
		for _, v := range []string{".nan", ".NaN", ".inf", "-.inf", "-1", "1e9"} {
			if _, err := Parse([]byte(base + key + ": " + v + "\n")); err == nil || !strings.Contains(err.Error(), key) {
				t.Errorf("%s: %s accepted (%v)", key, v, err)
			}
		}
	}
	for _, key := range []string{"agent", "proxmox", "ups", "history", "stream"} {
		for _, v := range []string{".nan", ".inf", "-.inf", "0.5", "301"} {
			if _, err := Parse([]byte(base + "intervals: {" + key + ": " + v + "}\n")); err == nil || !strings.Contains(err.Error(), "intervals."+key) {
				t.Errorf("intervals.%s: %s accepted (%v)", key, v, err)
			}
		}
	}
	c, err := Parse([]byte(base + "request_timeout: 2.5\noffline_after: 90\nintervals: {agent: 1, proxmox: 300, ups: 2.5}\n"))
	if err != nil || c.RequestTimeout != 2.5 || c.OfflineAfter != 90 || c.Intervals.Proxmox != 300 || c.Intervals.Stream != 1 {
		t.Fatal("ordinary values must still load:", err)
	}
	if c, err := Parse([]byte(base)); err != nil || c.RequestTimeout != 4 || c.OfflineAfter != 60 {
		t.Fatal("defaults:", err)
	}
}

// T-05: the session secret signs every session cookie, so a value everybody can read is no secret. The
// example line of deploy/fairy.env.example and one character repeated are refused, with a message that says
// what to do; a real random value is taken as before.
func TestLoadSecretsRefusesTheExampleAndRepeatedCharacters(t *testing.T) {
	example, err := os.ReadFile(filepath.Join("..", "..", "deploy", "fairy.env.example"))
	if err != nil {
		t.Fatal(err)
	}
	var placeholder string
	for _, l := range strings.Split(string(example), "\n") {
		if v, ok := strings.CutPrefix(l, "FAIRY_SESSION_SECRET="); ok {
			placeholder = v
		}
	}
	if len(placeholder) < 32 {
		t.Fatalf("deploy/fairy.env.example has no FAIRY_SESSION_SECRET line of 32+ characters: %q", placeholder)
	}
	for _, v := range []string{placeholder, `"` + placeholder + `"`, placeholder + "1", strings.Repeat("s", 32), strings.Repeat("0", 64)} {
		t.Setenv("FAIRY_SESSION_SECRET", v)
		_, err := LoadSecrets()
		if err == nil {
			t.Fatalf("%q is accepted as the session secret", v)
		}
		if !strings.Contains(err.Error(), "openssl rand -base64 48") || !strings.Contains(err.Error(), "FAIRY_SESSION_SECRET") {
			t.Fatalf("the message does not say what to do: %v", err)
		}
		if SessionSecretProblem(v) == "" {
			t.Fatalf("SessionSecretProblem(%q) finds nothing", v)
		}
	}
	// what `openssl rand -base64 48`, `openssl rand -hex 32` and a password manager give
	for _, v := range []string{"b0pV3m2x9QmJt8sYw1Zr5uKc7NfHd4Ea6LgTi0Oq2WsXy3Rv8BnMk1Jh5GfDc7Vb", "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
		"correct horse battery staple and then some", strings.Repeat("ab", 16)} {
		t.Setenv("FAIRY_SESSION_SECRET", v)
		if s, err := LoadSecrets(); err != nil || s.SessionSecret != v {
			t.Fatalf("%q is refused: %v", v, err)
		}
		if p := SessionSecretProblem(v); p != "" {
			t.Fatalf("SessionSecretProblem(%q) = %q", v, p)
		}
	}
	t.Setenv("FAIRY_SESSION_SECRET", "tiny-xyz")
	if _, err := LoadSecrets(); err == nil || err.Error() != "FAIRY_SESSION_SECRET is missing or shorter than 32 characters" {
		t.Fatalf("the message for a short secret changed: %v", err)
	}
}

// wake_update: the timings of "Update unpowered hosts". Left out, the defaults; a value outside its
// range stops the panel at start instead of making a run wait for a day or hammer an agent.
func TestWakeUpdateTimings(t *testing.T) {
	c, err := Parse([]byte(base))
	if err != nil || c.WakeUpdate != (WakeUpdate{OnlineTimeoutS: 300, SettleS: 60, ShutdownDelayS: 120, AgentRequestsPerMin: 8}) {
		t.Fatal("defaults:", c.WakeUpdate, err)
	}
	c, err = Parse([]byte(base + "wake_update: {online_timeout_s: 60, settle_s: 10, shutdown_delay_s: 10, agent_requests_per_min: 1}\n"))
	if err != nil || c.WakeUpdate != (WakeUpdate{OnlineTimeoutS: 60, SettleS: 10, ShutdownDelayS: 10, AgentRequestsPerMin: 1}) {
		t.Fatal("the smallest values:", c.WakeUpdate, err)
	}
	for _, bad := range []string{"online_timeout_s: 59", "online_timeout_s: 1801", "settle_s: 9", "settle_s: 601", "shutdown_delay_s: 9",
		"shutdown_delay_s: 3601", "agent_requests_per_min: 31", "agent_requests_per_min: -1", "online_timeout_s: soon", "hosts: [a]"} {
		if _, err := Parse([]byte(base + "wake_update: {" + bad + "}\n")); err == nil {
			t.Errorf("wake_update: {%s} accepted", bad)
		}
	}
	// r1-p4-07 (audit 5): a number with a fraction was cut off without a word (settle_s: 30.5 ran as 30). It is
	// refused, as for the session timings, and the message names the key and the line.
	for bad, want := range map[string]string{
		"settle_s: 30.5":                      "wake_update.settle_s: 30.5 is not a whole number",
		"online_timeout_s: 300.25":            "wake_update.online_timeout_s: 300.25 is not a whole number",
		"shutdown_delay_s: 1.2e1":             "", // 12: a whole number written another way
		"shutdown_delay_s: 1.25e1":            "wake_update.shutdown_delay_s: 1.25e1 is not a whole number",
		"agent_requests_per_min: 7.9":         "wake_update.agent_requests_per_min: 7.9 is not a whole number",
		"agent_requests_per_min: 8.0":         "", // what the plain field took and did not change
		"settle_s: 30":                        "",
		"settle_s: 0x1e":                      "",
		"settle_s: '30'":                      "cannot unmarshal !!str",
		"settle_s: [30]":                      "cannot unmarshal !!seq",
		"settle_s: 30, settle_s: 40":          "already defined",
		"settle_s: 30, settel_s: 40":          "field settel_s not found in wake_update",
		"online_timeout_s: .inf":              "cannot unmarshal",
		"online_timeout_s: 1e30":              "cannot unmarshal",
		"settle_s: ~, shutdown_delay_s: null": "",
	} {
		c, err := Parse([]byte(base + "wake_update: {" + bad + "}\n"))
		switch {
		case want == "" && err != nil:
			t.Errorf("wake_update: {%s}: %v", bad, err)
		case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
			t.Errorf("wake_update: {%s}: want an error with %q, got %v (%+v)", bad, want, err, c)
		case want != "" && !strings.Contains(err.Error(), "line "):
			t.Errorf("wake_update: {%s}: the error names no line: %v", bad, err)
		}
	}
	// r2-p4-13 (audit 5): 0 was taken for every key and ran as the default (shutdown_delay_s: 0 was 120
	// seconds, agent_requests_per_min: 0 was 8), while the example gives ranges that begin at 60, 10, 10 and 1.
	// A 0 that is written is refused with the key, the line, the range and the way to get the default.
	for bad, want := range map[string]string{
		"online_timeout_s: 0":               "wake_update.online_timeout_s: 0 is not in 60-1800 (leave the key out for the default, 300)",
		"settle_s: 0":                       "wake_update.settle_s: 0 is not in 10-600 (leave the key out for the default, 60)",
		"shutdown_delay_s: 0":               "wake_update.shutdown_delay_s: 0 is not in 10-3600 (leave the key out for the default, 120)",
		"agent_requests_per_min: 0":         "wake_update.agent_requests_per_min: 0 is not in 1-30 (leave the key out for the default, 8)",
		"settle_s: -0":                      "wake_update.settle_s: -0 is not in 10-600",
		"settle_s: 0.0":                     "wake_update.settle_s: 0.0 is not in 10-600",
		"settle_s: 0x0":                     "wake_update.settle_s: 0x0 is not in 10-600",
		"settle_s: 30, online_timeout_s: 0": "wake_update.online_timeout_s: 0 is not in 60-1800",
	} {
		_, err := Parse([]byte(base + "wake_update: {" + bad + "}\n"))
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "line ") {
			t.Errorf("wake_update: {%s}: want an error with %q, got %v", bad, want, err)
		}
	}
	// the range the example file gives for each key is the one that is checked
	ex, err := os.ReadFile("../../deploy/config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for key, r := range wakeUpdateRanges {
		if !regexp.MustCompile(fmt.Sprintf(`(?s)#   %s: %d .*?\(%d-%d\)`, key, r[2], r[0], r[1])).Match(ex) {
			t.Errorf("deploy/config.example.yaml does not show %s with the default %d and the range %d-%d", key, r[2], r[0], r[1])
		}
		for _, v := range []int{r[0], r[1]} {
			if _, err := Parse([]byte(fmt.Sprintf("%swake_update: {%s: %d}\n", base, key, v))); err != nil {
				t.Errorf("%s: %d must be accepted: %v", key, v, err)
			}
		}
		for _, v := range []int{r[0] - 1, r[1] + 1} {
			if _, err := Parse([]byte(fmt.Sprintf("%swake_update: {%s: %d}\n", base, key, v))); err == nil {
				t.Errorf("%s: %d must be refused", key, v)
			}
		}
	}
	if !strings.Contains(string(ex), "0 is refused") {
		t.Error("deploy/config.example.yaml does not say that 0 is refused")
	}
	for _, empty := range []string{"wake_update:\n", "wake_update: {}\n", "wake_update: ~\n"} {
		if c, err := Parse([]byte(base + empty)); err != nil || c.WakeUpdate.SettleS != 60 {
			t.Errorf("%q: %v", empty, err)
		}
	}
	c, err = Parse([]byte(base + "wake_update: {shutdown_delay_s: 1.2e1, agent_requests_per_min: 8.0, settle_s: 0x1e}\n"))
	if err != nil || c.WakeUpdate != (WakeUpdate{OnlineTimeoutS: 300, SettleS: 30, ShutdownDelayS: 12, AgentRequestsPerMin: 8}) {
		t.Fatal("whole numbers written another way:", c.WakeUpdate, err)
	}
}

// r1-p4-02 and r1-p4-03 (audit 5): the last release refuses a config.yaml with a wake_update: block ("field
// wake_update not found"), and it stores a user's settings without the pop-up settings. The README's paragraph
// about going back named neither. It names every block this release adds to config.yaml, and the pop-ups.
func TestReadmeGoingBackNamesWhatTheOlderPanelRefuses(t *testing.T) {
	b, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	_, para, ok := strings.Cut(string(b), "\nGoing back to the older panel:")
	if !ok {
		t.Fatal("README.md has no paragraph \"Going back to the older panel\"")
	}
	para, _, _ = strings.Cut(para, "\n\n")
	para = strings.Join(strings.Fields(para), " ")
	for _, want := range []string{
		"It does not start when config.yaml has a `wake_update:` block: take the block out first.",
		"`enroll: {allow_non_tailscale: ...}`", "`step_up_per_action`", "`4h`",
		"pop-up settings (on or off, seconds, events) are the defaults again",
		"custom model names", "the machines switched on for \"Update unpowered hosts\"",
	} {
		if !strings.Contains(para, want) {
			t.Errorf("the paragraph lacks %q:\n%s", want, para)
		}
	}
	// the block is one this panel reads and an older one cannot: if it is ever renamed, the paragraph must follow
	if _, err := Parse([]byte(base + "wake_update: {settle_s: 30}\n")); err != nil {
		t.Fatal(err)
	}
}

// r2-p4-12 (audit 5): deploy/fairy.env.example had its comments behind the values. Uncommented as written,
// `FAIRY_INSECURE_COOKIES=1   # local http testing ONLY` is one value, comment included (for systemd's
// EnvironmentFile= and for LoadEnvFile), the setting was off and nothing said so.
func TestEnvExampleHasNoCommentBehindAValue(t *testing.T) {
	ex, err := os.ReadFile("../../deploy/fairy.env.example")
	if err != nil {
		t.Fatal(err)
	}
	assign := regexp.MustCompile(`^(?:# )?([A-Z][A-Z0-9_]*)=(.*)$`)
	seen := map[string]bool{}
	for i, line := range strings.Split(string(ex), "\n") {
		m := assign.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		seen[m[1]] = true
		if strings.ContainsAny(m[2], " \t#") {
			t.Errorf("deploy/fairy.env.example line %d: something follows the value of %s (a comment must be on a line of its own)", i+1, m[1])
		}
	}
	for _, name := range []string{"FAIRY_SESSION_SECRET", "FAIRY_OIDC_CLIENT_SECRET", "FAIRY_PROXMOX_TOKEN_SECRET", "AGENT_TOKEN_AGENT_1", "FAIRY_NOTIFY_URL", "FAIRY_INSECURE_COOKIES"} {
		if !seen[name] {
			t.Errorf("deploy/fairy.env.example no longer shows %s", name)
		}
	}
	// the file with its optional lines uncommented, loaded the way the panel loads it: the setting is on
	dir := t.TempDir()
	p := filepath.Join(dir, "fairy.env")
	_ = os.WriteFile(p, []byte(strings.ReplaceAll(string(ex), "\n# FAIRY_", "\nFAIRY_")), 0o600)
	for name := range seen {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	if err := LoadEnvFile(p); err != nil {
		t.Fatal(err)
	}
	if s, _ := LoadSecrets(); !s.InsecureCookies || s.OIDCClientSecret != "REPLACE_ME" || os.Getenv("FAIRY_NOTIFY_URL") != "https://ntfy.sh/your-private-topic" {
		t.Fatalf("the example's optional lines, uncommented, are not read as written: %+v", s.InsecureCookies)
	}
	if p := EnvProblems(); len(p) != 0 {
		t.Fatalf("the example, uncommented: %q", p)
	}
}

// A comment behind a value is named at start and by `fairy admin config check`: the variable, never its value.
func TestEnvProblemsNameACommentBehindAValue(t *testing.T) {
	for _, name := range []string{"FAIRY_INSECURE_COOKIES", "FAIRY_OIDC_CLIENT_SECRET", "FAIRY_NOTIFY_URL", "FAIRY_SESSION_SECRET", "AGENT_TOKEN_AGENT_1"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	if p := EnvProblems(); len(p) != 0 {
		t.Fatalf("a clean environment: %q", p)
	}
	// the old example's three lines, as systemd hands them over
	t.Setenv("FAIRY_INSECURE_COOKIES", "1                       # local http testing ONLY")
	t.Setenv("FAIRY_OIDC_CLIENT_SECRET", "s3cr3t-value-of-the-client            # omit for a Pocket ID public client (PKCE only)")
	t.Setenv("AGENT_TOKEN_AGENT_1", "tok-0123456789abcdefghij\t# the laptop")
	t.Setenv("FAIRY_SESSION_SECRET", "a-secret-with-a-#-inside-but-no-blank-before-it-0123456789")
	t.Setenv("OTHER_THING", "x # y") // not the panel's
	p := EnvProblems()
	all := strings.Join(p, "\n")
	if len(p) != 4 {
		t.Fatalf("want 4 problems, got %d:\n%s", len(p), all)
	}
	for _, want := range []string{"AGENT_TOKEN_AGENT_1 has a blank and a \"#\" in its value", "FAIRY_INSECURE_COOKIES has a blank and a \"#\" in its value",
		"FAIRY_OIDC_CLIENT_SECRET has a blank and a \"#\" in its value", "FAIRY_INSECURE_COOKIES is set to something that is neither"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in:\n%s", want, all)
		}
	}
	for _, secret := range []string{"s3cr3t-value", "tok-0123456789", "a-secret-with", "local http testing"} {
		if strings.Contains(all, secret) {
			t.Fatalf("a value is shown: %s", all)
		}
	}
	if s, _ := LoadSecrets(); s.InsecureCookies {
		t.Fatal("the value with a comment must not switch the setting on")
	}
	for v, problems := range map[string]int{"1": 0, "on": 0, "0": 0, "off": 0, " yes ": 0, "": 0, "2": 1, "enabled": 1} {
		for _, name := range []string{"FAIRY_OIDC_CLIENT_SECRET", "AGENT_TOKEN_AGENT_1"} {
			os.Unsetenv(name)
		}
		t.Setenv("FAIRY_INSECURE_COOKIES", v)
		if got := EnvProblems(); len(got) != problems {
			t.Errorf("FAIRY_INSECURE_COOKIES=%q: %q", v, got)
		}
	}
}
