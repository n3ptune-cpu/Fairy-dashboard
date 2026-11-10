package enrollment

import (
	"bytes"
	"context"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/enroll"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/managed"
)

// M3: the panel's half of pairing v2. The handlers here play the machine with the shared code of the agent
// (enroll.CheckRequestV2 / SealAnswer); the real agent handler is in internal/agentcontract.

var goodAnswer = enroll.Response{AgentVersion: "20-abcdef0", Hostname: "box", Platform: "linux", Features: map[string]bool{"power_control": true},
	Token: "v2-read-token-0123456789abcdef", ControlToken: "v2-control-token-0123456789abcdef"}

func keyOf(t *testing.T, code string) []byte {
	t.Helper()
	k, err := enroll.PairingKey(code)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// readV2 reads a pairing request the way the new agent does.
func readV2(r *http.Request) (enroll.RequestV2, []byte, error) {
	var q enroll.RequestV2
	b, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil {
		return q, b, err
	}
	return q, b, json.Unmarshal(b, &q)
}

// v2Agent answers POST /pair like an agent that holds key: a correct request gets the sealed answer.
func v2Agent(key []byte, resp enroll.Response) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q, _, err := readV2(r)
		if err != nil || enroll.CheckRequestV2(key, q) != nil {
			w.WriteHeader(403)
			_, _ = w.Write([]byte(`{"error":"wrong join code","code":"wrong_code"}`))
			return
		}
		a, err := enroll.SealAnswer(key, q, resp)
		if err != nil {
			w.WriteHeader(500)
			return
		}
		_ = json.NewEncoder(w).Encode(a)
	}
}

// oldAgent answers POST /pair like Fairy-agent 0.5.x (pkg/agent/pairing.go of v0.5.2): five requests a minute
// in all, then a body that must be {"proof": "..."}. It records what it was sent.
type oldAgent struct {
	mu     sync.Mutex
	hits   int
	bodies [][]byte
	proofs int // requests that carried a proof (a panel that fell back to the old exchange)
}

func (o *oldAgent) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	defer o.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path != enroll.Path || r.Method != http.MethodPost {
		w.WriteHeader(404)
		return
	}
	o.hits++
	if o.hits > enroll.RatePerMin {
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
	o.proofs++
	w.WriteHeader(403)
	_, _ = w.Write([]byte(`{"error":"wrong join code","code":"wrong_code"}`))
}

// swap is a server whose handler can be replaced while it runs (the machine before and after the command).
type swap struct {
	h   atomic.Value
	srv *httptest.Server
}

func newSwap(t *testing.T, h http.Handler) *swap {
	t.Helper()
	s := &swap{}
	s.h.Store(&h)
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { (*s.h.Load().(*http.Handler)).ServeHTTP(w, r) }))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *swap) set(h http.Handler) { s.h.Store(&h) }

// rig is a manager whose pairing requests go, with the real HTTP code, to a test server on loopback.
type rig struct {
	m      *Manager
	store  *managed.Store
	mu     sync.Mutex
	events []string // kind: detail
	adds   atomic.Int32
}

func newRig(t *testing.T, url string) *rig {
	t.Helper()
	r := &rig{store: managed.NewStore(t.TempDir())}
	r.m = New(r.store, Hooks{
		Config: func() *config.Panel { return testConfig(t) },
		Remove: func(string) {},
		Add:    func(managed.Entry) error { r.adds.Add(1); return nil },
		Online: func(string) (bool, string) { return true, "" },
		Event: func(_ Enrollment, kind, detail string) {
			r.mu.Lock()
			r.events = append(r.events, kind+": "+detail)
			r.mu.Unlock()
		},
	}, "100.88.109.47", Fixed(testInstallers))
	r.m.Interval, r.m.OnlineWait = 10*time.Millisecond, 2*time.Second
	r.m.Pair = func(ctx context.Context, _ string, key []byte) (int, enroll.Response, string, error) {
		return HTTPPairAny(ctx, url, key) // the host's own address is a name:9101 that does not exist here
	}
	return r
}

