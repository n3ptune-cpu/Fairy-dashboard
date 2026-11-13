package config

import (
	"strings"
	"testing"
)

// Two systems: entries with one id were accepted, and only the last one counted, whole. The README's line for
// a machine's own timings, added under the entry that has the machine's wol:, took its Wake-on-LAN away; two
// entries with different timings gave the second one's. The file is refused now, with the id.
func TestTwoSystemsEntriesWithOneIDAreRefused(t *testing.T) {
	t.Setenv("TOK", strings.Repeat("0123456789abcdef", 2))
	head := "public_url: https://panel.example.com\noidc: {issuer: \"https://id.example.com\", client_id: cid, allowed_groups: [viewers], control_groups: [controllers]}\n" +
		"agents:\n  - {id: relay, url: \"http://relay:9101\", token_env: TOK, control_token_env: TOK2, wol_relay: true}\n  - {id: pc-a, url: \"http://pc-a:9101\", token_env: TOK}\n"
	t.Setenv("TOK2", strings.Repeat("fedcba9876543210", 2))
	const wol = "  - {id: pc-a, wol: {mac: \"00:00:5e:00:53:01\", relay: relay}}\n"
	for name, systems := range map[string]string{
		"the timings under the entry with the wol:": wol + "  - {id: pc-a, wake_update: {off_after_s: 360, shutdown_delay_s: 30}}\n",
		"two blocks of timings":                     "  - {id: pc-a, wake_update: {off_after_s: 3600}}\n  - {id: pc-a, wake_update: {off_after_s: 300}}\n",
		"the same entry twice":                      wol + wol,
	} {
		_, err := Parse([]byte(head + "systems:\n" + systems))
		if err == nil || !strings.Contains(err.Error(), "systems: two entries with the id pc-a: write everything for that machine into one entry") {
			t.Errorf("%s: %v", name, err)
		}
		if err != nil && strings.Count(err.Error(), "two entries with the id") != 1 {
			t.Errorf("%s: said more than once: %v", name, err)
		}
	}
	// one entry with both, and two machines with an entry each: as before
	cfg, err := Parse([]byte(head + "systems:\n  - {id: pc-a, wol: {mac: \"00:00:5e:00:53:01\", relay: relay}, wake_update: {off_after_s: 360}}\n  - {id: pc-b, wake_update: {off_after_s: 300}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if o := cfg.Systems[0]; o.Wol == nil || o.WakeUpdate == nil || o.WakeUpdate.OffAfterS != 360 || len(cfg.Systems) != 2 {
		t.Errorf("one entry with both: %+v", cfg.Systems)
	}
}
