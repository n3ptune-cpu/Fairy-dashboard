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
	if err := adminstate.New(c.cfg.DataDir).SetPowerDisabled(args[0], args[2] == "disable"); err != nil {
		return 1, err
	}
	c.log("host.power_control."+args[2], args[0], nil)
	state := "enabled"
	if args[2] == "disable" {
		state = "disabled"
	}
	fmt.Fprintf(c.out, "Power control for %s: %s.\n", cell(args[0]), state)
	return 0, nil
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
