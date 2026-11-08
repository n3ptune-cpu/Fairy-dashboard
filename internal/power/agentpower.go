package power

import (
	"context"
	"time"

	"github.com/n3ptune-cpu/Fairy/internal/devices"
	"github.com/n3ptune-cpu/Fairy/internal/model"
)

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
