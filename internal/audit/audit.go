// Package audit is the append-only audit log (JSON lines) shared by the server and the admin CLI, plus a reverse
// reader for the UI.
//
// Entry: {ts, actor, sub, action, target, params, outcome, source, ip}. `source` is "ui" (browser session),
// "token:<name>" (API token), "cli" (admin CLI, actor = local OS user) or "demo". Nothing deletes entries.
package audit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var mu sync.Mutex

const block = 64 * 1024

// Entry is one audit-log line. Write fills in TS and defaults Params, Outcome ("ok") and Source ("ui").
type Entry struct {
	TS      string         `json:"ts"`
	Actor   string         `json:"actor"`
	Sub     *string        `json:"sub"`
	Action  string         `json:"action"`
	Target  string         `json:"target"`
	Params  map[string]any `json:"params"`
	Outcome string         `json:"outcome"`
	Source  string         `json:"source"`
	IP      *string        `json:"ip"`
}

// Write appends one entry with a single write() on an O_APPEND file: lines from the CLI and server never
// interleave.
func Write(path string, e Entry) (Entry, error) {
	e.TS = time.Now().UTC().Format("2006-01-02T15:04:05.000000-07:00")
	if e.Params == nil {
		e.Params = map[string]any{}
	}
	if e.Outcome == "" {
		e.Outcome = "ok"
	}
	if e.Source == "" {
		e.Source = "ui"
	}
	b, err := json.Marshal(e)
	if err != nil {
		return e, err
	}
	b = append(b, '\n')
	mu.Lock()
	defer mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return e, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return e, err
	}
	defer f.Close()
	_, err = f.Write(b)
	return e, err
}

type line struct {
	off int64
	b   []byte
}

// reverseLines calls fn for each line from the end (or from `before`) towards the start; fn returns false to stop.
func reverseLines(path string, before *int64, fn func(off int64, b []byte) bool) error {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	end, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if before != nil && *before < end {
		end = max(0, *before)
	}
	pos := end
	var buf []byte
	for pos > 0 {
		step := min(int64(block), pos)
		pos -= step
		chunk := make([]byte, step)
		if _, err := f.ReadAt(chunk, pos); err != nil && err != io.EOF {
			return err
		}
		buf = append(chunk, buf...)
		parts := bytes.Split(buf, []byte("\n"))
		buf = parts[0] // possibly partial: keep for the next block
		off := pos + int64(len(buf)) + 1
		lines := make([]line, 0, len(parts)-1)
		for _, ln := range parts[1:] {
			lines = append(lines, line{off, ln})
			off += int64(len(ln)) + 1
		}
		for i := len(lines) - 1; i >= 0; i-- {
			if !fn(lines[i].off, lines[i].b) {
				return nil
			}
		}
	}
	if len(buf) > 0 {
		fn(0, buf)
	}
	return nil
}

func str(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// Matches applies the UI/CLI filters: user (substring of actor/sub/source), action (exact or prefix "x."),
// host (exact target).
func Matches(e map[string]any, user, action, host string) bool {
	if user != "" {
		u := strings.ToLower(user)
		ok := false
		for _, k := range []string{"actor", "sub", "source"} {
			if strings.Contains(strings.ToLower(str(e[k])), u) {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	if action != "" {
		a := str(e["action"])
		if a != action && !strings.HasPrefix(a, action+".") {
			return false
		}
	}
	return host == "" || str(e["target"]) == host
}

// Read returns entries newest first and the cursor for the next page (nil at the end). Each entry gets
// `id` = its byte offset.
func Read(path, user, action, host string, before *int64, limit int) ([]map[string]any, *int64) {
	out := []map[string]any{}
	var next *int64
	_ = reverseLines(path, before, func(off int64, raw []byte) bool {
		if len(bytes.TrimSpace(raw)) == 0 {
			return true
		}
		var e map[string]any
		if json.Unmarshal(raw, &e) != nil || e == nil {
			return true
		}
		// tolerate entries written by older versions (sub/email/system keys)
		if _, ok := e["actor"]; !ok {
			a := str(e["email"])
			if a == "" {
				a = str(e["sub"])
			}
			if a == "" {
				a = "?"
			}
			e["actor"] = a
		}
		if _, ok := e["target"]; !ok {
			e["target"] = str(e["system"])
		}
		if _, ok := e["source"]; !ok {
			e["source"] = "ui"
		}
		if Matches(e, user, action, host) {
			e["id"] = off
			out = append(out, e)
			if len(out) >= limit {
				o := off
				next = &o
				return false
			}
		}
		return true
	})
	return out, next
}
