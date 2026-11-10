package agentcontract_test

// Pairing v2 end to end (M3): the panel's real Add host / Re-pair code (web API, enrollment manager, HTTP
// pairing client, managed store, poller) against the real fairy-agent handler (its /pair, its tokens, its
// /metrics), over HTTP on loopback. The only thing played by the test is the installer: it writes the agent's
// pairing state exactly as `fairy-agent join --panel 127.0.0.1 [--pair-v2]` does (cmd/fairy-agent/join.go).

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/agenttest"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/enroll"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/demo"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/enrollment"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/managed"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/web"
)

// machine is a computer the panel adds: what answers at its address can be replaced while the test runs
// (nothing, an old agent, an impostor, the real agent), like a machine before and after the install command.
type machine struct {
	t     *testing.T
	srv   *httptest.Server
	h     atomic.Value // *http.Handler
	state string       // the real agent's pairing.json
	real  http.Handler // the real agent's handler
}

// newMachine builds the real agent (power control on, so it has a control token to hand over) with its real
// pairing handler, the way pkg/agent/run.go wires it. Nothing answers at its address until use() is called.
func newMachine(t *testing.T) *machine {
	t.Helper()
	m := &machine{t: t, state: filepath.Join(t.TempDir(), "pairing.json")}
	cfg, err := agent.Parse([]byte("listen: 127.0.0.1:9101\npower_control: {enabled: true}\n"), filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	tokens := agent.Tokens{Read: readTok, Control: ctlTok}
	var a *agent.Agent
	d := agent.Deps{Collector: agenttest.NewCollector(), Version: "20-abcdef0", Power: agent.NewPower(&agenttest.PowerExec{}, cfg.PowerControl.MinDelayS)}
	d.Pairer = agent.NewPairer(m.state, func() (agent.Tokens, error) { return tokens, nil }, func() enroll.Response { return a.PairInfo() })
	a = agent.New(cfg, tokens, d)
	m.real = a.Handler()
	m.use(http.NotFoundHandler())
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { (*m.h.Load().(*http.Handler)).ServeHTTP(w, r) }))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *machine) use(h http.Handler) { m.h.Store(&h) }

// join is what the install command leaves behind: `fairy-agent join --panel 127.0.0.1 --code <code>`, with
// --pair-v2 when the panel's command carried it.
func (m *machine) join(code string, pairV2 bool) {
	m.t.Helper()
	now := time.Now()
	st := enroll.State{Version: 1, Panel: "127.0.0.1", CreatedAt: now.Unix(), ExpiresAt: now.Add(enroll.TTL).Unix()}
	if pairV2 {
		key, err := enroll.PairingKey(code)
		if err != nil {
			m.t.Fatal(err)
		}
		st.Key2 = hex.EncodeToString(key)
	} else {
		proof, err := enroll.Proof(code)
		if err != nil {
			m.t.Fatal(err)
		}
		st.Verifier = enroll.Verifier(proof)
	}
	if err := enroll.WriteState(m.state, st); err != nil {
		m.t.Fatal(err)
	}
}

func (m *machine) pairState() enroll.State {
	m.t.Helper()
	st, err := enroll.ReadState(m.state)
	if err != nil {
		m.t.Fatal(err)
	}
	return st
}

// panel is the real panel with a signed-in admin's browser. Its pairing requests are the real ones
// (enrollment.HTTPPairAny: loopback is not a Tailscale address); the test lets them through one at a time, so
// that the agent's budget of five pairing requests a minute is not spent while the test prepares the machine.
type panel struct {
	t      *testing.T
	ws     *web.Server
	p      *poller.Poller
	admin  browser
	permit chan struct{}
	to     atomic.Value // string: where the next pairing requests go ("" = the host's own URL)
	asked  atomic.Int32
}

