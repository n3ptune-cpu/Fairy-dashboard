package admin

import (
	"errors"
	"fmt"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/configutil"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/access"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/managed"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
)

// secretEnv is an environment variable that must hold a secret, and what it is for.
type secretEnv struct{ what, env string }

// secretEnvs lists the secret variables the config refers to: agent read and control tokens, then Proxmox
// read-only and power tokens.
func secretEnvs(cfg *config.Panel) []secretEnv {
	var envs []secretEnv
	for _, a := range cfg.Agents {
		envs = append(envs, secretEnv{"agent " + a.ID, a.TokenEnv})
	}
	for _, a := range cfg.Agents {
		if e := a.ControlEnv(); e != "" {
			envs = append(envs, secretEnv{"agent " + a.ID + " control", e})
		}
	}
	for _, p := range cfg.Proxmox {
		e := p.TokenSecretEnv
		if e == "" {
			e = "FAIRY_PROXMOX_TOKEN_SECRET"
		}
		envs = append(envs, secretEnv{"proxmox " + p.ID, e})
	}
	for _, p := range cfg.Proxmox {
		if p.PowerTokenSecretEnv != "" {
			envs = append(envs, secretEnv{"proxmox " + p.ID + " power", p.PowerTokenSecretEnv})
		}
	}
	return envs
}

// configCheck validates config.yaml and the environment. Secrets are reported as present/missing only.
func (c *cli) configCheck() int {
	var problems, notes []string
	cfg, err := config.LoadFull(c.cfgPath) // the one check of the file: what `fairy serve` starts with
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(c.out, "FAIL  config not found: %s\n", c.cfgPath)
		} else {
			fmt.Fprintf(c.out, "FAIL  config %s is invalid:\n%v\n", c.cfgPath, err)
		}
		return 1
	}
	notes = append(notes, fmt.Sprintf("config %s: valid", c.cfgPath))
	if p := config.SessionSecretProblem(os.Getenv("FAIRY_SESSION_SECRET")); p != "" {
		// too short, the example value or one character repeated: `fairy serve` does not start with it
		problems = append(problems, "FAIRY_SESSION_SECRET: "+p)
	} else {
		notes = append(notes, "FAIRY_SESSION_SECRET: present")
	}
	if cfg.OIDC.Issuer == "" {
		problems = append(problems, "oidc.issuer is empty: there is no Pocket ID to sign in with")
	} else {
		notes = append(notes, fmt.Sprintf("oidc.issuer: %s  (callback %s/auth/callback)", cfg.OIDC.Issuer, cfg.BaseURL()))
	}
	if os.Getenv("FAIRY_OIDC_CLIENT_SECRET") != "" {
		notes = append(notes, "FAIRY_OIDC_CLIENT_SECRET: present")
	} else {
		notes = append(notes, "FAIRY_OIDC_CLIENT_SECRET: not set (public client: PKCE only)")
	}
	if !strings.HasPrefix(cfg.PublicURL, "https://") {
		problems = append(problems, "public_url "+cfg.BaseURL()+" is not https")
	}
	if len(cfg.OIDC.AllowedGroups) == 0 {
		problems = append(problems, "oidc.allowed_groups is empty: any Pocket ID user of this client may sign in")
	}
	limit := fmt.Sprintf("control_rate_per_min: %d control requests a minute for one admin", cfg.ControlRatePerMin)
	if cfg.ControlRatePerMin != config.DefaultControlRatePerMin { // the example file used to set 6: such a file keeps it
		limit += fmt.Sprintf(" (set in config.yaml; without the line it is %d)", config.DefaultControlRatePerMin)
	}
	notes = append(notes, limit)
	// "Run timings": what an admin changed in the panel about the timings of "Update unpowered hosts"
	// (state.json). The lines below about a timing are about the value in force.
	rt, rtOK, rtWarn, rtSaid := runTimingsCheck(cfg)
	problems, notes = approvalCheck(cfg, rt, problems, notes)
	problems, notes = adminCheck(cfg, problems, notes)
	problems, notes = managedCheck(cfg, problems, notes)
	problems, inferred := nodeLinkCheck(cfg, problems)
	okLines, said := wakeUpdateCheck(cfg, rt, hostRuns(cfg))
	notes, inferred = append(notes, okLines...), append(inferred, said...)
	notes, problems, inferred = append(notes, rtOK...), append(problems, rtWarn...), append(inferred, rtSaid...)
	inferred = append(inferred, systemIDCheck(cfg)...)
	okLines, said = powerLinkCheck(cfg)
	notes, inferred = append(notes, okLines...), append(inferred, said...)
	okLines, said = socketLinkCheck(cfg) // what hangs on the sockets of those plugs, and where the agent's Home Assistant runs
	notes, inferred = append(notes, okLines...), append(inferred, said...)
	okLines, ignored, said := hostOptionsCheck(cfg) // "Edit host": what the panel changed about these links (state.json)
	notes, problems, inferred = append(notes, okLines...), append(problems, ignored...), append(inferred, said...)
	dayLine, said := powerDayCheck(cfg) // the power sources' last 24 hours: where they are kept (power-day.json)
	notes, inferred = append(notes, dayLine), append(inferred, said...)
	for _, e := range secretEnvs(cfg) {
		if os.Getenv(e.env) != "" {
			notes = append(notes, fmt.Sprintf("%s (%s): present", e.env, e.what))
		} else {
			problems = append(problems, fmt.Sprintf("%s (%s): missing", e.env, e.what))
		}
	}
	problems = append(problems, config.EnvProblems()...) // a comment behind a value in fairy.env
	on := "off"
	if os.Getenv("FAIRY_NOTIFY_URL") != "" {
		on = "on"
	}
	notes = append(notes, "notifications: "+on+" (FAIRY_NOTIFY_URL)")
	for _, n := range notes {
		fmt.Fprintln(c.out, "ok    "+n)
	}
	for _, n := range cfg.Ignored() { // read, not used, and never a reason for exit code 1
		fmt.Fprintln(c.out, "note  "+n)
	}
	for _, n := range inferred { // a node worked out from an id_prefix, a short off_after_s: said, and never a reason for exit code 1
		fmt.Fprintln(c.out, "note  "+n)
	}
	for _, p := range problems {
		fmt.Fprintln(c.out, "WARN  "+p)
	}
	if len(problems) > 0 {
		return 1
	}
	return 0
}

