package config

import (
	"fmt"
	"os"
	"regexp"

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
	}{{"power_sources", c.PowerSources, power.ValidateConfigs}, {"ups", c.UPS, ups.ValidateConfigs}} {
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
