package enrollment

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/enroll"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/managed"
)

// An Add host under an id that the list of added hosts (managed-hosts.yaml) already has is refused.
//
// The check of an Add (managed.Check) looks at the running configuration. A host that was added in the panel
// and that a start left out (its tokens file is damaged: "Re-pair it") is not in it, so an Add under its name
// was accepted like any other; at pairing it took that host's place in the list, and Hooks.Fresh took out what
// "Edit host" had stored for it, which is kept for its Re-pair (re-review of Edit host round 2, H1). Such a
// host is the panel's still: it is paired again or removed, not added a second time.
//
// The same holds at pairing, for an id that came into the list after the code was made, and for a list that
// cannot be read: nothing of the host is stored, and Fresh is not asked.
const takenSentence = "the panel already has a host with the id box-a (BOX-A, added with Add host) that is not polled right now: " +
	"the list of this dialog says why. Re-pair that host, or Remove it first; an Add under its id would take its place. Nothing was stored"

func TestAnAddUnderTheIdOfAStoredHostIsRefused(t *testing.T) {
	waiting := managed.Host{ID: "box-a", Name: "BOX-A", URL: "http://box-a:9101", OS: "linux"}
	const oldToken = "old-read-token-xxxxxxxxxxxxxxxxxx"
	stillTheOldHost := func(t *testing.T, store *managed.Store) {
		t.Helper()
		hosts, err := store.Load()
		sec, secErr := store.Secret("box-a")
		if err != nil || len(hosts) != 1 || hosts[0].URL != waiting.URL || secErr != nil || sec.Token != oldToken {
			t.Fatalf("the stored host is not as it was: %v %+v, tokens %v", err, hosts, secErr)
		}
	}
	never := func(managed.Host) error { return errors.New("never asked") }
	t.Run("when the code is asked for", func(t *testing.T) {
		m, store, calls := freshRig(t, never)
		if err := store.Put(waiting, managed.Secret{Token: oldToken}); err != nil {
			t.Fatal(err)
		}
		_, err := m.Create(context.Background(), Request{Name: "BOX-A", OS: "linux"}, nil, "alice", func(string) string { return "" })
		var v *Validation
		if !errors.As(err, &v) || v.Msg != takenSentence {
			t.Fatalf("the Add was not refused with the sentence:\n got %v\nwant %s", err, takenSentence)
		}
		if len(*calls) != 0 || len(m.List()) != 0 {
			t.Fatalf("a refused Add asked the hooks or left an enrollment: %q, %d", *calls, len(m.List()))
		}
		stillTheOldHost(t, store)
	})
	t.Run("at pairing", func(t *testing.T) {
		m, store, calls := freshRig(t, never)
		m.Pair = func(context.Context, string, []byte) (int, enroll.Response, string, error) {
			// the id comes into the list while the machine pairs
			if err := store.Put(waiting, managed.Secret{Token: oldToken}); err != nil {
				t.Error(err)
			}
			return 200, enroll.Response{AgentVersion: "8-abcdef0", Token: "new-read-token-xxxxxxxxxxxxxxxxxx"}, "", nil
		}
		cr, err := m.Create(context.Background(), Request{Name: "BOX-A", OS: "linux"}, nil, "alice", func(string) string { return "" })
		if err != nil {
			t.Fatal(err)
		}
		e := done(t, m, cr.ID)
		if want := "cannot add it: " + takenSentence + "; make a new code after that"; e.State != Failed || e.Message != want {
			t.Fatalf("state %q, message %q\n                want %q", e.State, e.Message, want)
		}
		if len(*calls) != 0 {
			t.Fatalf("the hooks were asked (Fresh would take out what is kept for the stored host): %q", *calls)
		}
		stillTheOldHost(t, store)
	})
	t.Run("a list that cannot be read", func(t *testing.T) {
		m, store, calls := freshRig(t, never)
		if err := os.WriteFile(store.Path(), []byte("hosts: [\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := m.Create(context.Background(), Request{Name: "BOX-A", OS: "linux"}, nil, "alice", func(string) string { return "" })
		var v *Validation
		if !errors.As(err, &v) || !strings.HasPrefix(v.Msg, "the list of hosts added in the panel cannot be read (") ||
			!strings.HasSuffix(v.Msg, "): whether the id box-a is free is not known, so no host is added. Nothing was stored") {
			t.Fatalf("the Add was not refused: %v", err)
		}
		if len(*calls) != 0 || len(m.List()) != 0 {
			t.Fatalf("a refused Add asked the hooks or left an enrollment: %q, %d", *calls, len(m.List()))
		}
	})
	t.Run("its Re-pair goes on working", func(t *testing.T) {
		m, store, calls := freshRig(t, never)
		if err := store.Put(waiting, managed.Secret{Token: oldToken}); err != nil {
			t.Fatal(err)
		}
		h := waiting
		cr, err := m.Create(context.Background(), Request{}, &h, "alice", nil)
		if err != nil {
			t.Fatal(err)
		}
		if e := done(t, m, cr.ID); e.State != Online {
			t.Fatalf("%+v", e)
		}
		if want := []string{"remove box-a", "add box-a (1 hosts stored, list there: true)"}; len(*calls) != 2 || (*calls)[0] != want[0] || (*calls)[1] != want[1] {
			t.Fatalf("the hooks of a Re-pair, in order:\n got %q\nwant %q", *calls, want)
		}
		if sec, err := store.Secret("box-a"); err != nil || sec.Token != "new-read-token-xxxxxxxxxxxxxxxxxx" {
			t.Fatalf("the Re-pair's tokens were not stored: %v", err)
		}
	})
}