// approvalCheck explains the approval: block: the Pocket ID client "Fairy PC actions", with which a person
// approves a fan, power or update action for the machine itself to verify (README: "The check on the machine
// itself"). The block is optional. Its secret is reported as present / not set only.
func approvalCheck(cfg *config.Panel, rt adminstate.RunTimings, problems, notes []string) ([]string, []string) {
	if !cfg.ApprovalOn() {
		return problems, append(notes, "approval: not set. Machines that check a passkey approval themselves cannot be switched on "+
			"for that check or controlled from this panel (approval: {client_id: ...}, the Client ID of a separate client for PC actions in Pocket ID)")
	}
	iss := cfg.ApprovalIssuer()
	from := "approval.issuer"
	if cfg.Approval.Issuer == "" {
		from = "oidc.issuer"
	}
	notes = append(notes, fmt.Sprintf("approval.client_id: %s  (at %s, from %s; it must be the Client ID of the separate client for PC actions in Pocket ID, not the sign-in client's)", cfg.Approval.ClientID, iss, from),
		fmt.Sprintf("approval callback to register in Pocket ID for that client: %s  (Requires Re-Authentication: ON, PKCE: ON)", cfg.ApprovalCallback()))
	if why := cfg.ApprovalIssuerProblem(); why != "" { // the words of the Approval dialog's "Not ready" and of the log at start
		problems = append(problems, "approval: "+why+"; it is "+iss+". The machines only accept an https issuer, so the check "+
			"cannot be switched on for any of them")
	}
	if os.Getenv(config.ApprovalSecretEnv) != "" {
		notes = append(notes, config.ApprovalSecretEnv+": present")
	} else {
		notes = append(notes, config.ApprovalSecretEnv+": not set (public client: PKCE only)")
	}
	notes = append(notes, fmt.Sprintf("approval.users: %d more than the panel's admins and a host's owner; approval.owners: %d owner labels "+
		"with a Pocket ID user (they may approve on a machine that is switched on for the check from now on; a machine that already has "+
		"its list is changed only on the machine)", len(cfg.Approval.Users), len(cfg.Approval.Owners)),
		fmt.Sprintf("approval.run_approval_wait_s: %d%s (how long a run of \"Update unpowered hosts\" asks for its second passkey once the woken machines that check approvals are up; "+
			"not confirmed in that time: not updated, and a machine that was armed switches itself off again)",
			inForce(cfg, rt, config.KeyRunApprovalWait), fromPanel(cfg, rt, config.KeyRunApprovalWait)))
	return problems, notes
}

