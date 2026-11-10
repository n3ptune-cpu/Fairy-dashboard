package poller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/normalise"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// N1: what a hostile Proxmox API can do to the panel. A machine added with Add host as "a new Proxmox node"
// hands the panel its own API address, token, CA and certificate name at pairing, so whoever controls that
// machine controls this API. Before, this path had none of M1's limits: 32 MiB of answer, any number of rows,
// uncut texts, maps that only grew, one connection per listed node.

func heapMiB() float64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return float64(m.HeapAlloc) / (1 << 20)
}

// pveRows is a cluster/resources answer with n guests whose ids start at first; extra goes into every row.
func pveRows(n, first int, extra string) []byte {
	var b bytes.Buffer
	b.WriteString(`{"data":[`)
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"type":"lxc","vmid":%d,"node":"n","status":"running","netin":1,"netout":1%s}`, first+i, extra)
	}
	b.WriteString(`]}`)
	return b.Bytes()
}

// pveNodes is an answer that lists n nodes with this status.
func pveNodes(n int, status string) []byte {
	var b bytes.Buffer
	b.WriteString(`{"data":[`)
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"type":"node","node":"n%d","status":"%s"}`, i, status)
	}
	b.WriteString(`]}`)
	return b.Bytes()
}

// pvePoller is a poller with one Proxmox cluster at each of the given handlers (real HTTPS, the real client)
// and no agent that answers.
func pvePoller(t *testing.T, handlers ...http.HandlerFunc) *Poller {
	t.Helper()
	var urls []string
	for _, h := range handlers {
		srv := httptest.NewTLSServer(h)
		t.Cleanup(srv.Close)
		urls = append(urls, srv.URL)
	}
	cfg := testutil.Config(t, urls[:1], deadURL(t), deadURL(t))
	for i, u := range urls[1:] {
		c := cfg.Proxmox[0]
		c.ID, c.URLs, c.ClusterName = fmt.Sprintf("evil%d", i), []string{u}, fmt.Sprintf("evil%d", i)
		c.IDPrefix = model.P(fmt.Sprintf("e%d-", i))
		cfg.Proxmox = append(cfg.Proxmox, c)
	}
	cfg.RequestTimeout = 20
	p, err := New(cfg, config.Secrets{SessionSecret: "x", ProxmoxTokenSecret: "s"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// pveSource is the status of the first Proxmox source.
func pveSource(p *Poller) model.SourceStatus {
	for _, src := range p.Summary().Sources {
		if strings.HasPrefix(src.Name, "proxmox:") {
			return src
		}
	}
	return model.SourceStatus{}
}

func pveSystems(p *Poller) (nodes, guests int) {
	for _, s := range p.Snapshot() {
		switch {
		case s.Source != "proxmox":
		case s.Kind == "node":
			nodes++
		default:
			guests++
		}
	}
	return
}

func kept(p *Poller) (refs, fps, cpus, rates int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.pveRefs), len(p.fingerprints), len(p.nodeCPU), p.rates.Len()
}

// The reviewer's first proof: one answer of 15 MiB with 200,000 guests became 200,002 systems and 543 MiB that
// stayed (the unit's MemoryMax is 512M), and with new ids on every poll the poller's maps grew without end.
func TestHostileProxmoxAnswerIsBounded(t *testing.T) {
	var body atomic.Value
	p := pvePoller(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(body.Load().([]byte)) })
	ctx := context.Background()
	base := heapMiB()

	// the reviewer's answer: over the 4 MiB limit, so it is not an answer at all
	big := pveRows(200_000, 1, "")
	body.Store(big)
	p.PollOnce(ctx)
	if n, g := pveSystems(p); n+g != 0 {
		t.Fatalf("an answer of %.1f MiB was taken: %d systems", float64(len(big))/(1<<20), n+g)
	}
	if src := pveSource(p); src.OK || model.Deref(src.LastError) != "invalid response" {
		t.Fatalf("the source must show why: %+v", src)
	}
	if h := heapMiB(); h-base > 20 {
		t.Errorf("heap after a refused answer: %.0f MiB (was %.0f)", h, base)
	}

	// the most rows that fit into 4 MiB, with new ids on every poll, five polls
	const rows = 50_000
	rounds := 5
	if raceEnabled {
		rounds = 2 // the same checks, less time under the race detector
	}
	for round := 0; round < rounds; round++ {
		b := pveRows(rows, 1+round*rows, "")
		if len(b) > maxProxmoxBody {
			t.Fatalf("the test's answer is %d bytes", len(b))
		}
		body.Store(b)
		p.PollOnce(ctx)
		nodes, guests := pveSystems(p)
		refs, fps, cpus, rates := kept(p)
		if nodes != 0 || guests != normalise.MaxProxmoxGuests {
			t.Fatalf("round %d: %d nodes, %d guests (limit %d)", round, nodes, guests, normalise.MaxProxmoxGuests)
		}
		// what the poller keeps per id is what it keeps for the systems that exist now, nothing of earlier polls
		if refs != guests || fps > guests+8 || cpus != 0 || rates > guests+8 {
			t.Fatalf("round %d: the poller keeps %d refs, %d fingerprints, %d CPU models, %d counters for %d systems", round, refs, fps, cpus, rates, guests)
		}
		if snap(p)[fmt.Sprintf("lxc-%d", 1+round*rows)].ID == "" || snap(p)[fmt.Sprintf("lxc-%d", round*rows)].ID != "" {
			t.Fatalf("round %d: the kept rows must be the first of this answer, none of the last one", round)
		}
	}
	js, _ := p.SnapshotJSON()
	total := 0
	for _, x := range js {
		total += len(x)
	}
	after := heapMiB()
	t.Logf("4 MiB answers with %d rows each, new ids on every poll: %d guests kept, heap %.0f -> %.0f MiB, snapshot JSON %.1f MiB",
		rows, normalise.MaxProxmoxGuests, base, after, float64(total)/(1<<20))
	if !raceEnabled && after-base > 40 {
		t.Errorf("heap grew by %.0f MiB", after-base)
	}

	// the cluster shrinks to nothing: nothing is left behind for it
	body.Store([]byte(`{"data":[]}`))
	p.PollOnce(ctx)
	if refs, fps, cpus, rates := kept(p); refs != 0 || fps > 8 || cpus != 0 || rates > 8 {
		t.Fatalf("an empty cluster leaves %d refs, %d fingerprints, %d CPU models, %d counters", refs, fps, cpus, rates)
	}
}

// Texts from the Proxmox API are cut like an agent's: megabytes of name or tags were copied into every
// snapshot and stream.
func TestHostileProxmoxTextsAreCut(t *testing.T) {
	long := strings.Repeat("A", 1<<20)
	manyTags := strings.Repeat("tag;", 5000)
	p := pvePoller(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/status") {
			_, _ = fmt.Fprintf(w, `{"data":{"cpuinfo":{"model":"%s"}}}`, strings.Repeat("C", 100_000))
			return
		}
		_, _ = fmt.Fprintf(w, `{"data":[{"type":"lxc","vmid":100,"node":"%s","status":"running","name":"%s","tags":"%s"},`+
			`{"type":"lxc","vmid":101,"node":"n","status":"running","name":"ok","tags":"%s"},`+
			`{"type":"node","node":"%s","status":"online"},{"type":"node","node":"pve1","status":"online"}]}`,
			long, long, long, manyTags, strings.Repeat("N", 400))
	})
	p.PollOnce(context.Background())
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) { // the CPU model arrives in the background
		p.PollOnce(context.Background())
		if snap(p)["node-pve1"].CPUModel != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	all := snap(p)
	s, t101, node := all["lxc-100"], all["lxc-101"], all["node-pve1"]
	if len(s.Name) > 260 || s.Parent == nil || len(*s.Parent) > 132 || len(s.Tags) != 1 || len(s.Tags[0]) > 68 {
		t.Fatalf("name %d bytes, parent %d bytes, %d tags of %d bytes", len(s.Name), len(model.Deref(s.Parent)), len(s.Tags), len(s.Tags[0]))
	}
	if len(t101.Tags) != 32 || t101.Name != "ok" {
		t.Fatalf("%d tags, name %q", len(t101.Tags), t101.Name)
	}
	if node.CPUModel == nil || len(*node.CPUModel) > maxCPUModel+4 {
		t.Fatalf("CPU model: %d bytes", len(model.Deref(node.CPUModel)))
	}
	for id, x := range all {
		if b, _ := json.Marshal(x); len(b) > 4096 {
			t.Errorf("%s is %d bytes of JSON", id, len(b))
		}
	}
}

