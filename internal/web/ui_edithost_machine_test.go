package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// "Edit host" on the page, the machine itself (stage 1b): the two groups "Machine" and "Wake-on-LAN" above
// the links. The dialog's own functions are run in node, as in ui_edithost_test.go.

const ehHostFixture = `
  const host = (o = {}) => ({ key: "pc-b",
    name: { value: "PC-B", from: "config", by: null, at: null, config: "PC-B", ...(o.name || {}) },
    site: { value: "site-a", from: "config", default: "site-a", choices: ["site-a", "site-b"],
      plans: { "site-b": { line: "Site: site-b (was site-a)", notes: ["Viewers of site-b will see PC-B", "Viewers of site-a will no longer see PC-B"] } }, ...(o.site || {}) },
    wol: { on: true, macs: ["02:00:00:00:00:b1"], relay: "relay-a", relay_name: "Relay A", port: 9, from: "config", by: null, at: null,
      config: { macs: ["02:00:00:00:00:b1"], relay: "relay-a", relay_name: "Relay A", port: 9 }, target: null, password: false, relay_fixed: null, unused: null,
      relays: [{ id: "relay-a", name: "Relay A", online: true }, { id: "relay-b", name: "Relay <B>", online: false }], ...(o.wol || {}) } });
`

// What the groups show, what each field does to the draft, the lines and the request Save would make, and
// what is only shown: the address a wake packet goes to and that it has a password.
func TestUIEditHostMachineGroups(t *testing.T) {
	runEhPageScript(t, "eh-machine", ehHostFixture+`
  // an answer without "host" (an older panel): the links alone, as before
  let d = data({ ups: [ups("rack")] }), draft = ehDraft();
  let html = ehView(d, draft), text = words(html);
  if (text.includes("Machine") || text.includes("Wake-on-LAN") || html.includes("eh-name")) fail("groups without d.host: " + text);
  if (JSON.stringify(ehPlan(d, draft)) !== JSON.stringify({ lines: [], body: {}, notes: [], problems: [] })) fail("a plan without d.host: " + JSON.stringify(ehPlan(d, draft)));

  d = data({ host: host() }); draft = ehDraft();
  html = ehView(d, draft); text = words(html);
  for (const want of ["Machine Name from config.yaml", "Site from config.yaml", "site-a (default)", "Wake-on-LAN from config.yaml", "This machine can be woken", "MAC address", "Sent by", "Port"])
    if (!text.includes(want)) fail("the groups lack \"" + want + "\": " + text);
  if (html.indexOf("Machine") > html.indexOf(">UPS<") || html.indexOf("Wake-on-LAN") > html.indexOf(">UPS<")) fail("the groups are not above the links");
  if (!html.includes('id="eh-name" data-eh-f="name" maxlength="64" autocomplete="off" spellcheck="false" value="PC-B"')) fail("the name's field: " + html);
  if (!html.includes('<option value="" selected>site-a (default)</option><option value="site-b">site-b</option>')) fail("the site's choices: " + html);
  if (!html.includes('value="02:00:00:00:00:b1"') || !html.includes('<option value="relay-a" selected>Relay A</option><option value="relay-b">Relay &lt;B&gt; (offline)</option>')) fail("the Wake-on-LAN fields: " + html);
  if (html.includes("data-eh-reset") || text.includes("This will change") || text.includes("set in config.yaml") || ehCanSave(ehPlan(d, draft))) fail("nothing was changed: " + text);

  // the name: typed, with spaces around; the same name again is no change; an empty one is said, and nothing is sent
  ehField(d, draft, "name", "  Gaming   PC ");
  let plan = ehPlan(d, draft);
  if (JSON.stringify(plan) !== JSON.stringify({ lines: ["Name: Gaming PC (was PC-B)"], body: { name: "Gaming PC" }, notes: [], problems: [] })) fail("a new name: " + JSON.stringify(plan));
  if (!words(ehHostTag(d, draft, "name")).includes("changed here")) fail("the name's tag after typing");
  if (!ehView(d, draft).includes('value="  Gaming   PC "')) fail("what was typed is not in the field as it was typed");
  ehField(d, draft, "name", "PC-B");
  if (ehPlan(d, draft).lines.length || !words(ehHostTag(d, draft, "name")).includes("from config.yaml")) fail("the name it has: " + JSON.stringify(ehPlan(d, draft)));
  ehField(d, draft, "name", "   ");
  plan = ehPlan(d, draft);
  if (plan.lines.length || "name" in plan.body || plan.problems[0] !== "Type a name." || ehCanSave(plan) || !words(ehPlanView(plan)).includes("Type a name.")) fail("an empty name: " + JSON.stringify(plan));
  ehField(d, draft, "name", "<img src=x onerror=alert(1)>");
  html = ehView(d, draft);
  if (html.includes("<img") || !html.includes("&lt;img src=x onerror=alert(1)&gt;")) fail("a typed name is put into the page as markup");
  draft = ehDraft();

  // the site: the panel says what each choice moves; the choice it has is no change
  ehField(d, draft, "site", "site-b");
  plan = ehPlan(d, draft);
  if (JSON.stringify(plan) !== JSON.stringify({ lines: ["Site: site-b (was site-a)"], body: { site: "site-b" }, notes: ["Viewers of site-b will see PC-B", "Viewers of site-a will no longer see PC-B"], problems: [] })) fail("another site: " + JSON.stringify(plan));
  text = words(ehView(d, draft));
  if (!text.includes("Site changed here") || !text.includes("Viewers of site-a will no longer see PC-B")) fail("the dialog after another site: " + text);
  ehField(d, draft, "site", "");
  if (draft.site !== undefined || ehPlan(d, draft).lines.length) fail("the site it has is a change");
  ehField(d, draft, "site", "site-z");  // a choice the panel gave no plan for: nothing is sent
  if (ehPlan(d, draft).lines.length || "site" in ehPlan(d, draft).body) fail("a site without a plan is sent");
  draft = ehDraft();

  // Wake-on-LAN: the whole of what it shall be is sent when something of it changed
  ehField(d, draft, "wol-macs", "02-00-00-00-00-EE, 02:00:00:00:00:ef");
  ehField(d, draft, "wol-relay", "relay-b");
  ehField(d, draft, "wol-port", "7");
  plan = ehPlan(d, draft);
  if (JSON.stringify(plan) !== JSON.stringify({ lines: ["Wake-on-LAN: to 02:00:00:00:00:ee, 02:00:00:00:00:ef through Relay <B>, port 7 (was to 02:00:00:00:00:b1 through Relay A, port 9)"],
    body: { wol: { macs: ["02:00:00:00:00:ee", "02:00:00:00:00:ef"], port: 7, relay: "relay-b" } }, notes: [], problems: [] })) fail("MACs, relay and port: " + JSON.stringify(plan));
  html = ehView(d, draft);
  if (!html.includes('value="02-00-00-00-00-EE, 02:00:00:00:00:ef"') || !words(html).includes("Wake-on-LAN changed here")) fail("the fields after typing: " + html);
  if (html.includes("Relay <B>") || !html.includes("through Relay &lt;B&gt;, port 7")) fail("a relay's name is put into the page as markup");
  for (const [typed, problem] of [["02:00:00:00:00", "A MAC address looks like aa:bb:cc:dd:ee:ff."], ["10.0.0.255", "A MAC address looks like aa:bb:cc:dd:ee:ff."], ["", "A MAC address looks like aa:bb:cc:dd:ee:ff."],
    ["02:00:00:00:00:01 02:00:00:00:00:01", "One to eight MAC addresses, each once."]]) {
    ehField(d, draft, "wol-macs", typed);
    plan = ehPlan(d, draft);
    if (plan.problems[0] !== problem || "wol" in plan.body || plan.lines.length || ehCanSave(plan)) fail("the MAC \"" + typed + "\": " + JSON.stringify(plan));
  }
  draft = ehDraft();
  ehField(d, draft, "wol-on", false);
  plan = ehPlan(d, draft);
  if (JSON.stringify(plan.body) !== JSON.stringify({ wol: { off: true } }) || plan.lines[0] !== "Wake-on-LAN: off (was to 02:00:00:00:00:b1 through Relay A, port 9)") fail("off: " + JSON.stringify(plan));
  if (ehView(d, draft).includes("eh-macs")) fail("the fields of a Wake-on-LAN that is off are shown");
  ehField(d, draft, "wol-on", true);
  if (ehPlan(d, draft).lines.length) fail("off and on again is a change");

  // changed here before: Reset for the name and for the Wake-on-LAN, each by itself; what was typed goes with it
  d = data({ host: host({ name: { value: "Den", from: "panel", by: "admin" }, wol: { from: "panel", by: "admin", macs: ["02:00:00:00:00:ee"], port: 7 } }) }); draft = ehDraft();
  html = ehView(d, draft); text = words(html);
  if (!html.includes('data-eh-reset="name:"') || !html.includes('data-eh-reset="wol:"') || !text.includes("Changed here: used instead of config.yaml until Reset.")) fail("changed before: " + html);
  ehField(d, draft, "name", "Other");
  ehReset(d, draft, "name:"); ehReset(d, draft, "wol:");
  plan = ehPlan(d, draft);
  if (JSON.stringify(plan) !== JSON.stringify({ lines: ["Name: back to config.yaml: PC-B", "Wake-on-LAN: back to config.yaml: to 02:00:00:00:00:b1 through Relay A, port 9"],
    body: { reset: [{ field: "name" }, { field: "wol" }] }, notes: [], problems: [] })) fail("two resets: " + JSON.stringify(plan));
  html = ehView(d, draft);
  if (!html.includes('value="PC-B"') || html.includes('data-eh-reset="name:"') || words(html).includes("Changed here: used instead")) fail("the dialog after the resets: " + html);
  ehField(d, draft, "name", "Again");  // typing after a Reset: the Reset is off
  if (draft.reset["name:"] || ehPlan(d, draft).body.name !== "Again") fail("typing after a Reset");
  const plain = data({ host: host() }), pd = ehDraft();
  ehReset(plain, pd, "name:"); ehReset(plain, pd, "wol:");
  if (Object.keys(pd.reset).length) fail("a Reset of what was not changed here");

  // a machine without Wake-on-LAN: the tick, then the fields, with the first relay; without any relay the tick is off
  d = data({ host: host({ wol: { on: false, macs: [], relay: null, relay_name: null, config: null } }) }); draft = ehDraft();
  if (ehView(d, draft).includes("eh-macs")) fail("fields without Wake-on-LAN");
  ehField(d, draft, "wol-on", true);
  plan = ehPlan(d, draft);
  if (plan.problems[0] !== "A MAC address looks like aa:bb:cc:dd:ee:ff." || !ehView(d, draft).includes("eh-macs")) fail("ticked, no MAC yet: " + JSON.stringify(plan));
  ehField(d, draft, "wol-macs", "02:00:00:00:00:b9");
  plan = ehPlan(d, draft);
  if (JSON.stringify(plan.body) !== JSON.stringify({ wol: { macs: ["02:00:00:00:00:b9"], port: 9, relay: "relay-a" } }) || plan.lines[0] !== "Wake-on-LAN: to 02:00:00:00:00:b9 through Relay A, port 9 (was off)") fail("a first Wake-on-LAN: " + JSON.stringify(plan));
  d = data({ host: host({ wol: { on: false, macs: [], relay: null, relay_name: null, config: null, relays: [] } }) });
  html = ehView(d, ehDraft());
  if (!html.includes('data-eh-f="wol-on" disabled') || !words(html).includes("No machine is set up to send wake packets.")) fail("no relay at all: " + html);

  // switched off here before, ticked again: the fields start from what config.yaml has, and that is what is sent
  d = data({ host: host({ wol: { on: false, from: "panel", macs: [], relay: null, relay_name: null } }) }); draft = ehDraft();
  ehField(d, draft, "wol-on", true);
  plan = ehPlan(d, draft);
  if (JSON.stringify(plan.body) !== JSON.stringify({ wol: { macs: ["02:00:00:00:00:b1"], port: 9, relay: "relay-a" } }) || plan.lines[0] !== "Wake-on-LAN: to 02:00:00:00:00:b1 through Relay A, port 9 (was off)"
    || !ehView(d, draft).includes('value="02:00:00:00:00:b1"')) fail("ticked again after off: " + JSON.stringify(plan));

  // config.yaml sets where the packet goes, and a password: the relay is shown and cannot be chosen, the address
  // is shown, the password only as "with a password"; a change of the MAC sends no relay
  d = data({ host: host({ wol: { target: "10.20.30.255", password: true, relay_fixed: "config.yaml sets a target address and a password for this machine's wake packet: they belong to the relay written there" } }) }); draft = ehDraft();
  html = ehView(d, draft); text = words(html);
  if (!text.includes("Sent by Relay A to 10.20.30.255 with a password: set in config.yaml.") || !html.includes('data-eh-f="wol-relay" disabled title="config.yaml sets a target address and a password')
    || html.includes("relay-b") || html.includes("WOL_")) fail("a fixed relay: " + html);
  ehField(d, draft, "wol-macs", "02:00:00:00:00:c9");
  ehField(d, draft, "wol-relay", "relay-b");  // a page that asks all the same: not sent
  plan = ehPlan(d, draft);
  if (JSON.stringify(plan.body) !== JSON.stringify({ wol: { macs: ["02:00:00:00:00:c9"], port: 9 } }) || plan.lines[0] !== "Wake-on-LAN: to 02:00:00:00:00:c9 through Relay A, port 9 (was to 02:00:00:00:00:b1 through Relay A, port 9)") fail("a MAC next to a fixed relay: " + JSON.stringify(plan));
  // decisions the panel does not use are said
  d = data({ host: host({ wol: { from: "panel", unused: "the relay relay-z is no agent of this panel" } }) });
  if (!words(ehView(d, ehDraft())).includes("What was changed here is not used: the relay relay-z is no agent of this panel.")) fail("unused decisions are not said");

  // the request the dialog sends, with the links: one body
  d = data({ ups: [ups("rack")], host: host() }); draft = ehDraft();
  ehTick(d, draft, "ups", "nas-agent/rack", true); ehField(d, draft, "name", "Den"); ehField(d, draft, "site", "site-b");
  plan = ehPlan(d, draft);
  if (JSON.stringify(plan.body) !== JSON.stringify({ ups: [{ id: "nas-agent/rack", on: true }], name: "Den", site: "site-b" })
    || JSON.stringify(plan.lines) !== JSON.stringify(["Site: site-b (was site-a)", "Name: Den (was PC-B)", "UPS RACK UPS (read by NAS): powers PC-B"])) fail("links and the machine in one request: " + JSON.stringify(plan));
`)
}

