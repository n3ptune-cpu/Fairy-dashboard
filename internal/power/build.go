package power

import (
	"bytes"
	"fmt"
	"net/url"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/n3ptune-cpu/Fairy/internal/config"
	"github.com/n3ptune-cpu/Fairy/internal/devices"
	"github.com/n3ptune-cpu/Fairy/internal/model"
)

func httpURL(id, field, u string) error {
	p, err := url.Parse(u)
	if err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" {
		return fmt.Errorf("power source %s: %s must be an http(s) URL", id, field)
	}
	return nil
}

// DecodeStrict re-decodes a generic YAML mapping into a typed struct, rejecting unknown keys.
func DecodeStrict(raw map[string]any, out any) error {
	b, err := yaml.Marshal(raw)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	return dec.Decode(out)
}

var Types = []string{"agent", "ipmi", "nut", "redfish", "shelly", "tasmota", "tuya"}

// Extra holds additional source types (the demo simulator registers "demo").
var Extra = map[string]func(raw map[string]any) (Source, error){}

// NewBase decodes and validates the common fields (for Extra types).
func NewBase(raw map[string]any) (Base, error) {
	b := Base{}
	b.ID, _ = raw["id"].(string)
	b.Type, _ = raw["type"].(string)
	b.Name, _ = raw["name"].(string)
	b.System, _ = raw["system"].(string)
	if v, ok := raw["include_in_total"].(bool); ok {
		b.IncludeInTotal = &v
	}
	return b, b.validate()
}

// Defaults can be embedded by Extra source types.
type Defaults = defaults

// Build validates one config entry and constructs the source.
func Build(raw map[string]any, agentPower AgentPowerFn) (Source, error) {
	typ, _ := raw["type"].(string)
	id := fmt.Sprint(raw["id"])
	strict := func(out any) error {
		if err := DecodeStrict(raw, out); err != nil {
			return fmt.Errorf("power source %q: %v", id, err)
		}
		return nil
	}
	if fn, ok := Extra[typ]; ok {
		return fn(raw)
	}
	switch typ {
	case "shelly":
		c := ShellyConfig{Gen: "auto", RPCMethod: "Switch.GetStatus", Username: "admin"}
		if err := strict(&c); err != nil {
			return nil, err
		}
		if err := c.validate(); err != nil {
			return nil, err
		}
		if err := httpURL(c.ID, "url", c.URL); err != nil {
			return nil, err
		}
		if c.Gen != "auto" && c.Gen != "1" && c.Gen != "2" {
			return nil, fmt.Errorf("power source %s: gen must be auto, 1 or 2", c.ID)
		}
		if c.Channel < 0 {
			return nil, fmt.Errorf("power source %s: channel must be >= 0", c.ID)
		}
		return &shelly{c: c}, nil
	case "tasmota":
		c := TasmotaConfig{Username: "admin"}
		if err := strict(&c); err != nil {
			return nil, err
		}
		if err := c.validate(); err != nil {
			return nil, err
		}
		if err := httpURL(c.ID, "url", c.URL); err != nil {
			return nil, err
		}
		return &tasmota{c: c}, nil
	case "nut":
		c := NutConfig{Port: 3493, UPS: "ups"}
		if err := strict(&c); err != nil {
			return nil, err
		}
		if err := c.validate(); err != nil {
			return nil, err
		}
		if c.Host == "" {
			return nil, fmt.Errorf("power source %s: host is required", c.ID)
		}
		return &nut{c: c}, nil
	case "redfish", "ipmi":
		c := RedfishConfig{ChassisID: "System.Embedded.1", Username: "root"}
		if err := strict(&c); err != nil {
			return nil, err
		}
		if err := c.validate(); err != nil {
			return nil, err
		}
		if err := httpURL(c.ID, "url", c.URL); err != nil {
			return nil, err
		}
		if c.PasswordEnv == "" {
			return nil, fmt.Errorf("power source %s: password_env is required", c.ID)
		}
		tc, err := TLSConfig(c.VerifyTLS == nil || *c.VerifyTLS, c.CAFile)
		if err != nil {
			return nil, fmt.Errorf("power source %s: %v", c.ID, err)
		}
		return &redfish{c: c, tls: tc}, nil
	case "agent":
		c := AgentPowerConfig{Metric: "bmc_w"}
		f := false
		c.IncludeInTotal = &f // component power is not wall power (bmc_w is PSU input; opt in)
		if err := strict(&c); err != nil {
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
	case "tuya":
		c := TuyaConfig{Version: "3.3"}
		if err := strict(&c); err != nil {
			return nil, err
		}
		if err := c.validate(); err != nil {
			return nil, err
		}
		if c.DeviceID == "" || c.IP == "" || c.LocalKeyEnv == "" {
			return nil, fmt.Errorf("power source %s: device_id, ip and local_key_env are required", c.ID)
		}
		switch c.Version {
		case "3.1", "3.2", "3.3", "3.4", "3.5":
		default:
			return nil, fmt.Errorf("power source %s: version must be 3.1-3.5", c.ID)
		}
		if len(c.DPS) == 0 {
			c.DPS = devices.DefaultTuyaDPS()
		}
		for k, f := range c.DPS {
			if f.Scale == 0 {
				f.Scale = 1
				c.DPS[k] = f
			}
		}
		return &tuyaSource{c: c}, nil
	}
	return nil, fmt.Errorf("power source %q: unknown type %q (known: %s)", id, typ, strings.Join(Types, ", "))
}

// ValidateConfigs checks every entry (config check / startup).
func ValidateConfigs(raws []map[string]any) error {
	seen := map[string]bool{}
	for _, r := range raws {
		s, err := Build(r, nil)
		if err != nil {
			return err
		}
		if seen[s.Info().ID] {
			return fmt.Errorf("power source %s: duplicate id", s.Info().ID)
		}
		seen[s.Info().ID] = true
	}
	return nil
}
