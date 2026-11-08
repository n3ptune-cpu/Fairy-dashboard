// Package power: pluggable power sources (wall power from plugs / UPS / BMC, plus agent component power), the
// per-source runners with backoff, and the manager (overview, linked watts, history, agent-relayed devices).
//
// Adding a type = one Source implementation plus a case in build().
package power

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/n3ptune-cpu/Fairy/internal/config"
	"github.com/n3ptune-cpu/Fairy/internal/devices"
	"github.com/n3ptune-cpu/Fairy/internal/model"
)

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Base holds the fields every source type has.
type Base struct {
	ID             string   `yaml:"id"`
	Type           string   `yaml:"type"`
	Name           string   `yaml:"name"`
	System         string   `yaml:"system"`           // link to a system -> System.power_w
	IncludeInTotal *bool    `yaml:"include_in_total"` // false for downstream plugs already counted by a UPS
	Interval       *float64 `yaml:"interval"`         // default per type
}

func (b Base) DisplayName() string {
	if b.Name != "" {
		return b.Name
	}
	return b.ID
}

func (b Base) validate() error {
	if !idRe.MatchString(b.ID) {
		return fmt.Errorf("power source %q: id must be lowercase letters, digits and dashes", b.ID)
	}
	if b.System != "" && !idRe.MatchString(b.System) {
		return fmt.Errorf("power source %s: invalid system id", b.ID)
	}
	if b.Interval != nil && (*b.Interval < 1 || *b.Interval > 300) {
		return fmt.Errorf("power source %s: interval must be 1-300 s", b.ID)
	}
	return nil
}

// Source reads one device.
type Source interface {
	Info() Base
	Read(ctx context.Context, timeout time.Duration) ([]model.PowerReading, error)
	DefaultInterval() float64
	FloorInterval() float64
	MaxBackoff() float64
	Close()
}

type defaults struct{}

func (defaults) DefaultInterval() float64 { return 2 }
func (defaults) FloorInterval() float64   { return 1 }
func (defaults) MaxBackoff() float64      { return 60 }
func (defaults) Close()                   {}

// Reading builds a PowerReading for a source.
func Reading(b Base, r devices.Reading) model.PowerReading {
	pr := model.PowerReading{SourceID: b.ID, Name: b.DisplayName(), Watts: r.Watts, Voltage: r.Voltage,
		Current: r.Current, EnergyKWhTotal: r.EnergyKWh, Timestamp: time.Now().UTC()}
	if b.System != "" {
		pr.SystemID = model.P(b.System)
	}
	return pr
}

func secret(env, what string) (string, error) {
	if env == "" {
		return "", nil
	}
	return config.EnvSecret(env, what, 1)
}