// managedCheck reports the hosts added in the panel (Add host): managed-hosts.yaml and their tokens in the data
// folder, validated like config.yaml's own entries (a host with the same id in config.yaml wins).
func managedCheck(cfg *config.Panel, problems, notes []string) ([]string, []string) {
	st := managed.NewStore(cfg.DataDir)
	c := *cfg
	c.Agents, c.Systems = slices.Clone(cfg.Agents), slices.Clone(cfg.Systems)
	added, skipped, err := st.Merge(&c)
	if err != nil {
		return append(problems, "hosts added in the panel: "+err.Error()), notes
	}
	for _, e := range added {
		where := e.Agent.URL
		if e.Host.Node != "" {
			where += ", on node " + e.Host.Node
		}
		notes = append(notes, fmt.Sprintf("managed host %s (%s): valid, tokens present  (%s)", e.Host.ID, e.Host.Name, where))
	}
	if len(added) == 0 && len(skipped) == 0 {
		notes = append(notes, "managed hosts (Add host): none ("+st.Path()+")")
	}
	return append(problems, skipped...), notes
}

// nodeLinkCheck is the warning `fairy serve` logs at its start for an agent on a Proxmox node (node:) when the
// configuration does not say which node that is although a `systems:` entry is written under an id the node
// can have: after a restart of the panel while that machine is off, its wol: or bmc_power: is then not found
// (poller.NodeWarnings has the rule). The hosts added in the panel are part of the question, as at the start;
// what is wrong with their list itself, managedCheck has said.
//
// inferred is the other thing the start logs: one line for every agent whose node was worked out from an
// id_prefix (poller.NodeInferred), so that no link is guessed without a word, and one for every agent whose
// node is not decided and for which there is no warning (poller.NodeUndecided: no `systems:` entry is written
// for its node), which says where the rows put that agent. They are notes, not warnings.
func nodeLinkCheck(cfg *config.Panel, problems []string) (out, inferred []string) {
	c := *cfg
	c.Agents, c.Systems, c.Proxmox = slices.Clone(cfg.Agents), slices.Clone(cfg.Systems), slices.Clone(cfg.Proxmox)
	_, _, _ = managed.NewStore(cfg.DataDir).Merge(&c)
	return append(problems, poller.NodeWarnings(&c)...), append(poller.NodeInferred(&c), poller.NodeUndecided(&c)...)
}

// systemIDCheck is one note for every `systems:` entry of config.yaml whose id is no machine's: not the id of
// an agent (config.yaml's or one added in the panel), and not of the form a node or a guest of one of the
// proxmox: entries has (<id_prefix>node-<name>, <id_prefix>qemu-<vmid>, <id_prefix>lxc-<vmid>). Such an entry
// is read and counts for nothing, which is what a typing mistake in the id looks like: the machine it was
// written for runs with the panel's own values, and nothing else says so. A note, not a warning: whether a
// node or guest of that name exists is only known once its Proxmox API has answered, and an entry may be
// written before its machine is added.
func systemIDCheck(cfg *config.Panel) []string {
	c := *cfg
	c.Agents, c.Systems, c.Proxmox = slices.Clone(cfg.Agents), slices.Clone(cfg.Systems), slices.Clone(cfg.Proxmox)
	_, _, _ = managed.NewStore(cfg.DataDir).Merge(&c)
	known := map[string]bool{}
	for _, a := range c.Agents {
		known[a.ID] = true
	}
	var out []string
	for _, o := range cfg.Systems {
		fits := known[o.ID]
		for _, p := range c.Proxmox {
			for _, kind := range []string{"node-", "qemu-", "lxc-"} {
				fits = fits || (strings.HasPrefix(o.ID, p.Prefix()+kind) && len(o.ID) > len(p.Prefix()+kind))
			}
		}
		if fits {
			continue
		}
		what := "the entry counts for no machine"
		if o.WakeUpdate != nil {
			what = "the entry and its wake_update: block count for no machine (the machine it was written for has the panel's own timings)"
		}
		out = append(out, fmt.Sprintf("system %s: no agent has this id, and it is not the id of a node or a guest of a proxmox: entry "+
			"(<id_prefix>node-<name>, <id_prefix>qemu-<vmid>, <id_prefix>lxc-<vmid>): %s. A typing mistake in the id?", o.ID, what))
	}
	return out
}

