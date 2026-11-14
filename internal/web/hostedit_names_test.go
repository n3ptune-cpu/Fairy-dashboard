package web

import (
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
)

// "Edit host", one name for one host. A host's name is typed to confirm a power action on it, so no two rows
// that can be asked for may be told apart by nothing: not now, not after a Reset, and not by a spelling that
// only looks different.

// hedGive stores a name for a host the way an older panel (or a hand) left it in state.json: without the
// checks of a request.
func (r *hedRig) hedGive(id, name string) {
	r.t.Helper()
	if _, _, err := r.s.Poller.Admin.EditHostOptions("test", func(o *adminstate.HostOptions) error {
		e := o.Host(id).Clone()
		e.Name = name
		o.SetHost(id, e)
		return nil
	}); err != nil {
		r.t.Fatal(err)
	}
	r.s.Poller.Bump()
}

// The finding of the review (R6, S1): PC-A is renamed, pc-b takes "PC-A", PC-A is reset: two rows called
// PC-A, and a shutdown of pc-b confirmed by typing the name of the other machine.
func TestEditHostNameResetCannotMakeTwoHostsOfOneName(t *testing.T) {
	r := hedSetup(t)
	r.ok("pc-a", map[string]any{"name": "Den"})
	// PC-A is still the name config.yaml has for pc-a: no other host can take it, so a Reset there is always free
	for _, name := range []string{"PC-A", "pc-a", " Pc-A "} {
		p := r.put("pc-b", map[string]any{"name": name})
		if p.code != 422 || !strings.Contains(string(p.raw), "PC-A is the name config.yaml has for another host (shown as Den now)") {
			t.Errorf("pc-b named %q while pc-a is only shown as Den: %d %s", name, p.code, p.raw)
		}
	}
	if r.storedHost("pc-b") != nil || r.row("pc-b")["name"] != "PC-B" {
		t.Fatalf("pc-b: stored %v, shown as %v", r.storedHost("pc-b"), r.row("pc-b")["name"])
	}
	// ... and where a file has it all the same (written by the build before this one, or by hand), the Reset is
	// what is refused, and it says which host is in the way
	r.hedGive("pc-b", "PC-A")
	for what, body := range map[string]map[string]any{
		"Reset":                    {"reset": []any{map[string]any{"field": "name"}}},
		"config.yaml's name typed": {"name": "PC-A"},
	} {
		p := r.put("pc-a", body)
		if p.code != 422 || !strings.Contains(string(p.raw), "config.yaml's name of this host is PC-A, and another host (pc-b) was given that name here: rename that host first") {
			t.Errorf("%s: %d %s", what, p.code, p.raw)
		}
	}
	if a, b := r.row("pc-a")["name"], r.row("pc-b")["name"]; a != "Den" || b != "PC-A" || r.storedHost("pc-a")["name"] != "Den" {
		t.Fatalf("after the refused Reset: pc-a %v, pc-b %v, stored %v", a, b, r.storedHost("pc-a"))
	}
	// the other things of a refused request are not made either: all of it or none
	if p := r.put("pc-a", map[string]any{"reset": []any{map[string]any{"field": "name"}}, "plugs": []any{map[string]any{"id": "pc-a/desk", "in_total": false}}}); p.code != 422 || r.storedAgent("pc-a") != nil {
		t.Errorf("a Reset with a link: %d %s, stored %v", p.code, p.raw, r.storedAgent("pc-a"))
	}
	if e, _ := r.lastChange("pc-a"); !strings.HasPrefix(e["outcome"].(string), "error 422") {
		t.Errorf("audit: %v", e)
	}
	// the way out is the one the refusal names: that host is renamed first
	r.ok("pc-b", map[string]any{"name": "Attic"})
	if p := r.ok("pc-a", map[string]any{"reset": []any{map[string]any{"field": "name"}}}); r.row("pc-a")["name"] != "PC-A" || r.row("pc-b")["name"] != "Attic" {
		t.Errorf("Reset after the rename: %s", p.raw)
	}
}

// The check is made with what state.json holds at the moment of the save, under its lock: of two admins who
// give two hosts one name at the same time, the second is told so.
func TestEditHostNameIsCheckedWhenItIsStored(t *testing.T) {
	r := hedSetup(t)
	done := false
	r.s.hostEditStep = func(step string) {
		if step == "planned" && !done { // the other admin's save lands after this request was looked at
			done = true
			r.hedGive("pc-b", "Den")
		}
	}
	p := r.put("pc-a", map[string]any{"name": "Den"})
	if !done || p.code != 422 || !strings.Contains(string(p.raw), "another host is called Den already") {
		t.Fatalf("the second of two renames to one name: %d %s", p.code, p.raw)
	}
	if a, b := r.row("pc-a")["name"], r.row("pc-b")["name"]; a != "PC-A" || b != "Den" || r.storedHost("pc-a") != nil {
		t.Errorf("pc-a %v, pc-b %v, stored %v", a, b, r.storedHost("pc-a"))
	}
}

