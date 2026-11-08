package power

import (
	"bufio"
	"context"
	"encoding/json"
	"encoding/pem"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy/internal/devices"
	"github.com/n3ptune-cpu/Fairy/internal/model"
)

func manager(t *testing.T, srcs []map[string]any, ap AgentPowerFn, clock func() float64) *Manager {
	t.Helper()
	if clock == nil {
		clock = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }
	}
	m, err := NewManager(srcs, 5, time.Second, 60, 3600, ap, clock)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func jsonHandler(v any) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode(v) }
}

func byID(ov model.PowerOverview) map[string]model.PowerSourceState {
	out := map[string]model.PowerSourceState{}
	for _, s := range ov.Sources {
		out[s.ID] = s
	}
	return out
}

func TestShellyGen2AndGen1Fallback(t *testing.T) {
	gen2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rpc/Switch.GetStatus" || r.URL.Query().Get("id") != "0" {
			http.NotFound(w, r)
			return
		}
		jsonHandler(map[string]any{"id": 0, "apower": 42.5, "voltage": 231.2, "current": 0.21, "aenergy": map[string]any{"total": 12345.6}})(w, r)
	}))
	defer gen2.Close()
	gen1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/status" {
			jsonHandler(map[string]any{"meters": []any{map[string]any{"power": 17.0, "total": 600000}}})(w, r)
			return
		}
		http.NotFound(w, r)
	}))
	defer gen1.Close()
	pm := manager(t, []map[string]any{{"id": "p2", "type": "shelly", "url": gen2.URL, "system": "cyrene"},
		{"id": "p1", "type": "shelly", "url": gen1.URL, "include_in_total": false}}, nil, nil)
	pm.PollAll(context.Background())
	ov := byID(pm.Overview())
	r2, r1 := ov["p2"].Readings[0], ov["p1"].Readings[0]
	if *r2.Watts != 42.5 || *r2.Voltage != 231.2 || *r2.Current != 0.21 || math.Abs(*r2.EnergyKWhTotal-12.3456) > 1e-9 || *r2.SystemID != "cyrene" {
		t.Fatalf("%+v", r2)
	}
	if *r1.Watts != 17 || *r1.EnergyKWhTotal != 10 { // watt-minutes -> kWh
		t.Fatalf("%+v", r1)
	}
	if *pm.Overview().TotalW != 42.5 || pm.LinkedWatts()["cyrene"] != 42.5 { // p1 excluded from total
		t.Fatal("total / linked")
	}
	h := pm.HistoryView("")
	if len(h.Points) != 1 || *h.Points[0].Sources["p1"] != 17 {
		t.Fatal("history")
	}
}

func TestShellyGen2DigestAuth(t *testing.T) {
	t.Setenv("SHELLY_PW", "s3cret")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a := r.Header.Get("Authorization")
		if !strings.HasPrefix(a, "Digest ") || !strings.Contains(a, `username="admin"`) || !strings.Contains(a, "algorithm=SHA-256") {
			w.Header().Set("WWW-Authenticate", `Digest qop="auth", realm="shellyplus1pm-abc", nonce="60dc59c6", algorithm=SHA-256`)
			w.WriteHeader(401)
			return
		}
		jsonHandler(map[string]any{"apower": 5.0})(w, r)
	}))
	defer srv.Close()
	pm := manager(t, []map[string]any{{"id": "p", "type": "shelly", "url": srv.URL, "gen": "2", "password_env": "SHELLY_PW"}}, nil, nil)
	pm.PollAll(context.Background())
	if st := pm.Overview().Sources[0]; st.Status != "ok" || *st.Readings[0].Watts != 5 {
		t.Fatalf("%+v", st)
	}
}

func TestShellyGen1Emeter(t *testing.T) {
	r, err := devices.ParseShellyGen1(map[string]any{"emeters": []any{map[string]any{"power": 100.0, "voltage": 230.0, "current": 0.5, "total": 2500.0}}}, 0)
	if err != nil || *r.Watts != 100 || *r.Voltage != 230 || *r.Current != 0.5 || *r.EnergyKWh != 2.5 {
		t.Fatal(r, err)
	}
}

