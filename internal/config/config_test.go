package config

import (
	"os"
	"path/filepath"
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
	if c.BaseURL() != "https://panel.example.com" || c.Intervals.Agent != 1 || c.Intervals.Proxmox != 2 || c.StepUpWindowS != 300 ||
		len(c.Trusted()) != 2 || c.Proxmox[0].Prefix() != "" || !c.Boost() || c.LoginRatePerMin != 20 {
		t.Fatalf("%+v", c)
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
