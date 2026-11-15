package web

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// The rig of the plug tests: a panel in front of three agents that read plugs with sockets, a PC that hangs on
// some of the sockets, and the machine the panel runs on. The agents are testutil.Machine: what the panel
// sends them goes through Fairy-agent's real gate and the agent's own rules for a socket (MachinePlugs), and
// the passkey is a token of the tests' issuer. All names are made up.
//
//	reader    reads desk-bar (8 sockets), pc-plug (1) and own-plug (1); runs on the Proxmox node PVE
//	          (lab-node-pve); its Home Assistant is the VM lab-qemu-100 on that node
//	other     reads rack-bar (2); its Home Assistant is the same VM
//	third     reads spare-bar (2); its home_assistant_system names a row that is not there
//	bare      reads bare-bar (1); the panel has no control token for it
//	desk-pc   a PC: on desk-bar/ch3 (socket_links) and on pc-plug (power_links)
//	panel-box the machine the panel runs on (panel_hosts): on desk-bar/ch6
type plugEnv struct {
	*approvalEnv
	reader, other, third *testutil.Machine
	mu                   sync.Mutex
	pc                   string             // what the PC's agent answers: "online", "down"
	rows                 map[string]string  // the socket rows the agents report, changed by a test: "<agent>/<plug>/<socket>" -> member=value
	watts                map[string]float64 // what a plug measures, changed by a test: plug id -> watts (40 without a line)
}

const plugConfig = `
public_url: https://panel.example.com
request_timeout: 1
offline_after: 60
audit_log: %q
data_dir: %q
oidc: {issuer: "https://id.example.com", client_id: cid, allowed_groups: [viewers], control_groups: [controllers]}
proxmox:
  - {id: lab, id_prefix: lab-, cluster_name: PVE, standalone: true, urls: ["https://pve:8006"],
     token_id: "fairy@pve!panel", token_secret_env: PVE_SECRET, verify_tls: false}
agents:
  - {id: reader, name: READER, url: "http://reader", token_env: TOK_A, control_token_env: CTL_A, node: PVE, site: Home,
     power_links: {pc-plug: desk-pc},
     socket_links: {desk-bar/ch3: desk-pc, desk-bar/ch4: lab-node-pve, desk-bar/ch5: lab-qemu-100, desk-bar/ch6: panel-box, desk-bar/ch7: old-pc},
     home_assistant_system: lab-qemu-100}
  - {id: other, name: OTHER, url: "http://other", token_env: TOK_B, control_token_env: CTL_A,
     socket_links: {rack-bar/node: lab-node-pve}, home_assistant_system: lab-qemu-100}
  - {id: third, name: THIRD, url: "http://third", token_env: TOK_C, control_token_env: CTL_A,
     socket_links: {spare-bar/pc: desk-pc}, home_assistant_system: lab-qemu-999}
  - {id: bare, name: BARE, url: "http://bare", token_env: TOK_D}
  - {id: desk-pc, name: DESK-PC, url: "http://desk-pc", token_env: TOK_E, site: Home}
  - {id: panel-box, name: PANEL-BOX, url: "http://panel-box", token_env: TOK_F}
panel_hosts: [panel-box]
`

// plugTag is the tag of a socket of the rig, as its agent makes it.
func plugTag(plug, socket string) string {
	return api.SocketTag(plug, socket, "name of "+socket, "switch.example_"+strings.ReplaceAll(plug, "-", "_")+"_"+socket)
}

func newPlugEnv(t *testing.T) *plugEnv { return newPlugEnvWith(t, plugConfig) }

