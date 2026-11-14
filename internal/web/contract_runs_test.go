package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/agenttest"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	agentapproval "github.com/n3ptune-cpu/Fairy-agent/pkg/approval"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/release"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/release/releasetest"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/approval"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/powerctl"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

// The contract of runs against Fairy-agent's REAL HTTP handler (the agent the module is built against): the
// panel's routes, its run and its client code on one side; on the other the agent's own server (agent.New(...)
// .Handler(): its routes, its strict decoding of every body, its control budgets), its real gate and run
// watch, its real power part with a fake shutdown command, and its real updater against a fake release
// server. The farm still says when the machine is on, what its uptime and its boot are and when an update is
// done; everything a run sends to the machine, and everything the machine says about its check and its run,
// goes over HTTP to that handler. The tests in wakeupdate_checked_test.go use the agent's gate without its
// handler (testutil.Machine); this file is where the two are held against each other.

const (
	contractRead    = "contract-read-word"
	contractControl = "contract-control-word"
)

// realAgent is one machine's fairy-agent.
type realAgent struct {
	t     *testing.T
	r     *runRig
	id    string
	trust string
	runs  bool // its checker takes runs (false: an agent as v0.8.0 was)
	srv   *httptest.Server
	h     atomic.Value // http.Handler: the agent process of this boot
	gate  *agentapproval.Gate
	watch *agentapproval.RunWatch
	exec  *agenttest.PowerExec
	upd   *agent.Updater
	gh    *releasetest.Server

	mu       sync.Mutex
	boot     string
	posts    []string // "<path> <status>" of every POST the panel sent
	up       bool
	minDelay int
	uptime   func() time.Duration // nil: the farm's
	clock    time.Duration        // added to the farm's clock: a machine whose clock is wrong
	farmNow  time.Time            // the farm's clock and this machine's start, as of the latest sync
	booted   time.Time
	applied  atomic.Int32 // installers started
}

// sync takes the farm's clock and the machine's start over. The agent's handler runs on the HTTP server's
// goroutines: what it reads of the farm is read from here, under this machine's own lock.
func (a *realAgent) sync() {
	a.r.b.Mu.Lock()
	booted := a.r.f.booted[a.id]
	a.r.b.Mu.Unlock()
	a.mu.Lock()
	a.farmNow, a.booted = a.r.f.now, booted
	a.mu.Unlock()
}

// set changes something of the machine under its lock.
func (a *realAgent) set(fn func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	fn()
}

func newRealAgent(t *testing.T, r *runRig, id, trust string, runs bool) *realAgent {
	t.Helper()
	a := &realAgent{t: t, r: r, id: id, trust: trust, runs: runs, minDelay: 10, up: true}
	a.gh = releasetest.New(t, release.AgentRepo)
	a.gh.Publish("build-17", release.AgentProgram, farmNew, map[string][]byte{release.AssetName(release.AgentProgram, "linux", "amd64"): []byte("fairy-agent 17")}, nil)
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) { a.h.Load().(http.Handler).ServeHTTP(w, q) }))
	t.Cleanup(a.srv.Close)
	a.start("boot-1")
	r.gb.Relay(id, a.relay)
	r.gb.Live(id, a.block)
	return a
}

func (a *realAgent) now() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.farmNow.Add(a.clock)
}

func (a *realAgent) since() time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.uptime != nil {
		return a.uptime()
	}
	return a.farmNow.Sub(a.booted)
}

