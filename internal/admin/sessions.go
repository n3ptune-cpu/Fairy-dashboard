package admin

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
)

func (c *cli) sessions(args []string) (int, error) {
	if len(args) == 0 {
		return 2, errUsage
	}
	if err := c.load(); err != nil {
		return 1, err
	}
	st := adminstate.New(c.cfg.DataDir)
	switch args[0] {
	case "list":
		list, err := st.ListSessions()
		if err != nil {
			return 1, err
		}
		// What the Sessions page shows: when the person signed in (not when a "Confirm it's you" last renewed
		// the entry, r3-p1-09) and when the session was last used. An entry stays in sessions.json for up to
		// 30 days after its session ended by age or idleness; the panel refuses such a session, the page
		// leaves it out, and here it is marked, so that the list does not show sessions nobody can use as
		// if they were open. What this command cannot see is a use of the last minute or two: the file gets
		// it once a minute, so "ended" by idleness is said only two minutes past the timeout.
		now := float64(time.Now().Unix())
		at := func(ts float64) string {
			if ts <= 0 {
				return "not known"
			}
			t := time.Unix(int64(ts), 0)
			return ago(&t)
		}
		var rows [][]string
		ended := 0
		for _, s := range list {
			who := s.Email
			if who == "" {
				who = s.Name
			}
			if who == "" {
				who = s.Sub
			}
			ip := s.IP
			if ip == "" {
				ip = "?"
			}
			state := "open"
			switch {
			case s.SignedIn() > 0 && now-s.SignedIn() > float64(c.cfg.SessionMaxAge):
				state = "ended (signed in too long ago)"
			case s.SeenTS > 0 && now-s.SeenTS > float64(c.cfg.SessionIdleTimeout)+120:
				state = "ended (not used)"
			}
			if state != "open" {
				ended++
			}
			rows = append(rows, []string{s.SID[:min(8, len(s.SID))] + "…", who, ip, at(s.SignedIn()), at(s.SeenTS), state})
		}
		if len(rows) == 0 {
			fmt.Fprintln(c.out, "No active sessions.")
		} else {
			fmt.Fprintln(c.out, table(rows, []string{"SESSION", "USER", "IP", "SIGNED IN", "LAST USED", "STATE"}))
			if ended > 0 {
				fmt.Fprintf(c.out, "ended: the panel no longer takes this session (session_max_age, session_idle_timeout). Its entry goes when its browser comes back, or after 30 days.\n")
			}
		}
		return 0, nil
	case "revoke":
		if len(args) != 2 {
			return 2, errUsage
		}
		prefix := strings.TrimSuffix(args[1], "…")
		// whose sessions these are, for the audit log: the log names the person and the address, never a
		// session id or a part of one (README, Security model)
		var subs, ips []string
		if list, err := st.ListSessions(); err == nil && len(prefix) >= 6 {
			for _, s := range list {
				if strings.HasPrefix(s.SID, prefix) {
					if !slices.Contains(subs, s.Sub) {
						subs = append(subs, s.Sub)
					}
					if s.IP != "" && !slices.Contains(ips, s.IP) {
						ips = append(ips, s.IP)
					}
				}
			}
		}
		n, err := st.RevokeSession(prefix)
		if err != nil {
			return 2, err
		}
		if n == 0 {
			fmt.Fprintf(c.errw, "no session starting with %q\n", args[1])
			return 1, nil
		}
		c.log("sessions.revoke", strings.Join(subs, ", "), map[string]any{"count": n, "sub": strings.Join(subs, ", "), "ip": strings.Join(ips, ", ")})
		fmt.Fprintf(c.out, "Revoked %d session(s). That browser must sign in with Pocket ID again.\n", n)
		return 0, nil
	case "revoke-all":
		n, err := st.RevokeAllSessions()
		if err != nil {
			return 1, err
		}
		c.log("sessions.revoke_all", "", map[string]any{"count": n})
		fmt.Fprintf(c.out, "Revoked %d session(s). Every browser must sign in with Pocket ID again.\n", n)
		return 0, nil
	}
	return 2, errUsage
}