// newPlugEnvWith is the rig with another config.yaml for the panel (plugConfig with a line changed).
func newPlugEnvWith(t *testing.T, yaml string) *plugEnv {
	t.Helper()
	for i, env := range []string{"TOK_A", "TOK_B", "TOK_C", "TOK_D", "TOK_E", "TOK_F"} {
		t.Setenv(env, testutil.ReadToken+string(rune('a'+i)))
	}
	t.Setenv("CTL_A", testutil.ControlToken)
	t.Setenv("PVE_SECRET", "pve-secret-uuid")
	dir := t.TempDir()
	cfg, err := config.Parse([]byte(fmt.Sprintf(yaml, dir+"/audit.log", dir+"/data")))
	if err != nil {
		t.Fatal(err)
	}
	pe := &plugEnv{approvalEnv: &approvalEnv{}, pc: "online", rows: map[string]string{}, watts: map[string]float64{}}
	a := pe.approvalEnv
	b := testutil.NewBackend()
	b.PVEOf = func(string) ([]jsonx.M, error) { return testutil.HostResources("PVE", "running"), nil }
	plain := func(host string) func() (jsonx.M, error) {
		return func() (jsonx.M, error) {
			m := testutil.AgentMetrics()
			m["hostname"] = host
			delete(m, "power_sources")
			return m, nil
		}
	}
	b.Metrics = map[string]func() (jsonx.M, error){"panel-box": plain("panel-box")}
	b.Metrics["desk-pc"] = func() (jsonx.M, error) {
		pe.mu.Lock()
		defer pe.mu.Unlock()
		if pe.pc != "online" {
			return nil, devices.Errf("timeout")
		}
		return plain("desk-pc")()
	}
	sock := func(agent, plug, id, name, state string, why string) jsonx.M {
		row := jsonx.M{"id": id, "name": name, "state": state, "switchable": why == "", "why": nil, "tag": plugTag(plug, id), "error": nil}
		if why != "" {
			row["why"] = why
		}
		pe.mu.Lock()
		defer pe.mu.Unlock()
		for k, v := range pe.rows { // a test's change of one member of one row
			if key, member, _ := strings.Cut(k, " "); key == agent+"/"+plug+"/"+id {
				switch member {
				case "tag", "state", "name":
					row[member] = v
				case "why":
					row["why"], row["switchable"] = v, false
				case "gone":
					return nil
				}
			}
		}
		return row
	}
	plug := func(id, name string, socks ...jsonx.M) jsonx.M {
		list := []any{}
		for _, s := range socks {
			if s != nil {
				list = append(list, s)
			}
		}
		pe.mu.Lock()
		defer pe.mu.Unlock()
		watts, set := pe.watts[id]
		if !set {
			watts = 40
		}
		return jsonx.M{"id": id, "name": name, "type": "homeassistant", "ok": true, "watts": watts, "sockets": list}
	}
	b.Metrics["reader"] = func() (jsonx.M, error) {
		m, _ := plain("reader")()
		own := plug("own-plug", "Reader's plug", sock("reader", "own-plug", "main", "This machine", "on", ""))
		own["system_hint"] = "self"
		m["power_sources"] = []any{
			plug("desk-bar", "Monitor powerbar",
				sock("reader", "desk-bar", "ch1", "Left monitor", "on", ""), sock("reader", "desk-bar", "ch2", "Lamp", "off", ""),
				sock("reader", "desk-bar", "ch3", "Desk PC", "on", ""), sock("reader", "desk-bar", "ch4", "Server", "on", ""),
				sock("reader", "desk-bar", "ch5", "Home Assistant box", "on", ""), sock("reader", "desk-bar", "ch6", "Panel box", "on", ""),
				sock("reader", "desk-bar", "ch7", "Old PC", "on", ""), sock("reader", "desk-bar", "ch8", "Printer", "on", api.WhyNotAllowed)),
			plug("pc-plug", "PC plug", sock("reader", "pc-plug", "main", "PC", "on", "")), own}
		return m, nil
	}
	b.Metrics["other"] = func() (jsonx.M, error) {
		m, _ := plain("other")()
		m["power_sources"] = []any{plug("rack-bar", "Rack bar", sock("other", "rack-bar", "node", "Node", "on", ""), sock("other", "rack-bar", "fan", "Fan", "off", ""))}
		return m, nil
	}
	b.Metrics["third"] = func() (jsonx.M, error) {
		m, _ := plain("third")()
		m["power_sources"] = []any{plug("spare-bar", "Spare bar", sock("third", "spare-bar", "pc", "PC", "on", ""), sock("third", "spare-bar", "lamp", "Lamp", "off", ""))}
		return m, nil
	}
	b.Metrics["bare"] = func() (jsonx.M, error) {
		m, _ := plain("bare")()
		m["power_sources"] = []any{plug("bare-bar", "Bare bar", sock("bare", "bare-bar", "lamp", "Lamp", "off", ""))}
		return m, nil
	}
	a.gb = testutil.NewGatedBackend(b)
	a.gb.WaitOf = poller.AgentWait
	p, err := poller.New(cfg, config.Secrets{SessionSecret: strings.Repeat("s", 48)}, a.gb)
	if err != nil {
		t.Fatal(err)
	}
	p.PollOnce(context.Background())
	p.PollOnce(context.Background()) // the node's row and its agent in one list
	s := New(cfg, config.Secrets{SessionSecret: strings.Repeat("s", 48)}, p, nil, "test")
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	a.env = &env{t: t, s: s, srv: srv, b: b}
	a.iss = testutil.NewIssuer(t)
	s.Cfg.Approval = config.Approval{ClientID: testApprovalClient, Issuer: a.iss.URL(), RunApprovalWaitS: 900}
	s.SetIDPClient(a.iss.Client())
	s.control, s.approvalRL = newRateLimiter(1000), newRateLimiter(1000)
	pe.reader, pe.other, pe.third = a.gb.Add(t, "reader"), a.gb.Add(t, "other"), a.gb.Add(t, "third")
	a.gb.Add(t, "bare")
	machineSockets := func(plug string, ids ...string) map[string]*testutil.MachineSocket {
		out := map[string]*testutil.MachineSocket{}
		for _, id := range ids {
			out[plug+"/"+id] = &testutil.MachineSocket{Tag: plugTag(plug, id), State: "on"}
		}
		return out
	}
	pe.reader.Plugs = &testutil.MachinePlugs{Sockets: machineSockets("desk-bar", "ch1", "ch2", "ch3", "ch4", "ch5", "ch6", "ch7", "ch8")}
	pe.reader.Plugs.Sockets["desk-bar/ch2"].State = "off"
	pe.reader.Plugs.Sockets["desk-bar/ch8"].Refuse = api.CodeNotAllowed
	pe.reader.Plugs.Sockets["pc-plug/main"] = &testutil.MachineSocket{Tag: plugTag("pc-plug", "main"), State: "on"}
	pe.reader.Plugs.Sockets["own-plug/main"] = &testutil.MachineSocket{Tag: plugTag("own-plug", "main"), State: "on"}
	pe.other.Plugs = &testutil.MachinePlugs{Sockets: machineSockets("rack-bar", "node", "fan")}
	pe.third.Plugs = &testutil.MachinePlugs{Sockets: machineSockets("spare-bar", "pc", "lamp")}
	a.admin = a.cookie(controller(), time.Hour)
	a.csrf = []string{"Cookie", a.admin, "X-Requested-With", "fairy", "Content-Type", "application/json"}
	a.require(t, pe.reader, pe.other, pe.third) // the three machines have the passkey check on, and their ids are on record
	return pe
}