// Every online node the API listed started a request for its CPU model at once, each on its own connection:
// 3000 listed nodes were 3000 connections from the panel and 15,000 goroutines.
func TestHostileProxmoxNodeListOpensTwoConnections(t *testing.T) {
	const nodes = 3000
	var inflight, peak atomic.Int32
	release := make(chan struct{})
	p := pvePoller(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/status") {
			n := inflight.Add(1)
			for {
				old := peak.Load()
				if n <= old || peak.CompareAndSwap(old, n) {
					break
				}
			}
			select {
			case <-release:
			case <-r.Context().Done():
			}
			inflight.Add(-1)
			return
		}
		_, _ = w.Write(pveNodes(nodes, "online"))
	})
	before := runtime.NumGoroutine()
	for range 5 {
		p.PollOnce(context.Background())
		time.Sleep(100 * time.Millisecond)
	}
	n, _ := pveSystems(p)
	t.Logf("%d nodes listed: %d shown, %d status requests open at once, goroutines %d -> %d", nodes, n, peak.Load(), before, runtime.NumGoroutine())
	if n != normalise.MaxProxmoxNodes {
		t.Errorf("%d nodes shown, limit %d", n, normalise.MaxProxmoxNodes)
	}
	if pk := peak.Load(); pk < 1 || pk > maxNodeLookups {
		t.Errorf("%d status requests at once, want 1 to %d", pk, maxNodeLookups)
	}
	if g := runtime.NumGoroutine(); g > before+40 {
		t.Errorf("goroutines %d -> %d", before, g)
	}
	close(release)
}