// powerLinkCheck is about the plugs an agent relays, as the panel's entry of that agent links them
// (power_links: {<device id on the agent>: <system id>}) and leaves them out of the total
// (power_exclude_from_total: [<device id>]). One ok line per agent says what is set, and notes say what cannot
// work:
//
//   - a device id that no device can have (not an id as the agent's own power_sources take them);
//   - power_links to an id that is no machine's (no agent's id, not of the form a node or a guest has);
//   - power_links to the id of a node whose agent the configuration does not decide: the watts are on the
//     node's row while that row is there, and on no row after a restart of the panel with the machine off
//     (poller.ShownID follows a machine to its agent's row only where the configuration says whose it is).
//
// Whether the agent has a device with that id is not known here: this command reads files and asks no
// machine. The running panel knows it from the agent's answers and says it once in its log (power.Manager,
// UpdateRemote: "... names devices the agent does not report"); the ok line says where to look.
//
// Notes, not warnings: nothing is refused, and the exit code stays as it is.
func powerLinkCheck(cfg *config.Panel) (ok, said []string) {
	c := *cfg
	c.Agents, c.Systems, c.Proxmox = slices.Clone(cfg.Agents), slices.Clone(cfg.Systems), slices.Clone(cfg.Proxmox)
	_, _, _ = managed.NewStore(cfg.DataDir).Merge(&c)
	agents := map[string]bool{}
	for _, a := range c.Agents {
		agents[a.ID] = true
	}
	upsEntries := map[string]bool{}
	for _, raw := range cfg.UPS {
		if id, _ := raw["id"].(string); id != "" {
			upsEntries[id] = true
		}
	}
	_, undecided := poller.NodeAgents(&c)
	form := func(id string, kinds ...string) bool {
		for _, p := range c.Proxmox {
			for _, kind := range kinds {
				if strings.HasPrefix(id, p.Prefix()+kind) && len(id) > len(p.Prefix()+kind) {
					return true
				}
			}
		}
		return false
	}
	sorted := func(m map[string]string) []string {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		return keys
	}
	for _, a := range cfg.Agents {
		if len(a.PowerLinks)+len(a.PowerExcludeFromTotal) == 0 {
			continue
		}
		var parts []string
		for _, dev := range sorted(a.PowerLinks) {
			parts = append(parts, dev+" -> "+a.PowerLinks[dev])
		}
		line := "agent " + a.ID + ": devices it relays"
		if len(parts) > 0 {
			line += ": linked " + strings.Join(parts, ", ")
		}
		if ex := slices.Sorted(slices.Values(a.PowerExcludeFromTotal)); len(ex) > 0 {
			line += "; not in the total: " + strings.Join(ex, ", ")
		}
		ok = append(ok, line+" (the device ids are the agent's own: the running panel says in its log when the agent reports no device with such an id)")
		for _, dev := range slices.Sorted(slices.Values(append(sorted(a.PowerLinks), a.PowerExcludeFromTotal...))) {
			if !configutil.ValidID(dev) {
				said = append(said, fmt.Sprintf("agent %s: %q in power_links / power_exclude_from_total is not an id a device can have "+
					"(lowercase letters, digits and dashes, as in the agent's own power_sources): nothing is linked or left out for it", a.ID, dev))
			}
		}
		for _, dev := range sorted(a.PowerLinks) {
			to := a.PowerLinks[dev]
			switch {
			case to == "" || agents[to]:
			case len(undecided[to]) > 0:
				said = append(said, fmt.Sprintf("agent %s: power_links: device %s is linked to the node %s, and the configuration does not say which agent's machine that node is "+
					"(it can be the node of %s): the watts are on the node's row while it is there, and on no row after a restart of the panel with the machine off. "+
					"To say whose node it is, write links_to: %s in that agent's entry", a.ID, dev, to, strings.Join(undecided[to], " or of "), to))
			case form(to, "node-", "qemu-", "lxc-"):
			default:
				said = append(said, fmt.Sprintf("agent %s: power_links: device %s is linked to %s, which is no agent's id and not the id of a node or a guest of a proxmox: entry: "+
					"its watts are on no row. A typing mistake in the id?", a.ID, dev, to))
			}
		}
	}
	return ok, said
}

