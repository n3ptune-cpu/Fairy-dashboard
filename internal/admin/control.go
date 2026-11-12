package admin

import (
	"errors"
	"flag"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/health"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/managed"
)

func (c *cli) control(args []string) (int, error) {
	if len(args) == 0 || (args[0] != "disable" && args[0] != "enable") {
		return 2, errUsage
	}
	fs := flag.NewFlagSet("control", flag.ContinueOnError)
	fs.SetOutput(c.errw)
	reason := fs.String("reason", "", "shown in the UI banner and the audit log")
	if err := fs.Parse(args[1:]); err != nil {
		return 2, errUsage
	}
	if err := c.load(); err != nil {
		return 1, err
	}
	disable := args[0] == "disable"
	if err := adminstate.New(c.cfg.DataDir).SetControlDisabled(disable, osUser()+" (cli)", *reason); err != nil {
		return 1, err
	}
	params := map[string]any{}
	if *reason != "" {
		params["reason"] = *reason
	}
	c.log("control."+args[0], "", params)
	if disable {
		fmt.Fprintln(c.out, "Control DISABLED: fan, power, Wake-on-LAN and polling changes are refused for everyone")
		fmt.Fprintln(c.out, "(sessions and API tokens alike). The UI shows a banner. Re-enable with: fairy admin control enable")
	} else {
		fmt.Fprintln(c.out, "Control enabled again.")
	}
	return 0, nil
}

func (c *cli) host(args []string) (int, error) {
	if len(args) != 3 || args[1] != "power-control" || (args[2] != "disable" && args[2] != "enable") {
		return 2, errUsage
	}
	if err := c.load(); err != nil {
		return 1, err
	}
	st := adminstate.New(c.cfg.DataDir)
	// The switch is looked up by a system's id, exactly as the panel has it. The command took any word and
	// answered "Power control for <word>: disabled.": with the id of a Proxmox node's agent, or the node's
	// name, nothing was switched off, and the answer said it was (audit 6, r4-p2-05). An id the panel does
	// not have is refused. (Enabling an id that is in the list is always taken: it only takes an entry out.)
	listed, _ := st.PowerDisabledList()
	checked := true
	if !(args[2] == "enable" && slices.Contains(listed, args[0])) {
		var why string
		if checked, why = c.systemID(args[0]); why != "" {
			return 1, errors.New(why)
		}
	}
	if err := st.SetPowerDisabled(args[0], args[2] == "disable"); err != nil {
		return 1, err
	}
	c.log("host.power_control."+args[2], args[0], nil)
	state := "enabled"
	if args[2] == "disable" {
		state = "disabled"
	}
	fmt.Fprintf(c.out, "Power control for %s: %s.\n", cell(args[0]), state)
	if !checked {
		fmt.Fprintf(c.out, "(The panel has not written which systems it has (no health.json yet), so Fairy could not check that %s is one of them. "+
			"The switch counts for a system with exactly this id.)\n", cell(args[0]))
	}
	return 0, nil
}

// systemID checks an id that a command takes for a system of the panel. why is not "" when it is not one: a
// sentence that says so, and names the id to use when the word is the id of an agent that reports for another
// system (the agent of a Proxmox node). checked is false when the panel has not said which systems it has
// (health.json, written by the running panel every few seconds and kept when it stops): then only what the
// configuration shows to be another system's agent is refused.
func (c *cli) systemID(id string) (checked bool, why string) {
	// agents that report for another system: config.yaml's links_to, and hosts added in the panel
	other := map[string]string{}
	for _, a := range c.cfg.Agents {
		if a.LinksTo != "" && a.LinksTo != a.ID {
			other[a.ID] = a.LinksTo
		}
	}
	if hosts, err := managed.NewStore(c.cfg.DataDir).Load(); err == nil {
		for _, h := range hosts {
			if h.SystemID != "" && h.SystemID != h.ID {
				other[h.ID] = h.SystemID
			}
		}
	}
	h, err := health.Read(c.cfg.DataDir)
	known := err == nil && len(h.SystemIDs) > 0
	if known && slices.Contains(h.SystemIDs, id) {
		return true, ""
	}
	if sys := other[id]; sys != "" {
		return known, fmt.Sprintf("%s is the id of an agent that reports for the system %s: power control is switched by the system's id. "+
			"Nothing was changed. Use: fairy admin host %s power-control ...", cell(id), cell(sys), cell(sys))
	}
	if !known {
		return false, ""
	}
	return true, fmt.Sprintf("the panel has no system with the id %s, so nothing was changed. Power control is switched by a system's id as the panel has it "+
		"(a Proxmox node's is node-<name>, not the id of its agent): the ids are in the \"system_ids\" of %s", cell(id), health.Path(c.cfg.DataDir))
}

func (c *cli) poll(args []string) (int, error) {
	if len(args) != 2 {
		return 2, errUsage
	}
	id, v := args[0], args[1]
	var value any
	switch v {
	case "default":
	case "paused":
		value = "paused"
	default:
		f, err := strconv.ParseFloat(strings.TrimSuffix(v, "s"), 64)
		if err != nil {
			return 2, errors.New("value must be seconds (1-300), 'default' or 'paused'")
		}
		if f < 1 || f > 300 {
			return 2, errors.New("interval must be between 1 and 300 seconds")
		}
		value = f
	}
	if err := c.load(); err != nil {
		return 1, err
	}
	if h, err := health.Read(c.cfg.DataDir); err == nil && len(h.SystemIDs) > 0 {
		if !slices.Contains(h.SystemIDs, id) {
			fmt.Fprintf(c.errw, "warning: %q is not a known system id right now\n", id)
		}
	}
	if err := adminstate.New(c.cfg.DataDir).SetPoll(id, value); err != nil {
		return 1, err
	}
	c.log("poll.set", id, map[string]any{"interval_s": v})
	fmt.Fprintf(c.out, "Polling for %s: %s. The running panel applies it within a second.\n", cell(id), cell(v))
	return 0, nil
}
