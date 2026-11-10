package enrollment

import (
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/enroll"
)

// Something answers "paired" at the machine's address without the join code (another program on the port,
// another machine at the name, an old agent's answer in clear). The enrollment keeps waiting on purpose: the
// real agent can still pair with the same code. So the state the dialog shows must say plainly that what
// answered is not the agent and that nothing was added, and it must carry nothing secret: not the code, not
// its key, and nothing of what the other side sent.
func TestSomethingElseAnsweringIsSaidPlainly(t *testing.T) {
	const theirs = "impostor-token-0123456789abcdefghijklmnop"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"agent_version":"99-evil","hostname":"EVIL-BOX","platform":"linux","token":"` + theirs + `","control_token":"` + theirs + `"}`))
	}))
	defer srv.Close()
	r := newRig(t, srv.URL)
	cr := r.add(t, "box")
	e := r.wait(t, cr.ID, "the hint", func(e Enrollment) bool { return e.Hint == HintUnverified && e.Attempts >= 3 })
	if e.State != Waiting || e.Done || e.Pairing != "" || e.AgentHostname != "" {
		t.Fatalf("%+v", e)
	}
	for _, want := range []string{
		"not added: something answers at box, but it is not a Fairy agent that has this join code",
		"(it is not a pairing v2 answer)",
		"Nothing it sent was used, and the panel keeps waiting",
		"another program or another machine answers at that address: check the name and the address",
		"cancel this, make a new code and run the new command",
	} {
		if !strings.Contains(e.Message, want) {
			t.Errorf("the message lacks %q:\n%s", want, e.Message)
		}
	}
	norm, _ := enroll.Normalize(cr.Code)
	key := hex.EncodeToString(keyOf(t, cr.Code))
	secret := func(where, s string) {
		t.Helper()
		for _, bad := range []string{cr.Code, norm, key, theirs, "EVIL-BOX", "99-evil"} {
			if strings.Contains(s, bad) {
				t.Errorf("%s carries %q: %s", where, bad, s)
			}
		}
	}
	secret("the message", e.Message)
	r.mu.Lock()
	n := 0
	for _, ev := range r.events {
		secret("the audit entry", ev)
		if strings.HasPrefix(ev, "unverified: ") {
			n++
			if !strings.Contains(ev, "did not prove the join code (it is not a pairing v2 answer): not added") {
				t.Error(ev)
			}
		}
	}
	r.mu.Unlock()
	if n != 1 {
		t.Fatal("audit entries about the answer:", n)
	}
	if hosts, _ := r.store.Load(); len(hosts) != 0 || r.adds.Load() != 0 {
		t.Fatal("added:", hosts)
	}
	if !r.m.Cancel(cr.ID) {
		t.Fatal("it could not be cancelled")
	}
}

// When the code runs out while only such answers came, the finished enrollment still says so (the dialog may
// have been closed for the whole 15 minutes).
func TestSomethingElseAnsweringIsStillSaidWhenTheCodeExpires(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		defer letEnrollmentsEnd()
		r := newPaceRig(t)
		r.answer = func(string, int) (int, enroll.Response, string, error) {
			return 200, enroll.Response{}, "", &Unverified{Why: "it was not made with this join code for this request"}
		}
		cr := r.add(t, "box")
		time.Sleep(enroll.TTL + time.Minute)
		e, _ := r.m.Get(cr.ID)
		if e.State != Expired || !e.Done || e.Hint != HintUnverified {
			t.Fatalf("%+v", e)
		}
		if want := "the code expired and nothing was added: until the end, what answered at box was not a Fairy agent that has the join code"; !strings.Contains(e.Message, want) {
			t.Fatal(e.Message)
		}
		if strings.Contains(e.Message, cr.Code) {
			t.Fatal("the code is in the message")
		}
		// an enrollment that simply ran out says what it always said
		r.answer = notYet
		cr = r.add(t, "other-box")
		time.Sleep(enroll.TTL + time.Minute)
		if e, _ = r.m.Get(cr.ID); e.State != Expired || e.Hint != "" || e.Message != "the code expired before the machine ran the command: make a new one" {
			t.Fatalf("%+v", e)
		}
	})
}