func (r *rig) count(kind string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.events {
		if strings.HasPrefix(e, kind+":") {
			n++
		}
	}
	return n
}

func (r *rig) add(t *testing.T, name string) Created {
	t.Helper()
	cr, err := r.m.Create(context.Background(), Request{Name: name, OS: "linux", Options: managed.Options{PowerControl: true}}, nil, "neptune",
		func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	return cr
}

// wait polls the enrollment until ok says so (or fails the test).
func (r *rig) wait(t *testing.T, id string, what string, ok func(Enrollment) bool) Enrollment {
	t.Helper()
	var e Enrollment
	for i := 0; i < 600; i++ {
		if e, _ = r.m.Get(id); ok(e) {
			return e
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s: never reached; last: %+v", what, e)
	return e
}

// The request the panel sends is a pairing v2 request and nothing else: a new nonce every time, a MAC under
// the code's key, never the old proof.
func TestPairRequestIsV2WithAFreshNonce(t *testing.T) {
	key := keyOf(t, "MFRG-GZDF-MZTW-Q2LK-NNWG-23TP-OBYX-E43U")
	var mu sync.Mutex
	var bodies [][]byte
	var ctype string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies, ctype = append(bodies, b), r.Header.Get("Content-Type")
		mu.Unlock()
		if r.URL.Path != "/pair" || r.Method != "POST" {
			t.Error(r.Method, r.URL.Path)
		}
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":"x","code":"pairing_inactive"}`))
	}))
	defer srv.Close()
	for i := 0; i < 5; i++ {
		if st, resp, code, err := HTTPPairAny(context.Background(), srv.URL+"/", key); st != 404 || code != "pairing_inactive" || err != nil || resp.Token != "" {
			t.Fatal(st, code, err)
		}
	}
	if ctype != "application/json" {
		t.Fatal("the agent refuses anything but JSON:", ctype)
	}
	seen := map[string]bool{}
	for _, b := range bodies {
		var m map[string]json.RawMessage
		var q enroll.RequestV2
		if json.Unmarshal(b, &m) != nil || json.Unmarshal(b, &q) != nil || len(m) != 3 || m["v"] == nil || m["nonce"] == nil || m["mac"] == nil {
			t.Fatalf("request shape: %s", b)
		}
		if err := enroll.CheckRequestV2(key, q); err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte("proof")) || seen[string(q.Nonce)] {
			t.Fatalf("an old proof or a repeated nonce: %s", b)
		}
		seen[string(q.Nonce)] = true
	}
	if len(seen) != 5 {
		t.Fatal(len(seen))
	}
	if _, _, _, err := HTTPPairAny(context.Background(), srv.URL, []byte("short")); !errors.Is(err, enroll.ErrBadKey) {
		t.Fatal("a request was made without a pairing key:", err)
	}
}

