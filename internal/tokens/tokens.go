// Package tokens manages personal API tokens (scripted access to /api with `Authorization: Bearer fy_...`).
//
// Created only with the admin CLI. Stored as SHA-256 hashes in <data_dir>/tokens.json; the plaintext is shown
// once. Scope `read` or `control`. Tokens never work on /auth/* or the settings endpoints.
//
// A read token may be limited to some sites (Sites); without a list it sees every site, as tokens did before
// sites existed. A token never sees more than its owner: when the owner is a user in users.json, the panel caps
// the token to that user's role and sites (see web.tokenGrant).
package tokens

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/n3ptune-cpu/Fairy/internal/statefile"
)

// Prefix starts every API token.
const Prefix = "fy_"

// Record is a stored token: never the token itself, only its SHA-256 hash and a 4-character hint.
type Record struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Owner     string     `json:"owner"`           // an OIDC sub, or "system"
	Scope     string     `json:"scope"`           // read | control
	Sites     []string   `json:"sites,omitempty"` // read tokens only: limited to these sites (nil = every site)
	CreatedAt time.Time  `json:"created_at"`
	CreatedBy string     `json:"created_by"`
	ExpiresAt *time.Time `json:"expires_at"`
	RevokedAt *time.Time `json:"revoked_at"`
	LastUsed  *time.Time `json:"last_used"`
	Hint      string     `json:"hint"` // last 4 characters, for recognising a token in listings
	Hash      string     `json:"hash"`
}

// Active reports whether the token is neither revoked nor expired.
func (r Record) Active() bool {
	return r.RevokedAt == nil && (r.ExpiresAt == nil || r.ExpiresAt.After(time.Now()))
}

// HashToken is the stored form of a token (hex SHA-256).
func HashToken(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

// Store is tokens.json. Every lookup re-checks the file, so a revoke from the CLI applies at once.
type Store struct {
	Path    string
	watch   *statefile.Watched
	mu      sync.Mutex
	touched map[string]time.Time
}

// NewStore returns the token store in dataDir.
func NewStore(dataDir string) *Store {
	p := filepath.Join(dataDir, "tokens.json")
	return &Store{Path: p, watch: statefile.NewWatchedEvery(p, 0), touched: map[string]time.Time{}}
}

func records(d statefile.Data) map[string]Record {
	out := map[string]Record{}
	toks, _ := d["tokens"].(map[string]any)
	for id, raw := range toks {
		b, _ := json.Marshal(raw)
		var r Record
		if json.Unmarshal(b, &r) == nil && r.Hash != "" && (r.Scope == "read" || r.Scope == "control") {
			out[id] = r
		}
	}
	return out
}

// List returns every token record (active, expired and revoked).
func (s *Store) List() []Record {
	out := []Record{}
	for _, r := range records(statefile.Read(s.Path)) {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

func toMap(r Record) map[string]any {
	b, _ := json.Marshal(r)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

// Create returns the plaintext token (shown once) and its record. The token sees every site.
func (s *Store) Create(name, owner, scope, createdBy string, expiresDays int) (string, Record, error) {
	return s.CreateForSites(name, owner, scope, createdBy, expiresDays, nil)
}

// CreateForSites is Create for a read token limited to some sites (nil = every site). Control tokens act as
// an admin and cannot be limited.
func (s *Store) CreateForSites(name, owner, scope, createdBy string, expiresDays int, sites []string) (string, Record, error) {
	if name == "" || len(name) > 64 {
		return "", Record{}, errors.New("name must be 1-64 characters")
	}
	if scope != "read" && scope != "control" {
		return "", Record{}, errors.New("scope must be read or control")
	}
	if sites != nil && scope != "read" {
		return "", Record{}, errors.New("only read tokens can be limited to sites (a control token acts as an admin)")
	}
	if sites != nil && len(sites) == 0 {
		return "", Record{}, errors.New("give at least one site")
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", Record{}, err
	}
	raw := Prefix + base64.RawURLEncoding.EncodeToString(buf)
	idb := make([]byte, 4)
	_, _ = rand.Read(idb)
	now := time.Now().UTC()
	r := Record{ID: hex.EncodeToString(idb), Name: name, Owner: owner, Scope: scope, CreatedAt: now,
		CreatedBy: createdBy, Hint: raw[len(raw)-4:], Hash: HashToken(raw), Sites: sites}
	if expiresDays > 0 {
		e := now.Add(time.Duration(expiresDays) * 24 * time.Hour)
		r.ExpiresAt = &e
	}
	_, err := statefile.Update(s.Path, func(d statefile.Data) error {
		toks, _ := d["tokens"].(map[string]any)
		if toks == nil {
			toks = map[string]any{}
			d["tokens"] = toks
		}
		for _, t := range toks {
			m, _ := t.(map[string]any)
			if m["name"] == name && m["revoked_at"] == nil {
				return fmt.Errorf("an active token named %q already exists", name)
			}
		}
		toks[r.ID] = toMap(r)
		return nil
	})
	if err != nil {
		return "", Record{}, err
	}
	s.watch.Refresh()
	return raw, r, nil
}

// ErrNotFound means no active token has that id or name.
var ErrNotFound = errors.New("no active token with that id or name")

// Revoke by id or name.
func (s *Store) Revoke(ident string) (Record, error) {
	var found map[string]any
	_, err := statefile.Update(s.Path, func(d statefile.Data) error {
		toks, _ := d["tokens"].(map[string]any)
		ids := make([]string, 0, len(toks))
		for id := range toks {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			m, _ := toks[id].(map[string]any)
			if (id == ident || m["name"] == ident) && m["revoked_at"] == nil {
				m["revoked_at"] = time.Now().UTC().Format(time.RFC3339Nano)
				found = m
				return nil
			}
		}
		return ErrNotFound
	})
	if err != nil {
		return Record{}, err
	}
	s.watch.Refresh()
	b, _ := json.Marshal(found)
	var r Record
	_ = json.Unmarshal(b, &r)
	return r, nil
}

// Verify returns the active record for a presented token (constant-time hash comparison).
func (s *Store) Verify(raw string) *Record {
	if !strings.HasPrefix(raw, Prefix) || len(raw) > 100 {
		return nil
	}
	h := []byte(HashToken(raw))
	for _, r := range records(s.watch.Get()) {
		if subtle.ConstantTimeCompare([]byte(r.Hash), h) == 1 {
			if r.Active() {
				rr := r
				return &rr
			}
			return nil
		}
	}
	return nil
}

// Touch records last use, at most once a minute per token.
func (s *Store) Touch(r *Record) {
	s.mu.Lock()
	if time.Since(s.touched[r.ID]) < time.Minute {
		s.mu.Unlock()
		return
	}
	s.touched[r.ID] = time.Now()
	s.mu.Unlock()
	_, _ = statefile.Update(s.Path, func(d statefile.Data) error {
		toks, _ := d["tokens"].(map[string]any)
		if t, ok := toks[r.ID].(map[string]any); ok {
			t["last_used"] = time.Now().UTC().Format(time.RFC3339Nano)
		}
		return nil
	})
}
