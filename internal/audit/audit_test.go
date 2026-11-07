package audit

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestAppendOnlyReverseReaderFiltersAndCursor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "audit.log")
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ { // concurrent writers never interleave lines
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			act := "poll.set"
			if i%2 == 0 {
				act = "power.shutdown"
			}
			ip := "1.2.3.4"
			_, _ = Write(path, Entry{Actor: "n@example.com", Action: act, Target: "host-" + strings.Repeat("x", i%7), IP: &ip,
				Params: map[string]any{"big": strings.Repeat("y", 900)}})
		}(i)
	}
	wg.Wait()
	_, _ = Write(path, Entry{Actor: "root", Action: "control.disable", Source: "cli"})
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatal(st.Mode())
	}
	all, next := Read(path, "", "", "", nil, 1000)
	if len(all) != 201 || next != nil || all[0]["action"] != "control.disable" || all[0]["source"] != "cli" {
		t.Fatalf("%d %v", len(all), all[0])
	}
	// paging with the byte-offset cursor visits every entry exactly once
	seen, cur := 0, (*int64)(nil)
	for {
		page, n := Read(path, "", "", "", cur, 17)
		seen += len(page)
		if n == nil {
			break
		}
		cur = n
	}
	if seen != 201 {
		t.Fatal(seen)
	}
	p, _ := Read(path, "", "power", "", nil, 1000)
	h, _ := Read(path, "ROOT", "", "", nil, 1000)
	if len(p) != 100 || len(h) != 1 {
		t.Fatal(len(p), len(h))
	}
	if !Matches(map[string]any{"action": "power.shutdown"}, "", "power", "") || Matches(map[string]any{"action": "powerx"}, "", "power", "") {
		t.Fatal("prefix match")
	}
	none, _ := Read(filepath.Join(t.TempDir(), "missing.log"), "", "", "", nil, 5)
	if len(none) != 0 {
		t.Fatal("missing file")
	}
}
