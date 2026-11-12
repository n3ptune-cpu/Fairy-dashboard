package web

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/access"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/powerctl"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/scheduler"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

// r2-p2-02 (audit 5, round 2): a Proxmox node that has a display name in config.yaml
// (systems: - {id: node-..., name: ...}) and a running guest was switched off by a run. The run counted a
// node's guests by comparing each guest's node name (its name in Proxmox) with the name the panel SHOWS for
// the node, so a renamed node had no guests. The `systems:` entry is also where a node's wol: is written, so
// a node that can take part in a run is exactly a node with such an entry. The released code's "the panel
// itself runs on it" (powerctl.KillsPanel), a guest's site and the page's "Guests on this node" compared the
// same way.
//
// A guest is now matched to its node by the node's system id (model.System.ParentID, poller.GuestOf), in
// every place.

// setupSystems is setup with `systems:` entries (and panel_hosts) of the test's own.
func setupSystems(t *testing.T, panelHosts []string, systems ...config.SystemOverride) *env {
	t.Helper()
	cfg := testutil.Config(t, []string{"https://pve1:8006"}, "http://laptop", "http://tower")
	cfg.Systems = append(cfg.Systems, systems...)
	cfg.PanelHosts = panelHosts
	b := testutil.NewBackend()
	b.PVE = func() ([]jsonx.M, error) { // both nodes are up; win11 runs on SRV-PVEA, truenas on SRV-PVEB
		rows := testutil.PVEResources()
		rows[1]["status"], rows[4]["status"] = "online", "running"
		return rows, nil
	}
	p, err := poller.New(cfg, config.Secrets{SessionSecret: strings.Repeat("s", 48)}, b)
	if err != nil {
		t.Fatal(err)
	}
	// SRV-PVEA has an agent of its own (so it is a machine the Updates page lists)
	if err := p.AddAgent(config.Agent{ID: "pve-agent", Name: "PVE agent", LinksTo: "node-srv-pvea", Managed: true, Token: "read-word", ControlToken: "control-word",
		URL: "http://pve-agent", PowerControl: true, SelfUpdate: true}, nil); err != nil {
		t.Fatal(err)
	}
	b.Metrics["pve-agent"] = func() (jsonx.M, error) { return testutil.AgentMetrics(), nil }
	p.PollOnce(context.Background())
	s := New(cfg, config.Secrets{SessionSecret: strings.Repeat("s", 48)}, p, nil, "test")
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &env{t: t, s: s, srv: srv, b: b}
}

func snapOf(e *env, id string) model.System {
	for _, x := range e.s.Poller.Snapshot() {
		if x.ID == id {
			return x
		}
	}
	e.t.Fatalf("no system %s", id)
	return model.System{}
}

