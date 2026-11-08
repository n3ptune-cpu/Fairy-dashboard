package devices

// ParseShellyGen2: Gen2+/Plus/Pro RPC Switch.GetStatus (or PM1.GetStatus). aenergy.total is Wh.
func ParseShellyGen2(d map[string]any) Reading {
	return Reading{f64(d["apower"]), f64(d["voltage"]), f64(d["current"]), div(f64(obj(d["aenergy"])["total"]), 1000)}
}

// ParseShellyGen1: /status `meters` (Plug S, 1PM; total in watt-minutes) or `emeters` (EM/3EM; total in Wh).
func ParseShellyGen1(d map[string]any, channel int) (Reading, error) {
	if em, ok := d["emeters"].([]any); ok && len(em) > 0 {
		if channel >= len(em) {
			return Reading{}, Errf("shelly: no meter on that channel")
		}
		m := obj(em[channel])
		return Reading{f64(m["power"]), f64(m["voltage"]), f64(m["current"]), div(f64(m["total"]), 1000)}, nil
	}
	meters, _ := d["meters"].([]any)
	if channel >= len(meters) {
		return Reading{}, Errf("shelly: no meter on that channel")
	}
	m := obj(meters[channel])
	return Reading{Watts: f64(m["power"]), EnergyKWh: div(f64(m["total"]), 60000)}, nil
}

func ParseTasmotaStatus8(d map[string]any) (Reading, error) {
	e := obj(obj(d["StatusSNS"])["ENERGY"])
	if len(e) == 0 {
		return Reading{}, Errf("tasmota: no ENERGY block (not a metering device?)")
	}
	num := func(v any) *float64 { // multi-channel devices return lists
		if l, ok := v.([]any); ok {
			s := 0.0
			for _, x := range l {
				if p := f64(x); p != nil {
					s += *p
				}
			}
			return &s
		}
		return f64(v)
	}
	return Reading{num(e["Power"]), num(e["Voltage"]), num(e["Current"]), num(e["Total"])}, nil
}

func ParseRedfishPower(d map[string]any) (watts, volts *float64, err error) {
	if pw, ok := d["PowerWatts"]; ok { // newer EnvironmentMetrics schema
		return f64(obj(pw)["Reading"]), nil, nil
	}
	if pc, ok := d["PowerControl"].([]any); ok && len(pc) > 0 {
		watts = f64(obj(pc[0])["PowerConsumedWatts"])
	}
	if ps, ok := d["PowerSupplies"].([]any); ok {
		for _, p := range ps {
			if v := f64(obj(p)["LineInputVoltage"]); v != nil && *v != 0 {
				volts = v
				break
			}
		}
	}
	if watts == nil {
		return nil, nil, Errf("redfish: no PowerConsumedWatts")
	}
	return watts, volts, nil
}

type DpsField struct {
	DP    string  `json:"dp" yaml:"dp"`
	Scale float64 `json:"scale" yaml:"scale"`
}

// DefaultTuyaDPS: common metering plugs. DP 17 ("add_ele") is an increment, not a total, so it isn't mapped.
func DefaultTuyaDPS() map[string]DpsField {
	return map[string]DpsField{"power": {"19", 0.1}, "voltage": {"20", 0.1}, "current": {"18", 0.001}}
}

func ParseTuyaDPS(dps map[string]any, mapping map[string]DpsField) Reading {
	if len(mapping) == 0 {
		mapping = DefaultTuyaDPS()
	}
	get := func(field string) *float64 {
		m, ok := mapping[field]
		if !ok {
			return nil
		}
		raw := f64(dps[m.DP])
		if raw == nil {
			return nil
		}
		scale := m.Scale
		if scale == 0 {
			scale = 1
		}
		v := round(*raw*scale, 3)
		return &v
	}
	return Reading{get("power"), get("voltage"), get("current"), get("energy_kwh_total")}
}