// One cluster whose API never answers a node's status request does not keep another cluster's nodes waiting.
func TestNodeLookupsAreBoundedPerCluster(t *testing.T) {
	hang := make(chan struct{})
	defer close(hang)
	evil := func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/status") {
			select {
			case <-hang:
			case <-r.Context().Done():
			}
			return
		}
		_, _ = w.Write(pveNodes(50, "online"))
	}
	good := func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/status") {
			_, _ = w.Write([]byte(`{"data":{"cpuinfo":{"model":"Real CPU"}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"type":"node","node":"real","status":"online"}]}`))
	}
	p := pvePoller(t, good, evil)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p.PollOnce(context.Background())
		if m := snap(p)["node-real"].CPUModel; m != nil && *m == "Real CPU" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the other cluster's node never got its CPU model")
}

// The limits do not change what a real cluster shows. A cluster/resources answer as Proxmox VE sends it (nodes,
// VMs, containers, a template, storage, an SDN zone and a pool row, every usual field) gives exactly the systems
// the normaliser makes of the same answer decoded as a whole, and nothing is logged about limits.
func TestRealClusterAnswerIsUnchangedByTheLimits(t *testing.T) {
	var b bytes.Buffer
	b.WriteString(`{"data":[`)
	for n := 0; n < 5; n++ {
		fmt.Fprintf(&b, `{"id":"node/pve%d","type":"node","node":"pve%d","status":"online","cpu":0.0312,"maxcpu":16,"mem":17179869184,"maxmem":67430088704,`+
			`"disk":9876543210,"maxdisk":100861726720,"uptime":1234567,"level":"","cgroup-mode":2},`, n, n)
		fmt.Fprintf(&b, `{"id":"storage/pve%d/local","type":"storage","node":"pve%d","storage":"local","status":"available","disk":1,"maxdisk":2,"content":"iso,vztmpl","plugintype":"dir","shared":0},`, n, n)
		fmt.Fprintf(&b, `{"id":"sdn/pve%d/localnetwork","type":"sdn","node":"pve%d","sdn":"localnetwork","status":"ok"},`, n, n)
	}
	for i := 0; i < 300; i++ {
		typ, status := "qemu", "running"
		if i%3 == 0 {
			typ = "lxc"
		}
		if i%7 == 0 {
			status = "stopped"
		}
		fmt.Fprintf(&b, `{"id":"%s/%d","type":"%s","vmid":%d,"node":"pve%d","name":"guest-%d","status":"%s","cpu":0.0%d,"maxcpu":4,"mem":%d,"maxmem":8589934592,`+
			`"disk":0,"maxdisk":34359738368,"netin":%d,"netout":%d,"diskread":5,"diskwrite":6,"uptime":%d,"template":0,"tags":"prod;web,db backup","pool":"main","hastate":"started","lock":""},`,
			typ, 100+i, typ, 100+i, i%5, i, status, i%9+1, 1000000+i, 5000+i, 7000+i, 86400+i)
	}
	b.WriteString(`{"id":"qemu/9000","type":"qemu","vmid":9000,"node":"pve0","name":"tmpl","status":"stopped","template":1,"maxcpu":2,"maxmem":1024,"maxdisk":1024},`)
	b.WriteString(`{"id":"/pool/main","type":"pool","pool":"main"}`)
	b.WriteString(`],"extra":{"a":[1,2,3]}}`)
	answer := b.Bytes()

	var logs bytes.Buffer
	var logMu sync.Mutex
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(lockedWriter{&logMu, &logs}, nil)))
	defer slog.SetDefault(old)

	p := pvePoller(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/status") {
			w.WriteHeader(404)
			return
		}
		_, _ = w.Write(answer)
	})
	p.PollOnce(context.Background())
	got := map[string]model.System{}
	for _, s := range p.Snapshot() {
		if s.Source == "proxmox" {
			got[s.ID] = s
		}
	}
	var whole struct {
		Data []jsonx.M `json:"data"`
	}
	if err := json.Unmarshal(answer, &whole); err != nil {
		t.Fatal(err)
	}
	pve := p.Clusters()[0]
	now, ts := time.Now(), float64(time.Now().Unix())
	want := normalise.NormaliseProxmox(whole.Data, now, ts, normalise.NewRateTracker(), normalise.ProxmoxOpts{
		Prefix: pve.Prefix(), ClusterID: model.P(pve.ID), Cluster: model.P(pve.ClusterName), Group: pve.Group, Owner: pve.Owner})
	if len(want) != 305 || len(got) != len(want) {
		t.Fatalf("%d systems from the poller, %d from the whole answer (want 5 nodes and 300 guests)", len(got), len(want))
	}
	for _, w := range want {
		g := got[w.ID]
		// what the poller adds on top of the normaliser, and the clock, are not part of the comparison
		for _, s := range []*model.System{&g, &w} {
			s.LastSeen, s.UpdatedAt, s.PollIntervalS, s.PollMode, s.Boosted, s.ProcessesAvailable = nil, nil, nil, "", false, false
			s.PowerActions, s.CPUModel = nil, nil
		}
		if !reflect.DeepEqual(g, w) {
			gj, _ := json.Marshal(g)
			wj, _ := json.Marshal(w)
			t.Fatalf("%s differs:\n got %s\nwant %s", w.ID, gj, wj)
		}
	}
	// the references for power actions are those of the same answer
	p.mu.Lock()
	ref, n := p.pveRefs["qemu-101"], len(p.pveRefs)
	p.mu.Unlock()
	if n != 305 || ref.Node != "pve1" || ref.Kind != "qemu" || ref.VMID == nil || *ref.VMID != 101 { // one per system that is shown
		t.Fatalf("%d refs, qemu-101: %+v", n, ref)
	}
	logMu.Lock()
	defer logMu.Unlock()
	if strings.Contains(logs.String(), "more than the panel takes") {
		t.Fatalf("a real cluster was reported as over the limits:\n%s", logs.String())
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l lockedWriter) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(b)
}

