//go:build fairytest

package web

import (
	"testing"
)

// "Set model name" in a test build: the test admin sets, sees and removes a host's model name exactly as an
// admin of a real panel does. The checks are the same ones (the page's header, the name's rules); only the
// sign-in differs.
func TestTestModeSetsACustomModel(t *testing.T) {
	e := testPanel(t)
	ck := e.open()
	write := []string{"Cookie", ck, "X-Requested-With", "fairy"}
	if r := e.setModel("laptop", "Gaming PC"); r.code != 401 {
		t.Fatal("without a session:", r.code)
	}
	if r := e.setModel("laptop", "Gaming PC", "Cookie", ck); r.code != 403 {
		t.Fatal("without the CSRF header:", r.code)
	}
	if r := e.setModel("laptop", "Gaming PC", append([]string{"Origin", "http://evil.example"}, write...)...); r.code != 403 {
		t.Fatal("from another origin:", r.code)
	}
	if r := e.setModel("laptop", "Gaming\nPC", write...); r.code != 422 {
		t.Fatal("a name with a line break:", r.code, string(r.raw))
	}
	if r := e.setModel("laptop", "  Gaming   PC ", write...); r.code != 200 || r.body["custom_model"] != "Gaming PC" {
		t.Fatalf("the test admin: %d %s", r.code, r.raw)
	}
	if c, _ := e.models("laptop", "Cookie", ck); c != "Gaming PC" {
		t.Fatal("the system:", c)
	}
	en := e.auditEntries("", "host.model")
	if len(en) != 1 || en[0]["sub"] != "test-admin" || en[0]["target"] != "laptop" {
		t.Fatalf("the audit log: %v", en)
	}
	if r := e.setModel("laptop", "", write...); r.code != 200 || r.body["custom_model"] != nil {
		t.Fatalf("removal: %d %s", r.code, r.raw)
	}
	if c, _ := e.models("laptop", "Cookie", ck); c != nil {
		t.Fatal("after the removal:", c)
	}
}
