// Package statefile: small JSON state files shared by the server and the admin CLI (state.json, sessions.json,
// tokens.json).
//
// Writes take an exclusive lock on <file>.lock, re-read, modify and atomically replace the file (mode 0600), so
// the CLI and the running server never lose each other's changes. Readers reload when the file's mtime changes.
package statefile

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Data = map[string]any

// Read returns the file's top-level object ({} when missing or invalid).
func Read(path string) Data {
	b, err := os.ReadFile(path)
	if err != nil {
		return Data{}
	}
	var d Data
	if json.Unmarshal(b, &d) != nil || d == nil {
		return Data{}
	}
	return d
}

// Update is a locked read-modify-write. fn mutates the map in place; returning an error aborts the write.
func Update(path string, fn func(Data) error) (Data, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	unlock, err := lockFile(path + ".lock")
	if err != nil {
		return nil, err
	}
	defer unlock()
	d := Read(path)
	if err := fn(d); err != nil {
		return nil, err
	}
	b, err := json.MarshalIndent(d, "", " ")
	if err != nil {
		return nil, err
	}
	tmp := filepath.Join(filepath.Dir(path), fmt.Sprintf(".%s.%d.tmp", filepath.Base(path), os.Getpid()))
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return nil, err
	}
	return d, nil
}

// Watched is a cached view of a state file, reloaded when its mtime changes (checked at most every `every`).
type Watched struct {
	Path    string
	every   time.Duration
	mu      sync.Mutex
	data    Data
	mtime   int64
	checked time.Time
	Version int
}

func NewWatched(path string) *Watched { return NewWatchedEvery(path, time.Second) }

// NewWatchedEvery: every=0 stats the file on every Get (used for API tokens, so a revoke applies at once).
func NewWatchedEvery(path string, every time.Duration) *Watched {
	return &Watched{Path: path, every: every, data: Data{}, mtime: -1}
}

func (w *Watched) Get() Data {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.getLocked(false)
}

func (w *Watched) Refresh() Data {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.getLocked(true)
}

func (w *Watched) getLocked(force bool) Data {
	now := time.Now()
	if !force && now.Sub(w.checked) < w.every {
		return w.data
	}
	w.checked = now
	var mt int64 = -2
	if st, err := os.Stat(w.Path); err == nil {
		mt = st.ModTime().UnixNano() ^ st.Size()<<1
	}
	if mt != w.mtime {
		w.mtime = mt
		if mt == -2 {
			w.data = Data{}
		} else {
			w.data = Read(w.Path)
		}
		w.Version++
	}
	return w.data
}