// The limits themselves, and that reaching them is said once in the log while it lasts.
func TestProxmoxRowLimitsAreLoggedOnce(t *testing.T) {
	var logs bytes.Buffer
	var logMu sync.Mutex
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(lockedWriter{&logMu, &logs}, nil)))
	defer slog.SetDefault(old)
	count := func() int {
		logMu.Lock()
		defer logMu.Unlock()
		return strings.Count(logs.String(), "more than the panel takes")
	}

	var body atomic.Value
	p := pvePoller(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/status") {
			w.WriteHeader(404)
			return
		}
		_, _ = w.Write(body.Load().([]byte))
	})
	mix := func(nodes, guests int) []byte {
		n, g := pveNodes(nodes, "online"), pveRows(guests, 1, "")
		return append(append(n[:len(n)-2], ','), g[len(`{"data":[`):]...)
	}
	// exactly at the limits: everything is shown, nothing is logged
	body.Store(mix(normalise.MaxProxmoxNodes, normalise.MaxProxmoxGuests))
	p.PollOnce(context.Background())
	if n, g := pveSystems(p); n != normalise.MaxProxmoxNodes || g != normalise.MaxProxmoxGuests || count() != 0 {
		t.Fatalf("at the limits: %d nodes, %d guests, %d log lines", n, g, count())
	}
	// one more of each: the first rows stay, the log says it once however often it is polled
	body.Store(mix(normalise.MaxProxmoxNodes+1, normalise.MaxProxmoxGuests+1))
	for range 4 {
		p.PollOnce(context.Background())
	}
	all := snap(p)
	n, g := pveSystems(p)
	if n != normalise.MaxProxmoxNodes || g != normalise.MaxProxmoxGuests || count() != 1 {
		t.Fatalf("over the limits: %d nodes, %d guests, %d log lines", n, g, count())
	}
	if all["node-n0"].ID == "" || all[fmt.Sprintf("node-n%d", normalise.MaxProxmoxNodes)].ID != "" ||
		all["lxc-1"].ID == "" || all[fmt.Sprintf("lxc-%d", normalise.MaxProxmoxGuests+1)].ID != "" {
		t.Fatal("the rows that are kept must be the first ones")
	}
	logMu.Lock()
	line := logs.String()
	logMu.Unlock()
	for _, want := range []string{"nodes_left_out=1", "guests_left_out=1", "max_nodes=64", "max_guests=2000", "cluster="} {
		if !strings.Contains(line, want) {
			t.Errorf("the log line lacks %s: %s", want, line)
		}
	}
	// it fits again, then not: said again
	body.Store(mix(3, 10))
	p.PollOnce(context.Background())
	body.Store(mix(normalise.MaxProxmoxNodes+5, 1))
	p.PollOnce(context.Background())
	if count() != 2 {
		t.Fatalf("%d log lines after it fitted and overflowed again", count())
	}
	// templates that are not shown do not use up the limit
	tmpl := pveRows(normalise.MaxProxmoxGuests, 100_000, `,"template":1`)
	g2 := pveRows(normalise.MaxProxmoxGuests, 1, "")
	body.Store(append(append(tmpl[:len(tmpl)-2], ','), g2[len(`{"data":[`):]...))
	p.PollOnce(context.Background())
	if _, g := pveSystems(p); g != normalise.MaxProxmoxGuests || snap(p)["lxc-1"].ID == "" {
		t.Fatalf("%d guests behind %d hidden templates", g, normalise.MaxProxmoxGuests)
	}
	// the same limits hold for rows a backend hands over already decoded
	rows := make([]jsonx.M, 0, 5000)
	for i := range 5000 {
		rows = append(rows, jsonx.M{"type": "qemu", "vmid": float64(i + 1), "node": "n", "status": "running"})
	}
	taken, l := normalise.LimitProxmoxRows(rows, false)
	if len(taken) != normalise.MaxProxmoxGuests || l.DroppedGuests != 5000-normalise.MaxProxmoxGuests || l.DroppedNodes != 0 {
		t.Fatalf("%d taken, %+v", len(taken), l)
	}
}

