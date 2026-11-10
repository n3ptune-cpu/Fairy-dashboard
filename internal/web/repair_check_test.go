package web

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/enroll"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// repairRig is a panel with two Proxmox nodes added with Add host (alpha and beta). answer is what the next
// pairing request gets.
type repairRig struct {
	e      *env
	cookie string
	mu     sync.Mutex
	answer enroll.Response
}

func nodeAnswer(node, read string) enroll.Response {
	return enroll.Response{AgentVersion: "43-abc", Platform: "linux", Token: read + "-read-token-xxxxxxxxxxxxxxxxxxxx",
		ControlToken: read + "-control-token-yyyyyyyyyyyyyyyyy", Proxmox: &enroll.ProxmoxGrant{TokenID: "fairy@pve!panel-" + strings.ToLower(node),
			TokenSecret: "0b7c4e2a-1111-2222-3333-" + (read + "000000000000")[:12], Node: node, ServerName: node}}
}

func (r *repairRig) set(a enroll.Response) {
	r.mu.Lock()
	r.answer = a
	r.mu.Unlock()
}

// run posts an Add host or Re-pair and waits until its enrollment is done; it returns the final state and message.
func (r *repairRig) run(t *testing.T, path string, body map[string]any) (string, string) {
	t.Helper()
	hdr := []string{"Cookie", r.cookie, "X-Requested-With", "fairy"}
	p := r.e.do("POST", path, body, hdr...)
	if p.code != 201 {
		t.Fatal(path, p.code, string(p.raw))
	}
	id := p.body["id"].(string)
	for i := 0; i < 300; i++ {
		time.Sleep(20 * time.Millisecond)
		r.e.s.Poller.PollOnce(context.Background())
		g := r.e.do("GET", "/api/admin/enrollments/"+id, nil, "Cookie", r.cookie)
		if g.body["done"] == true {
			return g.body["state"].(string), g.body["message"].(string)
		}
	}
	t.Fatal("the enrollment never finished", path)
	return "", ""
}

func newRepairRig(t *testing.T) *repairRig {
	t.Helper()
	e := setup(t)
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
	for _, id := range []string{"alpha", "beta"} {
		e.b.Metrics[id] = func() (jsonx.M, error) { return testutil.AgentMetrics(), nil }
	}
	e.b.Mu.Unlock()
	for _, n := range []string{"alpha", "beta"} {
		r.set(nodeAnswer("PVE-"+strings.ToUpper(n), n))
		if st, msg := r.run(t, "/api/admin/hosts", map[string]any{"name": n, "os": "linux", "site": "Alice", "proxmox_new": true}); st != "online" {
			t.Fatal(n, st, msg)
		}
	}
	return r
}

// managedClusters is the id of every Proxmox node added with Add host that the poller polls, sorted by Add order.
func (r *repairRig) managedClusters() []string {
	var out []string
	for _, c := range r.e.s.Poller.Clusters() {
		if c.Managed {
			out = append(out, c.ID)
		}
	}
	return out
}

