// Package updates finds the newest Fairy-agent release for the Updates page: one GitHub lookup at most every
// 10 minutes (the signed VERSION file is checked when this build has the release key), never blocking a
// request, and harmless when the panel has no internet access.
package updates

import (
	"context"
	"crypto/ed25519"
	"strings"
	"sync"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/release"
)

// Latest is the newest release as the Updates page shows it.
type Latest struct {
	Tag       string     `json:"tag,omitempty"`
	Version   string     `json:"version,omitempty"` // from the signed VERSION file ("" when not verified)
	Build     *int       `json:"build"`             // release build number (from VERSION, else from the tag)
	Verified  bool       `json:"verified"`          // signature checked with this panel's release key
	URL       string     `json:"url,omitempty"`
	Published *time.Time `json:"published_at,omitempty"`
	CheckedAt *time.Time `json:"checked_at,omitempty"`
	Error     string     `json:"error,omitempty"` // why there is nothing (newer); the last good answer is kept
	Disabled  bool       `json:"disabled,omitempty"`
}

// Source answers the Updates page's "latest release" question.
type Source interface {
	Latest() Latest
}

// Static is a fixed answer (demo mode, or updates.check_latest: false).
type Static Latest

// Latest returns the fixed answer.
func (s Static) Latest() Latest { return Latest(s) }

// Checker looks the latest release up on GitHub in the background and caches it.
type Checker struct {
	Client  *release.Client
	Key     ed25519.PublicKey // nil: show the tag without checking VERSION
	Program string
	TTL     time.Duration // default 10 min
	ErrTTL  time.Duration // after a failure, default 2 min

	mu       sync.Mutex
	last     Latest
	at       time.Time
	ok       bool
	inflight bool
}

// NewChecker returns a checker for repo's releases of program.
func NewChecker(repo, program, userAgent string, key ed25519.PublicKey) (*Checker, error) {
	c, err := release.NewClient(repo, userAgent)
	if err != nil {
		return nil, err
	}
	return &Checker{Client: c, Key: key, Program: program}, nil
}

// Latest returns the cached answer and starts a refresh when it is old. It never waits for GitHub.
func (c *Checker) Latest() Latest {
	c.mu.Lock()
	defer c.mu.Unlock()
	ttl, errTTL := c.TTL, c.ErrTTL
	if ttl == 0 {
		ttl = 10 * time.Minute
	}
	if errTTL == 0 {
		errTTL = 2 * time.Minute
	}
	stale := c.at.IsZero() || (c.ok && time.Since(c.at) > ttl) || (!c.ok && time.Since(c.at) > errTTL)
	if stale && !c.inflight {
		c.inflight = true
		go c.refresh()
	}
	l := c.last
	if c.at.IsZero() {
		l.Error = "checking GitHub…"
	}
	return l
}

// Refresh looks the release up now and waits for the answer (tests, the CLI).
func (c *Checker) Refresh() Latest {
	c.refresh()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
}

func (c *Checker) refresh() {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	l, err := c.lookup(ctx)
	now := time.Now().UTC()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inflight, c.at = false, now
	if err != nil {
		c.ok = false
		c.last.Error = "cannot check GitHub: " + devices.Describe(err)
		return
	}
	l.CheckedAt = &now
	c.last, c.ok = l, true
}

func (c *Checker) lookup(ctx context.Context) (Latest, error) {
	var l Latest
	if len(c.Key) > 0 {
		ch, err := c.Client.Check(ctx, c.Key, c.Program)
		if err != nil {
			return l, err
		}
		l = fromRelease(ch.Release)
		l.Version, l.Verified = ch.Version, true
		if n, ok := release.BuildNumber(ch.Version); ok {
			l.Build = &n
		}
		return l, nil
	}
	rel, err := c.Client.Latest(ctx)
	if err != nil {
		return l, err
	}
	return fromRelease(rel), nil
}

func fromRelease(r *release.Release) Latest {
	l := Latest{Tag: r.Tag, URL: r.URL}
	if !r.Published.IsZero() {
		t := r.Published.UTC()
		l.Published = &t
	}
	if n, ok := release.BuildNumber(strings.TrimPrefix(r.Tag, "build-")); ok {
		l.Build = &n
	}
	return l
}