// How an answer is read: row by row, only the fields the panel uses, only plain values.
func TestProxmoxAnswerIsDecodedRowByRow(t *testing.T) {
	dec := func(s string) ([]jsonx.M, error) {
		return decodePVEResources([]byte(s), &normalise.ProxmoxLimit{})
	}
	rows, err := dec(`{"junk":{"data":[{"type":"node","node":"x"}]},"data":[
		{"type":"qemu","vmid":100,"node":"pve","name":"web","status":"running","cpu":0.5,"template":0,"unknown":{"deep":[1,2,{"a":"b"}]},"tags":"a;b"},
		{"type":"storage","storage":"local"},
		{"type":"lxc","vmid":"101","node":["not","a","name"],"name":{"x":1},"cpu":[1,2,3],"mem":null,"maxmem":true,"netin":1e999},
		{"type":"node","node":"pve","status":"online","maxcpu":8}
	],"more":[1,2,3]}`)
	if err != nil || len(rows) != 3 {
		t.Fatal(len(rows), err)
	}
	want := []jsonx.M{
		{"type": "qemu", "vmid": 100.0, "node": "pve", "name": "web", "status": "running", "cpu": 0.5, "template": 0.0, "tags": "a;b"},
		{"type": "lxc", "vmid": "101", "maxmem": true},
		{"type": "node", "node": "pve", "status": "online", "maxcpu": 8.0},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("\n got %v\nwant %v", rows, want)
	}
	for name, bad := range map[string]string{
		"not an object":        `[{"type":"node"}]`,
		"no data":              `{"rows":[]}`,
		"data is not a list":   `{"data":{"type":"node"}}`,
		"a row is not a row":   `{"data":[{"type":"node","node":"a"},7]}`,
		"cut off":              `{"data":[{"type":"node","node":"a"},`,
		"text after the end":   `{"data":[]} trailing`,
		"empty":                ``,
		"a field of the wrong": `{"data":[{"type":"node","node":"a"}],"x":}`,
	} {
		if _, err := dec(bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if rows, err := dec(`{"data":[]}`); err != nil || len(rows) != 0 {
		t.Error("an empty cluster is an answer", err)
	}
	// a second "data" is not read as rows (the first one counts)
	if rows, err := dec(`{"data":[{"type":"node","node":"a"}],"data":[{"type":"node","node":"b"}]}`); err != nil || len(rows) != 1 || rows[0]["node"] != "a" {
		t.Error(rows, err)
	}
	// a text of megabytes is cut where it enters
	if rows, _ := dec(`{"data":[{"type":"node","node":"` + strings.Repeat("n", 1<<20) + `"}]}`); len(rows) != 1 || len(rows[0]["node"].(string)) > maxPVEText+4 {
		t.Error("an uncut text")
	}
}

// The header of a Proxmox answer is bounded too (net/http's default is 10 MiB), and so are the small answers.
func TestProxmoxHeadersAndSmallAnswersAreBounded(t *testing.T) {
	var mode atomic.Value
	mode.Store("headers")
	p := pvePoller(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/status") {
			_, _ = fmt.Fprintf(w, `{"data":{"cpuinfo":{"model":"big"},"pad":"%s"}}`, strings.Repeat("x", maxProxmoxSmall))
			return
		}
		if mode.Load() == "headers" {
			for i := 0; i < 40; i++ {
				w.Header().Set(fmt.Sprintf("X-Pad-%d", i), strings.Repeat("h", 4096))
			}
		}
		_, _ = w.Write([]byte(`{"data":[{"type":"node","node":"a","status":"online"}]}`))
	})
	p.PollOnce(context.Background())
	if n, _ := pveSystems(p); n != 0 || pveSource(p).OK {
		t.Fatal("an answer with 160 KiB of headers was taken")
	}
	mode.Store("plain")
	for range 20 {
		p.PollOnce(context.Background())
		time.Sleep(10 * time.Millisecond)
	}
	if n, _ := pveSystems(p); n != 1 || snap(p)["node-a"].CPUModel != nil {
		t.Fatalf("%d nodes; a node status answer over %d KiB must not be used: %v", n, maxProxmoxSmall>>10, model.Deref(snap(p)["node-a"].CPUModel))
	}
}

