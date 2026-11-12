package web

import (
	"fmt"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/approval"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/audit"
)

// A machine id is for an admin in a browser: the approval page gives an API token none. The audit log did
// (audit 6, r4-p3-03): the description of a confirmed accept, written with the confirmation, held the first
// eight and the last six characters of the id that was accepted and of the one on record, and a control-scope
// token of an admin read them from GET /api/audit. They are exactly what a look-alike id needs (r4-p3-02).
//
// Now the description of an accept has no characters of an id, and a token reads the entries about approvals
// without the six characters they keep for an admin's session.
func TestTheAuditLogGivesATokenNoMachineIDCharacters(t *testing.T) {
	x := newAcceptAdmin(t)
	x.require(t, x.laptop)
	idL := machineIDOf("laptop")
	x.setTrustID(t, x.laptop, otherID)
	x.poll()
	x.poll() // the alarm: its entry names both ids by their first six
	r := x.press("laptop", map[string]any{"machine_id": otherID})
	if res := x.confirm(t, r); res["ok"] != true {
		t.Fatalf("the accept: %v", res)
	}
	// an entry as a build before this fix wrote it
	const old = "accept what gone shows about its own check of approvals now (it shows the machine id GONEmach…000004; on record is GONEmach…000004)"
	x.s.writeAudit(audit.Entry{Actor: "nora", Action: "auth.step_up", Target: "/api/admin/approval/accept/gone", Source: "session", Params: map[string]any{"runs": old}, Outcome: "ok"})

	read := func(hdr ...string) (string, []map[string]any) {
		t.Helper()
		r := x.do("GET", "/api/audit?limit=200", nil, hdr...)
		if r.code != 200 {
			t.Fatalf("the audit log: %d %s", r.code, r.raw)
		}
		var out []map[string]any
		for _, e := range r.body["entries"].([]any) {
			out = append(out, e.(map[string]any))
		}
		return string(r.raw), out
	}
	params := func(entries []map[string]any, action string) map[string]any {
		for _, e := range entries {
			if e["action"] == action && (e["outcome"] == "ok" || strings.HasPrefix(fmt.Sprint(e["outcome"]), "alarm")) {
				p, _ := e["params"].(map[string]any)
				return p
			}
		}
		t.Fatalf("no entry %s", action)
		return nil
	}
	// the admin in a browser: the entries as they are, with the first six characters; and the description of
	// the accept this build wrote has none
	raw, entries := read("Cookie", x.sess)
	if p := params(entries, "approval.accept"); p["machine_id"] != approval.ShortID(otherID) || p["on_record_before"] != approval.ShortID(idL) {
		t.Fatalf("an admin's view of the accept: %v", p)
	}
	if p := params(entries, "approval.alarm"); p["shown"] != approval.ShortID(otherID) || p["on_record"] != approval.ShortID(idL) {
		t.Fatalf("an admin's view of the alarm: %v", p)
	}
	if !strings.Contains(raw, "GONEmach…000004") {
		t.Fatal("an admin's session reads an old entry as it was written")
	}
	for _, e := range entries {
		if e["action"] != "auth.step_up" || e["target"] != "/api/admin/approval/accept/laptop" && !strings.Contains(fmt.Sprint(e["params"]), "accept what laptop shows") {
			continue
		}
		if text := fmt.Sprint(e["params"]); strings.Contains(text, otherID[:8]) || strings.Contains(text, idL[:6]) || strings.Contains(text, "…") {
			t.Fatalf("the description of the accept has characters of an id: %s", text)
		}
	}
	// a control-scope token of the admin (a read token does not get the audit log at all): no character of an id
	if tok, _, err := x.s.Tokens.Create("script-read", approvalAdminSub, "read", "test", 0); err != nil || x.do("GET", "/api/audit", nil, "Authorization", "Bearer "+tok).code != 403 {
		t.Fatalf("a read token and the audit log: %v", err)
	}
	for _, scope := range []string{"control"} {
		tok, _, err := x.s.Tokens.Create("script-"+scope, approvalAdminSub, scope, "test", 0)
		if err != nil {
			t.Fatal(err)
		}
		raw, entries := read("Authorization", "Bearer "+tok)
		for _, part := range []string{otherID[:8], approval.ShortID(otherID), idL[:6], "GONEma", "000004"} {
			if strings.Contains(raw, part) {
				t.Errorf("a %s token reads %q in the audit log", scope, part)
			}
		}
		if p := params(entries, "approval.accept"); p["machine_id"] != nil || p["on_record_before"] != nil || p["accepted"] != "id" || p["was"] != "changed" {
			t.Errorf("a %s token's view of the accept: %v", scope, p)
		}
		if p := params(entries, "approval.alarm"); p["shown"] != nil || p["on_record"] != nil || p["status"] != "changed" {
			t.Errorf("a %s token's view of the alarm: %v", scope, p)
		}
		if !strings.Contains(raw, "it shows the machine id (not given to an API token); on record is (not given to an API token)") {
			t.Errorf("a %s token's view of the old entry: it should say that the ids are left out", scope)
		}
	}
}