// set changes what an agent reports about one socket from the next poll on ("tag", "state", "name", "why", or
// "gone" for a socket that is no longer reported), and polls.
func (pe *plugEnv) set(socket, member, value string) {
	pe.mu.Lock()
	pe.rows[socket+" "+member] = value
	pe.mu.Unlock()
	pe.poll()
}

// measures changes what a plug measures from the next poll on, and polls.
func (pe *plugEnv) measures(plug string, watts float64) {
	pe.mu.Lock()
	pe.watts[plug] = watts
	pe.mu.Unlock()
	pe.poll()
}

// with is plugConfig with one piece of it replaced.
func plugConfigWith(t *testing.T, old, new string) string {
	t.Helper()
	if strings.Count(plugConfig, old) != 1 {
		t.Fatalf("the rig's config has %q %d times", old, strings.Count(plugConfig, old))
	}
	return strings.Replace(plugConfig, old, new, 1)
}

// pcIs makes the PC answer its polls ("online") or not ("down"), and polls.
func (pe *plugEnv) pcIs(state string) {
	pe.mu.Lock()
	pe.pc = state
	pe.mu.Unlock()
	pe.poll()
}

// pcOff makes the PC a machine that is known to be off: it stops answering, and the panel is told that it went
// off (as a run of "Update unpowered hosts" tells it: Poller.MarkOff).
func (pe *plugEnv) pcOff() {
	pe.t.Helper()
	pe.pcIs("down")
	pe.s.Poller.MarkOff("desk-pc")
	if x := pe.do("GET", "/api/systems/desk-pc", nil, "Cookie", pe.admin); x.body["status"] != "offline" {
		pe.t.Fatalf("the fixture: the PC is %v, want offline", x.body["status"])
	}
	if !pe.s.Poller.KnownOff("desk-pc") {
		pe.t.Fatal("the fixture: the PC is not marked as seen to go off")
	}
}

