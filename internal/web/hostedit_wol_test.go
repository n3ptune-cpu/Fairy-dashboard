package web

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// "Edit host", Wake-on-LAN: the MAC address of a wake packet is the address of one network card (R6).

// A MAC address with the group bit set is the address of a group of network cards, not of one: a wake packet
// "for" it would be a frame to many. It is refused like the all-zero and the broadcast address, before
// anybody is asked to confirm, and a state.json that holds one is not used.
func TestEditHostWolRefusesMulticastMACs(t *testing.T) {
	r := hedSetup(t)
	for _, mac := range []string{"01:00:5e:00:00:01", "33:33:00:00:00:01", "03:00:00:00:00:ee", "01-80-C2-00-00-00", "ff:ff:ff:ff:ff:fe"} {
		for _, macs := range [][]string{{mac}, {"02:00:00:00:00:ee", mac}} {
			p := r.do("PUT", "/api/admin/hosts/pc-a/options", map[string]any{"wol": map[string]any{"macs": macs}}, "Content-Type", "application/json", "Cookie", r.a.sess, "X-Requested-With", "fairy")
			if p.code != 422 || !strings.Contains(string(p.raw), "is not the address of a network card") {
				t.Errorf("%v: %d %s", macs, p.code, p.raw)
			}
		}
	}
	if r.storedOptions() != nil {
		t.Fatalf("stored: %v", r.storedOptions())
	}
	// the locally administered and the ordinary addresses of one card stay what they were: taken
	r.wol("pc-a", map[string]any{"macs": []string{"02:00:00:00:00:ee", "00:11:22:33:44:55", "fe:ff:ff:ff:ff:ff"}})

	// a state.json written by hand: the decision is ignored whole, and config.yaml's Wake-on-LAN counts
	cfg := hedConfig(t)
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	hand := `{"host_options": {"rev": 2, "hosts": {"pc-a": {"wol": {"macs": ["01:00:5e:00:00:01"], "relay": "relay-b", "port": 7}}}}}`
	if err := os.WriteFile(filepath.Join(cfg.DataDir, "state.json"), []byte(hand), 0o600); err != nil {
		t.Fatal(err)
	}
	r = hedStart(t, cfg, hedBackend())
	if w := r.host("pc-a", "wol"); !slices.Equal(strs(w["macs"]), []string{"02:00:00:00:00:a1"}) || w["relay"] != "relay-a" || w["port"] != 9.0 || w["from"] != "config" {
		t.Errorf("the dialog on a file with a multicast MAC: %v", w)
	}
	r.off("pc-a")
	if p := r.wake("pc-a"); p.code != 200 {
		t.Fatalf("wake: %d %s", p.code, p.raw)
	}
	if got := strings.ToLower(strings.Join(r.wakes(), "\n")); !strings.Contains(got, "02:00:00:00:00:a1") || strings.Contains(got, "01:00:5e") || !strings.Contains(got, "relay-a") {
		t.Errorf("the wake that was sent: %s", got)
	}
}