// Only an answer that opens under the join code's key for the request just sent is taken. Everything else that
// says "200" is refused with an empty answer: the old answer with tokens in clear, an answer made without the
// code, for another request, replayed, altered, echoed.
func TestOnlyAVerifiedAnswerIsAccepted(t *testing.T) {
	key := keyOf(t, "MFRG-GZDF-MZTW-Q2LK-NNWG-23TP-OBYX-E43U")
	other := keyOf(t, "ABCD-EFGH-IJKL-MNOP-QRST-UVWX-YZ23-4567")

	good := httptest.NewServer(v2Agent(key, goodAnswer))
	defer good.Close()
	st, resp, code, err := HTTPPairAny(context.Background(), good.URL, key)
	if st != 200 || err != nil || code != "" || resp.Token != goodAnswer.Token || resp.ControlToken != goodAnswer.ControlToken || resp.AgentVersion != "20-abcdef0" {
		t.Fatal(st, code, err, resp)
	}

	// an earlier, genuine exchange to replay
	var recorded []byte
	rec := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rr := httptest.NewRecorder()
		v2Agent(key, goodAnswer)(rr, r)
		recorded = rr.Body.Bytes()
		_, _ = w.Write(recorded)
	}))
	if st, _, _, err := HTTPPairAny(context.Background(), rec.URL, key); st != 200 || err != nil {
		t.Fatal(st, err)
	}
	rec.Close()

	clear, _ := json.Marshal(goodAnswer) // what an agent before v2 answers
	impostors := map[string]http.HandlerFunc{
		"the old answer, tokens in clear": func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(clear) },
		"sealed for this very request, with another join code": func(w http.ResponseWriter, r *http.Request) {
			q, _, _ := readV2(r)
			a, err := enroll.SealAnswer(other, requestUnder(other, q.Nonce), goodAnswer)
			if err != nil {
				t.Error(err)
			}
			_ = json.NewEncoder(w).Encode(a)
		},
		"the right code, but an answer to another request": func(w http.ResponseWriter, r *http.Request) {
			mine, _ := enroll.NewRequestV2(key)
			a, _ := enroll.SealAnswer(key, mine, goodAnswer)
			_ = json.NewEncoder(w).Encode(a)
		},
		"a recorded answer, replayed": func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(recorded) },
		"one bit of the sealed part changed": func(w http.ResponseWriter, r *http.Request) {
			q, _, _ := readV2(r)
			a, _ := enroll.SealAnswer(key, q, goodAnswer)
			a.Sealed[len(a.Sealed)/2] ^= 1
			_ = json.NewEncoder(w).Encode(a)
		},
		"the agent's nonce changed": func(w http.ResponseWriter, r *http.Request) {
			q, _, _ := readV2(r)
			a, _ := enroll.SealAnswer(key, q, goodAnswer)
			a.Nonce[0] ^= 1
			_ = json.NewEncoder(w).Encode(a)
		},
		"the request, sent back": func(w http.ResponseWriter, r *http.Request) { _, b, _ := readV2(r); _, _ = w.Write(b) },
		"v2 shape with the tokens next to it": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"v":2,"nonce":"AAAAAAAAAAAAAAAAAAAAAA==","sealed":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","token":"clear-read-token-0123456789abcdef","control_token":"clear-control-token-0123456789"}`))
		},
		"an empty object": func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{}`)) },
		"null":            func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`null`)) },
		"nothing":         func(w http.ResponseWriter, r *http.Request) {},
		"not JSON":        func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`<html>paired</html>`)) },
	}
	for name, h := range impostors {
		srv := httptest.NewServer(h)
		st, resp, code, err := HTTPPairAny(context.Background(), srv.URL, key)
		srv.Close()
		var u *Unverified
		if st != 200 || !errors.As(err, &u) || code != "" {
			t.Errorf("%s: %d %q %v", name, st, code, err)
		}
		if resp.Token != "" || resp.ControlToken != "" || resp.AgentVersion != "" || resp.Proxmox != nil || resp.Features != nil {
			t.Errorf("%s: something of the answer was kept: %+v", name, resp)
		}
		if u != nil && (strings.Contains(u.Why, "token") || strings.Contains(u.Error(), goodAnswer.Token)) {
			t.Errorf("%s: the reason quotes the answer: %s", name, u.Why)
		}
	}
}

// requestUnder is the request a holder of key would have sent with this nonce (the key schedule of
// Fairy-agent pkg/enroll/v2.go, written out): what an impostor with a guessed code can build from the panel's
// real nonce, to seal its answer for exactly that nonce.
func requestUnder(key, nonce []byte) enroll.RequestV2 {
	kreq, err := hkdf.Key(sha256.New, key, nil, "fairy-enroll-v2 request", enroll.KeySize)
	if err != nil {
		panic(err)
	}
	m := hmac.New(sha256.New, kreq)
	m.Write(nonce)
	return enroll.RequestV2{V: enroll.V2, Nonce: nonce, MAC: m.Sum(nil)}
}

// The largest answer an agent sends is 96 KiB (enroll.MaxAnswerBody): the old 64 KiB read limit would cut a
// valid answer of a Proxmox node with a large CA bundle. Exactly 96 KiB is accepted, one byte more is not.
func TestPairAnswerSizeLimit(t *testing.T) {
	key := keyOf(t, "MFRG-GZDF-MZTW-Q2LK-NNWG-23TP-OBYX-E43U")
	if enroll.MaxAnswerBody != 96<<10 {
		t.Fatal("the agent's limit changed:", enroll.MaxAnswerBody)
	}
	// a genuine large answer: the sealed part alone is over 64 KiB once it is base64
	big := goodAnswer
	big.Proxmox = &enroll.ProxmoxGrant{TokenID: "fairy@pve!panel-box", TokenSecret: "0b7c4e2a-1111-2222-3333-444455556666", Node: "box",
		CAPEM: strings.Repeat("A", 62<<10)}
	var size atomic.Int64
	pad := atomic.Int64{} // the body is padded with spaces to this many bytes (0: as it is)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q, _, _ := readV2(r)
		a, err := enroll.SealAnswer(key, q, big)
		if err != nil {
			t.Error(err)
			return
		}
		b, _ := json.Marshal(a)
		if n := pad.Load(); n > 0 {
			if int64(len(b)) > n {
				t.Error("the answer is already larger than", n)
			}
			b = append(b, bytes.Repeat([]byte(" "), int(n)-len(b))...)
		}
		size.Store(int64(len(b)))
		_, _ = w.Write(b)
	}))
	defer srv.Close()

	st, resp, _, err := HTTPPairAny(context.Background(), srv.URL, key)
	if st != 200 || err != nil || resp.Proxmox == nil || len(resp.Proxmox.CAPEM) != 62<<10 || resp.Token != goodAnswer.Token {
		t.Fatal("a genuine large answer was refused:", st, err)
	}
	if size.Load() <= 64<<10 {
		t.Fatal("this answer would have fitted the old limit; make it larger:", size.Load())
	}
	pad.Store(96 << 10) // exactly the limit: still a valid answer (JSON allows white space after the value)
	if st, resp, _, err := HTTPPairAny(context.Background(), srv.URL, key); st != 200 || err != nil || resp.Token != goodAnswer.Token || size.Load() != 96<<10 {
		t.Fatal("exactly 96 KiB was refused:", st, err, size.Load())
	}
	pad.Store(96<<10 + 1)
	st, resp, _, err = HTTPPairAny(context.Background(), srv.URL, key)
	var u *Unverified
	if st != 200 || !errors.As(err, &u) || !strings.Contains(u.Why, "larger than a pairing answer can be") || resp.Token != "" {
		t.Fatal("96 KiB + 1 was accepted:", st, err)
	}
	// an endless answer is cut off at the limit, with any status
	for _, status := range []int{200, 400} {
		endless := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			chunk := bytes.Repeat([]byte(`{"code":"bad_request"} `), 4096)
			for i := 0; i < 64; i++ {
				if _, err := w.Write(chunk); err != nil {
					return
				}
			}
		}))
		st, resp, code, err := HTTPPairAny(context.Background(), endless.URL, key)
		endless.Close()
		if resp.Token != "" || code != "" || st != status || (status == 200 && !errors.As(err, &u)) || (status != 200 && err != nil) {
			t.Fatal(status, st, code, err)
		}
	}
}

// Hard requirement: an agent that only knows the old exchange is reported as too old, the panel never sends
// the old proof, and the enrollment and its code stay usable: when the command has installed the newer agent,
// the same code pairs.
func TestOldAgentIsTooOldAndTheCodeSurvives(t *testing.T) {
	old := &oldAgent{}
	machine := newSwap(t, old)
	r := newRig(t, machine.srv.URL)
	cr := r.add(t, "old-box")

	// well past the old agent's five requests a minute: it answers 400 five times, then 429
	e := r.wait(t, cr.ID, "too old", func(e Enrollment) bool { return e.Attempts >= 12 })
	if e.State != Waiting || e.Done || e.Hint != HintAgentTooOld || e.Pairing != "" {
		t.Fatalf("%+v", e)
	}
	for _, want := range []string{"too old for this panel", "does not know pairing v2", "Update it first", "this same code"} {
		if !strings.Contains(e.Message, want) {
			t.Errorf("the message lacks %q: %s", want, e.Message)
		}
	}
	if r.adds.Load() != 0 || r.count("agent_too_old") != 1 || r.count("failed") != 0 || r.count("expired") != 0 {
		t.Fatalf("adds %d events %v", r.adds.Load(), r.events)
	}
	if hosts, _ := r.store.Load(); len(hosts) != 0 {
		t.Fatal("an old agent was added:", hosts)
	}
	old.mu.Lock()
	if old.hits < 6 || old.proofs != 0 {
		t.Fatalf("hits %d, requests with the old proof %d", old.hits, old.proofs)
	}
	for _, b := range old.bodies {
		var q enroll.RequestV2
		if bytes.Contains(b, []byte("proof")) || json.Unmarshal(b, &q) != nil || q.V != 2 {
			t.Fatalf("the panel sent something else than a v2 request: %s", b)
		}
	}
	old.mu.Unlock()

	// the admin runs the command: the newer agent is installed and stores the key of THE SAME code
	machine.set(v2Agent(keyOf(t, cr.Code), goodAnswer))
	e = r.wait(t, cr.ID, "online", func(e Enrollment) bool { return e.Done })
	if e.State != Online || e.Pairing != managed.PairingV2 || e.Hint != "" || e.ID != cr.ID {
		t.Fatalf("%+v", e)
	}
	sec, err := r.store.Secret("old-box")
	if err != nil || sec.Token != goodAnswer.Token || sec.ControlToken != goodAnswer.ControlToken {
		t.Fatal(sec, err)
	}
	if hosts, _ := r.store.Load(); len(hosts) != 1 || hosts[0].Pairing != managed.PairingV2 || hosts[0].AgentVersion != "20-abcdef0" {
		t.Fatalf("%+v", hosts)
	}
	if r.adds.Load() != 1 || r.count("agent_too_old") != 1 {
		t.Fatal(r.adds.Load(), r.events)
	}
}

// Other answers of machines the panel cannot pair with yet. None of them ends the enrollment, none is paired.
func TestAnswersThatKeepTheEnrollmentWaiting(t *testing.T) {
	jsonAnswer := func(status int, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status); _, _ = w.Write([]byte(body)) }
	}
	for _, tc := range []struct {
		name  string
		h     http.HandlerFunc
		hint  string
		text  string
		audit string
	}{
		{"an agent from before Add host: no /pair", http.NotFound, HintAgentTooOld, "too old for this panel", "agent_too_old"},
		{"400 without a code", jsonAnswer(400, `bad request`), HintAgentTooOld, "Update it first", "agent_too_old"},
		{"the join code was stored for the old pairing", jsonAnswer(400, `{"error":"this agent's join code is for pairing v1, the request is v2","code":"wrong_version","pair":1}`),
			HintLegacyJoinCode, "holds a join code for the old pairing", ""},
		{"no code yet", jsonAnswer(404, `{"error":"pairing is not active on this agent","code":"pairing_inactive"}`), "", "has no join code yet", ""},
		{"another code", jsonAnswer(403, `{"error":"wrong join code","code":"wrong_code"}`), "", "different join code", ""},
		{"the wrong panel address", jsonAnswer(403, `{"error":"x","code":"wrong_address"}`), "", "is not where the panel connects from", ""},
		{"slow down", jsonAnswer(429, `{"error":"x","code":"rate_limited"}`), "", "asks to slow down", ""},
		{"not JSON content", jsonAnswer(415, `{"error":"x","code":"unsupported_media_type"}`), "", "the agent answered 415", ""},
		{"an agent-side fault", jsonAnswer(500, `{"error":"x","code":"tokens_unreadable"}`), "", "the agent answered 500", ""},
		{"a redirect", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://127.0.0.1:1/pair", http.StatusTemporaryRedirect)
		}, "", "the agent answered 307", ""},
		{"tokens in clear, as the old exchange answered", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(goodAnswer)
		}, HintUnverified, "did not prove it has the join code", "unverified"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.h)
			defer srv.Close()
			r := newRig(t, srv.URL)
			cr := r.add(t, "box")
			e := r.wait(t, cr.ID, "a few attempts", func(e Enrollment) bool { return e.Attempts >= 6 })
			if e.State != Waiting || e.Done || e.Hint != tc.hint || !strings.Contains(e.Message, tc.text) || e.Pairing != "" {
				t.Fatalf("%+v", e)
			}
			if hosts, _ := r.store.Load(); len(hosts) != 0 || r.adds.Load() != 0 {
				t.Fatal("added:", hosts)
			}
			if tc.audit != "" && r.count(tc.audit) != 1 {
				t.Fatal("audit entries:", r.events)
			}
			if tc.audit == "" && len(r.events) != 0 {
				t.Fatal("unexpected audit entries:", r.events)
			}
			for _, ev := range r.events {
				if strings.Contains(ev, cr.Code) || strings.Contains(ev, goodAnswer.Token) {
					t.Fatal("a secret in an audit entry:", ev)
				}
			}
			if !r.m.Cancel(cr.ID) {
				t.Fatal("the enrollment could not be cancelled: it was already over")
			}
		})
	}
}

// An impostor that answers in the machine's place without the code is never adopted, whatever it answers and
// however long; when the real agent takes its place, the same code pairs.
func TestImpostorIsNeverAdopted(t *testing.T) {
	answers := atomic.Int32{}
	impostor := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch answers.Add(1) % 3 {
		case 0: // the old answer
			_ = json.NewEncoder(w).Encode(goodAnswer)
		case 1: // a v2 answer for the panel's own nonce, made with a guessed code
			q, _, _ := readV2(r)
			guess, _ := enroll.NewCode()
			k, _ := enroll.PairingKey(guess)
			a, _ := enroll.SealAnswer(k, requestUnder(k, q.Nonce), goodAnswer)
			_ = json.NewEncoder(w).Encode(a)
		default:
			_, _ = w.Write([]byte(`{"v":2,"nonce":"AAAAAAAAAAAAAAAAAAAAAA==","sealed":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}`))
		}
	})
	machine := newSwap(t, impostor)
	r := newRig(t, machine.srv.URL)
	cr := r.add(t, "box")
	e := r.wait(t, cr.ID, "many attempts", func(e Enrollment) bool { return e.Attempts >= 30 })
	if e.State != Waiting || e.Done || e.Hint != HintUnverified || r.adds.Load() != 0 || r.count("unverified") != 1 || r.count("paired") != 0 {
		t.Fatalf("%+v adds=%d events=%v", e, r.adds.Load(), r.events)
	}
	if hosts, _ := r.store.Load(); len(hosts) != 0 {
		t.Fatal("the impostor was adopted:", hosts)
	}
	if _, err := r.store.Secret("box"); err == nil {
		t.Fatal("tokens of an impostor were stored")
	}
	machine.set(v2Agent(keyOf(t, cr.Code), goodAnswer))
	if e = r.wait(t, cr.ID, "online", func(e Enrollment) bool { return e.Done }); e.State != Online || e.Pairing != managed.PairingV2 {
		t.Fatalf("%+v", e)
	}
}

// Add host and Re-pair refuse a Fairy-agent release whose install script does not know pairing v2: no code,
// no command, no enrollment (owner default: refuse, never pair the old way).
func TestCreateRefusesAReleaseWithoutPairingV2(t *testing.T) {
	none := func(string) string { return "" }
	for _, tc := range []struct {
		sh, ps1 bool
		os      string
		ok      bool
	}{
		{false, false, "linux", false}, {false, false, "windows", false},
		{true, false, "linux", true}, {true, false, "windows", false},
		{false, true, "linux", false}, {false, true, "windows", true},
		{true, true, "linux", true}, {true, true, "windows", true},
	} {
		in := testInstallers
		in.PairV2SH, in.PairV2PS1 = tc.sh, tc.ps1
		m := addrManager(t)
		m.Scripts = Fixed(in)
		m.Pair = func(context.Context, string, []byte) (int, enroll.Response, string, error) {
			return 0, enroll.Response{}, "", errors.New("nobody there")
		}
		cr, err := m.Create(context.Background(), Request{Name: "box", OS: tc.os}, nil, "neptune", none)
		var old *TooOld
		if tc.ok {
			if err != nil || !strings.Contains(cr.Command, map[string]string{"linux": " --pair-v2", "windows": " -PairV2"}[tc.os]) {
				t.Errorf("%+v: %v %s", tc, err, cr.Command)
			}
			m.Cancel(cr.ID)
			continue
		}
		if !errors.As(err, &old) || len(m.List()) != 0 || cr.Code != "" || cr.Command != "" {
			t.Errorf("%+v: not refused: %v", tc, err)
			continue
		}
		for _, want := range []string{"build-7 (Fairy-agent 7-6155fe6) is too old for this panel", "does not know pairing v2", "does not pair the old", "agent_release"} {
			if !strings.Contains(old.Msg, want) {
				t.Errorf("the refusal lacks %q: %s", want, old.Msg)
			}
		}
	}
	// Re-pair as well
	in := testInstallers
	in.PairV2SH = false
	m := addrManager(t)
	m.Scripts = Fixed(in)
	h := managed.Host{ID: "box", Name: "box", URL: "http://box:9101", OS: "linux"}
	var old *TooOld
	if _, err := m.Create(context.Background(), Request{}, &h, "neptune", none); !errors.As(err, &old) {
		t.Fatal(err)
	}
}

// The option is found in the signed script itself, like --sums-sha256: the case arm of install.sh, the
// parameter of install.ps1. A mention in a comment or a help text is not the option.
func TestVerifierFindsThePairingV2Option(t *testing.T) {
	const (
		shV2  = "#!/bin/sh\nwhile [ $# -gt 0 ]; do\n  case \"$1\" in\n    --sums-sha256) SUMS_PIN=\"$2\"; shift ;;\n    --join-stdin) CODE_STDIN=1 ;;\n    --pair-v2) PAIRV2=1 ;;\n  esac\n  shift\ndone\n"
		ps1V2 = "param(\n    [switch]$Ipmi,\n    [string]$SumsSha256 = '',\n    [switch]$PairV2,\n    [string]$ReleaseUrl = 'x'\n)\n"
	)
	for _, tc := range []struct {
		name    string
		sh, ps1 string
		wantSH  bool
		wantPS1 bool
	}{
		{"both", shV2, ps1V2, true, true},
		{"neither (the pinned build-13 style)", shWithPin, ps1WithPin, false, false},
		{"only install.sh", shV2, ps1WithPin, true, false},
		{"only install.ps1", shWithPin, ps1V2, false, true},
		{"mentioned in comments and help only", "#!/bin/sh\n# install.sh --pair-v2 is for newer panels\necho 'usage: [--pair-v2]'\nx=\"--pair-v2)\"\n",
			"# -PairV2: not here\nparam(\n    [string]$Note = '[switch]$PairV2'\n)\n", false, false},
		{"other spellings", "#!/bin/sh\ncase \"$1\" in\n  --pair-v2x) ;;\n  --pair-v20) ;;\nesac\n", "param(\n    [switch]$PairV2Later,\n    [string]$PairV2\n)\n", false, false},
		{"PowerShell is not case sensitive", shV2, "Param(\n\t[Switch]$pairv2\n)\n", true, true},
	} {
		f := newFakeRelease(t, "20-abcdef0", tc.sh, tc.ps1)
		in, err := f.verifier(t, "20-abcdef0").Installers(context.Background())
		if err != nil {
			t.Fatal(tc.name, err)
		}
		if in.PairV2SH != tc.wantSH || in.PairV2PS1 != tc.wantPS1 || in.PairsV2("linux") != tc.wantSH || in.PairsV2("windows") != tc.wantPS1 {
			t.Errorf("%s: sh %v ps1 %v", tc.name, in.PairV2SH, in.PairV2PS1)
		}
		lin, win := Command(in, managed.Host{OS: "linux"}, "CODE", "100.88.109.47"), Command(in, managed.Host{OS: "windows"}, "CODE", "100.88.109.47")
		if strings.Contains(lin, "--pair-v2") != tc.wantSH || strings.Contains(win, "-PairV2") != tc.wantPS1 {
			t.Errorf("%s: the command and the script disagree:\n%s\n%s", tc.name, lin, win)
		}
	}
}

// What the browser and the audit log get never holds the pairing key or the code.
func TestEnrollmentCopiesHoldNoKey(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	r := newRig(t, srv.URL)
	cr := r.add(t, "box")
	key := keyOf(t, cr.Code)
	e := r.wait(t, cr.ID, "one attempt", func(e Enrollment) bool { return e.Attempts >= 1 })
	if e.key != nil || cr.key != nil || e.reported != nil || e.held != nil {
		t.Fatal("a copy holds the key")
	}
	for _, v := range []any{e, r.m.List()} {
		b, _ := json.Marshal(v)
		norm, _ := enroll.Normalize(cr.Code)
		if bytes.Contains(b, key) || bytes.Contains(b, []byte(cr.Code)) || bytes.Contains(b, []byte(norm)) || bytes.Contains(bytes.ToLower(b), []byte(`"key"`)) {
			t.Fatalf("%s", b)
		}
	}
	r.m.Cancel(cr.ID)
}

// The old exchange is gone from the panel: no non-test file of the packages that pair or simulate pairing
// builds an old request, proof or verifier.
func TestNoPairingV1InThePanel(t *testing.T) {
	old := regexp.MustCompile(`enroll\.(Proof|Verifier|Check|Request)\b[^V]`)
	for _, dir := range []string{".", "../demo", "../web", "../../cmd/fairy"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil || len(files) == 0 {
			t.Fatal(dir, err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			if m := old.Find(b); m != nil {
				t.Errorf("%s uses the old exchange: %s", f, m)
			}
		}
	}
}

// requestUnder really is the agent's key schedule: the agent's own check accepts what it builds with the
// right key, so the impostor cases above fail for the wrong key and for no other reason.
func TestRequestUnderMatchesTheAgent(t *testing.T) {
	key := keyOf(t, "MFRG-GZDF-MZTW-Q2LK-NNWG-23TP-OBYX-E43U")
	q, err := enroll.NewRequestV2(key)
	if err != nil {
		t.Fatal(err)
	}
	mine := requestUnder(key, q.Nonce)
	if !bytes.Equal(mine.MAC, q.MAC) || enroll.CheckRequestV2(key, mine) != nil {
		t.Fatal("the test's key schedule is not the agent's")
	}
	a, err := enroll.SealAnswer(key, mine, goodAnswer)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(a)
	if resp, err := enroll.OpenAnswer(key, q, b); err != nil || resp.Token != goodAnswer.Token {
		t.Fatal(err)
	}
}