func (r *repairRig) secretFile(t *testing.T, id string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(r.e.s.Cfg.DataDir, "secrets", "agent-"+id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// L12. On Re-pair a machine that was added as one Proxmox node answers with the name of another managed node
// (it lies, or something answers in its place). The answer is checked like a new host's: refused, and the host
// list never holds two entries with the same Proxmox id. Nothing the panel had is changed.
func TestRepairRefusesAnAnswerThatCollidesWithAnotherNode(t *testing.T) {
	r := newRepairRig(t)
	if got := strings.Join(r.managedClusters(), ","); got != "alice-pve-alpha,alice-pve-beta" {
		t.Fatal(got)
	}
	before, _ := os.ReadFile(filepath.Join(r.e.s.Cfg.DataDir, "managed-hosts.yaml"))
	// alpha is re-paired and now claims to be beta's node: with the admin's "renamed" tick (so that only the
	// duplicate check stands in the way) and without it.
	for _, renamed := range []bool{true, false} {
		r.set(nodeAnswer("PVE-BETA", "liar"))
		st, msg := r.run(t, "/api/admin/hosts/alpha/repair", map[string]any{"confirm": "alpha", "node_renamed": renamed})
		if st != "failed" || !strings.Contains(msg, "Nothing was changed") {
			t.Fatal("renamed:", renamed, st, msg)
		}
		if renamed && !strings.Contains(msg, "alice-pve-beta") {
			t.Fatal("the refusal must name the entry it collides with:", msg)
		}
		if !renamed && !strings.Contains(msg, `added as "PVE-ALPHA"`) {
			t.Fatal(msg)
		}
		if got := strings.Join(r.managedClusters(), ","); got != "alice-pve-alpha,alice-pve-beta" {
			t.Fatal("clusters after a refused Re-pair:", got)
		}
		after, _ := os.ReadFile(filepath.Join(r.e.s.Cfg.DataDir, "managed-hosts.yaml"))
		if string(after) != string(before) {
			t.Fatalf("managed-hosts.yaml changed:\n%s", after)
		}
		if s := r.secretFile(t, "alpha"); strings.Contains(s, "liar") || !strings.Contains(s, "alpha-read-token") {
			t.Fatal("alpha's tokens were replaced by a refused answer:", s)
		}
		if a, ok := r.e.s.Poller.AgentByID("alpha"); !ok || !strings.HasPrefix(a.Token, "alpha-read-token") {
			t.Fatalf("alpha is no longer polled with its own token: %+v", a)
		}
		if _, ok := r.e.s.Poller.AgentByID("beta"); !ok {
			t.Fatal("beta is no longer polled")
		}
	}
	// a restart reads the same files: both hosts load, none is skipped
	hosts, err := r.e.s.Managed.Load()
	if err != nil || len(hosts) != 2 || hosts[0].Proxmox.ID == hosts[1].Proxmox.ID {
		t.Fatal(err, hosts)
	}
	if !strings.Contains(auditOutcomes(r.e.s.Cfg.AuditLog), "host.repair failed: cannot re-pair it") {
		t.Fatal(auditOutcomes(r.e.s.Cfg.AuditLog))
	}
}

// L12. A changed Proxmox node name needs the admin's word ("the node was renamed"); the same name does not.
func TestRepairNeedsTheSameNodeNameUnlessRenamed(t *testing.T) {
	r := newRepairRig(t)
	// the same node (another spelling of its name): accepted, new tokens
	r.set(nodeAnswer("pve-alpha", "second"))
	if st, msg := r.run(t, "/api/admin/hosts/alpha/repair", map[string]any{"confirm": "alpha"}); st != "online" {
		t.Fatal(st, msg)
	}
	if s := r.secretFile(t, "alpha"); !strings.Contains(s, "second-read-token") || strings.Contains(s, "alpha-read-token") {
		t.Fatal(s)
	}
	// another name that collides with nothing: refused without the tick, the host stays as it was
	r.set(nodeAnswer("PVE-GAMMA", "third"))
	st, msg := r.run(t, "/api/admin/hosts/alpha/repair", map[string]any{"confirm": "alpha"})
	if st != "failed" || !strings.Contains(msg, `Proxmox node "PVE-GAMMA"`) || !strings.Contains(msg, "the node was renamed") {
		t.Fatal(st, msg)
	}
	if s := r.secretFile(t, "alpha"); !strings.Contains(s, "second-read-token") {
		t.Fatal("tokens replaced by a refused answer:", s)
	}
	if got := strings.Join(r.managedClusters(), ","); got != "alice-pve-beta,alice-pve-alpha" {
		t.Fatal(got)
	}
	// with the tick: accepted, the node entry follows the new name, still two different entries
	st, msg = r.run(t, "/api/admin/hosts/alpha/repair", map[string]any{"confirm": "alpha", "node_renamed": true})
	if st != "online" {
		t.Fatal(st, msg)
	}
	if got := strings.Join(r.managedClusters(), ","); got != "alice-pve-beta,alice-pve-gamma" {
		t.Fatal(got)
	}
	if s := r.secretFile(t, "alpha"); !strings.Contains(s, "third-read-token") {
		t.Fatal(s)
	}
	out := auditOutcomes(r.e.s.Cfg.AuditLog)
	if !strings.Contains(out, "host.repair failed: the machine now says it is Proxmox node") {
		t.Fatal(out)
	}
	if b, _ := os.ReadFile(r.e.s.Cfg.AuditLog); !strings.Contains(string(b), `"node_renamed":true`) {
		t.Fatal("the admin's tick is not in the audit log")
	}
	// the body takes nothing else
	if p := r.e.do("POST", "/api/admin/hosts/alpha/repair", map[string]any{"confirm": "alpha", "force": true}, "Cookie", r.cookie, "X-Requested-With", "fairy"); p.code != 400 {
		t.Fatal(p.code, string(p.raw))
	}
}

// L24 through the API: the hosts of this rig run agent 43, the checked release is build-7. Re-pair answers with
// the warning, the dialog's progress keeps it, and the audit entry records that the admin was told.
func TestRepairWarnsWhenTheMachineRunsANewerAgent(t *testing.T) {
	r := newRepairRig(t)
	r.e.s.Enroll.Pair = func(context.Context, string, []byte) (int, enroll.Response, string, error) {
		return 404, enroll.Response{}, "pairing_inactive", nil
	}
	p := r.e.do("POST", "/api/admin/hosts/beta/repair", map[string]any{"confirm": "beta"}, "Cookie", r.cookie, "X-Requested-With", "fairy")
	w, _ := p.body["warning"].(string)
	if p.code != 201 || !strings.Contains(w, "This machine runs Fairy-agent 43-abc, which is newer than the release this command installs (build-7, Fairy-agent 7-6155fe6)") {
		t.Fatal(p.code, string(p.raw))
	}
	g := r.e.do("GET", "/api/admin/enrollments/"+p.body["id"].(string), nil, "Cookie", r.cookie)
	if g.body["warning"] != w {
		t.Fatal(string(g.raw))
	}
	if !strings.Contains(auditOutcomes(r.e.s.Cfg.AuditLog), "host.repair join code made (valid 15 min); warned: the machine runs a newer Fairy-agent than the release the command installs (build-7)") {
		t.Fatal(auditOutcomes(r.e.s.Cfg.AuditLog))
	}
	r.e.s.Enroll.Cancel(p.body["id"].(string))
}