func TestTasmotaWithPassword(t *testing.T) {
	t.Setenv("TASMOTA_PW", "hunter2")
	var q map[string][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q = r.URL.Query()
		jsonHandler(map[string]any{"StatusSNS": map[string]any{"ENERGY": map[string]any{"Total": 321.5, "Power": []any{10, 15}, "Voltage": 229, "Current": []any{0.05, 0.07}}}})(w, r)
	}))
	defer srv.Close()
	pm := manager(t, []map[string]any{{"id": "t", "type": "tasmota", "url": srv.URL, "password_env": "TASMOTA_PW"}}, nil, nil)
	pm.PollAll(context.Background())
	r := pm.Overview().Sources[0].Readings[0]
	if *r.Watts != 25 || *r.Voltage != 229 || *r.EnergyKWhTotal != 321.5 || math.Abs(*r.Current-0.12) > 1e-9 {
		t.Fatalf("%+v", r)
	}
	if q["cmnd"][0] != "Status 8" || q["user"][0] != "admin" || q["password"][0] != "hunter2" {
		t.Fatal(q)
	}
}

func TestRedfishParseAndBackoffWhenUnreachable(t *testing.T) {
	t.Setenv("IDRAC_PW", "calvin")
	var auth string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		if r.URL.Path != "/redfish/v1/Chassis/System.Embedded.1/Power" {
			http.NotFound(w, r)
			return
		}
		jsonHandler(map[string]any{"PowerControl": []any{map[string]any{"PowerConsumedWatts": 187}}, "PowerSupplies": []any{map[string]any{"LineInputVoltage": 232}}})(w, r)
	}))
	now := 1000.0
	pm := manager(t, []map[string]any{{"id": "bmc", "type": "redfish", "url": srv.URL, "password_env": "IDRAC_PW", "verify_tls": false}}, nil, func() float64 { return now })
	pm.PollAll(context.Background())
	st := pm.Overview().Sources[0]
	if st.Status != "ok" || *st.Readings[0].Watts != 187 || *st.Readings[0].Voltage != 232 || !strings.HasPrefix(auth, "Basic ") {
		t.Fatalf("%+v %q", st, auth)
	}
	// BMC not cabled: connection errors -> exponential backoff from 10 s, capped at 300 s, never raises
	srv.Close()
	r := pm.Runners[0]
	r.lastSuccess = nil
	var delays []float64
	for i := 0; i < 8; i++ {
		r.nextAttempt = 0
		pm.PollAll(context.Background())
		delays = append(delays, r.nextAttempt-now)
	}
	want := []float64{10, 20, 40, 80, 160, 300, 300, 300}
	for i := range want {
		if delays[i] != want[i] {
			t.Fatalf("delays %v", delays)
		}
	}
	st = pm.Overview().Sources[0]
	if st.Status != "unreachable" || *st.Error != "connection failed" || len(st.Readings) != 0 || *st.NextAttemptInS != 300 {
		t.Fatalf("%+v", st)
	}
}

// A BMC with its own CA (ca_file) is verified, and polls reuse one connection instead of opening a new TLS
// connection (and BMC session) every time.
func TestRedfishVerifiesWithCAFileAndReusesConnections(t *testing.T) {
	t.Setenv("IDRAC_PW", "calvin")
	var conns atomic.Int32
	srv := httptest.NewUnstartedServer(jsonHandler(map[string]any{"PowerControl": []any{map[string]any{"PowerConsumedWatts": 90}}}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.StartTLS()
	defer srv.Close()
	ca := filepath.Join(t.TempDir(), "bmc-ca.pem")
	_ = os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600)
	now := 1000.0
	pm := manager(t, []map[string]any{{"id": "bmc", "type": "redfish", "url": srv.URL, "password_env": "IDRAC_PW", "ca_file": ca}}, nil, func() float64 { return now })
	for i := 0; i < 3; i++ {
		pm.Runners[0].nextAttempt = 0
		pm.PollAll(context.Background())
		if st := pm.Overview().Sources[0]; st.Status != "ok" {
			t.Fatalf("poll %d: %+v", i, st)
		}
	}
	if n := conns.Load(); n != 1 {
		t.Fatal("TLS connections opened for 3 polls:", n)
	}
}

func TestNUTPowerSourceWithFakeUpsd(t *testing.T) {
	t.Setenv("NUT_PW", "secret")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan string, 10)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			got <- strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(line, "USERNAME"), strings.HasPrefix(line, "PASSWORD"):
				c.Write([]byte("OK\n"))
			case strings.HasPrefix(line, "LIST VAR ups"):
				c.Write([]byte("BEGIN LIST VAR ups\nVAR ups ups.load \"23\"\nVAR ups ups.realpower.nominal \"900\"\n" +
					"VAR ups input.voltage \"231.0\"\nVAR ups ups.status \"OL\"\nVAR ups device.model \"Back-UPS \\\"XS\\\"\"\nEND LIST VAR ups\n"))
			case strings.HasPrefix(line, "LOGOUT"):
				return
			}
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	pm := manager(t, []map[string]any{{"id": "u", "type": "nut", "host": "127.0.0.1", "port": port, "username": "monuser", "password_env": "NUT_PW"}}, nil, nil)
	pm.PollAll(context.Background())
	st := pm.Overview().Sources[0]
	if st.Status != "ok" || *st.Readings[0].Watts != 207 || *st.Readings[0].Voltage != 231 { // 23 % of 900 W
		t.Fatalf("%+v", st)
	}
	if a, b, c := <-got, <-got, <-got; a != "USERNAME monuser" || b != "PASSWORD secret" || c != "LIST VAR ups" {
		t.Fatal(a, b, c)
	}
	w, _, _ := devices.NUTWatts(map[string]string{"ups.realpower": "150"})
	if *w != 150 {
		t.Fatal(*w)
	}
}