// Look-alikes (R6, N1). What a person cannot tell apart is one name: letters of another alphabet that look
// the same, the other hyphens, full-width forms, case and spaces. A word in two alphabets is no name at all.
// A trailing dot and an accent can be seen, and stay names of their own.
func TestEditHostNameLookAlikes(t *testing.T) {
	r := hedSetup(t)
	for what, c := range map[string]struct{ name, want string }{
		"a Cyrillic A behind the hyphen": {"PC-А", "another host is called PC-A already"},
		"a Greek A behind the hyphen":    {"PC-Α", "another host is called PC-A already"},
		"a Cyrillic C in a Latin word":   {"PС-A", `name mixes Latin letters with Cyrillic or Greek ones in one word (\"PС\"): write the word in one alphabet`},
		"the Greek letter mu and Latin":  {"μServer", `name mixes Latin letters with Cyrillic or Greek ones in one word (\"μServer\"): write the word in one alphabet`},
		"all of it Cyrillic":             {"РС-А", "another host is called PC-A already"},
		"the non-breaking hyphen":        {"PC‑A", "another host is called PC-A already"},
		"an en dash":                     {"PC–A", "another host is called PC-A already"},
		"the minus sign":                 {"PC−A", "another host is called PC-A already"},
		"full-width letters":             {"ＰＣ-Ａ", "another host is called PC-A already"},
		"a full-width hyphen":            {"PC－A", "another host is called PC-A already"},
		"two spaces and another case":    {"relay  a", "another host is called Relay A already"},
	} {
		p := r.put("pc-b", map[string]any{"name": c.name})
		if p.code != 422 || !strings.Contains(string(p.raw), c.want) {
			t.Errorf("%s (%q): %d %s", what, c.name, p.code, p.raw)
		}
	}
	if r.storedOptions() != nil {
		t.Fatalf("a look-alike was stored: %v", r.storedOptions())
	}
	// honest names stay possible: another alphabet in a word of its own, a dot, an accent
	for _, name := range []string{"PC-A.", "PC-Á", "Дача", "Дача PC", "Σπίτι 2",
		// a word in one alphabet joined to a word in another by a hyphen (second look of the review, a1), and the
		// micro sign of a keyboard, which is no Greek letter
		"Сервер-NAS", "ПК-Dell", "NAS-Кухня", "Σπίτι-PC", "µServer"} {
		if p := r.ok("pc-b", map[string]any{"name": name}); r.row("pc-b")["name"] != name {
			t.Errorf("%q: %s, shown as %v", name, p.raw, r.row("pc-b")["name"])
		}
	}
	// ... and such a name is taken like any other, look-alikes of it included
	r.ok("pc-b", map[string]any{"name": "СОР"}) // Cyrillic: reads as COP
	if p := r.put("pc-a", map[string]any{"name": "cop"}); p.code != 422 || !strings.Contains(string(p.raw), "another host is called СОР already") {
		t.Errorf("the Latin look-alike of a Cyrillic name: %d %s", p.code, p.raw)
	}
}

// Whose names count (R6, N1). A guest's name is typed to confirm its shutdown or its stop, so a host cannot
// take it. The names of UPSes and of plugs are never typed and nothing is done to one by its name: a host may
// be called like one.
func TestEditHostNameOfAGuestIsTakenAndOfADeviceIsNot(t *testing.T) {
	r := ehSetup(t)
	guest, _ := r.row("lab-a-qemu-100")["name"].(string)
	if guest == "" {
		t.Fatal("the rig has no guest lab-a-qemu-100")
	}
	for _, name := range []string{guest, strings.ToUpper(guest), " " + guest + " "} {
		if p := r.put(ehNAS, map[string]any{"name": name}); p.code != 422 || !strings.Contains(string(p.raw), "a guest is called "+guest+" already") {
			t.Errorf("the NAS named %q like the guest: %d %s", name, p.code, p.raw)
		}
	}
	for _, name := range []string{"Rack UPS", "rack", "Corner plug"} { // a UPS by its description and by its name, a plug
		if p := r.ok(ehNAS, map[string]any{"name": name}); r.row(ehNAS)["name"] != name {
			t.Errorf("the NAS named %q like a device: %s", name, p.raw)
		}
	}
}
