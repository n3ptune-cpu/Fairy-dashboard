package power

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/textsafe"
)

// hostileAnswer is a Tasmota answer with a valid reading in front and about n bytes of JSON behind it whose
// decoding costs many times its size: shape "kv" is the auditors' {"":0},{"":0},..., shape "nest" the
// verifier's chains of nested one-key objects ({"":{"":{"": ... 0}}}), as deep as Go's decoder allows.
func hostileAnswer(n int, shape string) []byte {
	var b bytes.Buffer
	b.Grow(n + 64)
	b.WriteString(`{"StatusSNS":{"ENERGY":{"Power":5,"Voltage":230}},"p":[`)
	item := `{"":0},`
	if shape == "nest" {
		const depth = 9000
		item = strings.Repeat(`{"":`, depth) + "0" + strings.Repeat("}", depth) + ","
	}
	for b.Len()+len(item)+8 < n {
		b.WriteString(item)
	}
	b.WriteString("0]}")
	return b.Bytes()
}

// T-02, the panel's half. The panel reads the plugs, Shellies and BMCs of power_sources itself, with the
// Fairy-agent module's device readers. One answer of 4 MiB was read whole and decoded into generic JSON:
// 216 MiB live for the flat shape, more for the nested one, and the answer still counted as a good reading
// (5 W) so there was no back-off. With the unit's MemoryMax=512M ONE plug (nested shape) or two (flat) were a
// kill at every poll. The fix is in the agent module (pkg/devices: 256 KiB, 8192 values, 32 levels, one
// decoded answer at a time); this test holds the panel to it and fails with a module that lacks it.
func TestHostileDeviceAnswerCostsThePanelLittle(t *testing.T) {
	for _, shape := range []string{"kv", "nest"} {
		t.Run(shape, func(t *testing.T) {
			body := hostileAnswer(4<<20-16, shape)
			var asked atomic.Int64
			newPlug := func() *httptest.Server {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					asked.Add(1)
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write(body)
				}))
				t.Cleanup(srv.Close)
				return srv
			}
			// four such plugs, read at the same moment as the panel reads them
			var srcs []map[string]any
			for _, id := range []string{"p1", "p2", "p3", "p4"} {
				srcs = append(srcs, map[string]any{"id": id, "type": "tasmota", "url": newPlug().URL})
			}
			pm := manager(t, srcs, nil, nil)
			pm.Timeout = 20 * time.Second

			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			pm.PollAll(context.Background())
			runtime.ReadMemStats(&after)
			if asked.Load() == 0 {
				t.Fatal("no plug was asked")
			}
			if allocated := float64(after.TotalAlloc-before.TotalAlloc) / (1 << 20); allocated > 48 {
				t.Fatalf("reading four hostile plugs allocated %.0f MiB", allocated)
			}
			for _, s := range pm.Overview().Sources {
				if s.Status == "ok" || len(s.Readings) != 0 || s.Error == nil {
					t.Fatalf("%s: a 4 MiB answer is taken as a reading: status %s, readings %v", s.ID, s.Status, s.Readings)
				}
				// the text says which limit the answer passed, and is short and clean like every error text
				e := *s.Error
				if !strings.Contains(e, "too large") && !strings.Contains(e, "too many values") && !strings.Contains(e, "nested too deep") {
					t.Fatalf("%s: the error does not name a limit: %q", s.ID, e)
				}
				if len(e) > textsafe.MaxError || textsafe.Clean(e) != e {
					t.Fatalf("%s: error text %q", s.ID, e)
				}
				if s.NextAttemptInS == nil || *s.NextAttemptInS <= 0 {
					t.Fatalf("%s: no back-off after a refused answer: %v", s.ID, s.NextAttemptInS)
				}
			}
		})
	}
}

// T-02: an honest plug is read as before.
func TestHonestPlugStillReads(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"StatusSNS":{"Time":"2026-11-11T10:00:00","ENERGY":{"Power":42,"Voltage":231,"Current":0.18,"Total":12.5}}}`))
	}))
	defer srv.Close()
	pm := manager(t, []map[string]any{{"id": "desk", "type": "tasmota", "url": srv.URL}}, nil, nil)
	pm.PollAll(context.Background())
	s := pm.Overview().Sources[0]
	if s.Status != "ok" || s.Error != nil || len(s.Readings) == 0 || s.Readings[0].Watts == nil || *s.Readings[0].Watts != 42 {
		t.Fatalf("%+v", s)
	}
}
