package web

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
)

// "Edit host" keeps the site in another file (users.json) than the rest (state.json). A request with a site
// stores the site first; when the rest is then refused, the site is put back. The put-back takes out only
// what this request stored: a site somebody else gave the host in that moment stays, and the audit entry
// says so. And when the put-back cannot be written, the answer does not say "nothing was saved": the site is
// stored, and the answer and the audit entry say that.
func TestEditHostPutsBackOnlyTheSiteItStoredItself(t *testing.T) {
	rename := func(t *testing.T, r *hedRig, id, name string) {
		t.Helper()
		if _, _, err := r.s.Poller.Admin.EditHostOptions("other admin", func(o *adminstate.HostOptions) error {
			e := o.Hosts[id]
			e.Name = name
			o.SetHost(id, e)
			return nil
		}); err != nil {
			t.Error(err)
		}
		r.s.Poller.Bump()
	}
	// users.json as something the panel does not write to (a link): the put-back fails. mend puts the file back.
	breakUsers := func(t *testing.T, r *hedRig) (mend func()) {
		t.Helper()
		path := r.s.Access.Path
		if err := os.Rename(path, path+".real"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(path+".real", path); err != nil {
			t.Fatal(err)
		}
		return func() {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(path+".real", path); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, c := range []struct {
		what        string
		other       string // the site the Users page gives pc-b right after this request's site was stored ("": nobody does)
		broken      bool   // the put-back cannot be written
		wantStored  string
		wantMessage string
		wantOutcome string
	}{
		{"nobody else: the site goes back", "", false, "", hoNotShown, "error 409: the host was changed after the confirmation was asked for; nothing was saved"},
		{"another admin's site in that moment stays", "site-b", false, "site-b", hoNotShown, "error 409: the host was changed after the confirmation was asked for; nothing was saved"},
		{"the put-back cannot be written", "", true, "site-a", hoNotShownSiteStays,
			"error 409: the host was changed after the confirmation was asked for; the site was stored and could not be put back, nothing else was saved"},
	} {
		r := hedSetup(t)
		p := r.do("PUT", "/api/admin/hosts/pc-b/options", map[string]any{"site": "site-a", "name": "Den"}, "Content-Type", "application/json", "Cookie", r.a.sess, "X-Requested-With", "fairy")
		if d, _ := p.body["detail"].(map[string]any); !stepUpRequired(p) || len(strs(d["items"])) < 2 {
			t.Fatalf("%s: the rig: %d %s", c.what, p.code, p.raw)
		}
		mend, at := func() {}, 0
		r.s.hostEditStep = func(step string) {
			if step != "site stored" {
				return
			}
			at++
			if got := r.s.Access.Load().Assign["pc-b"]; got != "site-a" {
				t.Errorf("%s: at this step the request's site is stored: %q", c.what, got)
			}
			rename(t, r, "pc-b", "Attic") // the request's lines are no longer true: its locked write will refuse
			if c.other != "" {
				if _, err := r.s.Access.Assign("pc-b", c.other); err != nil {
					t.Error(err)
				}
			}
			if c.broken {
				mend = breakUsers(t, r)
			}
		}
		if cb := r.a.confirm(pendingID(p), false); cb.code != 303 || at != 1 {
			t.Fatalf("%s: the confirmation: %d, the site was stored %d times", c.what, cb.code, at)
		}
		r.s.hostEditStep = nil
		mend()
		res := r.a.rtResult()
		if res["ok"] != false || res["status"] != 409.0 || res["message"] != c.wantMessage {
			t.Errorf("%s: answered %v", c.what, res)
		}
		if msg := fmt.Sprint(res["message"]); c.broken && (strings.Contains(msg, "Nothing was saved") || !strings.HasSuffix(msg, hoSiteStays)) {
			t.Errorf("%s: the answer says nothing was saved, or not that the site is stored: %q", c.what, msg)
		}
		if got := r.s.Access.Load().Assign["pc-b"]; got != c.wantStored {
			t.Errorf("%s: the stored site of pc-b is %q, want %q", c.what, got, c.wantStored)
		}
		if r.row("pc-b")["name"] != "Attic" {
			t.Errorf("%s: the name: %v", c.what, r.row("pc-b")["name"])
		}
		e, params := r.lastChange("pc-b")
		left, hasLeft := params["site_left"]
		if e["outcome"] != c.wantOutcome || hasLeft != (c.other != "") || (c.other != "" && left != c.other) || (params["site_not_put_back"] != nil) != c.broken {
			t.Errorf("%s: the audit entry: %v", c.what, e)
		}
		if l := r.auditEntries("", "site.assign"); len(l) != 0 {
			t.Errorf("%s: a site change that was not made is in the audit log: %v", c.what, l)
		}
	}

	// a request that is refused for another reason after its site was stored (the name was taken in that
	// moment), and the put-back cannot be written: its own reason, and that the site is stored
	r := hedSetup(t)
	mend := func() {}
	r.s.hostEditStep = func(step string) {
		if step == "site stored" {
			rename(t, r, "pc-a", "Den")
			mend = breakUsers(t, r)
		}
	}
	p := r.put("pc-b", map[string]any{"site": "site-a", "name": "Den"})
	r.s.hostEditStep = nil
	mend()
	if msg := fmt.Sprint(p.body["detail"]); p.code != 422 || !strings.Contains(msg, "Den") || !strings.HasSuffix(msg, ". "+hoSiteStays) || strings.Contains(msg, "..") {
		t.Errorf("a name taken in that moment, and no put-back: %d %s", p.code, p.raw)
	}
	if got := r.s.Access.Load().Assign["pc-b"]; got != "site-a" || r.row("pc-b")["name"] != "PC-B" {
		t.Errorf("stored: site %q, name %v", got, r.row("pc-b")["name"])
	}
	if e, params := r.lastChange("pc-b"); e["outcome"] != "failed: the site was stored and could not be put back, nothing else was saved" || params["site_not_put_back"] == nil {
		t.Errorf("the audit entry: %v", e)
	}
}
