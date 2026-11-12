package web

import (
	"fmt"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/access"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/approval"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
)

// A viewer sees the systems of their sites. Two hosts that show one machine id name each other in what the
// panel says about each (binding_with, binding_held), and the alarm's notification named the other host too:
// so a viewer of one site, and an API token of theirs, read the name of a host of another site (audit 6,
// r4-p3-01). The other host is "another host" for whoever may not see it: no name, no id.
func TestAViewerLearnsNoNameOfAHostOfAnotherSite(t *testing.T) {
	a := newApprovalEnv(t)
	st := a.s.Access
	for _, site := range []string{"Home", "Secret"} {
		if err := st.CreateSite(site); err != nil {
			t.Fatal(err)
		}
	}
	for target, site := range map[string]string{"laptop": "Home", "tower": "Secret"} {
		if _, err := st.Assign(target, site); err != nil {
			t.Fatal(err)
		}
	}
	const sub = "home-viewer"
	if _, _, err := st.Seen(sub, sub+"@example.com", sub, access.RolePending, "test"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.SetRole(sub, access.RoleViewer, []string{"Home"}, "test", true); err != nil {
		t.Fatal(err)
	}
	viewer := []string{"Cookie", a.cookie(person(sub), 0)}
	tok, _, err := a.s.Tokens.Create("script", sub, "read", "test", 0)
	if err != nil {
		t.Fatal(err)
	}
	token := []string{"Authorization", "Bearer " + tok}
	tok, _, err = a.s.Tokens.CreateForSites("script-home", approvalAdminSub, "read", "test", 0, []string{"Home"})
	if err != nil {
		t.Fatal(err)
	}
	limited := []string{"Authorization", "Bearer " + tok} // an admin's token that is limited to one site
	if r := a.do("GET", "/api/systems/tower", nil, viewer...); r.code != 404 {
		t.Fatalf("the viewer of Home sees tower: %d", r.code)
	}

	// what the readers get about laptop, and that no answer names tower
	about := func(when string, hdr []string) map[string]any {
		t.Helper()
		var ap map[string]any
		for _, path := range []string{"/api/systems", "/api/systems/laptop", "/api/notifications?limit=200"} {
			r := a.do("GET", path, nil, hdr...)
			if r.code != 200 || strings.Contains(string(r.raw), "tower") {
				t.Fatalf("%s: %s names the host of another site: %d %s", when, path, r.code, r.raw)
			}
			if path == "/api/systems/laptop" {
				ap, _ = r.body["approval"].(map[string]any)
			}
		}
		if code, body := a.streamBody("/api/stream", hdr...); code != 200 || !strings.Contains(body, `"laptop"`) || strings.Contains(body, "tower") {
			t.Fatalf("%s: the stream names the host of another site: %d", when, code)
		}
		return ap
	}
	// 1. both show one id, neither is on record
	a.setTrust(t, a.laptop)
	a.setTrustID(t, a.tower, machineIDOf("laptop"))
	a.poll()
	a.poll()
	if b := a.s.Poller.BindingOf("laptop"); b.Status != approval.BindNoRecord || fmt.Sprint(b.With) != "[tower]" {
		t.Fatalf("laptop: %+v", b)
	}
	for who, hdr := range map[string][]string{"the viewer": viewer, "the viewer's token": token, "a token limited to the site": limited} {
		if ap := about(who+", both not on record", hdr); fmt.Sprint(ap["binding_with"]) != "[another host]" || ap["binding_held"] != nil {
			t.Fatalf("%s: laptop's approval: %v", who, ap)
		}
	}
	// 2. the id comes on record for tower: laptop's id is kept by a host the viewer may not see
	if what, _, err := a.s.Poller.AcceptMachine("tower", "test admin", poller.AcceptWant{MachineID: machineIDOf("laptop")}); what != "id" || err != nil {
		t.Fatalf("the accept: %q %v", what, err)
	}
	a.poll()
	if b := a.s.Poller.BindingOf("laptop"); b.Status != approval.BindDuplicate || fmt.Sprint(b.Held) != "[tower]" {
		t.Fatalf("laptop: %+v", b)
	}
	for who, hdr := range map[string][]string{"the viewer": viewer, "the viewer's token": token, "a token limited to the site": limited} {
		ap := about(who+", tower keeps the id", hdr)
		if fmt.Sprint(ap["binding_with"]) != "[another host]" || fmt.Sprint(ap["binding_held"]) != "[another host]" || ap["binding"] != "duplicate" {
			t.Fatalf("%s: laptop's approval: %v", who, ap)
		}
	}
	// the notification the viewer gets says what happened, without a name
	if r := a.do("GET", "/api/notifications?limit=200", nil, viewer...); !strings.Contains(string(r.raw), "has the same machine id as another host: no approvals for either until that is resolved") {
		t.Fatalf("the viewer's notifications: %s", r.raw)
	}
	// an admin, who sees both, reads the name as before
	r := a.do("GET", "/api/systems/laptop", nil, "Cookie", a.admin)
	if ap, _ := r.body["approval"].(map[string]any); fmt.Sprint(ap["binding_with"]) != "[tower]" || fmt.Sprint(ap["binding_held"]) != "[tower]" {
		t.Fatalf("an admin's view: %s", r.raw)
	}
	// the page starts the sentence with a capital when the only other host is one the reader may not see
	if js := appJS(t); !strings.Contains(js, `andList(l.map(String)).replace(/^another host/, "Another host")`) {
		t.Error("app.js does not start the sentence about another host with a capital")
	}
}