// socketLinkCheck is about the sockets of the plugs an agent relays, as the panel's entry of that agent says
// what hangs on them (socket_links: {"<device id>/<socket id>": <row id>}) and where the Home Assistant runs
// that the agent reads and switches them through (home_assistant_system: <row id>). One ok line per agent that
// has one of the two says what is set and what the panel does with it; notes say what cannot work as written:
// a row id that is no machine's (no agent's id, not of the form a node or a guest has).
//
// A line of the wrong form never gets here: config.yaml does not load with it (config.SocketLinkProblems).
// Whether the agent has such a socket is not known here: this command reads files and asks no machine. The
// running panel says it once in its log (power.Manager, NoteSocketLinks). Whether a socket can be switched at
// all is not in this file: that is set on the machine that reads the plug, in the agent's own config.yaml.
//
// Notes, not warnings: nothing is refused, and the exit code stays as it is.
func socketLinkCheck(cfg *config.Panel) (ok, said []string) {
	c := *cfg
	c.Agents, c.Systems, c.Proxmox = slices.Clone(cfg.Agents), slices.Clone(cfg.Systems), slices.Clone(cfg.Proxmox)
	_, _, _ = managed.NewStore(cfg.DataDir).Merge(&c)
	machine := func(id string) bool {
		for _, a := range c.Agents {
			if a.ID == id {
				return true
			}
		}
		for _, p := range c.Proxmox {
			for _, kind := range []string{"node-", "qemu-", "lxc-"} {
				if strings.HasPrefix(id, p.Prefix()+kind) && len(id) > len(p.Prefix()+kind) {
					return true
				}
			}
		}
		return false
	}
	for _, a := range cfg.Agents {
		if len(a.SocketLinks) == 0 && a.HomeAssistantSystem == "" {
			continue
		}
		names := slices.Sorted(maps.Keys(a.SocketLinks))
		var parts []string
		for _, name := range names {
			parts = append(parts, name+" -> "+a.SocketLinks[name])
		}
		line := "agent " + a.ID + ": sockets of the plugs it relays"
		if len(parts) > 0 {
			line += ": " + strings.Join(parts, ", ")
		}
		if a.HomeAssistantSystem != "" {
			line += "; its Home Assistant runs on " + a.HomeAssistantSystem
		}
		ok = append(ok, line+" (the panel asks before such a socket is switched off while its machine runs, and never switches the socket of the machine that reads the plugs, "+
			"of Home Assistant's machine or of its own; whether a socket can be switched at all is set in the agent's own config.yaml. "+
			"The running panel says in its log when the agent reports no socket with such a name)")
		for _, name := range names {
			if to := a.SocketLinks[name]; !machine(to) {
				said = append(said, fmt.Sprintf("agent %s: socket_links: socket %s is linked to %s, which is no agent's id and not the id of a node or a guest of a proxmox: entry. "+
					"The panel then cannot tell whether that machine runs: to switch the socket off, the id has to be typed. A typing mistake in the id?", a.ID, name, to))
			}
		}
		if h := a.HomeAssistantSystem; h != "" && !machine(h) {
			said = append(said, fmt.Sprintf("agent %s: home_assistant_system: %s is no agent's id and not the id of a node or a guest of a proxmox: entry. "+
				"While no row has this id the panel cannot tell which socket feeds Home Assistant, and switches no socket of this agent that is linked to a machine. "+
				"A typing mistake in the id?", a.ID, h))
		}
	}
	return ok, said
}