// pcSilent makes the PC a machine that has not answered for longer than offline_after, with nothing that says
// it is off: its row is "offline" (the panel's clock is put ten minutes ahead, then the PC stops answering).
func (pe *plugEnv) pcSilent() {
	pe.t.Helper()
	pe.s.Poller.Clock = func() time.Time { return time.Now().Add(10 * time.Minute) }
	pe.pcIs("down")
	if x := pe.do("GET", "/api/systems/desk-pc", nil, "Cookie", pe.admin); x.body["status"] != "offline" {
		pe.t.Fatalf("the fixture: the PC is %v, want offline", x.body["status"])
	}
	if pe.s.Poller.KnownOff("desk-pc") {
		pe.t.Fatal("the fixture: the PC is marked as seen to go off")
	}
}

// body is a switch request as the page sends it: the source by its id in the panel, the socket, the
// direction, and the tag the page was given.
func plugBody(source, socket, to string) map[string]any {
	_, plug, _ := strings.Cut(source, "/")
	return map[string]any{"source": source, "socket": socket, "to": to, "tag": plugTag(plug, socket)}
}

func (pe *plugEnv) flip(source, socket, to string) resp {
	return pe.post("/api/power/socket", plugBody(source, socket, to))
}

// socketOf is one socket as GET /api/power gives it to the holder of cookie.
func (pe *plugEnv) socketOf(cookie, source, socket string) map[string]any {
	pe.t.Helper()
	r := pe.do("GET", "/api/power", nil, "Cookie", cookie)
	if r.code != 200 {
		pe.t.Fatalf("GET /api/power: %d %s", r.code, r.raw)
	}
	srcs, _ := r.body["sources"].([]any)
	for _, s := range srcs {
		src, _ := s.(map[string]any)
		if src["id"] != source {
			continue
		}
		socks, _ := src["sockets"].([]any)
		for _, k := range socks {
			if m, _ := k.(map[string]any); m["id"] == socket {
				return m
			}
		}
	}
	return nil
}

// plugSent is the switch requests the machine got, in order.
func plugSent(m *testutil.Machine) []testutil.Sent {
	var out []testutil.Sent
	for _, s := range m.Asked() {
		if s.Path == api.PathPlugSwitch {
			out = append(out, s)
		}
	}
	return out
}

// plugAudits are the plug.switch entries of the audit log, newest first.
func (pe *plugEnv) plugAudits() []map[string]any { return pe.auditEntries("", plugAudit) }

// detail is the "detail" of a refusal: its sentence, and its code when it has one.
func plugDetailOf(r resp) (msg, code string) {
	switch d := r.body["detail"].(type) {
	case string:
		return d, ""
	case map[string]any:
		msg, _ = d["message"].(string)
		code, _ = d["code"].(string)
	}
	return msg, code
}

// viewerOf signs in a viewer of the site Home and returns the cookie.
func (pe *plugEnv) viewerOf(site string) string {
	pe.t.Helper()
	_ = pe.s.Access.EnsureSites([]string{site})
	v := model.User{Sub: "u7", Name: model.P("Vee"), Email: model.P("v@example.com"), Groups: []string{"viewers"}}
	ck := pe.cookie(v, 0)
	if _, _, err := pe.s.Access.SetRole("u7", "viewer", []string{site}, "test", false); err != nil {
		pe.t.Fatal(err)
	}
	return ck
}
