package config

import (
	"os"
	"path/filepath"
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
	if c, err := Parse([]byte(base + `enroll: {agent_release: build-9}`)); err != nil || c.Enroll.AgentRelease != "build-9" {
		t.Fatal(err)
	}
	c, err = Parse([]byte(base + `proxmox: [{id: straw, urls: ["https://a:8006"], token_id: "x@pve!y"}]`))
	if err != nil || c.Proxmox[0].Prefix() != "straw-" {
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
	agents := `agents: [{id: furina, url: "http://100.64.0.11:9101", token_env: T, control_token_env: C}, {id: ro, url: "http://100.64.0.12:9101", token_env: T}]
`
	ok := base + agents + `systems: [{id: nsx-cyrene, bmc_power: {relay: furina, target: cyrene-bmc}}]`
	c, err := Parse([]byte(ok))
	if err != nil || c.Systems[0].BMCPower.Relay != "furina" || c.Systems[0].BMCPower.Target != "cyrene-bmc" {
		t.Fatal(err)
	}
	for y, want := range map[string]string{
		base + agents + `systems: [{id: nsx-cyrene, bmc_power: {relay: ro, target: cyrene-bmc}}]`:       "control_token_env",
		base + agents + `systems: [{id: nsx-cyrene, bmc_power: {relay: nope, target: cyrene-bmc}}]`:     "control_token_env",
		base + agents + `systems: [{id: nsx-cyrene, bmc_power: {relay: furina, target: "Cyrene BMC"}}]`: "bmc_power.target",
		base + agents + `systems: [{id: furina, bmc_power: {relay: furina, target: x}}]`:                "itself",
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
