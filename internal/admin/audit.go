package admin

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/audit"
)

// fmtEntry is one audit entry as one line. Every field is text someone else chose (an agent's error in the
// outcome, a name from the identity provider, the requested address in the target), so each goes through cell:
// a line break inside a field is printed as \n and can never look like another entry.
func fmtEntry(e map[string]any) string {
	ts := cell(fmt.Sprint(e["ts"]))
	if len(ts) > 19 {
		ts = ts[:19]
	}
	ts = strings.Replace(ts, "T", " ", 1)
	s := fmt.Sprintf("%s  %s %s %s %s %s", ts, pad(cut(e["source"], 16), 16), pad(cut(e["actor"], 24), 24),
		pad(cut(e["action"], 26), 26), pad(cut(e["target"], 28), 28), cell(fmt.Sprint(e["outcome"])))
	if ip, ok := e["ip"].(string); ok && ip != "" {
		s += "  [" + cell(ip) + "]"
	}
	// L4: the account id (OIDC subject) of who did it. The actor column is a name or address the account
	// claims at the identity provider; the id is the part its owner cannot choose.
	if sub, ok := e["sub"].(string); ok && sub != "" && sub != fmt.Sprint(e["actor"]) {
		s += "  id " + cell(sub)
	}
	return s
}

func (c *cli) audit(args []string) (int, error) {
	if len(args) == 0 || args[0] != "tail" {
		return 2, errUsage
	}
	fs := flag.NewFlagSet("audit tail", flag.ContinueOnError)
	fs.SetOutput(c.errw)
	n := fs.Int("n", 20, "entries")
	follow := fs.Bool("follow", false, "keep printing new entries")
	fs.BoolVar(follow, "f", false, "shorthand for --follow")
	asJSON := fs.Bool("json", false, "raw JSON lines")
	if err := fs.Parse(args[1:]); err != nil {
		return 2, errUsage
	}
	if err := c.load(); err != nil {
		return 1, err
	}
	show := func(e map[string]any) {
		if *asJSON {
			b, _ := json.Marshal(e)
			fmt.Fprintln(c.out, string(b))
		} else {
			fmt.Fprintln(c.out, fmtEntry(e))
		}
	}
	entries, _ := audit.Read(c.cfg.AuditLog, "", "", "", nil, min(max(1, *n), maxTail))
	for i := len(entries) - 1; i >= 0; i-- {
		show(entries[i])
	}
	if !*follow {
		return 0, nil
	}
	if err := audit.Follow(c.cfg.AuditLog, show, nil, 500*time.Millisecond); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(c.errw, "no audit log yet at %s\n", c.cfg.AuditLog)
			return 1, nil
		}
		return 1, err
	}
	return 0, nil
}

// maxTail is the most entries `audit tail -n` prints.
const maxTail = 10000