func TestGuestsAreMatchedToTheirNodeByItsIdentity(t *testing.T) {
	long := time.Now().Add(-time.Hour)
	for _, c := range []struct {
		name             string
		systems          []config.SystemOverride
		wantA, wantB     string // the running guests of SRV-PVEA and of SRV-PVEB
		shownA, shownB   string
		panelOn, panelNo string // the panel's own guest is qemu-100: which node "the panel runs on", which not
	}{
		{name: "no display name (the control)", wantA: "win11", wantB: "truenas", shownA: "SRV-PVEA", shownB: "SRV-PVEB"},
		{name: "a renamed node", systems: []config.SystemOverride{{ID: "node-srv-pvea", Name: "Big server"}},
			wantA: "win11", wantB: "truenas", shownA: "Big server", shownB: "SRV-PVEB"},
		{name: "two nodes with the same display name", systems: []config.SystemOverride{{ID: "node-srv-pvea", Name: "Server"}, {ID: "node-srv-pveb", Name: "Server"}},
			wantA: "win11", wantB: "truenas", shownA: "Server", shownB: "Server"},
		{name: "a node shown under the other node's Proxmox name", systems: []config.SystemOverride{{ID: "node-srv-pveb", Name: "SRV-PVEA"}},
			wantA: "win11", wantB: "truenas", shownA: "SRV-PVEA", shownB: "SRV-PVEA"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := setupSystems(t, []string{"qemu-100"}, c.systems...)
			ops := runOps{e.s}
			a, b := snapOf(e, "node-srv-pvea"), snapOf(e, "node-srv-pveb")
			if a.Name != c.shownA || b.Name != c.shownB {
				t.Fatalf("the nodes are shown as %q and %q", a.Name, b.Name)
			}
			// the run: a guest keeps its own node on, and only that one (a guest on another node does not count)
			if n, known, names := ops.Guests("node-srv-pvea", long); n != 1 || !known || names != c.wantA {
				t.Errorf("guests of SRV-PVEA: %d known=%v %q, want 1 %q", n, known, names, c.wantA)
			}
			if n, known, names := ops.Guests("node-srv-pveb", long); n != 1 || !known || names != c.wantB {
				t.Errorf("guests of SRV-PVEB: %d known=%v %q, want 1 %q", n, known, names, c.wantB)
			}
			// "the panel itself runs on it": the node of the panel's guest, and no other
			if !powerctl.KillsPanel(e.s.Poller, a) || powerctl.KillsPanel(e.s.Poller, b) {
				t.Errorf("KillsPanel: SRV-PVEA %v (want true), SRV-PVEB %v (want false)", powerctl.KillsPanel(e.s.Poller, a), powerctl.KillsPanel(e.s.Poller, b))
			}
			page := e.do("GET", "/api/admin/wake-update", nil, "Cookie", e.cookie(controller(), 0))
			if row := wuHostRow(t, page, "node-srv-pvea"); row["can_join"] != false || row["why_not"] != "the panel itself runs on it" {
				t.Errorf("the node the panel runs on can join a run: %v", row)
			}
			// a guest's site is its own node's
			sites := e.s.sites(access.Data{Assign: map[string]string{"node-srv-pvea": "Lab A", "node-srv-pveb": "Lab B"}}, e.s.Poller.Snapshot())
			if got := sites["qemu-100"]; got.Site != "Lab A" || got.From != fromNode {
				t.Errorf("win11's site: %+v, want Lab A from its node", got)
			}
			if got := sites["qemu-120"]; got.Site != "Lab B" || got.From != fromNode {
				t.Errorf("truenas's site: %+v, want Lab B from its node", got)
			}
			// and the guest says which node it is on by that node's id
			if g := snapOf(e, "qemu-100"); model.Deref(g.ParentID) != "node-srv-pvea" || model.Deref(g.Parent) != "SRV-PVEA" {
				t.Errorf("win11: parent_id %q parent %q", model.Deref(g.ParentID), model.Deref(g.Parent))
			}
		})
	}
}

// The whole run with the farm: the node has a display name, its guest win11 runs after the wake. Before the
// fix: "Came online after the wake, updated ..., and switched off again." Now it is left on with the guest's name.
func TestWakeUpdateARenamedNodeWithARunningGuestIsLeftOn(t *testing.T) {
	for _, shown := range []string{"", "Big server"} {
		t.Run("shown as "+shown, func(t *testing.T) {
			e := setup(t)
			f := newFarm(t, e)
			h := runNodeWithGuest(t, e, f, shown, "running")
			if h.State != wakerun.StLeftOn || h.On != wakerun.OnYes || !strings.Contains(h.Text, "1 guest is running on it (win11)") {
				t.Errorf("the node ended as %s (%s): %s", h.State, h.On, h.Text)
			}
			for _, r := range f.requests() {
				if r == "pve-agent /power" {
					t.Errorf("RULE BROKEN: a node with a running guest was asked to shut down: %v", f.requests())
				}
			}
		})
	}
}