// wakeUpdateCheck lists the timings of "Update unpowered hosts" that are not the defaults a reader would
// expect: the panel's off_after_s when it is not 900, and every machine that has timings of its own (the
// wake_update: block of its systems: entry). ok lines are what is set; said is one note for every time
// below the default after which a machine counts as off, with what that risks. A note is no warning: the
// value is allowed, it is the owner's decision, and it does not change the exit code.
//
// One note more for a countdown (shutdown_delay_s, the panel's or a machine's own) that is also one a person
// gets from the panel: the Power menu offers 10 s, 1, 5 and 15 minutes, and a power request without a delay
// gets 60 s. A run knows its own countdown from a shutdown somebody asked for in its place by the length
// alone (internal/wakerun, callOff), so with such a value the two cannot be told apart, and a run that is
// cancelled or lets go of the machine calls the person's shutdown off (third review of build 14, S7).
//
// (build 16) The notes are about the values IN FORCE: a timing that was changed in the panel ("Run timings",
// rt) is named as that, and config.yaml's own line about off_after_s is left to runTimingsCheck, which says
// both values. A machine's own timing that was changed in "Edit host" (runs) is said in the line of its
// systems: entry, and the note about its value is the one of the host's line of "Edit host"
// (hostrun_check.go).
func wakeUpdateCheck(cfg *config.Panel, rt adminstate.RunTimings, runs machineRuns) (ok, said []string) {
	const def = 900
	same := countdownSame
	if v := inForce(cfg, rt, config.KeyShutdownDelay); personCountdown[v] {
		said = append(said, same("wake_update.shutdown_delay_s"+fromPanel(cfg, rt, config.KeyShutdownDelay), v))
	}
	mins := func(s int) string {
		if s%60 == 0 {
			return fmt.Sprintf("%d min", s/60)
		}
		return fmt.Sprintf("%d s", s)
	}
	risk := offAfterRisk
	_, changed := rt.Value(config.KeyOffAfter)
	if v := cfg.WakeUpdate.OffAfterS; v != 0 && v != def && !changed {
		ok = append(ok, fmt.Sprintf("wake_update.off_after_s: %d (a machine counts as off after %s without an answer; without the line it is %d)", v, mins(v), def))
	}
	if v := inForce(cfg, rt, config.KeyOffAfter); v != 0 && v < def {
		said = append(said, risk("wake_update.off_after_s"+fromPanel(cfg, rt, config.KeyOffAfter), v))
	}
	for _, o := range cfg.Systems {
		w := o.WakeUpdate
		if w == nil || (w.OffAfterS == 0 && w.ShutdownDelayS == 0) {
			continue
		}
		// a key the panel decided otherwise for this machine ("Edit host") is not in force: said here, and the
		// note about a value is made from the one in force (the host's line has it)
		panel := func(key string) string {
			if v := runs.decision(o.ID, key); v != 0 {
				return fmt.Sprintf("; NOT in force: changed in the panel (\"Edit host\") to %d", v)
			}
			return ""
		}
		var parts []string
		if w.OffAfterS != 0 {
			parts = append(parts, fmt.Sprintf("off_after_s %d (it counts as off after %s without an answer%s)", w.OffAfterS, mins(w.OffAfterS), panel(config.KeyOffAfter)))
		}
		if w.ShutdownDelayS != 0 {
			parts = append(parts, fmt.Sprintf("shutdown_delay_s %d (the countdown of its shutdown in a run%s)", w.ShutdownDelayS, panel(config.KeyShutdownDelay)))
		}
		ok = append(ok, fmt.Sprintf("system %s: wake_update of its own: %s", o.ID, strings.Join(parts, ", ")))
		if w.OffAfterS != 0 && w.OffAfterS < def && panel(config.KeyOffAfter) == "" {
			said = append(said, risk("system "+o.ID+": wake_update.off_after_s", w.OffAfterS))
		}
		if personCountdown[w.ShutdownDelayS] && panel(config.KeyShutdownDelay) == "" {
			said = append(said, same("system "+o.ID+": wake_update.shutdown_delay_s", w.ShutdownDelayS))
		}
	}
	return ok, said
}

// inForce is the value of a run timing that counts: what was changed in the panel (rt), else config.yaml's.
func inForce(cfg *config.Panel, rt adminstate.RunTimings, key string) int {
	if v, ok := rt.Value(key); ok {
		return v
	}
	return cfg.RunTiming(key)
}

