package web

import (
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/approval"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/notifications"
)

// From the owner's click-through of 2026-11-12. A greyed button read "Turn on… (0)": no button label of the
// approval feature ends with "…", the button for several machines says what it does and for how many, and with
// no machine ready there is no dead button, only one short line (the rest behind the info button).
func TestUITurnOnButtonsSayWhatTheyDo(t *testing.T) {
	js := appJS(t)
	feature := js[strings.Index(js, "// ---- the machine's own check of approvals"):strings.Index(js, "const statusText = (s) =>")]
	for _, gone := range []string{"Turn on…", "…</button>", "(${esc(d.ready)})", "No machine is ready to be switched on\"))}\">"} {
		if strings.Contains(feature, gone) {
			t.Errorf("the approval code still has %q", gone)
		}
	}
	for _, want := range []string{
		"${I.shield}Turn on</button>", // a machine's own page
		"${I.shield}Turn on the check for ${esc(n)} machine${n === 1 ? \"\" : \"s\"}</button>` : \"\"}</div>", // the Updates page: a real count, or no button
		"${n ? \"\" : `<p class=\"note\" id=\"ap-none\">No machine is waiting for the check.${infoBtn(\"ap-none\", \"No machine is waiting for the check\",",
		"Each machine's row says why it is not ready.</p>",
		"$(\"#ap-go\").textContent = n ? `Turn on for ${n} machine${n === 1 ? \"\" : \"s\"} (passkey)` : \"Turn on\";", // the dialog's own button, unchanged
	} {
		if !strings.Contains(feature, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
}

// The notification at the end of an alarm says how it ended. After "Accept that its check is off" it read "its
// approval check is in order again", as if the check were on. "In order again" is kept for an alarm that ended
// by itself: the check is on and matches the record.
func TestTheNotificationSaysHowAnAlarmEnded(t *testing.T) {
	a := newApprovalEnv(t)
	last := func(host string) string {
		t.Helper()
		// newest first
		for _, e := range a.s.Notes.List("", func(notifications.Event) bool { return true }, notifications.Filter{Limit: 200}).Events {
			if e.Kind == "approval_ok" && e.Host == host {
				return e.Text
			}
		}
		return ""
	}
	// not on record, accepted: "its machine id was accepted by ..."
	a.setTrust(t, a.laptop, a.tower)
	a.poll()
	if what, _, err := a.accept("laptop", "nora@example.test"); what != "id" || err != nil {
		t.Fatal(what, err)
	}
	if got := last("laptop"); got != "its machine id was accepted by nora@example.test" {
		t.Errorf("after the accept of an id that was not on record: %q", got)
	}
	if what, _, err := a.accept("tower", "nora@example.test"); what != "id" || err != nil {
		t.Fatal(what, err)
	}
	// a new id, accepted
	a.setTrustID(t, a.laptop, otherID)
	a.poll()
	if what, _, err := a.accept("laptop", "nora@example.test"); what != "id" || err != nil {
		t.Fatal(what, err)
	}
	if got := last("laptop"); got != "its new machine id was accepted by nora@example.test" {
		t.Errorf("after the accept of a new id: %q", got)
	}
	// an alarm that ends by itself: tower shows laptop's id for a while, then its own again. Laptop's check is
	// on and matches the record: "in order again" (and so is tower's, which nobody accepted anything for)
	a.setTrustID(t, a.tower, otherID)
	a.poll()
	if b := a.s.Poller.BindingOf("laptop"); b.Status != approval.BindDuplicate {
		t.Fatalf("laptop while tower shows its id: %+v", b)
	}
	a.setTrustID(t, a.tower, machineIDOf("tower"))
	a.poll()
	for _, h := range []string{"laptop", "tower"} {
		if got := last(h); got != "its approval check is in order again" || !a.s.Poller.BindingOf(h).OK() {
			t.Errorf("%s after the duplicate ended by itself: %q", h, got)
		}
	}
	// the check is gone, and an admin accepts that it is off: the check is OFF, and the line says so
	a.clearTrust(t, a.laptop)
	a.poll()
	if what, _, err := a.accept("laptop", "nora@example.test"); what != "off" || err != nil {
		t.Fatal(what, err)
	}
	if got := last("laptop"); got != "its check being off was accepted by nora@example.test" || strings.Contains(got, "in order") {
		t.Errorf("after \"Accept that its check is off\": %q", got)
	}
	// the audit log's entry for the end of an alarm is as it was
	if al := a.auditEntries("", "approval.alarm"); al[0]["outcome"] != "cleared" || al[0]["target"] != "laptop" {
		t.Errorf("the audit entry: %v", al[0])
	}
}
