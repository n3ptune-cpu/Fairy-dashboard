package web

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/updates"
)

// The Updates page's "Check for updates" button: look the latest Fairy-agent and Fairy-dashboard releases up on
// GitHub now instead of waiting for the next background check (every 10 minutes), then answer with the page.
// Read-only, so no step-up and no kill switch; still admins in a browser session only (no API tokens), CSRF,
// once per updateCheckEvery for the whole panel (it reaches GitHub, whose API is rate limited per address),
// audited (update_check). The lookups use the release check's own client: GitHub hosts only, size and time limits.

// updateCheckEvery is how often the panel may be asked to check GitHub (all admins together).
const updateCheckEvery = 30 * time.Second

// updateCheckState is the panel-wide limit of Check for updates.
type updateCheckState struct {
	mu      sync.Mutex
	last    time.Time // the last check started (zero: none since the panel started)
	running bool
}

// reserve starts a check at now, or says how many seconds to wait (and whether one is running).
func (u *updateCheckState) reserve(now time.Time) (wait int, running bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if d := updateCheckEvery - now.Sub(u.last); !u.last.IsZero() && d > 0 {
		return int(math.Ceil(d.Seconds())), u.running
	}
	u.last, u.running = now, true
	return 0, false
}

func (u *updateCheckState) done() {
	u.mu.Lock()
	u.running = false
	u.mu.Unlock()
}

// status is what the Updates page shows next to the button: the last check and the seconds until the next one
// is allowed (0: now).
func (u *updateCheckState) status(now time.Time) (*time.Time, int, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.last.IsZero() {
		return nil, 0, false
	}
	at := u.last.UTC()
	wait := 0
	if d := updateCheckEvery - now.Sub(u.last); d > 0 {
		wait = int(math.Ceil(d.Seconds()))
	}
	return &at, wait, u.running
}

type updateCheckResult struct {
	OK        bool        `json:"ok"`
	Message   string      `json:"message"` // "Up to date", "2 updates available", or what went wrong
	CheckedAt time.Time   `json:"checked_at"`
	Page      updatesPage `json:"page"`
}

func (s *Server) updateCheck(c *reqCtx) error {
	u, err := c.sessionCSRF()
	if err != nil {
		return err
	}
	if _, g, _ := c.grant(); !g.Admin() {
		return c.deny(u, 403, "only admins can check for updates", "update check denied")
	}
	if s.Updates == nil && s.PanelUpdates == nil {
		return apierr.New(409, "the release check is off (updates.check_latest: false)")
	}
	now := time.Now()
	if wait, running := s.checks.reserve(now); wait > 0 {
		c.w.Header().Set("Retry-After", strconv.Itoa(wait))
		msg := fmt.Sprintf("Checked less than %d s ago; try again in %d s.", int(updateCheckEvery.Seconds()), wait)
		if running {
			msg = "A check is already running."
		}
		return apierr.New(429, map[string]any{"code": "update_check_too_soon", "retry_after_s": wait, "message": msg})
	}
	defer s.checks.done()

	var agent, panel updates.Latest
	var wg sync.WaitGroup
	for _, x := range []struct {
		src updates.Source
		out *updates.Latest
	}{{s.Updates, &agent}, {s.PanelUpdates, &panel}} {
		if x.src == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			*x.out = updates.CheckNow(c.r.Context(), x.src)
		}()
	}
	wg.Wait()
	s.checks.done() // before the page, which shows whether a check is running

	page := s.updatesPage(c)
	var errs []string
	if s.Updates != nil && agent.Error != "" {
		errs = append(errs, "Fairy-agent: "+agent.Error)
	}
	if s.PanelUpdates != nil && panel.Error != "" {
		errs = append(errs, "panel: "+panel.Error)
	}
	res := updateCheckResult{OK: len(errs) == 0, CheckedAt: now.UTC(), Page: page}
	switch {
	case len(errs) > 0:
		res.Message = strings.Join(errs, "; ")
	case page.Behind == 0:
		res.Message = "Up to date"
	case page.Behind == 1:
		res.Message = "1 update available"
	default:
		res.Message = fmt.Sprintf("%d updates available", page.Behind)
	}
	params := map[string]any{"agent_latest": releaseLabel(agent), "panel_latest": releaseLabel(panel), "behind": page.Behind}
	outcome := "ok"
	if !res.OK {
		outcome = "error: " + res.Message
	}
	c.audit(u, "update_check", "github", params, outcome)
	c.json(200, res)
	return nil
}

// releaseLabel is a release for the audit log: its signed version, else its tag ("" when unknown).
func releaseLabel(l updates.Latest) string {
	if l.Version != "" {
		return l.Version
	}
	return l.Tag
}
