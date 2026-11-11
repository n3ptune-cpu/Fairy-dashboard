package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/enroll"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/access"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// T-48 (the verifier's end-to-end check, with what must hold written down). Through the panel's own API, with
// the repo's fake backend as the network:
//
//  1. Add host "a new Proxmox node": the machine names its node itself, here like a node of the owner's
//     cluster (SRV-PVEA; Proxmox's default name is "pve" on every install).
//  2. Re-pair, and the machine answers with tokens but without a Proxmox token (a hostile machine, or one
//     that has joined a cluster since).
//
// Before, the host kept the node name it gave itself and its agent was linked BY NAME to the owner's node of
// that name: the node's page showed the machine's readings and its "pending shutdown", requests for the node
// (shutdown, processes, fans) went to the machine's agent and the admin's shutdown answered OK without
// happening, and the node with its guests moved to the machine's site, where a viewer of that site could read
// them. Now such a host is what the dialog says: a plain host, linked to nothing.

func noGrantSetup(t *testing.T, nodePower bool, clusterSite string, ownAgent bool) *env {
	t.Helper()
	t.Setenv("PVE_SECRET", "pve-secret-uuid")
	dir := t.TempDir()
	y := fmt.Sprintf(`
public_url: https://panel.example.com
request_timeout: 1
offline_after: 60
audit_log: %q
data_dir: %q
oidc: {issuer: "https://id.example.com", client_id: cid, allowed_groups: [viewers], control_groups: [controllers]}
proxmox: {urls: ["https://pve1:8006"], token_id: "fairy@pve!panel", token_secret_env: PVE_SECRET, verify_tls: false,
          power_token_id: "fairy@pve!power", power_token_secret_env: PVE_SECRET, node_power: %v}
`, dir+"/audit.log", dir+"/data", nodePower)
	if ownAgent {
		t.Setenv("TOK_OWN", testutil.ReadToken)
		t.Setenv("CTL_OWN", testutil.ControlToken)
		y += "agents:\n  - {id: own, url: \"http://own\", token_env: TOK_OWN, node: SRV-PVEA, power_control: true, control_token_env: CTL_OWN}\n"
	}
	cfg, err := config.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Proxmox[0].Site = clusterSite
	b := testutil.NewBackend()
	if ownAgent {
		b.Metrics["own"] = func() (jsonx.M, error) { return testutil.AgentMetrics(), nil }
	}
	p, err := poller.New(cfg, config.Secrets{SessionSecret: strings.Repeat("s", 48)}, b)
	if err != nil {
		t.Fatal(err)
	}
	p.PollOnce(context.Background())
	s := New(cfg, config.Secrets{SessionSecret: strings.Repeat("s", 48)}, p, nil, "test")
	s.Enroll.Scripts = testInstallers
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &env{t: t, s: s, srv: srv, b: b}
}

// friendBoxMetrics is what the machine's agent reports: a reading and a pending shutdown of its own choice.
func friendBoxMetrics() jsonx.M {
	m := testutil.AgentMetrics()
	m["hostname"] = "friend-box"
	m["temps"] = []any{jsonx.M{"sensor": "coretemp", "label": "HOSTILE READING", "current": 21.0}}
	m["power_control"] = jsonx.M{"enabled": true, "pending": jsonx.M{"action": "shutdown",
		"at": float64(time.Now().Add(time.Hour).Unix()), "delay_s": 3600.0, "requested_by": "nobody"}}
	return m
}

