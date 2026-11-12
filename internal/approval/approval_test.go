package approval

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
)

func p[T any](v T) *T { return &v }

// The challenges of the protocol's test vectors (part1-passkey-protocol.md, 4.3): the bytes 00..1f and 80..9f.
func vectorChallenge(first byte) string {
	b := make([]byte, 32)
	for i := range b {
		b[i] = first + byte(i)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

var (
	shutdown60 = api.ApprovalRequest{Verb: api.ApprovePower, Action: p("shutdown"), DelayS: p(60)}
	fan50      = api.ApprovalRequest{Verb: api.ApproveFanSet, Zone: p(0), Percent: p(50)}
)

// The machine ids of the protocol's vectors (section 4.3): A is the form a machine makes up itself (16 bytes in
// base64url), B a form a panel may choose (16 bytes in hex).
const (
	vectorIDA = "EBESExQVFhcYGRobHB0eHw"
	vectorIDB = "00112233445566778899aabbccddeeff"
)

// idOf is the machine id the tests have "on record" for an agent.
func idOf(agent string) string {
	if agent == "pc-a" {
		return vectorIDA
	}
	return vectorIDB
}

func bound(agent string, req api.ApprovalRequest, challenge string) Bound {
	canon, _ := req.Canonical()
	return Bound{Target: Target{AgentID: agent, Name: strings.ToUpper(agent), Request: req}, Canonical: canon, Challenge: challenge, MachineID: idOf(agent)}
}

// The plan's nonce is the protocol's: the vectors of section 4.3 (scheme 2: every entry names its machine),
// made from requests as the panel builds them (not from the texts), so the canonical text, the machine id and
// the nonce are all checked.
func TestPlanNonceVectors(t *testing.T) {
	a, b := vectorChallenge(0x00), vectorChallenge(0x80)
	if a != "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8" || b != "gIGCg4SFhoeIiYqLjI2Oj5CRkpOUlZaXmJmam5ydnp8" {
		t.Fatal("the vector challenges:", a, b)
	}
	ea, eb := bound("pc-a", shutdown60, a), bound("pc-b", fan50, b)
	if ea.Canonical != "power action=shutdown delay_s=60" || eb.Canonical != "fan_set zone=0 percent=50" {
		t.Fatal("canonical texts:", ea.Canonical, eb.Canonical)
	}
	for _, c := range []struct {
		name string
		list []Bound
		want string
	}{
		{"[A]", []Bound{ea}, "iuzbNkchoxmnpwfVpzW2U6MBub-LzMt-A2XI8o5LAes"},
		{"[A, B]", []Bound{ea, eb}, "MrQ7J0kZHsSvpcV4xJkpymWIwlm02CMZVJVGA_7jYhg"},
		{"[B, A]", []Bound{eb, ea}, "w6XDMvf0lQ47z2U8XxFGXFKfw2228j--NjgkS-oJvYw"},
		// entry A made for the other machine: what a panel would sign that took machine A's challenge for host B
		{"[A for id B]", []Bound{otherID(ea, vectorIDB)}, "f8xqscm9-nl4LOV17mn_6cgQPh7gPRgxEzPwPiX6a8A"},
	} {
		plan, err := NewPlan(c.list)
		if err != nil {
			t.Fatal(c.name, err)
		}
		if plan.Nonce() != c.want {
			t.Errorf("%s: nonce %s, the protocol says %s", c.name, plan.Nonce(), c.want)
		}
		if n, _ := api.ApprovalNonce(plan.Entries()); n != plan.Nonce() || len(plan.Entries()) != len(c.list) {
			t.Errorf("%s: the entries are not the list the nonce was made from", c.name)
		}
	}
}

func otherID(b Bound, id string) Bound {
	b.MachineID = id
	return b
}

// A plan is refused where a machine would refuse the list.
func TestPlanRefusals(t *testing.T) {
	a := vectorChallenge(0)
	wrongText := bound("pc-a", shutdown60, a)
	wrongText.Canonical = "power action=reboot delay_s=60"
	noCanon := bound("pc-a", api.ApprovalRequest{Verb: "format_disk"}, a)
	var many []Bound
	for i := 0; i <= api.MaxApprovalEntries; i++ {
		many = append(many, bound("pc", shutdown60, vectorChallenge(byte(i))))
	}
	for name, list := range map[string][]Bound{
		"empty":                     nil,
		"the same challenge twice":  {bound("pc-a", shutdown60, a), bound("pc-b", fan50, a)},
		"a short challenge":         {bound("pc-a", shutdown60, a[:42])},
		"a challenge with padding":  {bound("pc-a", shutdown60, a[:42]+"=")},
		"another text":              {wrongText},
		"no such request":           {noCanon},
		"65 entries":                many,
		"no machine id":             {otherID(bound("pc-a", shutdown60, a), "")},
		"a machine id that is none": {otherID(bound("pc-a", shutdown60, a), "short")},
		"a machine id with a space": {otherID(bound("pc-a", shutdown60, a), "0011223344556677 899aabbccddeeff")},
	} {
		if _, err := NewPlan(list); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := NewPlan(many[:api.MaxApprovalEntries]); err != nil {
		t.Error("64 entries:", err)
	}
}

// An approval is handed out only for a request the plan covers, to the agent it was asked for: changing any
// field, or sending it somewhere else, is refused here, before a machine is asked.
func TestApprovalIsBoundToTheExactRequest(t *testing.T) {
	plan, err := NewPlan([]Bound{bound("pc-a", shutdown60, vectorChallenge(0)), bound("pc-b", fan50, vectorChallenge(0x80))})
	if err != nil {
		t.Fatal(err)
	}
	const token = "header.payload.signature"
	ap, err := For(plan, token, "pc-a", vectorIDA, shutdown60)
	if err != nil || ap.Token != token || len(ap.Entries) != 2 || ap.Entries[0].Request != "power action=shutdown delay_s=60" ||
		ap.Entries[0].MachineID != vectorIDA || ap.Entries[1].MachineID != vectorIDB {
		t.Fatal("the approved request:", ap, err)
	}
	for name, c := range map[string]struct {
		agent string
		req   api.ApprovalRequest
	}{
		"another delay":             {"pc-a", api.ApprovalRequest{Verb: api.ApprovePower, Action: p("shutdown"), DelayS: p(61)}},
		"another action":            {"pc-a", api.ApprovalRequest{Verb: api.ApprovePower, Action: p("reboot"), DelayS: p(60)}},
		"another verb":              {"pc-a", api.ApprovalRequest{Verb: api.ApproveApplyUpdate}},
		"another machine":           {"pc-b", shutdown60},
		"the other entry's fan":     {"pc-a", fan50},
		"another percent":           {"pc-b", api.ApprovalRequest{Verb: api.ApproveFanSet, Zone: p(0), Percent: p(51)}},
		"another zone":              {"pc-b", api.ApprovalRequest{Verb: api.ApproveFanSet, Zone: p(1), Percent: p(50)}},
		"a machine not in the plan": {"pc-c", shutdown60},
		"an extra field":            {"pc-a", api.ApprovalRequest{Verb: api.ApprovePower, Action: p("shutdown"), DelayS: p(60), Zone: p(0)}},
	} {
		if ap, err := For(plan, token, c.agent, idOf(c.agent), c.req); !errors.Is(err, ErrNotCovered) || ap != nil {
			t.Errorf("%s: handed out an approval (%v)", name, err)
		}
	}
	if ap, err := For(plan, "", "pc-a", vectorIDA, shutdown60); err == nil || ap != nil {
		t.Error("no token, no approval")
	}
	if ap, err := For(nil, token, "pc-a", vectorIDA, shutdown60); err == nil || ap != nil {
		t.Error("no plan, no approval")
	}
	// the id on record for the host is another one now than when the plan was made (an admin accepted a new
	// id while the person was at Pocket ID), or there is none: nothing is handed out
	for _, now := range []string{vectorIDB, "", "EBESExQVFhcYGRobHB0eHx"} {
		if ap, err := For(plan, token, "pc-a", now, shutdown60); !errors.Is(err, ErrNotCovered) || ap != nil {
			t.Errorf("the id on record is %q now: handed out an approval made for %q", now, vectorIDA)
		}
	}
	// two entries for the same request on the same machine: neither is "the" one
	twice, err := NewPlan([]Bound{bound("pc-a", shutdown60, vectorChallenge(0)), bound("pc-a", shutdown60, vectorChallenge(0x80))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := For(twice, token, "pc-a", vectorIDA, shutdown60); err == nil {
		t.Error("an ambiguous plan handed out an approval")
	}
}

// The panel's own checks of a verified token.
func TestCheckClaims(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	good := Claims{Nonce: "the-nonce", Sub: "user-1", AuthTime: float64(now.Unix() - 5), AMR: []string{"phr", "mfa"}}
	want := Want{Nonce: "the-nonce", Sub: "user-1", Now: now}
	if f := Check(good, want); f != nil {
		t.Fatal("a good token:", f)
	}
	edit := func(fn func(*Claims)) Claims { c := good; fn(&c); return c }
	for name, c := range map[string]struct {
		claims Claims
		reason string
	}{
		"another nonce":         {edit(func(c *Claims) { c.Nonce = "another" }), ReasonNonce},
		"no nonce":              {edit(func(c *Claims) { c.Nonce = "" }), ReasonNonce},
		"another person":        {edit(func(c *Claims) { c.Sub = "user-2" }), ReasonPerson},
		"no person":             {edit(func(c *Claims) { c.Sub = "" }), ReasonPerson},
		"no auth_time":          {edit(func(c *Claims) { c.AuthTime = 0 }), ReasonNoAuth},
		"signed in 121 s ago":   {edit(func(c *Claims) { c.AuthTime = float64(now.Unix() - 121) }), ReasonAuthTime},
		"signed in an hour ago": {edit(func(c *Claims) { c.AuthTime = float64(now.Unix() - 3600) }), ReasonAuthTime},
		"signed in in 121 s":    {edit(func(c *Claims) { c.AuthTime = float64(now.Unix() + 121) }), ReasonAuthTime},
		"no passkey":            {edit(func(c *Claims) { c.AMR = []string{"pwd"} }), ReasonAMR},
		"no amr":                {edit(func(c *Claims) { c.AMR = nil }), ReasonAMR},
	} {
		if f := Check(c.claims, want); f == nil || f.Reason != c.reason || f.Text == "" {
			t.Errorf("%s: %v, want the reason %s", name, f, c.reason)
		}
	}
	if f := Check(edit(func(c *Claims) { c.AuthTime = float64(now.Unix() - 120) }), want); f != nil {
		t.Error("exactly two minutes old is still fresh:", f)
	}
	// A test build has no signed-in person: only that one comparison is skipped.
	any := Want{Nonce: "the-nonce", AnyPerson: true, Now: now}
	if f := Check(edit(func(c *Claims) { c.Sub = "whoever" }), any); f != nil {
		t.Error("AnyPerson:", f)
	}
	if f := Check(edit(func(c *Claims) { c.Nonce = "another" }), any); f == nil || f.Reason != ReasonNonce {
		t.Error("AnyPerson must not skip the nonce:", f)
	}
	if f := Check(edit(func(c *Claims) { c.AuthTime = float64(now.Unix() - 600) }), any); f == nil || f.Reason != ReasonAuthTime {
		t.Error("AnyPerson must not skip the freshness:", f)
	}
	// an empty wanted person never matches an empty claim
	if f := Check(edit(func(c *Claims) { c.Sub = "" }), Want{Nonce: "the-nonce", Now: now}); f == nil || f.Reason != ReasonPerson {
		t.Error("no signed-in person and no sub must not match:", f)
	}
}

// A token larger than a machine takes is refused before it is looked at.
func TestCheckSize(t *testing.T) {
	if f := CheckSize(strings.Repeat("a", api.MaxApprovalToken)); f != nil {
		t.Error("8192 bytes:", f)
	}
	for _, n := range []int{0, api.MaxApprovalToken + 1, 1 << 20} {
		if f := CheckSize(strings.Repeat("a", n)); f == nil || f.Reason != ReasonSize {
			t.Errorf("%d bytes: %v", n, f)
		}
	}
}

// What the page says when a machine refuses an approval: the machine's own words, and one short sentence for
// the reason it gave. Short enough for the result line of a kept action (300 characters), with the longest
// reason the protocol has.
func TestErrorTextFitsTheResultLine(t *testing.T) {
	for reason, want := range map[string]string{
		"user":            "not on that machine's list",
		"clock":           "machine's clock",
		"auth_time":       "machine's clock",
		"token_key":       "helper-trust set --fetch-keys",
		"token_signature": "helper-trust set --fetch-keys",
		"audience":        "another client",
		"nonce":           "not the machine Fairy has on record",
		"no_challenge":    "press the button again",
	} {
		said := "approval refused (" + reason + "): this user may not approve actions on this machine"
		msg, ok := ErrorText("NSX-TEST-VM", api.CodeApprovalRefused, said)
		if !ok || !strings.Contains(msg, want) || !strings.Contains(msg, said) || !strings.Contains(msg, "Nothing was done.") {
			t.Errorf("%s: %q", reason, msg)
		}
		if len(msg) > 300 {
			t.Errorf("%s: %d characters, the result line holds 300: %q", reason, len(msg), msg)
		}
	}
	if _, ok := ErrorText("x", "something_else", "text"); ok {
		t.Error("a code that is not the approval protocol's has no text here")
	}
}
