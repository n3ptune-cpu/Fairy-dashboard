package power

import (
	"context"
	"fmt"
	"time"

	"github.com/n3ptune-cpu/Fairy/internal/config"
	"github.com/n3ptune-cpu/Fairy/internal/devices"
	"github.com/n3ptune-cpu/Fairy/internal/model"
)

// AgentPowerConfig is a `type: agent` power source: one metric of an agent's component power.
type AgentPowerConfig struct {
	Base   `yaml:",inline"`
	Agent  string `yaml:"agent"`
	Metric string `yaml:"metric"` // cpu_package_w | gpu_w | battery_w | bmc_w
}

// AgentPowerFn returns an agent's latest component power block (nil if unknown).
type AgentPowerFn func(agentID string) *model.ComponentPower

type agentPowerSrc struct {
	defaults
	c  AgentPowerConfig
	fn AgentPowerFn
}

func (s *agentPowerSrc) Info() Base { return s.c.Base }

func (s *agentPowerSrc) DefaultInterval() float64 { return 1 }

func (s *agentPowerSrc) Read(context.Context, time.Duration) ([]model.PowerReading, error) {
	var v *float64
	if p := s.fn(s.c.Agent); p != nil {
		switch s.c.Metric {
		case "cpu_package_w":
			v = p.CPUPackageW
		case "gpu_w":
			v = p.GPUW
		case "battery_w":
			v = p.BatteryW
		case "bmc_w":
			v = p.BMCW
		}
	}
	if v == nil {
		return nil, devices.Errf("agent %s reports no %s", s.c.Agent, s.c.Metric)
	}
	return []model.PowerReading{Reading(s.c.Base, devices.Reading{Watts: v})}, nil
}

// buildAgentPower validates a `type: agent` entry. It is not counted in the wall-power total unless the
// entry opts in (component power is not wall power; bmc_w is PSU input).
func buildAgentPower(decode configDecoder, agentPower AgentPowerFn) (Source, error) {
	c := AgentPowerConfig{Metric: "bmc_w"}
	f := false
	c.IncludeInTotal = &f // component power is not wall power (bmc_w is PSU input; opt in)
	if err := decode(&c); err != nil {
		return nil, err
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	if !config.ValidID(c.Agent) {
		return nil, fmt.Errorf("power source %s: agent must be an agent id", c.ID)
	}
	switch c.Metric {
	case "cpu_package_w", "gpu_w", "battery_w", "bmc_w":
	default:
		return nil, fmt.Errorf("power source %s: metric must be cpu_package_w, gpu_w, battery_w or bmc_w", c.ID)
	}
	if agentPower == nil {
		agentPower = func(string) *model.ComponentPower { return nil }
	}
	return &agentPowerSrc{c: c, fn: agentPower}, nil
}
