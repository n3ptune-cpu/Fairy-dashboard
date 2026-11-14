package enrollment

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/enroll"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/managed"
)

// freshRig is a manager whose machine answers every pairing at once, and that writes down what it asks of its
// hooks, in order, with what was stored at that moment.
func freshRig(t *testing.T, fresh func(managed.Host) error) (*Manager, *managed.Store, *[]string) {
	t.Helper()
	store := managed.NewStore(t.TempDir())
	var calls []string
	stored := func() string {
		hosts, _ := store.Load()
		_, err := os.Stat(store.Path())
		return fmt.Sprintf("%d hosts stored, list there: %v", len(hosts), err == nil)
	}
	m := &Manager{Store: store, Now: time.Now, Interval: time.Millisecond, OnlineWait: time.Second, PanelAddr: "100.64.0.10",
		Scripts: Fixed(testInstallers), items: map[string]*Enrollment{},
		Hooks: Hooks{
			Config: func() *config.Panel { return testConfig(t) },
			Remove: func(id string) { calls = append(calls, "remove "+id) },
			Add:    func(e managed.Entry) error { calls = append(calls, "add "+e.Agent.ID+" ("+stored()+")"); return nil },
			Fresh: func(h managed.Host) error {
				calls = append(calls, "fresh "+h.ID+" ("+stored()+")")
				return fresh(h)
			},
			Online: func(string) (bool, string) { return true, "" },
			Event:  func(Enrollment, string, string) {},
		},
		Pair: func(context.Context, string, []byte) (int, enroll.Response, string, error) {
			return 200, enroll.Response{AgentVersion: "8-abcdef0", Token: "new-read-token-xxxxxxxxxxxxxxxxxx"}, "", nil
		}}
	return m, store, &calls
}

func done(t *testing.T, m *Manager, id string) Enrollment {
	t.Helper()
	for i := 0; i < 600; i++ {
		if e, _ := m.Get(id); e.Done {
			return e
		}
		time.Sleep(5 * time.Millisecond)
	}
	e, _ := m.Get(id)
	t.Fatalf("the enrollment never finished: %+v", e)
	return e
}

// A host that is added starts clean: the panel is asked to take out what it still keeps under the new host's
// id (Hooks.Fresh) BEFORE anything of the host is stored and before it is polled. When that cannot be done
// the add is refused, with nothing stored: a host must never start with what was kept for another machine.
// A Re-pair is not an add, and asks nothing.
func TestAnAddedHostIsClearedBeforeAnythingOfItIsStored(t *testing.T) {
	t.Run("added", func(t *testing.T) {
		m, store, calls := freshRig(t, func(managed.Host) error { return nil })
		cr, err := m.Create(context.Background(), Request{Name: "BOX-A", OS: "linux"}, nil, "alice", func(string) string { return "" })
		if err != nil {
			t.Fatal(err)
		}
		if e := done(t, m, cr.ID); e.State != Online {
			t.Fatalf("%+v", e)
		}
		want := []string{"fresh box-a (0 hosts stored, list there: false)", "add box-a (1 hosts stored, list there: true)"}
		if !slices.Equal(*calls, want) {
			t.Fatalf("the hooks, in order:\n got %q\nwant %q", *calls, want)
		}
		if _, err := store.Secret("box-a"); err != nil {
			t.Fatal("the host's tokens:", err)
		}
	})
	t.Run("what is kept cannot be taken out", func(t *testing.T) {
		m, store, calls := freshRig(t, func(managed.Host) error {
			return errors.New("what the panel keeps under this id could not be taken out (disk full)")
		})
		cr, err := m.Create(context.Background(), Request{Name: "BOX-A", OS: "linux"}, nil, "alice", func(string) string { return "" })
		if err != nil {
			t.Fatal(err)
		}
		e := done(t, m, cr.ID)
		if want := "cannot add it: what the panel keeps under this id could not be taken out (disk full). Nothing was stored on the panel; make a new code to try again"; e.State != Failed || e.Message != want {
			t.Fatalf("state %q, message %q\n                want %q", e.State, e.Message, want)
		}
		if want := []string{"fresh box-a (0 hosts stored, list there: false)"}; !slices.Equal(*calls, want) {
			t.Fatalf("the hooks, in order (the host is not polled):\n got %q\nwant %q", *calls, want)
		}
		hosts, _ := store.Load()
		_, secErr := store.Secret("box-a")
		_, listErr := os.Stat(store.Path())
		if len(hosts) != 0 || secErr == nil || !os.IsNotExist(listErr) {
			t.Fatalf("something of the refused host was stored: hosts %v, tokens %v, list %v", hosts, secErr, listErr)
		}
	})
	t.Run("re-paired", func(t *testing.T) {
		m, store, calls := freshRig(t, func(managed.Host) error { return errors.New("never asked") })
		h := managed.Host{ID: "box-a", Name: "BOX-A", URL: "http://box-a:9101", OS: "linux"}
		if err := store.Put(h, managed.Secret{Token: "old-read-token-xxxxxxxxxxxxxxxxxx"}); err != nil {
			t.Fatal(err)
		}
		cr, err := m.Create(context.Background(), Request{}, &h, "alice", nil)
		if err != nil {
			t.Fatal(err)
		}
		if e := done(t, m, cr.ID); e.State != Online {
			t.Fatalf("%+v", e)
		}
		if want := []string{"remove box-a", "add box-a (1 hosts stored, list there: true)"}; !slices.Equal(*calls, want) {
			t.Fatalf("the hooks of a Re-pair, in order:\n got %q\nwant %q", *calls, want)
		}
	})
}
