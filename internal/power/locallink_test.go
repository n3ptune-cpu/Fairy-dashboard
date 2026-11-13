package power

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A source of the panel's own power_sources with `system:` keeps that link in its state while it has no
// reading: before its first poll, and while it fails. The link is what decides who sees the source and on
// which host's page its reason is shown (web: powerLinks); with the link in the readings alone, a source
// that failed was linked to nothing.
func TestALocalSourceKeepsItsLinkWhileItHasNoReading(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }))
	defer down.Close()
	pm, err := NewManager([]map[string]any{{"id": "desk", "type": "shelly", "url": down.URL, "system": "pc-a"}}, 5, time.Second, 60, 3600, nil, func() float64 { return 0 })
	if err != nil {
		t.Fatal(err)
	}
	link := func(when string) {
		t.Helper()
		st, ok := byID(pm.Overview())["desk"]
		if !ok || st.Status != "unreachable" || len(st.Readings) != 0 {
			t.Fatalf("%s: the fixture: a source without a reading: %+v", when, st)
		}
		if st.SystemID == nil || *st.SystemID != "pc-a" {
			t.Errorf("%s: the source has no reading and lost its link: system_id %v, want pc-a", when, st.SystemID)
		}
	}
	link("before its first poll")
	pm.Runners[0].Poll(context.Background(), time.Second)
	if st := byID(pm.Overview())["desk"]; st.Error == nil {
		t.Fatalf("the fixture: the poll should have failed: %+v", st)
	}
	link("after a poll that failed")
}
