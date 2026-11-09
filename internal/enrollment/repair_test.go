package enrollment

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/enroll"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/managed"
)

// Re-pair keeps none of the previous pairing's secrets: the tokens file holds only the new tokens, the poller
// is told to forget the old ones before the new ones are added, and a node that sends no Proxmox grant this
// time loses its old CA file.
func TestRepairDropsTheOldTokens(t *testing.T) {
	store := managed.NewStore(t.TempDir())
	h := managed.Host{ID: "nsx-kachina", Name: "nsx-kachina", URL: "http://nsx-kachina:9101", OS: "linux", ProxmoxWanted: true,
		Options: managed.Options{PowerControl: true}}
	old := managed.Secret{Token: "old-read-token-xxxxxxxxxxxxxxxxxx", ControlToken: "old-control-token-yyyyyyyyyyyyyyy", PVETokenSecret: "old-pve-secret-0000-1111"}
	if err := store.Put(h, old); err != nil {
		t.Fatal(err)
	}
	if err := store.PutCA(h.ID, []byte("-----BEGIN CERTIFICATE-----\nold\n-----END CERTIFICATE-----\n")); err != nil {
		t.Fatal(err)
	}
	var calls []string
	m := &Manager{Store: store, Now: time.Now, Interval: time.Millisecond, OnlineWait: time.Second, PanelAddr: "100.88.109.47",
		Scripts: Fixed(testInstallers), items: map[string]*Enrollment{},
		Hooks: Hooks{
			Config: func() *config.Panel { return &config.Panel{} },
			Remove: func(id string) { calls = append(calls, "remove "+id) },
			Add:    func(e managed.Entry) error { calls = append(calls, "add "+e.Agent.Token); return nil },
			Online: func(string) (bool, string) { return true, "" },
			Event:  func(Enrollment, string, string) {},
		},
		Pair: func(context.Context, string, string) (int, enroll.Response, string, error) {
			return 200, enroll.Response{AgentVersion: "8-abcdef0", Token: "new-read-token-xxxxxxxxxxxxxxxxxx",
				ControlToken: "new-control-token-yyyyyyyyyyyyyyy"}, "", nil
		}}
	cr, err := m.Create(context.Background(), Request{}, &h, "neptune", nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		if e, _ := m.Get(cr.ID); e.Done {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if e, _ := m.Get(cr.ID); e.State != Online {
		t.Fatalf("%+v", e)
	}
	sec, err := store.Secret(h.ID)
	if err != nil || sec.Token != "new-read-token-xxxxxxxxxxxxxxxxxx" || sec.ControlToken != "new-control-token-yyyyyyyyyyyyyyy" || sec.PVETokenSecret != "" {
		t.Fatalf("%+v %v", sec, err)
	}
	raw, _ := os.ReadFile(store.Path())
	files, _ := os.ReadDir(store.Path()[:strings.LastIndex(store.Path(), "/")] + "/secrets")
	for _, f := range files {
		b, _ := os.ReadFile(store.Path()[:strings.LastIndex(store.Path(), "/")] + "/secrets/" + f.Name())
		if strings.Contains(string(b), "old-") {
			t.Fatalf("old secret kept in %s", f.Name())
		}
	}
	if strings.Contains(string(raw), "old-") {
		t.Fatal("old secret in managed-hosts.yaml")
	}
	if _, err := os.Stat(store.CAPath(h.ID)); !os.IsNotExist(err) {
		t.Fatal("old CA kept", err)
	}
	if strings.Join(calls, ", ") != "remove nsx-kachina, add new-read-token-xxxxxxxxxxxxxxxxxx" {
		t.Fatal(calls)
	}
}

// Remove while a Re-pair is pairing: the host does not come back. CancelHost either stops the pairing before it
// stores anything or waits until it has (and the caller then removes it).
func TestRemoveDuringRepairDoesNotBringTheHostBack(t *testing.T) {
	store := managed.NewStore(t.TempDir())
	h := managed.Host{ID: "nsx-furina", Name: "nsx-furina", URL: "http://nsx-furina:9101", OS: "linux"}
	if err := store.Put(h, managed.Secret{Token: "old-read-token-xxxxxxxxxxxxxxxxxx"}); err != nil {
		t.Fatal(err)
	}
	answering := make(chan struct{})
	release := make(chan struct{})
	var adds atomic.Int32
	m := &Manager{Store: store, Now: time.Now, Interval: time.Millisecond, OnlineWait: time.Second, PanelAddr: "100.88.109.47",
		Scripts: Fixed(testInstallers), items: map[string]*Enrollment{},
		Hooks: Hooks{
			Config: func() *config.Panel { return &config.Panel{} },
			Remove: func(string) {},
			Add:    func(managed.Entry) error { adds.Add(1); return nil },
			Online: func(string) (bool, string) { return true, "" },
			Event:  func(Enrollment, string, string) {},
		},
		Pair: func(context.Context, string, string) (int, enroll.Response, string, error) {
			close(answering) // the machine answers: the pairing is in flight
			<-release
			return 200, enroll.Response{Token: "new-read-token-xxxxxxxxxxxxxxxxxx"}, "", nil
		}}
	cr, err := m.Create(context.Background(), Request{}, &h, "neptune", nil)
	if err != nil {
		t.Fatal(err)
	}
	<-answering
	m.CancelHost(h.ID) // Remove
	_ = store.Remove(h.ID)
	close(release)
	time.Sleep(50 * time.Millisecond)
	if e, _ := m.Get(cr.ID); e.State != Cancelled || adds.Load() != 0 {
		t.Fatalf("%+v adds=%d", e, adds.Load())
	}
	if hosts, _ := store.Load(); len(hosts) != 0 {
		t.Fatal("the removed host came back", hosts)
	}
	if m.Cancel(cr.ID) {
		t.Fatal("cancelled twice")
	}
}