func TestTuyaDPSParsingAndCustomMapping(t *testing.T) {
	r := devices.ParseTuyaDPS(map[string]any{"1": true, "17": 3.0, "18": 412.0, "19": 935.0, "20": 2301.0}, nil)
	if *r.Watts != 93.5 || *r.Voltage != 230.1 || *r.Current != 0.412 || r.EnergyKWh != nil {
		t.Fatalf("%+v", r)
	}
	custom := map[string]devices.DpsField{"power": {DP: "104", Scale: 1}, "voltage": {DP: "106", Scale: 0.1}, "energy_kwh_total": {DP: "101", Scale: 0.01}}
	r = devices.ParseTuyaDPS(map[string]any{"104": 55.0, "106": 2299.0, "101": 12345.0}, custom)
	if *r.Watts != 55 || *r.Voltage != 229.9 || r.Current != nil || *r.EnergyKWh != 123.45 {
		t.Fatalf("%+v", r)
	}
}

func TestAgentPowerSourceAndUnknownType(t *testing.T) {
	cp := &model.ComponentPower{CPUPackageW: model.P(33.3)}
	pm := manager(t, []map[string]any{{"id": "cpu", "type": "agent", "agent": "laptop", "metric": "cpu_package_w"},
		{"id": "bmc", "type": "agent", "agent": "laptop", "metric": "bmc_w"}}, func(string) *model.ComponentPower { return cp }, nil)
	pm.PollAll(context.Background())
	ov := byID(pm.Overview())
	if *ov["cpu"].Readings[0].Watts != 33.3 || ov["cpu"].IncludeInTotal {
		t.Fatal("agent cpu")
	}
	if ov["bmc"].Status != "unreachable" || !strings.Contains(*ov["bmc"].Error, "no bmc_w") {
		t.Fatalf("%+v", ov["bmc"])
	}
	if _, err := NewManager([]map[string]any{{"id": "x", "type": "zigbee"}}, 5, time.Second, 60, 60, nil, nil); err == nil || !strings.Contains(err.Error(), "unknown type") {
		t.Fatal(err)
	}
	if err := ValidateConfigs([]map[string]any{{"id": "x", "type": "shelly", "url": "http://a", "bogus": 1}}); err == nil {
		t.Fatal("unknown keys must be rejected")
	}
	if err := ValidateConfigs([]map[string]any{{"id": "x", "type": "tuya", "device_id": "d", "ip": "1.2.3.4", "local_key_env": "K", "version": "3.6"}}); err == nil {
		t.Fatal("bad tuya version accepted")
	}
}

func TestAgentReportedDevicesMerge(t *testing.T) {
	pm := manager(t, nil, nil, nil)
	rows := []map[string]any{{"id": "desk", "name": "Desk plug", "type": "tuya", "ok": true, "watts": 80.0, "voltage": 230.0, "system_hint": "self"},
		{"id": "tv", "type": "shelly", "ok": false, "error": "timeout"}}
	pm.UpdateRemote("laptop", rows, false, nil, []string{"tv"})
	ov := byID(pm.Overview())
	d := ov["laptop/desk"]
	if *d.Via != "laptop" || d.Status != "ok" || *d.Readings[0].Watts != 80 || *d.Readings[0].SystemID != "laptop" || *pm.Overview().TotalW != 80 {
		t.Fatalf("%+v", d)
	}
	if ov["laptop/tv"].Status != "unreachable" || ov["laptop/tv"].IncludeInTotal {
		t.Fatal("tv")
	}
	pm.UpdateRemote("laptop", nil, true, nil, nil) // agent disappears -> stale, not ok
	if byID(pm.Overview())["laptop/desk"].Status != "stale" {
		t.Fatal("stale")
	}
	pm.UpdateRemote("laptop", rows[:1], false, nil, nil) // device removed from the agent config -> gone
	if _, ok := byID(pm.Overview())["laptop/tv"]; ok {
		t.Fatal("removed device kept")
	}
}