func newPanel(t *testing.T) *panel {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.Parse([]byte(fmt.Sprintf("public_url: https://panel.example.com\nrequest_timeout: 2\ncontrol_rate_per_min: 60\ndata_dir: %q\naudit_log: %q\n"+
		"oidc: {issuer: \"https://id.example.com\", client_id: cid, allowed_groups: [homelab], control_groups: [admins]}\n"+
		"enroll: {panel_address: 127.0.0.1, allow_non_tailscale: true}\n", filepath.Join(dir, "data"), filepath.Join(dir, "audit.log"))))
	if err != nil {
		t.Fatal(err)
	}
	p, err := poller.New(cfg, config.Secrets{SessionSecret: strings.Repeat("s", 48)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	u := demo.User
	ws := web.New(cfg, config.Secrets{SessionSecret: strings.Repeat("s", 48), InsecureCookies: true}, p, &u, "test")
	if _, _, err := ws.Access.Seen(u.Sub, model.Deref(u.Email), model.Deref(u.Name), "admin", "contract test"); err != nil {
		t.Fatal(err)
	}
	if !ws.Enroll.AllowNonTailscale || ws.Enroll.PanelAddr != "127.0.0.1" {
		t.Fatal("the enroll: settings did not reach Add host")
	}
	// the checked release: its install scripts know pairing v2
	ws.Enroll.Scripts = enrollment.Fixed{Repo: "n3ptune-cpu/Fairy-agent", Tag: "build-20", Version: "20-abcdef0",
		Base: "https://github.com/n3ptune-cpu/Fairy-agent/releases/download/build-20", SH: strings.Repeat("a1", 32), PS1: strings.Repeat("b2", 32),
		Sums: strings.Repeat("c3", 32), PinSH: true, PinPS1: true, PairV2SH: true, PairV2PS1: true, StdinSH: true, EnvPS1: true, MemoryPS1: true}
	ws.Enroll.Interval, ws.Enroll.OnlineWait = 20*time.Millisecond, 3*time.Second
	pn := &panel{t: t, ws: ws, p: p, permit: make(chan struct{}, 64)}
	pn.to.Store("")
	ws.Enroll.Pair = func(ctx context.Context, url string, key []byte) (int, enroll.Response, string, error) {
		select {
		case <-pn.permit:
		case <-ctx.Done():
			return 0, enroll.Response{}, "", ctx.Err()
		}
		if to := pn.to.Load().(string); to != "" {
			url = to
		}
		pn.asked.Add(1)
		return enrollment.HTTPPairAny(ctx, url, key)
	}
	srv := httptest.NewServer(ws.Handler())
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	pn.admin = browser{t, srv, &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	return pn
}

// The Linux line: the code goes to the installer's standard input, never onto a command line.
var joinRe = regexp.MustCompile(`&& printf '%s' '([A-Z2-7-]{39})' \| (sudo )?env FAIRY_RELEASE_URL=\S+ sh "\$f" --sums-sha256 [0-9a-f]{64} --join-stdin( --pair-v2)? --panel 127\.0\.0\.1`)

// start makes an Add host (id "") or a Re-pair as the signed-in admin, confirms "it's you", and fetches the
// code and command the way the page does. It returns the enrollment id, the code and whether the command
// asks for pairing v2.
func (pn *panel) start(path string, body map[string]any) (id, code string, pairV2 bool) {
	pn.t.Helper()
	st, res := pn.admin.do("POST", path, body)
	if st != 201 {
		pn.t.Fatal(path, st, res)
	}
	id, _ = res["ref"].(string)
	if id == "" {
		id, _ = res["id"].(string)
	}
	cr := res
	if _, has := res["command"]; !has { // made after the step-up: the code is fetched once
		if st, cr = pn.admin.call("GET", "/api/admin/enrollments/"+id+"?reveal=1", nil); st != 200 {
			pn.t.Fatal("reveal", st, cr)
		}
	}
	cmd, _ := cr["command"].(string)
	m := joinRe.FindStringSubmatch(cmd)
	if m == nil || m[1] != cr["code"] || strings.Count(cmd, m[1]) != 1 {
		pn.t.Fatal("the command does not hand the code to the installer's standard input, with the panel's address:", cmd)
	}
	return id, m[1], m[3] == " "+enrollment.FlagPairV2SH
}

// ask lets n pairing requests through and returns the enrollment afterwards.
func (pn *panel) ask(id string, n int) map[string]any {
	pn.t.Helper()
	want := pn.asked.Load() + int32(n)
	for i := 0; i < n; i++ {
		pn.permit <- struct{}{}
	}
	for i := 0; i < 400 && pn.asked.Load() < want; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if pn.asked.Load() < want {
		pn.t.Fatal("the panel did not ask")
	}
	time.Sleep(60 * time.Millisecond) // the answer is taken in
	_, e := pn.admin.call("GET", "/api/admin/enrollments/"+id, nil)
	return e
}

// done waits for the enrollment to finish, polling like the running panel does.
func (pn *panel) done(id string) map[string]any {
	pn.t.Helper()
	var e map[string]any
	for i := 0; i < 300; i++ {
		pn.p.PollOnce(context.Background())
		if _, e = pn.admin.call("GET", "/api/admin/enrollments/"+id, nil); e["done"] == true {
			return e
		}
		time.Sleep(20 * time.Millisecond)
	}
	pn.t.Fatalf("the enrollment did not finish: %v", e)
	return nil
}

func (pn *panel) secret(id string) managed.Secret {
	pn.t.Helper()
	sec, err := pn.ws.Managed.Secret(id)
	if err != nil {
		pn.t.Fatal(err)
	}
	return sec
}

func (pn *panel) audit() string {
	b, _ := os.ReadFile(pn.ws.Cfg.AuditLog)
	return string(b)
}

// recorder passes everything on to the machine and keeps what crossed: someone listening on the path.
type recorder struct {
	mu       sync.Mutex
	next     http.Handler
	requests [][]byte
	answers  [][]byte
}

func (rc *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(b))
	rr := httptest.NewRecorder()
	rc.next.ServeHTTP(rr, r)
	if r.URL.Path == enroll.Path {
		rc.mu.Lock()
		rc.requests, rc.answers = append(rc.requests, b), append(rc.answers, bytes.Clone(rr.Body.Bytes()))
		rc.mu.Unlock()
	}
	for k, v := range rr.Header() {
		w.Header()[k] = v
	}
	w.WriteHeader(rr.Code)
	_, _ = w.Write(rr.Body.Bytes())
}

// crossed is a copy of what the listener has recorded so far.
func (rc *recorder) crossed() (requests, answers [][]byte) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return append([][]byte{}, rc.requests...), append([][]byte{}, rc.answers...)
}

// Re-pair of a host at the real agent's address, start to finish: the command asks for pairing v2, the agent
// stores the code's key, the panel's request is answered sealed, the panel stores the agent's real tokens and
// polls the agent with them until it is online. A listener on the path sees nothing it can use.
func TestPairingV2WithTheRealAgent(t *testing.T) {
	m := newMachine(t)
	rec := &recorder{next: m.real}
	m.use(rec)
	pn := newPanel(t)
	// the host as the panel has it: its tokens are lost (the reason to Re-pair)
	h := managed.Host{ID: "pairbox", Name: "pairbox", URL: m.srv.URL, OS: "linux", Options: managed.Options{PowerControl: true}}
	if err := pn.ws.Managed.Put(h, managed.Secret{Token: "lost-read-token-xxxxxxxxxxxxxxxxxxxxx"}); err != nil {
		t.Fatal(err)
	}
	id, code, pairV2 := pn.start("/api/admin/hosts/pairbox/repair", map[string]any{"confirm": "pairbox"})
	if !pairV2 {
		t.Fatal("the command does not ask for pairing v2")
	}
	// before the command ran: the agent has no code
	if e := pn.ask(id, 1); e["state"] != "waiting" || !strings.Contains(e["message"].(string), "no join code yet") {
		t.Fatal(e)
	}
	m.join(code, pairV2)
	if st := m.pairState(); st.PairVersion() != enroll.V2 || st.Verifier != "" {
		t.Fatalf("%+v", st)
	}
	pn.ask(id, 1)
	e := pn.done(id)
	if e["state"] != "online" || e["pairing"] != "v2" {
		t.Fatal(e)
	}
	// the panel holds the agent's real tokens, and uses them: /metrics and a control request work
	if sec := pn.secret("pairbox"); sec.Token != readTok || sec.ControlToken != ctlTok {
		t.Fatalf("%+v", sec)
	}
	var sys model.System
	for _, x := range pn.p.Snapshot() {
		if x.ID == "pairbox" {
			sys = x
		}
	}
	if sys.Status != "online" || sys.Kind != "agent" || sys.CPUPercent == nil {
		t.Fatalf("%+v", sys)
	}
	ag, ok := pn.p.AgentByID("pairbox")
	if !ok || !ag.Managed || !ag.PowerControl || ag.ControlToken != ctlTok {
		t.Fatalf("%+v", ag)
	}
	if st, body, err := pn.p.AgentRequest(context.Background(), ag, "POST", "/power", map[string]any{"action": "shutdown", "delay_s": 60}); err != nil || st != 200 {
		t.Fatal("a control request with the paired control token:", st, body, err)
	}
	if hosts, _ := pn.ws.Managed.Load(); len(hosts) != 1 || hosts[0].Pairing != managed.PairingV2 || hosts[0].AgentVersion != "20-abcdef0" || !hosts[0].ControlPaired {
		t.Fatalf("%+v", hosts)
	}
	// the agent closed pairing: the code is used, its key is gone from the state file
	if st := m.pairState(); st.Closed != "paired" || st.Key2 != "" || st.PairedBy != "127.0.0.1" {
		t.Fatalf("%+v", st)
	}
	if !strings.Contains(pn.audit(), `"pairing":"v2"`) || !strings.Contains(pn.audit(), "paired: 20-abcdef0") {
		t.Fatal(pn.audit())
	}

	// what crossed the wire: two requests and two answers, with nothing in them that helps anyone else
	requests, answers := rec.crossed()
	if len(requests) != 2 || len(answers) != 2 {
		t.Fatal(len(requests), len(answers))
	}
	key, _ := enroll.PairingKey(code)
	norm, _ := enroll.Normalize(code)
	proof, _ := enroll.Proof(code)
	crossed := bytes.Join(append(append([][]byte{}, requests...), answers...), []byte("\n"))
	for name, secret := range map[string]string{"read token": readTok, "control token": ctlTok, "code": code, "code without dashes": norm,
		"old proof": proof, "old verifier": enroll.Verifier(proof), "key": string(key), "key in hex": hex.EncodeToString(key), "version": "20-abcdef0",
		"a field name of the answer": `"token"`, "another field name": "control_token"} {
		if bytes.Contains(crossed, []byte(secret)) {
			t.Errorf("the %s crossed the wire in clear", name)
		}
	}
	var q1, q2 enroll.RequestV2
	if json.Unmarshal(requests[0], &q1) != nil || json.Unmarshal(requests[1], &q2) != nil || bytes.Equal(q1.Nonce, q2.Nonce) || bytes.Equal(q1.MAC, q2.MAC) {
		t.Fatalf("the two requests are not two fresh v2 requests: %s %s", requests[0], requests[1])
	}
	// the recorded answer is bound to its request: it opens for that one and for no other, and not without the code
	if resp, err := enroll.OpenAnswer(key, q2, answers[1]); err != nil || resp.Token != readTok {
		t.Fatal("the recorded answer should open for its own request (test sanity):", err)
	}
	fresh, _ := enroll.NewRequestV2(key)
	if _, err := enroll.OpenAnswer(key, fresh, answers[1]); err == nil {
		t.Fatal("a recorded answer opened for another request")
	}
	guess, _ := enroll.NewCode()
	gk, _ := enroll.PairingKey(guess)
	if _, err := enroll.OpenAnswer(gk, q2, answers[1]); err == nil {
		t.Fatal("a recorded answer opened without the code")
	}
	// sending the recorded request again gets nothing: pairing is closed
	resp, err := http.Post(m.srv.URL+enroll.Path, "application/json", bytes.NewReader(requests[1]))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 404 || bytes.Contains(b, []byte(readTok)) {
		t.Fatal("a replayed request after pairing:", resp.StatusCode, string(b))
	}
}

// oldAgentPair answers POST /pair the way Fairy-agent 0.5.x does (pkg/agent/pairing.go of v0.5.2: the body
// must be {"proof": "..."}; five requests a minute). The real 0.5.2 handler cannot be linked next to the new
// module; reports/P3.md has the run of this case against it.
type oldAgentPair struct {
	mu     sync.Mutex
	hits   int
	bodies [][]byte
}

func (o *oldAgentPair) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	defer o.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path != enroll.Path {
		w.WriteHeader(401)
		return
	}
	if o.hits++; o.hits > enroll.RatePerMin {
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":"too many pairing requests; try again in a minute","code":"rate_limited"}`))
		return
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, 1024))
	o.bodies = append(o.bodies, b)
	var req enroll.Request
	if err != nil || json.Unmarshal(b, &req) != nil || req.Proof == "" {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":"expected {\"proof\": \"...\"}","code":"bad_request"}`))
		return
	}
	w.WriteHeader(403)
	_, _ = w.Write([]byte(`{"error":"wrong join code","code":"wrong_code"}`))
}

// Add host for a machine that still runs an agent from before pairing v2. The panel says the agent is too old
// and must be updated first, never sends the old proof, and does not end the enrollment: when the command has
// installed the new agent (here: the real one takes the old one's place and joins with the same code), the
// panel pairs with the code it made at the start.
func TestAddHostWithAnOldAgentThenTheRealOne(t *testing.T) {
	m := newMachine(t)
	old := &oldAgentPair{}
	m.use(old)
	pn := newPanel(t)
	pn.ws.Enroll.OnlineWait = 300 * time.Millisecond // "oldbox:9101" is not a real address: the host cannot come online here
	pn.to.Store(m.srv.URL)
	id, code, pairV2 := pn.start("/api/admin/hosts", map[string]any{"name": "oldbox", "os": "linux", "options": map[string]any{"power_control": true}})
	if !pairV2 {
		t.Fatal("the command does not ask for pairing v2")
	}
	e := pn.ask(id, 8) // past the old agent's five a minute: 400 five times, then 429
	if e["state"] != "waiting" || e["done"] != false || e["hint"] != "agent_too_old" || e["pairing"] != nil {
		t.Fatal(e)
	}
	for _, want := range []string{"too old for this panel", "Update it first", "this same code"} {
		if !strings.Contains(e["message"].(string), want) {
			t.Errorf("the message lacks %q: %s", want, e["message"])
		}
	}
	old.mu.Lock()
	if old.hits != 8 || len(old.bodies) != enroll.RatePerMin {
		t.Fatal(old.hits, len(old.bodies))
	}
	for _, b := range old.bodies {
		var q enroll.RequestV2
		if bytes.Contains(b, []byte("proof")) || json.Unmarshal(b, &q) != nil || q.V != enroll.V2 {
			t.Fatalf("the panel fell back to the old exchange: %s", b)
		}
	}
	old.mu.Unlock()
	if hosts, _ := pn.ws.Managed.Load(); len(hosts) != 0 {
		t.Fatal("an old agent was added:", hosts)
	}
	if n := strings.Count(pn.audit(), "agent_too_old: "); n != 1 {
		t.Fatal("audit entries for the old agent:", n)
	}

	// the command runs: the new agent replaces the old one and stores THE SAME code
	m.join(code, pairV2)
	m.use(m.real)
	pn.ask(id, 1)
	e = pn.done(id)
	if e["pairing"] != "v2" || e["state"] != "paired" || e["id"] != id { // paired and stored; "online" needs a real address
		t.Fatal(e)
	}
	if sec := pn.secret("oldbox"); sec.Token != readTok || sec.ControlToken != ctlTok {
		t.Fatalf("%+v", sec)
	}
	if st := m.pairState(); st.Closed != "paired" || st.Failures != 0 {
		t.Fatalf("%+v", st)
	}
}

// The machine holds a join code that was stored for the old exchange (the command was run without --pair-v2,
// e.g. by an older install script). The real agent refuses the panel's v2 request without counting it; the
// panel says what is wrong, keeps waiting and never asks the old way, so the agent's tokens never leave in
// clear. Running the panel's command (same code, with --pair-v2) pairs.
func TestJoinCodeStoredForTheOldPairing(t *testing.T) {
	m := newMachine(t)
	rec := &recorder{next: m.real}
	m.use(rec)
	pn := newPanel(t)
	pn.ws.Enroll.OnlineWait = 300 * time.Millisecond
	pn.to.Store(m.srv.URL)
	id, code, _ := pn.start("/api/admin/hosts", map[string]any{"name": "v1box", "os": "linux"})
	m.join(code, false)
	e := pn.ask(id, 3)
	if e["state"] != "waiting" || e["done"] != false || e["hint"] != "legacy_join_code" || !strings.Contains(e["message"].(string), "join code for the old pairing") {
		t.Fatal(e)
	}
	if st := m.pairState(); st.Failures != 0 || st.Closed != "" || st.PairVersion() != 1 {
		t.Fatalf("the panel's requests cost the agent's code: %+v", st)
	}
	requests, answers := rec.crossed()
	for i, b := range requests {
		if bytes.Contains(b, []byte("proof")) || bytes.Contains(answers[i], []byte(readTok)) {
			t.Fatalf("the old exchange happened: %s -> %s", b, answers[i])
		}
	}
	m.join(code, true)
	pn.ask(id, 1)
	if e = pn.done(id); e["pairing"] != "v2" || pn.secret("v1box").Token != readTok {
		t.Fatal(e)
	}
}

// Something else answers at the machine's address and says "paired" with made-up tokens (it never had the
// code). It is not added, however often it answers; the audit log has one entry. When the real agent answers,
// the same code pairs.
func TestImpostorAtTheMachinesAddress(t *testing.T) {
	m := newMachine(t)
	m.use(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(enroll.Response{AgentVersion: "99-evil", Platform: "linux", Token: "impostor-read-token-xxxxxxxxxxxxxxxx",
			ControlToken: "impostor-control-token-yyyyyyyyyyyy"})
	}))
	pn := newPanel(t)
	pn.ws.Enroll.OnlineWait = 300 * time.Millisecond
	pn.to.Store(m.srv.URL)
	id, code, pairV2 := pn.start("/api/admin/hosts", map[string]any{"name": "target", "os": "linux", "options": map[string]any{"power_control": true}})
	e := pn.ask(id, 10)
	if e["state"] != "waiting" || e["done"] != false || e["hint"] != "unverified_answer" || !strings.Contains(e["message"].(string), "did not prove it has the join code") {
		t.Fatal(e)
	}
	if hosts, _ := pn.ws.Managed.Load(); len(hosts) != 0 {
		t.Fatal("the impostor was adopted:", hosts)
	}
	if _, ok := pn.p.AgentByID("target"); ok {
		t.Fatal("the impostor is polled")
	}
	if a := pn.audit(); strings.Count(a, "unverified: ") != 1 || strings.Contains(a, "impostor-read-token") || strings.Contains(a, `"pairing":"v2"`) {
		t.Fatal(a)
	}
	m.join(code, pairV2)
	m.use(m.real)
	pn.ask(id, 1)
	if e = pn.done(id); e["pairing"] != "v2" || pn.secret("target").Token != readTok {
		t.Fatal(e)
	}
}

// The limit that stays (FIX-PLAN, M3): someone who can send from the panel's address and repeats a request
// the panel sent earlier gets the agent's one answer, which they cannot read, and uses the code up. The panel
// then never pairs with that code (the admin makes a new one), and it adopts nothing.
func TestReplayedRequestGetsNothingReadable(t *testing.T) {
	m := newMachine(t)
	rec := &recorder{next: m.real}
	m.use(rec)
	pn := newPanel(t)
	pn.to.Store(m.srv.URL)
	id, code, pairV2 := pn.start("/api/admin/hosts", map[string]any{"name": "replay", "os": "linux"})
	pn.ask(id, 1) // sent before the command ran: answered 404, and recorded by the listener
	m.join(code, pairV2)
	requests, _ := rec.crossed()
	captured := requests[0]
	resp, err := http.Post(m.srv.URL+enroll.Path, "application/json", bytes.NewReader(captured))
	if err != nil {
		t.Fatal(err)
	}
	stolen, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal("the replay was not answered (the agent changed: adapt this test):", resp.StatusCode, string(stolen))
	}
	for _, secret := range []string{readTok, ctlTok, `"token"`, "20-abcdef0"} {
		if bytes.Contains(stolen, []byte(secret)) {
			t.Fatalf("%q is readable in the answer to a replayed request", secret)
		}
	}
	var a enroll.AnswerV2
	if json.Unmarshal(stolen, &a) != nil || a.V != enroll.V2 || len(a.Sealed) == 0 {
		t.Fatalf("%s", stolen)
	}
	e := pn.ask(id, 2)
	if e["state"] != "waiting" || e["pairing"] != nil || !strings.Contains(e["message"].(string), "no join code yet") {
		t.Fatal(e)
	}
	if hosts, _ := pn.ws.Managed.Load(); len(hosts) != 0 {
		t.Fatal(hosts)
	}
}
