package admin

import (
	"fmt"
	"slices"
	"strings"

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
		var rows [][]string
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
			rows = append(rows, []string{s.SID[:min(8, len(s.SID))] + "…", who, ip, agoISO(s.Created)})
		}
		if len(rows) == 0 {
			fmt.Fprintln(c.out, "No active sessions.")
		} else {
			fmt.Fprintln(c.out, table(rows, []string{"SESSION", "USER", "IP", "SIGNED IN"}))
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
