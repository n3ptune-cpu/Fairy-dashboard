package config

import (
	"fmt"
	"os"
	"regexp"
	"slices"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/power"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/ups"
	"gopkg.in/yaml.v3"
)

// LoadFull reads a config.yaml and checks all of it: Load, and then every entry of `power_sources:` and `ups:`.
// Those two lists are free-form in Panel (each type of device has its own keys), so Load alone accepts a
// mistake inside one of their entries.
//
// This is the ONE check of a config file. `fairy serve` starts with it, `fairy admin config check` reports it
// and `fairy update` asks it before it replaces the program (r3-p4-01: the update asked Load only, went on with
// a file that had `type: nutt` in a `ups:` entry, and no version started with that file). A test
// (TestOneCheckOfAConfigFile) holds that nothing else in the tree checks the device lists by itself.
func LoadFull(path string) (*Panel, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseFull(data)
}

// ParseFull is LoadFull for a file's content.
func ParseFull(data []byte) (*Panel, error) {
	c, err := Parse(data)
	if err != nil {
		return nil, err
	}
	if err := c.checkDevices(data); err != nil {
		return nil, err
	}
	return c, nil
}

// CheckDevices is the second half of ParseFull for a config that was not read from a file as it stands (the
// test build writes its own from the person's file): the same check, without a line.
func (c *Panel) CheckDevices() error { return c.checkDevices(nil) }

// checkDevices checks `power_sources:` and `ups:` and names the line of the entry that is refused. The checks
// stop at the first mistake, so the entry is the last one of the shortest beginning of the list that fails (a
// duplicate id is then named at its second entry).
func (c *Panel) checkDevices(data []byte) error {
	for _, list := range []struct {
		key   string
		raws  []map[string]any
		check func([]map[string]any) error
	}{{"power_sources", c.PowerSources, validatePanelPowerSources}, {"ups", c.UPS, ups.ValidateConfigs}} {
		if list.check(list.raws) == nil {
			continue
		}
		for i := range list.raws {
			if err := list.check(list.raws[:i+1]); err != nil {
				// the device's own check counts lines inside the entry, which are not lines of this file
				why := innerLine.ReplaceAllString(err.Error(), " ")
				if line := entryLine(data, list.key, i); line > 0 {
					return fmt.Errorf("config: line %d (%s, entry %d): %s", line, list.key, i+1, why)
				}
				return fmt.Errorf("config: %s, entry %d: %s", list.key, i+1, why)
			}
		}
	}
	return nil
}

// AgentOnlyPowerTypes are the `type:` values of a power source that an AGENT reads and the panel itself does
// not: they are refused in the panel's own power_sources.
//
// homeassistant (Fairy-agent v0.8.0). A Home Assistant token that is refused must not be sent again and again:
// Home Assistant counts each such request as a failed login and bans the address after some of them
// (ip_ban_enabled). The agent's relay loop therefore waits after a refusal, 5 minutes, then 15, then an hour
// (power.RefusedWait). The panel's own poller has no such wait: a source that fails is asked again after at
// most a minute, for good. So the type exists on agents only; an agent on that network reads the sensor and
// relays the watts in its /metrics like every other plug.
var AgentOnlyPowerTypes = []string{"homeassistant"}

// PanelPowerSource refuses an entry of the panel's own power_sources whose type only an agent reads
// (AgentOnlyPowerTypes), with what to do instead. nil: the panel reads this type (or it is no type at all,
// which the device's own check says).
func PanelPowerSource(raw map[string]any) error {
	typ, _ := raw["type"].(string)
	if !slices.Contains(AgentOnlyPowerTypes, typ) {
		return nil
	}
	id, _ := raw["id"].(string)
	return fmt.Errorf("power source %q: type %s is read by an agent, not by the panel: put it into the power_sources of an agent on that network "+
		"(the agent's own config.yaml), and link it here with power_links in that agent's entry. "+
		"The panel would send a refused token again every minute; an agent waits up to an hour", id, typ)
}

// validatePanelPowerSources is the check of the panel's power_sources: nothing that only an agent reads
// (PanelPowerSource), then each device's own check.
func validatePanelPowerSources(raws []map[string]any) error {
	for _, raw := range raws {
		if err := PanelPowerSource(raw); err != nil {
			return err
		}
	}
	return power.ValidateConfigs(raws)
}

var innerLine = regexp.MustCompile(`\s+line \d+: `)

// entryLine is the line on which entry i of the top-level list `key` begins (0: not found).
func entryLine(data []byte, key string, i int) int {
	var doc yaml.Node
	if yaml.Unmarshal(data, &doc) != nil || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return 0
	}
	top := doc.Content[0].Content
	for k := 0; k+1 < len(top); k += 2 {
		if top[k].Value == key && top[k+1].Kind == yaml.SequenceNode && i < len(top[k+1].Content) {
			return top[k+1].Content[i].Line
		}
	}
	return 0
}
