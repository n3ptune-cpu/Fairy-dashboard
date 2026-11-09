package enrollment

import (
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/enroll"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/managed"
)

func TestPVEFromGrant(t *testing.T) {
	h := managed.Host{ID: "asus-i7", URL: "http://100.64.0.22:9101", Site: "Straw's Lab"}
	good := enroll.ProxmoxGrant{TokenID: "fairy@pve!panel-asus-i7", TokenSecret: "0b7c4e2a-1111-2222-3333-444455556666", Node: "ASUS-i7"}
	p, ca, err := pveFromGrant(h, &good)
	if err != nil || ca != nil || p.ID != "straw-s-lab-asus-i7" || p.IDPrefix != "straw-s-lab-asus-i7-" || p.URL != "https://100.64.0.22:8006" ||
		p.ServerName != "ASUS-i7" || p.CA {
		t.Fatalf("%+v %v", p, err)
	}
	if p, _, _ := pveFromGrant(h, nil); p != nil {
		t.Fatal("no grant, no node")
	}
	for name, mod := range map[string]func(g *enroll.ProxmoxGrant){
		"token id": func(g *enroll.ProxmoxGrant) { g.TokenID = "root@pam!all" },
		"secret":   func(g *enroll.ProxmoxGrant) { g.TokenSecret = "short" },
		"node":     func(g *enroll.ProxmoxGrant) { g.Node = "bad node;" },
		"CA":       func(g *enroll.ProxmoxGrant) { g.CAPEM = "not a certificate" },
		"name":     func(g *enroll.ProxmoxGrant) { g.ServerName = "bad name!" },
	} {
		g := good
		mod(&g)
		if _, _, err := pveFromGrant(h, &g); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// the command asks the node for its token, in a root shell
	cmd := Command(testInstallers, managed.Host{OS: "linux", ProxmoxWanted: true}, "CODE", "100.88.109.47")
	if !strings.HasSuffix(cmd, " --proxmox") || strings.Contains(cmd, "sudo") {
		t.Fatal(cmd)
	}
}