// start is the agent starting: a new process (gate, run watch, power part, updater, handler) on the same
// files, in the boot it is told.
func (a *realAgent) start(boot string) {
	t := a.t
	a.sync()
	a.mu.Lock()
	a.boot = boot
	minDelay := a.minDelay
	a.mu.Unlock()
	cfg, err := agent.Parse([]byte(fmt.Sprintf("listen: 127.0.0.1:9101\npower_control: {enabled: true, min_delay_s: %d}\nself_update: {enabled: true}\ncontrol_rate_per_min: 60\n", minDelay)),
		filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	asset := release.AssetName(release.AgentProgram, "linux", "amd64")
	stage := t.TempDir()
	a.upd = agent.NewUpdater(agent.UpdaterOptions{Version: "contract", Key: a.gh.Pub, Client: a.gh.Client(), StageDir: filepath.Join(stage, "update"),
		ResultPath: filepath.Join(stage, "result.json"), Asset: asset, ResultWait: 100 * time.Millisecond,
		Apply: func(context.Context) error { a.applied.Add(1); return nil }})
	a.gate = agentapproval.NewGate(agentapproval.Options{Files: agentapproval.Files{Path: a.trust}, EnforcedBy: api.EnforcedByAgent, Runs: a.runs, MinDelayS: minDelay,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: a.now, Uptime: func() (time.Duration, error) { return a.since(), nil },
		BootID: func() (string, error) { a.mu.Lock(); defer a.mu.Unlock(); return a.boot, nil }})
	a.exec = &agenttest.PowerExec{}
	power := agent.NewPower(a.exec, minDelay)
	d := agent.Deps{Collector: agenttest.NewCollector(), GPU: agenttest.GPU{}, Updater: a.upd, Version: "contract", Approval: a.gate, Power: power}
	a.watch = nil
	if a.runs {
		// as the agent wires it (pkg/agent, Run): the self-off is its own power part's delayed shutdown
		a.watch = agentapproval.NewRunWatch(a.gate, agentapproval.RunHooks{Pending: power.Scheduled,
			Shutdown: func(delay time.Duration, message, who string) error {
				err := power.ScheduleOwn(delay, message, who)
				if err == nil { // the farm is what switches the machine off when the countdown is over
					at := a.now().Add(delay)
					a.r.b.Mu.Lock()
					a.r.f.pending[a.id] = at
					a.r.b.Mu.Unlock()
				}
				return err
			},
			Cancel: func() error { return power.CancelOwn("run") }, Updating: a.upd.Busy})
		d.Runs = a.watch
	}
	a.h.Store(agent.New(cfg, agent.Tokens{Read: contractRead, Control: contractControl}, d).Handler())
}

// reboot is the machine started anew right now: a new boot, a new agent process.
func (a *realAgent) reboot() {
	a.r.f.now = a.r.f.now.Add(time.Minute) // off for a minute: another boot, at another time
	a.r.b.Mu.Lock()
	a.r.f.on[a.id], a.r.f.booted[a.id] = true, a.r.f.now
	delete(a.r.f.pending, a.id)
	a.r.b.Mu.Unlock()
	a.r.boot[a.id] = a.r.f.now
	a.start("boot-" + a.r.f.now.Format(time.RFC3339Nano))
	a.reachable(true)
}

// reachable says whether the machine answers the panel's requests (off: the connection fails).
func (a *realAgent) reachable(up bool) {
	a.mu.Lock()
	was := a.up
	a.up = up
	a.mu.Unlock()
	if was == up {
		return
	}
	if up {
		a.r.gb.Fail(a.id, nil)
	} else {
		a.r.gb.Fail(a.id, devices.Errf("connection failed"))
	}
}

func (a *realAgent) do(method, path, token string, body any) (int, jsonx.M) {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			a.t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, a.srv.URL+path, rd)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	out := jsonx.M{}
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

// relay is every POST of the panel to this machine: sent to the real handler with the control token. What the
// agent accepted of an update or a power request is put on the test backend's list of requests, which is
// what the farm reads to let the update finish and the machine go off.
func (a *realAgent) relay(path string, body any) (int, jsonx.M) {
	a.sync()
	status, data := a.do("POST", path, contractControl, body)
	a.mu.Lock()
	a.posts = append(a.posts, fmt.Sprintf("%s %d", path, status))
	a.mu.Unlock()
	if status/100 == 2 && (path == "/update" || path == "/power") { // the real agent answers an update with 202
		a.r.b.Mu.Lock()
		a.r.b.Requests = append(a.r.b.Requests, fmt.Sprintf("%s POST %s %v", a.id, path, body))
		a.r.b.Mu.Unlock()
	}
	return status, data
}

// block is the approval block of the real agent's GET /metrics, now.
func (a *realAgent) block() jsonx.M {
	a.sync()
	status, data := a.do("GET", "/metrics", contractRead, nil)
	if status != 200 {
		a.t.Fatalf("GET /metrics of the real agent: %d %v", status, data)
	}
	block, _ := data["approval"].(map[string]any)
	return block
}

// sent counts the POSTs to path, and those of them the agent answered with status (0: any).
func (a *realAgent) sent(path string, status int) (n int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, p := range a.posts {
		if p == fmt.Sprintf("%s %d", path, status) || (status == 0 && strings.HasPrefix(p, path+" ")) {
			n++
		}
	}
	return n
}

func (a *realAgent) run() *api.RunStatus { a.sync(); return a.gate.StatusNow().Run }

// look is the run watch's look, as it looks every second on a real machine.
func (a *realAgent) look() {
	a.sync()
	if a.watch != nil {
		a.watch.Tick()
	}
}

func newContractRig(t *testing.T, runs bool) (*runRig, *realAgent) {
	t.Helper()
	r := newRunRigOpt(t, map[string]bool{"pc-a": runs}, "pc-a")
	return r, r.real["pc-a"]
}

// A whole run against the real handler: armed with the ticket, confirmed with the second passkey, the one
// update and the one shutdown let through with "run", a second of each refused.
func TestContractARunAgainstTheRealAgent(t *testing.T) {
	r, a := newContractRig(t, true)
	if h := wuHostRow(t, r.req("GET", "/api/admin/wake-update", nil), "pc-a"); h["choice"] != "wake" || h["checked"] != true {
		t.Fatalf("pc-a, with the real agent's block on record: %v", h)
	}
	_, res := r.approved(t, "POST", "/api/admin/wake-update", map[string]any{"hosts": []string{"pc-a"}})
	if res["ok"] != true {
		t.Fatal(res)
	}
	run, _ := r.s.WakeRuns.Snapshot()
	r.until(t, "pc-a armed", 300, func() bool { return r.host("pc-a").Arm == wakerun.ArmArmed })
	if n := a.sent(api.PathRunArm, 0); n != 1 || a.sent(api.PathRunArm, 200) != 1 {
		t.Fatalf("%d arm requests: %v", n, a.posts)
	}
	if st := a.run(); st == nil || st.ID != run.ID || st.State != api.RunArmed {
		t.Fatalf("the real agent's word: %+v", st)
	}
	r.until(t, "the run asking", 60, func() bool { x, _ := r.s.WakeRuns.Snapshot(); return x != nil && x.Approval == "needed" })
	if a.sent(api.PathApprovalChallenge, 0) != 0 {
		t.Fatal("a challenge was asked before the click")
	}
	asked, res := r.approved(t, "POST", "/api/admin/wake-update/"+run.ID+"/confirm", map[string]any{})
	if items := fmt.Sprint(asked.body["detail"].(map[string]any)["items"]); !strings.Contains(items, "PC-A: update to "+farmNew+", then shut down in 120 s") {
		t.Fatalf("the confirmation's dialog: %s", items)
	}
	if res["ok"] != true || fmt.Sprint(res["items"]) != "[PC-A: confirmed]" || a.sent(api.PathApprovalChallenge, 200) != 1 || a.sent(api.PathRunGo, 200) != 1 || a.sent(api.PathRunGo, 0) != 1 {
		t.Fatalf("the confirmation: %v %v", res, a.posts)
	}
	if st := a.run(); st == nil || st.State != api.RunConfirmed {
		t.Fatalf("the real agent's word after the confirmation: %+v", st)
	}
	r.until(t, "the update sent", 120, func() bool { return a.sent("/update", 0) > 0 })
	if a.sent("/update", 202) != 1 || a.sent("/update", 0) != 1 {
		t.Fatalf("the run's update was not let through: %v", a.posts)
	}
	for end := time.Now().Add(5 * time.Second); a.applied.Load() == 0 && time.Now().Before(end); { // the real updater downloads and verifies
		time.Sleep(10 * time.Millisecond)
	}
	if a.applied.Load() != 1 {
		t.Fatalf("the real updater started %d installers", a.applied.Load())
	}
	// a second update of the same run: the machine's own "no", and what the panel's client makes of it
	ag, _ := r.s.Poller.AgentByID("pc-a")
	_, err := powerctl.UpdateAgentRun(context.Background(), r.s.Poller, ag, "contract", run.ID)
	var ref *powerctl.AgentRefusal
	if !errors.As(err, &ref) || ref.Status != 403 || ref.Code != api.CodeApprovalRequired {
		t.Fatalf("a second update of the run: %v (%+v)", err, ref)
	}
	if oe, _ := opError(err).(*wakerun.OpError); oe == nil || oe.Code != wakerun.CodeApproval {
		t.Fatalf("what the run makes of it: %+v", opError(err))
	}
	r.until(t, "the shutdown sent", 400, func() bool { return a.sent("/power", 0) > 0 })
	pow := r.sent("pc-a", "/power")
	if a.sent("/power", 200) != 1 || len(pow) != 1 || !strings.Contains(pow[0], "run:"+run.ID) || !strings.Contains(pow[0], "delay_s:120") {
		t.Fatalf("the run's shutdown: %v %v", a.posts, pow)
	}
	// a second shutdown of the same run (the pending one was cancelled, say): refused by the machine
	x, _ := runOps{r.s}.system("pc-a")
	d := 120
	_, err = powerctl.Execute(context.Background(), r.s.Poller, x, powerctl.Request{Action: "shutdown", DelayS: &d, Confirm: &x.Name, OnlyVia: "agent", Run: run.ID}, "contract")
	ref = nil
	if !errors.As(err, &ref) || ref.Code != api.CodeApprovalRequired {
		t.Fatalf("a second shutdown of the run: %v (%+v)", err, ref)
	}
	r.until(t, "the run over", 600, func() bool { x, _ := r.s.WakeRuns.Snapshot(); return x == nil })
	if h := r.host("pc-a"); h.State != wakerun.StOff || h.Update != wakerun.UpdOK {
		t.Fatalf("pc-a: %s %s %q", h.State, h.Update, h.Text)
	}
	r.noTokenAnywhere(t)
}

// armDirect plays the panel's part of an arming without a run of the engine: a ticket for run is signed now
// (the issuer mints the token for the plan's nonce), kept, and handed over by the panel's own client code
// (runOps.Arm). For the refusals a run of the engine cannot be brought to.
func (r *runRig) armDirect(t *testing.T, run string) (time.Duration, *wakerun.OpError) {
	t.Helper()
	tm := r.s.runTimings()
	tk := approval.Ticket{Run: run, ValidS: tm.ValidS, WaitS: tm.WaitS, NoticeS: tm.NoticeS}
	b, err := approval.TicketBound(tk, "pc-a", "PC-A", r.s.Poller.BindingOf("pc-a").Record.MachineID, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := approval.NewPlan([]approval.Bound{b})
	if err != nil {
		t.Fatal(err)
	}
	if !r.s.tickets.put(run, r.iss.Mint(testApprovalClient, plan.Nonce()), plan, map[string]api.ApprovalRequest{"pc-a": tk.Request()}, nil, time.Duration(tm.ValidS)*time.Second) {
		t.Fatal("the ticket was not kept")
	}
	off, err := runOps{r.s}.Arm(context.Background(), "pc-a", run)
	if err == nil {
		return off, nil
	}
	oe, ok := err.(*wakerun.OpError)
	if !ok {
		t.Fatalf("Arm: %v", err)
	}
	return off, oe
}

// Every refusal of a ticket the real agent can be brought to, through the panel's client: the machine's word
// for the rule, and the panel's words for it. Nothing is matched on the machine's own text.
func TestContractArmRefusalsOfTheRealAgent(t *testing.T) {
	r, a := newContractRig(t, true)
	refused := func(what string, run, reason string) {
		t.Helper()
		_, oe := r.armDirect(t, run)
		if oe == nil || oe.Code != wakerun.CodeRunRefused || oe.Reason != reason || oe.Text != runReasonWords[reason] || oe.Text == "" {
			t.Fatalf("%s: want %s, got %+v (%v)", what, reason, oe, a.posts)
		}
		if st := a.run(); st != nil && st.ID == run && st.State == api.RunArmed {
			t.Fatalf("%s: refused, and armed all the same", what)
		}
	}
	const run1, run2, run3 = "00000000000000a1", "00000000000000a2", "00000000000000a3"
	// a machine that has just started takes the ticket
	a.reboot()
	if off, oe := r.armDirect(t, run1); oe != nil || off != 960*time.Second {
		t.Fatalf("a fresh machine: %v %+v", off, oe)
	}
	// another ticket while that run is armed (the real agent's word for it is "already armed": one ticket per
	// boot comes before "another run" in its rules); and, after a cancel, in the boot a ticket was taken in
	refused("a second run while one is armed", run2, api.RunReasonArmedThisBoot)
	if st := a.run(); st == nil || st.ID != run1 || st.State != api.RunArmed {
		t.Fatalf("the first run after a second ticket was refused: %+v", st)
	}
	if st, out := a.relay("/power", map[string]any{"action": "cancel", "requested_by": "contract"}); st != 200 {
		t.Fatalf("the cancel: %d %v", st, out)
	}
	if st := a.run(); st == nil || st.State != api.RunCancelled {
		t.Fatalf("after the cancel: %+v", st)
	}
	refused("a ticket in a boot one was taken in", run2, api.RunReasonArmedThisBoot)
	// after the next start: a ticket of a run that was accepted here before
	a.reboot()
	refused("a ticket of a run it has had", run1, api.RunReasonRunUsed)
	// a machine that was running when the ticket was signed; and one that has been up for long
	a.reboot()
	a.set(func() { a.uptime = func() time.Duration { return 5 * time.Minute } })
	refused("a machine that was up before the ticket", run2, api.RunReasonStartedBefore)
	a.set(func() { a.uptime = func() time.Duration { return 11 * time.Minute } })
	refused("a machine that has been up for 11 minutes", run2, api.RunReasonNotFresh)
	a.set(func() { a.uptime = nil })
	// the machine allows no countdown as short as the ticket's notice
	a.set(func() { a.minDelay = 300 })
	a.reboot()
	refused("a notice below the machine's shortest delay", run2, api.RunReasonLimits)
	a.set(func() { a.minDelay = 10 })
	// its clock is behind a time it showed before
	a.reboot()
	a.look() // it looks, and notes its clock
	a.set(func() { a.clock = -10 * time.Minute })
	refused("a clock that was set back", run2, api.RunReasonClockBack)
	a.set(func() { a.clock = 0 })
	// its run record is gone: made anew, it takes no ticket from before
	a.reboot()
	if err := os.Remove(filepath.Join(filepath.Dir(a.trust), agentapproval.RunFileName)); err != nil {
		t.Fatal(err)
	}
	a.start(a.boot) // the service starts again and finds no record
	// (the ticket is signed by armDirect after this; so the record is made first, by the watch's look, and the
	// boot is closed for tickets: "already armed". A ticket from before the record: "before record".)
	tm := r.s.runTimings()
	tk := approval.Ticket{Run: run3, ValidS: tm.ValidS, WaitS: tm.WaitS, NoticeS: tm.NoticeS}
	b, _ := approval.TicketBound(tk, "pc-a", "PC-A", r.s.Poller.BindingOf("pc-a").Record.MachineID, nil)
	plan, err := approval.NewPlan([]approval.Bound{b})
	if err != nil {
		t.Fatal(err)
	}
	token := r.iss.Mint(testApprovalClient, plan.Nonce())
	r.f.now = r.f.now.Add(200 * time.Second)
	a.look() // the record is made anew now, after the ticket was signed
	r.s.tickets.put(run3, token, plan, map[string]api.ApprovalRequest{"pc-a": tk.Request()}, nil, 400*time.Second)
	_, err = runOps{r.s}.Arm(context.Background(), "pc-a", run3)
	if oe, _ := err.(*wakerun.OpError); oe == nil || oe.Reason != api.RunReasonBeforeRecord || oe.Text != runReasonWords[api.RunReasonBeforeRecord] {
		t.Fatalf("a ticket from before the record was made anew: %+v", err)
	}
	if n := a.sent(api.PathRunArm, 200); n != 1 {
		t.Fatalf("%d tickets were accepted, one was to be", n)
	}
}

// A cancel at the machine while the run waits (somebody there kept it on): the click on Confirm is answered
// by the real agent's own refusal of the challenge, in the panel's words, and no passkey is asked for.
func TestContractAConfirmationTheRealAgentRefuses(t *testing.T) {
	r, a := newContractRig(t, true)
	_, res := r.approved(t, "POST", "/api/admin/wake-update", map[string]any{"hosts": []string{"pc-a"}})
	if res["ok"] != true {
		t.Fatal(res)
	}
	run, _ := r.s.WakeRuns.Snapshot()
	r.until(t, "the run asking", 300, func() bool { x, _ := r.s.WakeRuns.Snapshot(); return x != nil && x.Approval == "needed" })
	if st, out := a.relay("/power", map[string]any{"action": "cancel", "requested_by": "somebody at the machine"}); st != 200 {
		t.Fatalf("the cancel: %d %v", st, out)
	}
	tokens := len(r.iss.Tokens())
	no := r.req("POST", "/api/admin/wake-update/"+run.ID+"/confirm", map[string]any{})
	if no.code != 409 || !strings.Contains(string(no.raw), "PC-A does not take the run's confirmation: this run is over on it: it was cancelled or finished there, or the machine was started again") {
		t.Fatalf("the click after a cancel at the machine: %d %s", no.code, no.raw)
	}
	if len(r.iss.Tokens()) != tokens || a.sent(api.PathRunGo, 0) != 0 {
		t.Fatal("a passkey was asked for, or a confirmation sent")
	}
	r.until(t, "the run over", 300, func() bool { x, _ := r.s.WakeRuns.Snapshot(); return x == nil })
	if h := r.host("pc-a"); h.State != wakerun.StLeftOn || !strings.Contains(h.Text, "Its agent reports that its switch-off was called off") || !strings.HasSuffix(h.Text, "It is ON.") {
		t.Fatalf("pc-a: %s %q", h.State, h.Text)
	}
}

// Keep on against the real agent, and a cancel the machine cannot write down. While the record cannot be
// written the agent answers the cancel with 500 and still shows its run as armed: Keep on says "NOT
// cancelled" with the time, the strip keeps naming the machine, and a run that was cancelled "leave on"
// sends its cancel again and again. When the record can be written again, the next cancel is the one that
// counts, and only then does anything say "stays ON".
func TestContractACancelTheRealAgentCannotWriteDown(t *testing.T) {
	r, a := newContractRig(t, true)
	_, res := r.approved(t, "POST", "/api/admin/wake-update", map[string]any{"hosts": []string{"pc-a"}})
	if res["ok"] != true {
		t.Fatal(res)
	}
	run, _ := r.s.WakeRuns.Snapshot()
	r.until(t, "pc-a armed", 300, func() bool { return r.host("pc-a").Arm == wakerun.ArmArmed })
	at := r.host("pc-a").SelfOffAt.UTC().Format("15:04 UTC")
	r.admin = r.cookie(controller(), 0)
	r.csrf = []string{"Cookie", r.admin, "X-Requested-With", "fairy", "Content-Type", "application/json"}
	// the writers' lock of the run record cannot be taken: nothing of a run can be written down
	lock := filepath.Join(filepath.Dir(a.trust), agentapproval.RunLockName)
	_ = os.Remove(lock)
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	no := r.req("POST", "/api/systems/pc-a/wake-update/keep-on", map[string]any{})
	if no.code == 200 || !strings.Contains(string(no.raw), "NOT cancelled (") || !strings.Contains(string(no.raw), "PC-A may switch itself off at "+at+". Try again, or stop it at the machine.") {
		t.Fatalf("Keep on while the record cannot be written: %d %s", no.code, no.raw)
	}
	if a.sent("/power", 500) != 1 {
		t.Fatalf("the real agent's answer to that cancel: %v", a.posts)
	}
	if st := a.run(); st == nil || st.State != api.RunArmed {
		t.Fatalf("the real agent shows its run as %+v after a cancel it answered 500", st)
	}
	armed := func() int {
		list, _ := r.req("GET", "/api/admin/wake-update/waiting", nil).body["armed"].([]any)
		return len(list)
	}
	if armed() != 1 {
		t.Fatal("the strip no longer names the machine")
	}
	// the run is cancelled, "the woken machines stay on": its cancels get the same answer, again and again
	if c := r.req("DELETE", "/api/admin/wake-update/"+run.ID, map[string]any{"switch_off": false}); c.code != 200 {
		t.Fatalf("the run's cancel: %d %s", c.code, c.raw)
	}
	r.tick(45)
	if n := a.sent("/power", 500); n < 4 {
		t.Fatalf("the run sent %d cancels in 45 s: %v", n-1, a.posts)
	}
	if h := r.host("pc-a"); h.Final() || armed() != 1 || a.run().State != api.RunArmed {
		t.Fatalf("while no cancel got through: final %v (%q), armed in the strip %d, the agent %+v", h.Final(), h.Text, armed(), a.run())
	}
	if h := r.host("pc-a"); !strings.Contains(h.Text, "not cancelled") && !strings.Contains(h.Text, "NOT cancelled") && !strings.Contains(h.Text, "switch itself off") {
		t.Fatalf("the row while the cancels fail says nothing of it: %q", h.Text)
	}
	// the record can be written again
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	r.until(t, "the run over", 120, func() bool { x, _ := r.s.WakeRuns.Snapshot(); return x == nil })
	if st := a.run(); st == nil || st.State != api.RunCancelled || a.sent("/power", 200) < 1 {
		t.Fatalf("after the record could be written: %+v %v", st, a.posts)
	}
	if h := r.host("pc-a"); h.State != wakerun.StLeftOn || !strings.Contains(h.Text, "its switch-off was called off") || !strings.HasSuffix(h.Text, "It is ON.") || h.MayGoOff {
		t.Fatalf("pc-a: %s %q", h.State, h.Text)
	}
	if armed() != 0 {
		t.Fatal("the strip still names a machine that was kept on")
	}
	// and Keep on for a machine with no run: answered 200 by the real agent, "stays ON"
	yes := r.req("POST", "/api/systems/pc-a/wake-update/keep-on", map[string]any{})
	if yes.code != 200 || yes.body["stays_on"] != true || yes.body["message"] != "PC-A stays ON: its switch-off was called off (it shows its run as cancelled)." {
		t.Fatalf("Keep on afterwards: %d %s", yes.code, yes.raw)
	}
}

// An agent as v0.8.0 was: the check, and a checker that takes no runs. Its block has no "runs", it is in no
// run with the sentence that names v0.9.0, and what it answers to a ticket all the same is "not armed".
func TestContractAnAgentWithoutRuns(t *testing.T) {
	r, a := newContractRig(t, false)
	if block := a.block(); block["runs"] != nil || block["run"] != nil || block["state"] != "required" {
		t.Fatalf("the block of an agent without runs: %v", block)
	}
	h := wuHostRow(t, r.req("GET", "/api/admin/wake-update", nil), "pc-a")
	if h["no_run"] != true || h["checked"] != nil || !strings.HasPrefix(fmt.Sprint(h["why"]), "needs Fairy-agent v0.9.0 or newer to be in a run") {
		t.Fatalf("its row: %v", h)
	}
	if no := r.req("POST", "/api/admin/wake-update", map[string]any{"hosts": []string{"pc-a"}}); no.code != 400 || !strings.HasPrefix(fmt.Sprint(no.body["detail"]), "PC-A needs Fairy-agent v0.9.0 or newer to be in a run") {
		t.Fatalf("a start with it: %d %s", no.code, no.raw)
	}
	a.reboot()
	_, oe := r.armDirect(t, "00000000000000b1")
	// the real agent has no such route for a checker that keeps no runs: 404, which the panel reads as "not armed"
	if oe == nil || oe.Code != wakerun.CodeRunRefused || oe.Text != "its agent, or its running helper, does not take runs (or power control is off on it)" || a.sent(api.PathRunArm, 404) != 1 {
		t.Fatalf("a ticket for an agent without runs: %+v (%v)", oe, a.posts)
	}
}