// fromPanel is " (changed in the panel; config.yaml: N)" behind the name of a timing whose value in force is
// not config.yaml's, "" otherwise.
func fromPanel(cfg *config.Panel, rt adminstate.RunTimings, key string) string {
	if _, ok := rt.Value(key); ok {
		return fmt.Sprintf(" (changed in the panel; config.yaml: %d)", cfg.RunTiming(key))
	}
	return ""
}

// runTimingsCheck is about "Run timings": the timings of "Update unpowered hosts" that an admin changed in
// the panel (state.json, "run_timings": adminstate.RunTimings). They are not in config.yaml, so a reader of
// config.yaml alone would not know that a line of it is not used; this says it:
//
//   - ok: one line per timing that is changed: the value in force, config.yaml's value, who changed it and
//     when. It is used INSTEAD of config.yaml's until Reset;
//   - warn: one line for everything the running panel ignores: a value that is no whole number in its key's
//     range, a key that is no timing. The panel starts with all of them, and config.yaml's value counts;
//   - said: nothing yet (the notes about a short off_after_s and a countdown that is also the Power menu's
//     are wakeUpdateCheck's, made from the values in force).
//
// rt is the overlay, for the other checks (empty when state.json cannot be read: the panel does not start
// then). `fairy check-config`, which an update asks before it installs a version, reads config.yaml only.
func runTimingsCheck(cfg *config.Panel) (rt adminstate.RunTimings, ok, warn, said []string) {
	rt, ignored, err := adminstate.ReadRunTimings(cfg.DataDir)
	if err != nil {
		return rt, ok, warn, append(said, "Run timings: state.json cannot be read, so the timings changed in the panel were not looked at")
	}
	for _, line := range ignored {
		warn = append(warn, "Run timings: a timing in state.json is ignored: "+cell(line))
	}
	for _, tm := range config.RunTimings() {
		v, changed := rt.Value(tm.Key)
		if !changed {
			continue
		}
		who := "changed in the panel (\"Run timings\")"
		if m := rt.Marks[tm.Key]; m.By != "" {
			who += " by " + cell(m.By)
		}
		if m := rt.Marks[tm.Key]; !m.At.IsZero() {
			who += ", " + m.At.UTC().Format("2006-01-02 15:04 UTC")
		}
		unit := "s"
		if tm.Key == config.KeyRequestsPerMin {
			unit = "a minute"
		}
		ok = append(ok, fmt.Sprintf("%s.%s: %d %s, %s, used INSTEAD of config.yaml's %d until Reset", tm.Block, tm.Key, v, unit, who, cfg.RunTiming(tm.Key)))
	}
	return rt, ok, warn, said
}

// personCountdown: the countdowns, in seconds, of a shutdown a person asks for in the panel: the Power menu's
// four (app.js, "m-delay"), of which 60 is also what a power request without a delay gets.
var personCountdown = map[int]bool{10: true, 60: true, 300: true, 900: true}

// adminCheck reports who can administer the panel. Roles live in users.json (Users page / `fairy admin users`);
// oidc.control_groups and oidc.bootstrap_admins only make someone admin while users.json has no admin at all.
func adminCheck(cfg *config.Panel, problems, notes []string) ([]string, []string) {
	st := access.NewStore(cfg.DataDir)
	d := st.Load()
	bootstrap := len(cfg.OIDC.ControlGroups)+len(cfg.OIDC.BootstrapAdmins) > 0
	switch {
	case st.Err() != nil:
		problems = append(problems, "users.json cannot be read: nobody gets access until it is fixed (see SECURITY-FIXES.md, M6)")
	case d.Admins() > 0:
		notes = append(notes, fmt.Sprintf("admins in users.json: %d (roles and sites are managed on the Users page)", d.Admins()))
	case bootstrap:
		notes = append(notes, "users.json has no admin yet: the first sign-in through oidc.control_groups / oidc.bootstrap_admins becomes admin")
	default:
		problems = append(problems, "users.json has no admin and oidc.control_groups / oidc.bootstrap_admins are empty: nobody becomes "+
			"admin by signing in (after your first sign-in: fairy admin users set-role <your email or subject> admin)")
	}
	return problems, notes
}