// runNodeWithGuest: the node SRV-PVEA has an agent of its own, Wake-on-LAN through the NAS and the display
// name shown; it is off, a run wakes it, and the cluster then shows its guest win11 with guestStatus.
func runNodeWithGuest(t *testing.T, e *env, f *farm, shown, guestStatus string) *wakerun.Host {
	t.Helper()
	p := e.s.Poller
	nodeUp := false
	e.b.Mu.Lock()
	e.b.PVE = func() ([]jsonx.M, error) {
		rows := testutil.PVEResources()
		if !nodeUp { // the node is off: the cluster shows it and its guests as that
			rows[0]["status"], rows[2]["status"] = "offline", "stopped"
		} else {
			rows[2]["status"] = guestStatus
		}
		return rows, nil
	}
	e.b.Mu.Unlock()
	if err := p.AddAgent(config.Agent{ID: "pve-agent", Name: "PVE agent", LinksTo: "node-srv-pvea", Managed: true, Token: "read-word", ControlToken: "control-word",
		URL: "http://pve-agent", PowerControl: true, SelfUpdate: true},
		&config.SystemOverride{ID: "node-srv-pvea", Name: shown, Wol: &config.Wol{MAC: "00:00:5e:00:53:09", Relay: "nas"}}); err != nil {
		t.Fatal(err)
	}
	f.version["pve-agent"] = "16-aaaaaaa"
	e.b.Mu.Lock()
	e.b.Metrics["pve-agent"] = func() (jsonx.M, error) { return f.metrics("pve-agent") }
	e.b.Mu.Unlock()
	f.offline()
	f.optIn("node-srv-pvea")
	e.b.Reply = func(agent, path string) (int, jsonx.M, error) {
		if path == "/power" {
			return 200, jsonx.M{"action": "shutdown", "pending": jsonx.M{"action": "shutdown", "at": float64(f.now.Add(120 * time.Second).Unix()), "delay_s": 120.0, "requested_by": "x"}}, nil
		}
		return 0, nil, nil
	}
	if _, err := e.s.WakeRuns.Start(wakerun.StartRequest{Hosts: []string{"node-srv-pvea"}, By: "Alice", BySub: "u2"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 600; i++ {
		if a, _ := e.s.WakeRuns.Snapshot(); a != nil && hostOf(a, "node-srv-pvea").WakeSentAt != nil {
			break
		}
		f.tick()
	}
	// the farm's wake only knows pc-a and pc-b: the node starts as the packet would start it, its guest with it
	f.on["pve-agent"], f.booted["pve-agent"], f.ready["pve-agent"] = true, f.now.Add(5*time.Second), f.now.Add(20*time.Second)
	e.b.Mu.Lock()
	nodeUp = true
	e.b.Mu.Unlock()
	return hostOf(f.finish(), "node-srv-pvea")
}

// The page groups guests under their node the same way: by the node's id.
func TestUIGuestsAreListedUnderTheirNodeByItsId(t *testing.T) {
	js := appJS(t)
	for _, want := range []string{
		`const guestsOf = (node) => state.systems.filter((g) => (g.kind === "qemu" || g.kind === "lxc") && g.parent_id === node.id && g.cluster_id === node.cluster_id);`,
		`const parentOf = (g) => (g.parent ? nodesOf().find((n) => n.id === g.parent_id && n.cluster_id === g.cluster_id) : null);`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	if m := regexp.MustCompile(`\.parent === \w+\.name|\.name === \w+\.parent`).FindAllString(js, -1); len(m) != 0 {
		t.Errorf("app.js still matches a guest to a node by a name: %v", m)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the page's own functions are not run here")
	}
	var parts []string
	for _, re := range []string{`(?m)^const nodesOf = .*$`, `(?m)^const guestsOf = .*$`, `(?m)^const parentOf = .*$`} {
		m := regexp.MustCompile(re).FindString(js)
		if m == "" {
			t.Fatalf("app.js has no %s any more", re)
		}
		parts = append(parts, m)
	}
	script := `const state = { systems: [
  { id: "node-a", kind: "node", name: "Server", cluster_id: "c" }, { id: "node-b", kind: "node", name: "Server", cluster_id: "c" },
  { id: "qemu-100", kind: "qemu", name: "win11", parent: "SRV-PVEA", parent_id: "node-a", cluster_id: "c" },
  { id: "qemu-120", kind: "qemu", name: "truenas", parent: "SRV-PVEB", parent_id: "node-b", cluster_id: "c" } ] };
` + strings.Join(parts, "\n") + `
const names = (l) => l.map((g) => g.name).join(",");
console.log([names(guestsOf(state.systems[0])), names(guestsOf(state.systems[1])), parentOf(state.systems[2]).id, parentOf(state.systems[3]).id].join(" "));
`
	file := filepath.Join(t.TempDir(), "guests.js")
	if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, file).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "win11 truenas node-a node-b" {
		t.Errorf("two nodes with the same shown name: %q, want each node its own guest", got)
	}
}

// r2-p2-07 (audit 5, round 2): a guest that the Proxmox API reports as "unknown" on a node that is online was
// counted as not running (the systems list shows every state it does not know as "offline"), and the node
// was switched off. Only a guest that is stopped, in that word, lets its node go: every other state keeps the
// node on, and so does a Proxmox API that fails or has not answered since the node came online.
func TestOnlyAStoppedGuestLetsItsNodeBeSwitchedOff(t *testing.T) {
	long := time.Now().Add(-time.Hour)
	for _, c := range []struct {
		status  any // what the cluster says about win11 on SRV-PVEA (nil: no status at all)
		running int
		known   bool
		why     string
	}{
		{"stopped", 0, true, ""},
		{"running", 1, true, "win11"},
		{"unknown", 0, false, "the Proxmox API does not show as stopped: win11 (unknown)"},
		{"paused", 0, false, "the Proxmox API does not show as stopped: win11 (paused)"},
		{"suspended", 0, false, "the Proxmox API does not show as stopped: win11 (suspended)"},
		{"prelaunch", 0, false, "the Proxmox API does not show as stopped: win11 (state not known)"},
		{"STOPPED", 0, false, "the Proxmox API does not show as stopped: win11 (state not known)"},
		{"", 0, false, "the Proxmox API does not show as stopped: win11 (state not known)"},
		{nil, 0, false, "the Proxmox API does not show as stopped: win11 (state not known)"},
		{7.0, 0, false, "the Proxmox API does not show as stopped: win11 (state not known)"},
	} {
		e := setup(t)
		e.b.Mu.Lock()
		e.b.PVE = func() ([]jsonx.M, error) {
			rows := testutil.PVEResources()
			if c.status == nil {
				delete(rows[2], "status")
			} else {
				rows[2]["status"] = c.status
			}
			return rows, nil
		}
		e.b.Mu.Unlock()
		e.s.Poller.PollOnce(context.Background())
		n, known, why := runOps{e.s}.Guests("node-srv-pvea", long)
		if n != c.running || known != c.known || why != c.why {
			t.Errorf("win11 is %v: Guests = %d, known %v, %q; want %d, %v, %q", c.status, n, known, why, c.running, c.known, c.why)
		}
	}

	t.Run("a guest row that names no node counts for every node of its cluster", func(t *testing.T) {
		e := setup(t)
		e.b.Mu.Lock()
		e.b.PVE = func() ([]jsonx.M, error) {
			rows := testutil.PVEResources()
			rows[2]["status"] = "stopped"
			delete(rows[3], "node") // the container: stopped, and the cluster does not say where
			return rows, nil
		}
		e.b.Mu.Unlock()
		e.s.Poller.PollOnce(context.Background())
		if n, known, why := (runOps{e.s}).Guests("node-srv-pvea", long); n != 0 || known || why != "the Proxmox API does not show as stopped: minecraft (no node given)" {
			t.Errorf("Guests = %d, known %v, %q", n, known, why)
		}
	})

	t.Run("a guest whose polling is paused", func(t *testing.T) {
		e := setup(t)
		e.b.Mu.Lock()
		e.b.PVE = func() ([]jsonx.M, error) {
			rows := testutil.PVEResources()
			rows[2]["status"] = "stopped"
			return rows, nil
		}
		e.b.Mu.Unlock()
		e.s.Poller.PollOnce(context.Background())
		if n, known, _ := (runOps{e.s}).Guests("node-srv-pvea", long); n != 0 || !known {
			t.Fatalf("all guests stopped: %d %v", n, known)
		}
		// the panel keeps what it last saw of win11, and that may be old
		if err := e.s.Poller.SetPoll("qemu-100", &scheduler.Override{Paused: true}); err != nil {
			t.Fatal(err)
		}
		if n, known, why := (runOps{e.s}).Guests("node-srv-pvea", long); n != 0 || known || why != "the Proxmox API does not show as stopped: win11 (polling paused)" {
			t.Errorf("Guests = %d, known %v, %q", n, known, why)
		}
	})

	t.Run("a Proxmox API that fails", func(t *testing.T) {
		e := setup(t)
		e.b.Mu.Lock()
		e.b.PVE = func() ([]jsonx.M, error) { return nil, context.DeadlineExceeded }
		e.b.Mu.Unlock()
		e.s.Poller.PollOnce(context.Background())
		if n, known, why := (runOps{e.s}).Guests("node-srv-pvea", long); n != 0 || known || why != "its Proxmox API has not answered since it came online" {
			t.Errorf("Guests = %d, known %v, %q", n, known, why)
		}
	})

	// the whole run: the node is woken, its guest shows as "unknown", the node is left on and no shutdown is sent
	for _, status := range []string{"unknown", "paused"} {
		t.Run("the run, guest "+status, func(t *testing.T) {
			e := setup(t)
			f := newFarm(t, e)
			h := runNodeWithGuest(t, e, f, "", status)
			want := "Left ON: Fairy cannot see whether guests run on it (the Proxmox API does not show as stopped: win11 (" + status + "))."
			if h.State != wakerun.StLeftOn || h.On != wakerun.OnYes || !strings.HasPrefix(h.Text, want) {
				t.Errorf("the node ended as %s (%s): %s", h.State, h.On, h.Text)
			}
			for _, r := range f.requests() {
				if r == "pve-agent /power" {
					t.Errorf("RULE BROKEN: a node with a guest of state %q was asked to shut down: %v", status, f.requests())
				}
			}
		})
	}
}
