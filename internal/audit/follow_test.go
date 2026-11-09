package audit

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The reverse reader skips a line too long to hold (a damaged file) and still finds the entries on both sides.
func TestReadSkipsOverlongLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	if _, err := Write(path, Entry{Actor: "a", Action: "x.before"}); err != nil {
		t.Fatal(err)
	}
	f, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	_, _ = f.Write(append(bytes.Repeat([]byte(`{"actor":"`), 300_000), '\n')) // ~3 MiB, one line
	f.Close()
	if _, err := Write(path, Entry{Actor: "a", Action: "x.after"}); err != nil {
		t.Fatal(err)
	}
	got, next := Read(path, "", "", "", nil, 10)
	if len(got) != 2 || got[0]["action"] != "x.after" || got[1]["action"] != "x.before" || next != nil {
		t.Fatalf("%v %v", got, next)
	}
	// paging from the offset of the newest entry finds the older one past the long line
	off := got[0]["id"].(int64)
	if older, _ := Read(path, "", "", "", &off, 10); len(older) != 1 || older[0]["action"] != "x.before" {
		t.Fatal(older)
	}
}

// --follow: a line written in two parts is shown once, whole; an overlong line is skipped; after a rotation
// the new file is followed.
func TestFollow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	if _, err := Write(path, Entry{Actor: "a", Action: "x.old"}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var seen []string
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- Follow(path, func(e map[string]any) {
			mu.Lock()
			seen = append(seen, e["action"].(string))
			mu.Unlock()
		}, stop, 5*time.Millisecond)
	}()
	wait := func(n int) {
		t.Helper()
		for i := 0; i < 400; i++ {
			mu.Lock()
			l := len(seen)
			mu.Unlock()
			if l >= n {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("seen %v, want %d", seen, n)
	}
	time.Sleep(30 * time.Millisecond) // Follow starts at the end: x.old is not shown
	app := func(s string) {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString(s)
		f.Close()
	}
	app(`{"actor":"a","action":"x.sp`)
	time.Sleep(30 * time.Millisecond)
	app(`lit"}` + "\n")
	wait(1)
	app(strings.Repeat("y", 3<<20) + "\n")
	app(`{"actor":"a","action":"x.after-long"}` + "\n")
	wait(2)
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	app(`{"actor":"a","action":"x.rotated"}` + "\n")
	wait(3)
	close(stop)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(seen, ",") != "x.split,x.after-long,x.rotated" {
		t.Fatal(seen)
	}
}
