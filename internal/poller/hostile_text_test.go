package poller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/textsafe"
)

// hostile: an ANSI colour sequence, a NUL, a line break, a direction override (U+202E), a one-character CSI
// (U+009B) and a sequence that sets the window title; hostileClean is what is left of it.
var hostile = "a\x1b[31m\x00b\r\n" + string(rune(0x202e)) + string(rune(0x9b)) + "2Jc\x1b]0;title\x07"

const hostileClean = "ab  c"

// dirtyTexts returns every text in v (through JSON, as the API sends it) that textsafe.Clean would change.
func dirtyTexts(t *testing.T, v any) (out []string) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var x any
	if err := json.Unmarshal(b, &x); err != nil {
		t.Fatal(err)
	}
	var walk func(any)
	walk = func(v any) {
		switch v := v.(type) {
		case string:
			if textsafe.Clean(v) != v {
				out = append(out, v)
			}
		case map[string]any:
			for k, e := range v {
				walk(k)
				walk(e)
			}
		case []any:
			for _, e := range v {
				walk(e)
			}
		}
	}
	walk(x)
	return out
}

// poison appends the hostile text to every text of v (and adds it as a key to every object).
func poison(v any) any {
	switch x := v.(type) {
	case string:
		return x + hostile
	case map[string]any:
		out := make(map[string]any, len(x)+1)
		for k, e := range x {
			out[k] = poison(e)
		}
		out["x"+hostile] = hostile
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = poison(e)
		}
		return out
	}
	return v
}

// N6, at the place where an answer enters the panel: a real agent and a real Proxmox API over HTTP whose every
// text carries control characters, escape sequences and a direction override. Nothing of it is in the
// snapshot, the power sources, the process list, an answer that is passed on (fan status) or an error text.
func TestHostileTextsAreCleanedWhereAnAnswerComesIn(t *testing.T) {
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "no" + hostile, "code": hostile})
		case strings.HasPrefix(r.URL.Path, "/processes"):
			_ = json.NewEncoder(w).Encode(poison(map[string]any{"sort": "cpu", "processes": []any{
				map[string]any{"pid": 1.0, "name": "init", "username": "root", "status": "running", "cmdline": "/sbin/init --x"}}}))
		case r.URL.Path == "/metrics":
			m := testutil.AgentMetrics()
			m["agent_version"] = "20-abcdef0"
			m["self_update"] = map[string]any{"state": "failed", "message": "download", "target": "21-abcdef0",
				"last": map[string]any{"ok": false, "id": "u1", "from": "19-abcdef0", "to": "20-abcdef0", "error": "sum"}}
			m["ups"] = []any{map[string]any{"name": "ups1", "state": "online", "model": "Back-UPS", "mfr": "APC", "status_raw": "OL"}}
			p := poison(m).(map[string]any)
			p["power_sources"].([]any)[0].(map[string]any)["id"] = "desk" // an id must be one, or the row is dropped
			p["ups"].([]any)[0].(map[string]any)["name"] = "ups1"
			_ = json.NewEncoder(w).Encode(p)
		default:
			_ = json.NewEncoder(w).Encode(poison(map[string]any{"hostname": "laptop", "mode": "auto", "fans": []any{map[string]any{"name": "fan1"}}}))
		}
	}))
	defer agent.Close()
	pve := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/status") {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"cpuinfo": map[string]any{"model": "Xeon" + hostile}}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
			map[string]any{"type": "node", "node": "pve1", "status": "online"},
			map[string]any{"type": "lxc", "vmid": 100, "node": "pve1" + hostile, "status": "running", "name": "ct" + hostile, "tags": "t1;" + hostile}}})
	}))
	defer pve.Close()
	cfg := testutil.Config(t, []string{pve.URL}, agent.URL, deadURL(t))
	p, err := New(cfg, config.Secrets{SessionSecret: "x"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for range 50 { // the node's CPU model arrives in the background
		p.PollOnce(ctx)
		if snap(p)["node-pve1"].CPUModel != nil {
			break
		}
	}
	all := snap(p)
	lap, ct, node := all["laptop"], all["lxc-100"], all["node-pve1"]
	if lap.Status != "online" || ct.Name != "ct"+hostileClean || node.CPUModel == nil || *node.CPUModel != "Xeon"+hostileClean {
		t.Fatalf("laptop %s (%v), guest %q, node CPU %v", lap.Status, lap.Error, ct.Name, node.CPUModel)
	}
	if lap.Disks[0].Mount != "/"+hostileClean || *lap.OS != "Ubuntu 24.04.1 LTS"+hostileClean || lap.Temps[0].Sensor != "coretemp"+hostileClean {
		t.Fatalf("%+v", lap)
	}
	// the name of the host is the configured one or the agent's own; either way it is clean
	what := map[string]any{"systems": p.Snapshot(), "summary": p.Summary(), "power": p.Power.Overview(), "ups": p.UPS.Overview(true)}
	if src := p.Power.Overview().Sources; len(src) != 1 || src[0].Name != "Desk plug"+hostileClean {
		t.Fatalf("power sources: %+v", src)
	}
	if u := p.UPS.Overview(true).UPS; len(u) != 1 || u[0].Model == nil || *u[0].Model != "Back-UPS"+hostileClean {
		t.Fatalf("UPS: %+v", u)
	}
	info, _ := p.AgentInfo("laptop")
	what["agent"] = info
	if info.Version != "" || fmt.Sprint(info.SelfUpdate["message"]) != "download"+hostileClean ||
		info.SelfUpdate["state"] != "unknown" || info.SelfUpdate["target"] != nil {
		t.Fatalf("agent info: %+v", info) // a version or a state with such a text in it is not one
	}
	pl, err := p.FetchProcesses(ctx, cfg.Agents[0], "laptop", 25, "cpu")
	if err != nil || len(pl.Processes) != 1 || *pl.Processes[0].Name != "init"+hostileClean || !strings.HasPrefix(*pl.Processes[0].Cmdline, "/sbin/init --x"+hostileClean) {
		t.Fatalf("%+v %v", pl, err)
	}
	what["processes"] = pl
	_, fan, err := p.AgentRequest(ctx, cfg.Agents[0], "GET", "/fan/status", nil) // passed on to the page as it is
	if err != nil || fan["hostname"] != "laptop"+hostileClean || fan["x"+hostileClean] != hostileClean {
		t.Fatalf("%v %v", fan, err)
	}
	what["fan status"] = fan
	status, refused, err := p.AgentRequest(ctx, cfg.Agents[0], "POST", "/power", jsonx.M{"action": "reboot"})
	if err != nil || status != 400 || refused["error"] != "no"+hostileClean {
		t.Fatalf("%d %v %v", status, refused, err)
	}
	what["refusal"] = refused
	for k, v := range what {
		if d := dirtyTexts(t, v); len(d) > 0 {
			t.Errorf("%s: texts kept as sent: %q", k, d)
		}
	}
}
