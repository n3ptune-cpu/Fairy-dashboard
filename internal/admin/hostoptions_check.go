package admin

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/managed"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
)

// hostOptionsCheck is about "Edit host": what an admin changed in the panel about the links of the devices an
// agent reads (state.json, "host_options": adminstate.HostOptions). The changes are not in config.yaml, so
// a reader of config.yaml alone would not know that a line of it is not used; this says it:
//
//   - ok: one line per agent with changes: who made the last one and when, and every decision with what
//     config.yaml says about the same thing ("rack powers pc-b (config.yaml: not)"). A decision is used
//     INSTEAD of config.yaml's word about that one thing until Reset; everything else follows config.yaml;
//   - warn: one line for everything the running panel ignores: an entry or a field that is not what Fairy
//     writes, changes for an id that is no agent's, a row id that no row of this panel can have. The
//     panel starts with all of them, config.yaml's value counts for what is ignored;
//   - notes: what the shared check of the link fields (config.LinkProblems) finds in config.yaml itself and
//     no other part of this command says already: the UPS links, and lists longer than an agent can relay.
//     config.yaml is never refused for them.
//
// `fairy check-config`, which an update asks before it installs a version, reads config.yaml only and is not
// changed by any of this.
func hostOptionsCheck(cfg *config.Panel) (ok, warn, notes []string) {
	c := *cfg
	c.Agents, c.Systems, c.Proxmox = slices.Clone(cfg.Agents), slices.Clone(cfg.Systems), slices.Clone(cfg.Proxmox)
	_, _, _ = managed.NewStore(cfg.DataDir).Merge(&c)
	for _, a := range c.Agents {
		for _, p := range config.LinkProblems(a.Links()) {
			if p.Field != config.FieldUPSPowers && p.Kind != "count" && p.Kind != "twice" {
				continue // powerLinkCheck has a note for a device id or a link that cannot work
			}
			notes = append(notes, fmt.Sprintf("agent %s: %s: nothing is refused for it", a.ID, cell(p.Text)))
		}
	}
	o, ignored, err := adminstate.ReadHostOptions(cfg.DataDir)
	if err != nil {
		return ok, warn, append(notes, "Edit host: state.json cannot be read, so the overrides made in the panel were not looked at")
	}
	for _, line := range ignored {
		warn = append(warn, "Edit host: an override in state.json is ignored: "+cell(line))
	}
	byID := map[string]config.Agent{}
	for _, a := range c.Agents {
		byID[a.ID] = a
	}
	for _, id := range slices.Sorted(maps.Keys(o.Agents)) {
		e := o.Agents[id]
		a, known := byID[id]
		if !known {
			warn = append(warn, fmt.Sprintf("Edit host: state.json has overrides for %s, which is no agent's id (config.yaml and the hosts added in the panel): they are ignored", id))
			continue
		}
		who := "changed in the panel (\"Edit host\")"
		if e.By != "" {
			who += " by " + cell(e.By)
		}
		if !e.At.IsZero() {
			who += ", " + e.At.UTC().Format("2006-01-02 15:04 UTC")
		}
		base := a.Links()
		var parts, rows []string
		for _, name := range slices.Sorted(maps.Keys(e.UPS)) {
			for _, row := range slices.Sorted(maps.Keys(e.UPS[name])) {
				on, was := e.UPS[name][row], slices.Contains(base.UPSPowers[name], row)
				parts = append(parts, fmt.Sprintf("%s: %s %s %s (config.yaml: %s)", config.FieldUPSPowers, name, pick(on, "powers", "does not power"), row, pick(was, "it does", "not")))
				if on {
					rows = append(rows, row)
				}
			}
		}
		for _, dev := range slices.Sorted(maps.Keys(e.Links)) {
			to := e.Links[dev]
			parts = append(parts, fmt.Sprintf("%s: %s -> %s (config.yaml: %s)", config.FieldPowerLinks, dev, pick(to != "", to, "none"), pick(base.PowerLinks[dev] != "", base.PowerLinks[dev], "none")))
			if to != "" {
				rows = append(rows, to)
			}
		}
		for _, dev := range slices.Sorted(maps.Keys(e.Exclude)) {
			parts = append(parts, fmt.Sprintf("%s: %s %s (config.yaml: %s)", config.FieldExclude, dev, pick(e.Exclude[dev], "left out", "counted"),
				pick(slices.Contains(base.PowerExcludeFromTotal, dev), "left out", "counted")))
		}
		ok = append(ok, fmt.Sprintf("agent %s: %s, used INSTEAD of what config.yaml says about the same thing until Reset (the rest follows config.yaml): %s", id, who, cell(strings.Join(parts, "; "))))
		slices.Sort(rows)
		for _, row := range slices.Compact(rows) {
			if !poller.PossibleRow(row, c.Agents, c.Proxmox) {
				warn = append(warn, fmt.Sprintf("Edit host: an override of agent %s names the row %s, which is no agent's id and not the id of a node or a guest of a proxmox: entry: "+
					"it is ignored (a host that was removed?)", id, row))
			}
		}
	}
	return ok, warn, notes
}

func pick(c bool, yes, no string) string {
	if c {
		return yes
	}
	return no
}