// The heap with several hostile sources at once: five Proxmox APIs, each answering the largest body it may
// (4 MiB, as many rows as fit, megabyte texts in some, new ids on every poll), polled together for ten rounds.
// The unit runs with MemoryMax=512M.
func TestHeapWithSeveralHostileProxmoxSources(t *testing.T) {
	if testing.Short() {
		t.Skip("measurement")
	}
	var round atomic.Int64
	mk := func(i int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/status") {
				_, _ = fmt.Fprintf(w, `{"data":{"cpuinfo":{"model":"%s"}}}`, strings.Repeat("C", 200_000))
				return
			}
			n := int(round.Load())
			var b bytes.Buffer
			switch i % 3 {
			case 0: // as many small rows as fit
				b.WriteString(`{"data":[`)
				for j := 0; b.Len() < maxProxmoxBody-256; j++ {
					fmt.Fprintf(&b, `{"type":"lxc","vmid":%d,"node":"n","status":"running","netin":1,"netout":1},`, 1+j+n*100_000)
				}
				b.WriteString(`{}]}`)
			case 1: // empty objects and junk rows: the worst case for a decoder that builds everything
				b.WriteString(`{"data":[`)
				for b.Len() < maxProxmoxBody-64 {
					b.WriteString(`{},`)
				}
				b.WriteString(`{}]}`)
			default: // nodes and guests with long texts and nested junk
				b.WriteString(`{"data":[`)
				long := strings.Repeat("L", 1500)
				for j := 0; b.Len() < maxProxmoxBody-8192; j++ {
					typ := "qemu"
					if j%10 == 0 {
						typ = "node"
					}
					fmt.Fprintf(&b, `{"type":"%s","vmid":%d,"node":"n%d-%d","status":"running","name":"%s","tags":"%s","x":[[[[{}]]]],"cpu":{"a":[1,2,3]}},`,
						typ, 1+j+n*100_000, n, j, long, long)
				}
				b.WriteString(`{}]}`)
			}
			_, _ = w.Write(b.Bytes())
		}
	}
	p := pvePoller(t, mk(0), mk(1), mk(2), mk(3), mk(4))
	base := heapMiB()
	var peak atomic.Uint64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { // the largest heap in use while the answers are taken in
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			if m.HeapInuse > peak.Load() {
				peak.Store(m.HeapInuse)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	polls := 10
	if raceEnabled {
		polls = 2 // the measurement is for a normal build; under the race detector only that nothing races
	}
	for range polls {
		round.Add(1)
		p.PollOnce(context.Background())
		_ = p.Snapshot()
		_, _ = p.SnapshotJSON()
	}
	close(stop)
	<-done
	nodes, guests := pveSystems(p)
	rest := heapMiB()
	refs, fps, cpus, rates := kept(p)
	t.Logf("5 hostile Proxmox sources, %d polls of 4 MiB answers each: %d nodes and %d guests shown; heap at rest %.0f -> %.0f MiB; "+
		"largest heap in use while polling %.0f MiB (this process also holds the five hostile servers, which build 20 MiB of answers per poll); "+
		"kept per id: %d refs, %d fingerprints, %d CPU models, %d counters (race detector: %v)",
		polls, nodes, guests, base, rest, float64(peak.Load())/(1<<20), refs, fps, cpus, rates, raceEnabled)
	if guests > 5*normalise.MaxProxmoxGuests || nodes > 5*normalise.MaxProxmoxNodes || guests < 2*normalise.MaxProxmoxGuests {
		t.Errorf("%d nodes, %d guests (two of the five sources list more guests than the limit)", nodes, guests)
	}
	if refs > nodes+guests || fps > nodes+guests+8 || rates > nodes+guests+8 {
		t.Errorf("the poller keeps more than it shows: %d refs, %d fingerprints, %d counters for %d systems", refs, fps, rates, nodes+guests)
	}
	if !raceEnabled { // the race detector multiplies memory use; the numbers are for a normal build
		if rest-base > 64 {
			t.Errorf("heap at rest grew by %.0f MiB (want well under MemoryMax=512M: at most 64)", rest-base)
		}
		if pk := float64(peak.Load()) / (1 << 20); pk > 256 {
			t.Errorf("largest heap in use while polling: %.0f MiB (want at most 256)", pk)
		}
	}
}
