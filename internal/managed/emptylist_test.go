package managed

import (
	"os"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// Audit 6, r4-p2-06: managed-hosts.yaml with nothing in it (0 bytes, blanks, "null", only the comment lines,
// "hosts:" without hosts) decodes as "no hosts": the panel started without every machine that was added in it,
// with no line in the log, while their tokens were still in secrets/. The start still goes on (an empty list is
// a valid list), and Merge now gives one line for it, which the start logs, the Add host dialog shows and
// `fairy admin config check` lists.
func TestAnEmptiedListOfAddedHostsIsSaid(t *testing.T) {
	two := func(t *testing.T) *Store {
		t.Helper()
		s := NewStore(t.TempDir())
		for _, id := range []string{"pc-b", "pc-a"} {
			if err := s.Put(Host{ID: id, Name: id, URL: "http://" + id + ":9101", OS: "linux"}, Secret{Token: tokA + id, ControlToken: tokB + id}); err != nil {
				t.Fatal(err)
			}
		}
		return s
	}
	merge := func(t *testing.T, s *Store) (int, []string) {
		t.Helper()
		cfg := testutil.Config(t, []string{"https://pve.example:8006"}, "http://127.0.0.1:1", "http://127.0.0.1:2")
		added, skipped, err := s.Merge(cfg)
		if err != nil {
			t.Fatalf("Merge: %v", err)
		}
		return len(added), skipped
	}
	for _, c := range []struct{ name, content, what string }{
		{"0 bytes", "", FileName + " is empty (it does not even hold the two comment lines the panel writes)"},
		{"blank", " \n", FileName + " is empty (it does not even hold the two comment lines the panel writes)"},
		{"null", "null\n", FileName + " is empty (it does not even hold the two comment lines the panel writes)"},
		{"only the comment lines", header, FileName + " lists no host"},
		{"hosts: without hosts", header + "hosts:\n", FileName + " lists no host"},
		{"hosts: []", header + "hosts: []\n", FileName + " lists no host"},
	} {
		s := two(t)
		if err := os.WriteFile(s.Path(), []byte(c.content), 0o600); err != nil {
			t.Fatal(err)
		}
		n, skipped := merge(t, s)
		want := c.what + ", but the tokens of 2 host(s) added in the panel are still in "
		if n != 0 || len(skipped) != 1 || !strings.HasPrefix(skipped[0], want) || !strings.Contains(skipped[0], "(pc-a, pc-b): the list was emptied, cut off or lost, and these hosts are NOT loaded.") {
			t.Errorf("%s: %d hosts, lines %q; want one line that begins %q and names pc-a, pc-b", c.name, n, skipped, want)
		}
	}
	// the list is gone altogether, the tokens are there
	s := two(t)
	if err := os.Remove(s.Path()); err != nil {
		t.Fatal(err)
	}
	if n, skipped := merge(t, s); n != 0 || len(skipped) != 1 || !strings.HasPrefix(skipped[0], FileName+" is missing, but the tokens of 2 host(s)") {
		t.Errorf("the list missing: %d hosts, lines %q", n, skipped)
	}
	// an emptied file without any tokens: nothing shows that hosts were added, but the panel did not write it
	s = NewStore(t.TempDir())
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.Path(), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if n, skipped := merge(t, s); n != 0 || len(skipped) != 1 || !strings.Contains(skipped[0], "is empty") || !strings.Contains(skipped[0], "no host added in the panel is loaded") {
		t.Errorf("an empty file, no tokens: %d hosts, lines %q", n, skipped)
	}
	// what must stay silent: no list and no tokens (nothing was ever added); the panel's own file after the last
	// host was removed in the panel; and a list with its hosts
	if n, skipped := merge(t, NewStore(t.TempDir())); n != 0 || len(skipped) != 0 {
		t.Errorf("a fresh data folder: %d hosts, lines %q", n, skipped)
	}
	s = two(t)
	for _, id := range []string{"pc-a", "pc-b"} {
		if err := s.Remove(id); err != nil {
			t.Fatal(err)
		}
	}
	if n, skipped := merge(t, s); n != 0 || len(skipped) != 0 {
		t.Errorf("after both hosts were removed in the panel: %d hosts, lines %q", n, skipped)
	}
	if n, skipped := merge(t, two(t)); n != 2 || len(skipped) != 0 {
		t.Errorf("a list with its two hosts: %d hosts, lines %q", n, skipped)
	}
}