func TestRepairAnsweredWithoutAProxmoxTokenLinksTheHostToNothing(t *testing.T) {
	for _, tc := range []struct {
		name        string
		nodePower   bool
		clusterSite string
		ownAgent    bool
		firstNode   string
	}{
		{"the name of the owner's node, node_power on", true, "", false, "SRV-PVEA"},
		{"the name of the owner's node, node_power off", false, "", false, "SRV-PVEA"},
		{"the owner's cluster has a site of its own", true, "Home", false, "SRV-PVEA"},
		{"the owner's node has an agent of its own", true, "", true, "SRV-PVEA"},
		{"another node name", true, "", false, "PVE-FRIEND"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := noGrantSetup(t, tc.nodePower, tc.clusterSite, tc.ownAgent)
			e.s.Enroll.Interval = 10 * time.Millisecond
			e.s.control = newRateLimiter(1000)
			e.s.Enroll.PanelAddr = "100.64.0.10"
			r := &repairRig{e: e, cookie: e.cookie(controller(), 0)}
			e.s.Enroll.Pair = func(context.Context, string, []byte) (int, enroll.Response, string, error) {
				r.mu.Lock()
				defer r.mu.Unlock()
				return 200, r.answer, "", nil
			}
			e.b.Mu.Lock()
			e.b.Metrics["friend-box"] = func() (jsonx.M, error) { return friendBoxMetrics(), nil }
			e.b.Mu.Unlock()
			admin := []string{"Cookie", r.cookie, "X-Requested-With", "fairy"}
			node := func() map[string]any { return e.do("GET", "/api/systems/node-srv-pvea", nil, "Cookie", r.cookie).body }
			shutdownOf := func() string {
				acts, _ := e.do("GET", "/api/systems/node-srv-pvea/power", nil, "Cookie", r.cookie).body["actions"].([]any)
				for _, a := range acts {
					if m := a.(map[string]any); m["action"] == "shutdown" {
						return fmt.Sprintf("available=%v via=%v", m["available"], m["via"])
					}
				}
				return "not offered"
			}
			// a viewer who may see the site the machine was added to, and nothing else
			_ = e.s.Access.CreateSite("Friend")
			if _, _, err := e.s.Access.Seen("friend", "friend@example.com", "friend", access.RolePending, "test"); err != nil {
				t.Fatal(err)
			}
			if _, _, err := e.s.Access.SetRole("friend", access.RoleViewer, []string{"Friend"}, "test", true); err != nil {
				t.Fatal(err)
			}
			friend := e.cookie(person("friend"), 0)
			friendSees := func() []string {
				var wrap struct {
					Systems []struct {
						ID string `json:"id"`
					} `json:"systems"`
				}
				rr := e.do("GET", "/api/systems", nil, "Cookie", friend)
				if rr.code != 200 || json.Unmarshal(rr.raw, &wrap) != nil {
					t.Fatalf("the viewer's list: %d %s", rr.code, rr.raw)
				}
				ids := []string{}
				for _, x := range wrap.Systems {
					ids = append(ids, x.ID)
				}
				sort.Strings(ids)
				return ids
			}
			if got := friendSees(); len(got) != 0 {
				t.Fatalf("before anything the viewer of site Friend sees %v", got)
			}
			shutdownBefore, tempsBefore := shutdownOf(), fmt.Sprint(node()["temps"])

			// 1. first pairing: a new Proxmox node, named by the machine
			r.set(nodeAnswer(tc.firstNode, "first"))
			if st, msg := r.run(t, "/api/admin/hosts", map[string]any{"name": "friend-box", "os": "linux", "site": "Friend",
				"proxmox_new": true, "options": map[string]any{"power_control": true}}); st != "online" {
				t.Fatal(st, msg)
			}
			if a, _ := e.s.Poller.AgentByID("friend-box"); a.LinksTo == "" || a.Node != "" {
				t.Fatalf("after Add host the agent is linked by name: node=%q links_to=%q", a.Node, a.LinksTo)
			}

			// 2. Re-pair, answered with tokens and no Proxmox token
			r.set(enroll.Response{AgentVersion: "43-abc", Platform: "linux", Token: "second-read-token-xxxxxxxxxxxxxxxxxxxx",
				ControlToken: "second-control-token-yyyyyyyyyyyyyyy"})
			st, msg := r.run(t, "/api/admin/hosts/friend-box/repair", map[string]any{"confirm": "friend-box"})
			if st != "online" || !strings.Contains(msg, "plain host") {
				t.Fatalf("Re-pair: %s: %s", st, msg)
			}
			e.s.Poller.PollOnce(context.Background())

			// the stored host is a plain host: no node name, no node system id
			y, _ := os.ReadFile(filepath.Join(e.s.Cfg.DataDir, "managed-hosts.yaml"))
			for _, l := range strings.Split(string(y), "\n") {
				if l = strings.TrimSpace(l); strings.HasPrefix(l, "node:") || strings.HasPrefix(l, "system_id:") || strings.HasPrefix(l, "proxmox:") {
					t.Errorf("managed-hosts.yaml still has %q", l)
				}
			}
			a, _ := e.s.Poller.AgentByID("friend-box")
			if a.Node != "" || a.LinksTo != "" || e.s.Poller.LinkOf(a) != "" {
				t.Errorf("the agent is linked: node=%q links_to=%q -> %q", a.Node, a.LinksTo, e.s.Poller.LinkOf(a))
			}
			if got, ok := e.s.Poller.AgentFor("node-srv-pvea"); got.ID == "friend-box" || ok != tc.ownAgent {
				t.Errorf("requests for the owner's node go to agent %q (%v)", got.ID, ok)
			}
			// it shows as a host of its own, in its own site, and that is all the site's viewer sees
			if own := e.do("GET", "/api/systems/friend-box", nil, "Cookie", r.cookie); own.code != 200 {
				t.Errorf("friend-box is not a system of its own: %d", own.code)
			}
			if got := friendSees(); len(got) != 1 || got[0] != "friend-box" {
				t.Errorf("the viewer of site Friend sees %v, want only friend-box", got)
			}
			// the owner's node shows what it showed before: not the machine's reading, not its pending shutdown
			n := node()
			if got := fmt.Sprint(n["temps"]); got != tempsBefore || strings.Contains(got, "HOSTILE") {
				t.Errorf("the owner's node shows temps %v (before: %v)", got, tempsBefore)
			}
			if !tc.ownAgent && n["power_pending"] != nil {
				t.Errorf("the owner's node shows a pending shutdown: %v", n["power_pending"])
			}
			if got := shutdownOf(); got != shutdownBefore {
				t.Errorf("shutdown of the owner's node: %s (before: %s)", got, shutdownBefore)
			}
			// 3. the admin shuts the owner's node down: it goes where it went before, never to the machine
			e.b.Mu.Lock()
			e.b.Requests, e.b.PVECalls = nil, nil
			e.b.Mu.Unlock()
			pw := e.do("POST", "/api/systems/node-srv-pvea/power", map[string]any{"action": "shutdown", "delay_s": 60, "confirm": "SRV-PVEA"}, admin...)
			pr := e.do("GET", "/api/systems/node-srv-pvea/processes", nil, "Cookie", r.cookie)
			e.b.Mu.Lock()
			reqs, pve := strings.Join(e.b.Requests, "; "), strings.Join(e.b.PVECalls, "; ")
			e.b.Mu.Unlock()
			if strings.Contains(reqs, "friend-box") {
				t.Errorf("a request for the owner's node went to the machine's agent: %s", reqs)
			}
			switch {
			case tc.ownAgent:
				if pw.code != 200 || !strings.Contains(reqs, "own POST /power") {
					t.Errorf("shutdown with the node's own agent: %d via=%v; agent requests: %s", pw.code, pw.body["via"], reqs)
				}
			case tc.nodePower:
				if pw.code != 200 || pw.body["via"] != "proxmox-api" || !strings.Contains(pve, "command=shutdown") {
					t.Errorf("shutdown with node_power: %d via=%v; Proxmox calls: %s", pw.code, pw.body["via"], pve)
				}
				if pr.code == 200 {
					t.Errorf("a process list for a node without an agent: %d", pr.code)
				}
			default:
				if pw.code == 200 {
					t.Errorf("shutdown of a node without agent and without node_power answered %d %s", pw.code, pw.raw)
				}
			}
		})
	}
}