// The lines the dialog lists before Save are the panel's own: for the same changes, on the panel's real
// answer, the page's list and what "Confirm it's you" shows (and the audit log gets) are the same sentences,
// and the request the page makes is taken as it is.
func TestUIEditHostMachineLinesAreThePanelsWords(t *testing.T) {
	r := hedSetup(t)
	step := func(id, fields string) {
		t.Helper()
		r.a.clk.advance(time.Duration(r.s.Cfg.StepUpWindowS+5) * time.Second) // the confirmation of the step before is over
		g := r.options(id)
		file := filepath.Join(t.TempDir(), "d.json")
		if err := os.WriteFile(file, g.raw, 0o600); err != nil {
			t.Fatal(err)
		}
		out := runEhPageScript(t, "eh-machine-words", `
  const d = JSON.parse(require("fs").readFileSync(process.argv[2], "utf8")), draft = ehDraft();
  for (const [field, v] of JSON.parse(process.argv[3])) { if (field === "reset") ehReset(d, draft, v); else ehField(d, draft, field, v); }
  console.log(JSON.stringify(ehPlan(d, draft)));
`, file, fields)
		var plan struct {
			Lines    []string       `json:"lines"`
			Body     map[string]any `json:"body"`
			Notes    []string       `json:"notes"`
			Problems []string       `json:"problems"`
		}
		if err := json.Unmarshal([]byte(out), &plan); err != nil || len(plan.Lines) == 0 || len(plan.Problems) != 0 {
			t.Fatalf("the page's plan for %s: %v\n%s", fields, err, out)
		}
		// what the panel would do with the page's request, in its own words: the items of "Confirm it's you"
		old := r.do("PUT", "/api/admin/hosts/"+id+"/options", plan.Body, "Content-Type", "application/json", "Cookie", r.a.sess, "X-Requested-With", "fairy")
		d, _ := old.body["detail"].(map[string]any)
		if !stepUpRequired(old) {
			t.Fatalf("%s on %s: the page's request %v: %d %s", fields, id, plan.Body, old.code, old.raw)
		}
		if got, want := strs(d["items"]), append(slices.Clone(plan.Lines), plan.Notes...); !slices.Equal(got, want) {
			t.Errorf("%s on %s:\nthe page lists %q\nthe panel says %q", fields, id, want, got)
		}
		if cb := r.a.confirm(pendingID(old), false); cb.code != 303 {
			t.Fatalf("the confirmation: %d", cb.code)
		}
		if res, _ := r.do("GET", "/api/me", nil, "Cookie", r.a.sess).body["pending_result"].(map[string]any); res["ok"] != true {
			t.Fatalf("%s on %s: the panel did not take the page's request: %v", fields, id, res)
		}
		_, params := r.lastChange(id)
		if got := strs(params["lines"]); !slices.Equal(got, plan.Lines) {
			t.Errorf("%s on %s, the audit entry:\nthe page lists %q\nthe panel wrote %q", fields, id, plan.Lines, got)
		}
	}
	step("pc-a", `[["name", " Gaming  PC "]]`)
	step("pc-a", `[["site", "site-b"]]`)
	step("pc-a", `[["wol-macs", "02-00-00-00-00-EE 02:00:00:00:00:ef"], ["wol-relay", "relay-b"], ["wol-port", "7"]]`)
	step("pc-a", `[["name", "Den"], ["site", ""], ["wol-on", false]]`)
	step("pc-a", `[["reset", "name:"], ["reset", "wol:"]]`)
	step("pc-b", `[["site", "site-a"], ["wol-on", true], ["wol-macs", "02:00:00:00:00:b1"]]`) // a first site and a first Wake-on-LAN: the first relay
	step("pc-b", `[["site", ""], ["wol-port", "7"], ["name", "Attic"]]`)
	step("box-c", `[["wol-macs", "02:00:00:00:00:c9"]]`) // next to a target address: no relay is sent
	step("box-d", `[["wol-on", false]]`)
	step("box-d", `[["wol-on", true]]`) // ticked again: what config.yaml has, and the panel's line says the same
	step("box-d", `[["wol-on", false]]`)
	step("box-d", `[["reset", "wol:"]]`)
	step("relay-a", `[["name", "Closet"], ["site", "site-b"]]`)
	if r.row("pc-a")["name"] != "PC-A" || r.row("pc-b")["name"] != "Attic" || r.row("relay-a")["name"] != "Closet" || r.s.Access.Load().Assign["relay-a"] != "site-b" {
		t.Errorf("after the steps: %v %v %v %v", r.row("pc-a")["name"], r.row("pc-b")["name"], r.row("relay-a")["name"], r.s.Access.Load().Assign)
	}
}
