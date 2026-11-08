package power

import (
	powersrc "github.com/n3ptune-cpu/Fairy-agent/pkg/power"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// AgentPowerFn returns an agent's latest component power block (nil if unknown). `type: agent` power sources
// read one metric of it.
type AgentPowerFn func(agentID string) *model.ComponentPower

// metric adapts fn to the per-metric callback of the power source package.
func (fn AgentPowerFn) metric() powersrc.AgentPowerFn {
	if fn == nil {
		return nil
	}
	return func(agentID, metric string) *float64 {
		p := fn(agentID)
		if p == nil {
			return nil
		}
		switch metric {
		case "cpu_package_w":
			return p.CPUPackageW
		case "gpu_w":
			return p.GPUW
		case "battery_w":
			return p.BatteryW
		case "bmc_w":
			return p.BMCW
		}
		return nil
	}
}
