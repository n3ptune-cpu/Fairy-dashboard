// Package power: pluggable power sources (wall power from plugs / UPS / BMC, plus agent component power), the
// per-source runners with backoff, and the manager (overview, linked watts, history, agent-relayed devices).
//
// Adding a type = one Source implementation plus a case in build().
package power

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/n3ptune-cpu/Fairy/internal/config"
	"github.com/n3ptune-cpu/Fairy/internal/devices"
	"github.com/n3ptune-cpu/Fairy/internal/model"
	"github.com/n3ptune-cpu/Fairy/internal/normalise"
	"github.com/n3ptune-cpu/Fairy/internal/tuya"
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

// ------------------------------------------------------------------ Shelly
type ShellyConfig struct {
	Base        `yaml:",inline"`
	URL         string `yaml:"url"` // http://192.168.1.50
	Gen         string `yaml:"gen"` // auto | 1 | 2
	Channel     int    `yaml:"channel"`
	RPCMethod   string `yaml:"rpc_method"` // e.g. PM1.GetStatus for Shelly PM Mini
	Username    string `yaml:"username"`
	PasswordEnv string `yaml:"password_env"`
}

type shelly struct {
	defaults
	c ShellyConfig
}

func (s *shelly) Info() Base { return s.c.Base }

func (s *shelly) Read(ctx context.Context, timeout time.Duration) ([]model.PowerReading, error) {
	c := s.c
	pw, err := secret(c.PasswordEnv, "power source "+c.ID)
	if err != nil {
		return nil, devices.Errf("%s", err.Error())
	}
	base := strings.TrimRight(c.URL, "/")
	hc := devices.LANClient(timeout)
	if c.Gen == "auto" || c.Gen == "2" {
		auth := ""
		if pw != "" {
			auth = "digest" // Gen2 auth is digest
		}
		d, err := devices.GetJSON(ctx, hc, fmt.Sprintf("%s/rpc/%s?id=%d", base, url.PathEscape(c.RPCMethod), c.Channel), auth, c.Username, pw)
		var he *devices.HTTPStatusError
		if err == nil {
			return []model.PowerReading{Reading(c.Base, devices.ParseShellyGen2(d))}, nil
		}
		if !errors.As(err, &he) || he.Code != 404 || c.Gen == "2" {
			return nil, err
		}
	}
	auth := ""
	if pw != "" {
		auth = "basic"
	}
	d, err := devices.GetJSON(ctx, hc, base+"/status", auth, c.Username, pw)
	if err != nil {
		return nil, err
	}
	r, err := devices.ParseShellyGen1(d, c.Channel)
	if err != nil {
		return nil, err
	}
	return []model.PowerReading{Reading(c.Base, r)}, nil
}

// ------------------------------------------------------------------ Tasmota
type TasmotaConfig struct {
	Base        `yaml:",inline"`
	URL         string `yaml:"url"`
	Username    string `yaml:"username"`
	PasswordEnv string `yaml:"password_env"`
}

type tasmota struct {
	defaults
	c TasmotaConfig
}

func (s *tasmota) Info() Base { return s.c.Base }

func (s *tasmota) Read(ctx context.Context, timeout time.Duration) ([]model.PowerReading, error) {
	c := s.c
	q := url.Values{"cmnd": {"Status 8"}}
	pw, err := secret(c.PasswordEnv, "power source "+c.ID)
	if err != nil {
		return nil, devices.Errf("%s", err.Error())
	}
	if pw != "" { // Tasmota's own scheme; the URL is never logged
		q.Set("user", c.Username)
		q.Set("password", pw)
	}
	d, err := devices.GetJSON(ctx, devices.LANClient(timeout), strings.TrimRight(c.URL, "/")+"/cm?"+q.Encode(), "", "", "")
	if err != nil {
		return nil, err
	}
	r, err := devices.ParseTasmotaStatus8(d)
	if err != nil {
		return nil, err
	}
	return []model.PowerReading{Reading(c.Base, r)}, nil
}

// ------------------------------------------------------------------ NUT (UPS wall power)
type NutConfig struct {
	Base        `yaml:",inline"`
	Host        string `yaml:"host"`
	Port        int    `yaml:"port"`
	UPS         string `yaml:"ups"`
	Username    string `yaml:"username"`
	PasswordEnv string `yaml:"password_env"`
}

type nut struct {
	defaults
	c NutConfig
}

func (s *nut) Info() Base { return s.c.Base }

func (s *nut) Read(ctx context.Context, timeout time.Duration) ([]model.PowerReading, error) {
	c := s.c
	pw, err := secret(c.PasswordEnv, "power source "+c.ID)
	if err != nil {
		return nil, devices.Errf("%s", err.Error())
	}
	all, err := devices.NUTReadAll(ctx, c.Host, c.Port, c.Username, pw, timeout, []string{c.UPS})
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, devices.Errf("nut: no such UPS")
	}
	w, v, a := devices.NUTWatts(all[0].Vars)
	if w == nil {
		return nil, devices.Errf("nut: UPS reports neither ups.realpower nor ups.load + ups.realpower.nominal")
	}
	return []model.PowerReading{Reading(c.Base, devices.Reading{Watts: w, Voltage: v, Current: a})}, nil
}

// ------------------------------------------------------------------ Redfish (iDRAC, iLO, ...)
type RedfishConfig struct {
	Base        `yaml:",inline"`
	URL         string `yaml:"url"`        // https://idrac.lan
	ChassisID   string `yaml:"chassis_id"` // iDRAC default System.Embedded.1; HPE iLO uses "1"
	PowerPath   string `yaml:"power_path"` // override, e.g. /redfish/v1/Chassis/1/EnvironmentMetrics
	Username    string `yaml:"username"`
	PasswordEnv string `yaml:"password_env"`
	VerifyTLS   *bool  `yaml:"verify_tls"`
	CAFile      string `yaml:"ca_file"`
}

type redfish struct {
	c   RedfishConfig
	tls *tls.Config
}

func (s *redfish) Info() Base               { return s.c.Base }
func (s *redfish) DefaultInterval() float64 { return 10 } // BMC sensors update slowly; don't hammer them
func (s *redfish) FloorInterval() float64   { return 10 }
func (s *redfish) MaxBackoff() float64      { return 300 } // BMCs are often not cabled: back off up to 5 min
func (s *redfish) Close()                   {}

func (s *redfish) Read(ctx context.Context, timeout time.Duration) ([]model.PowerReading, error) {
	c := s.c
	path := c.PowerPath
	if path == "" {
		path = "/redfish/v1/Chassis/" + url.PathEscape(c.ChassisID) + "/Power"
	}
	pw, err := secret(c.PasswordEnv, c.ID)
	if err != nil {
		return nil, devices.Errf("%s", err.Error())
	}
	d, err := devices.GetJSON(ctx, devices.TLSClient(timeout, s.tls), strings.TrimRight(c.URL, "/")+path, "basic", c.Username, pw)
	if err != nil {
		return nil, err
	}
	w, v, err := devices.ParseRedfishPower(d)
	if err != nil {
		return nil, err
	}
	return []model.PowerReading{Reading(c.Base, devices.Reading{Watts: w, Voltage: v})}, nil
}

// TLSConfig builds the client TLS settings (custom CA or verification off).
func TLSConfig(verify bool, caFile string) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if !verify {
		cfg.InsecureSkipVerify = true //nolint:gosec // owner's explicit choice for self-signed BMCs
		return cfg, nil
	}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("no certificates in " + caFile)
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}

// ------------------------------------------------------------------ agent (component power)
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

func (s *agentPowerSrc) Info() Base               { return s.c.Base }
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

// ------------------------------------------------------------------ Tuya (local LAN, built-in protocol)
type TuyaConfig struct {
	Base        `yaml:",inline"`
	DeviceID    string                      `yaml:"device_id"`
	IP          string                      `yaml:"ip"`
	LocalKeyEnv string                      `yaml:"local_key_env"`
	Version     string                      `yaml:"version"`
	DPS         map[string]devices.DpsField `yaml:"dps"`
	RefreshDPS  *[]int                      `yaml:"refresh_dps"` // some plugs only update power DPS on request
}

// tuyaSource: Tuya devices accept ONE TCP connection and can drop/reset under aggressive polling, so this keeps a
// single persistent connection and defaults to 5 s (floor 3 s).
type tuyaSource struct {
	defaults
	c   TuyaConfig
	mu  sync.Mutex
	dev *tuya.Device
}

func (s *tuyaSource) Info() Base               { return s.c.Base }
func (s *tuyaSource) DefaultInterval() float64 { return 5 }
func (s *tuyaSource) FloorInterval() float64   { return 3 }

func (s *tuyaSource) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dev != nil {
		s.dev.Close()
		s.dev = nil
	}
}

func (s *tuyaSource) Read(_ context.Context, timeout time.Duration) ([]model.PowerReading, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.c
	if s.dev == nil {
		key, err := config.EnvSecret(c.LocalKeyEnv, "power source "+c.ID, 16)
		if err != nil {
			return nil, devices.Errf("%s", err.Error())
		}
		d, err := tuya.NewDevice(c.DeviceID, c.IP, key, c.Version, timeout)
		if err != nil {
			return nil, devices.Errf("%s", err.Error())
		}
		s.dev = d
	}
	s.dev.Timeout = timeout
	refresh := []int{18, 19, 20}
	if c.RefreshDPS != nil {
		refresh = *c.RefreshDPS
	}
	dps, err := s.dev.Status(refresh)
	if err != nil {
		return nil, devices.Errf("%s", err.Error()) // tuya errors never contain the key
	}
	return []model.PowerReading{Reading(c.Base, devices.ParseTuyaDPS(dps, c.DPS))}, nil
}

// ------------------------------------------------------------------ construction
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
		if !idRe.MatchString(c.Agent) {
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

// ------------------------------------------------------------------ runner
type Runner struct {
	Source       Source
	Interval     float64
	offlineAfter float64

	mu          sync.Mutex
	readings    []model.PowerReading
	lastSuccess *time.Time
	err         *string
	failures    int
	nextAttempt float64
	clock       func() float64
}

func (r *Runner) ID() string { return r.Source.Info().ID }

// Poll reads the source once (respecting its backoff).
func (r *Runner) Poll(ctx context.Context, timeout time.Duration) {
	mono := r.clock()
	r.mu.Lock()
	if mono < r.nextAttempt {
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	type res struct {
		rd  []model.PowerReading
		err error
	}
	ch := make(chan res, 1)
	go func() {
		rd, err := r.Source.Read(cctx, timeout)
		ch <- res{rd, err}
	}()
	var out res
	select {
	case out = <-ch:
	case <-cctx.Done():
		out = res{nil, devices.Errf("timeout")}
	}
	b := r.Source.Info()
	r.mu.Lock()
	defer r.mu.Unlock()
	if out.err != nil { // never let a source crash the poller
		r.failures++
		msg := normalise.Describe(out.err)
		r.err = &msg
		r.nextAttempt = mono + math.Min(r.Source.MaxBackoff(), r.Interval*math.Pow(2, float64(r.failures-1)))
		if r.failures == 1 {
			slog.Warn("power source failed", "id", b.ID, "type", b.Type, "error", msg)
		}
		return
	}
	if r.failures > 0 {
		slog.Info("power source recovered", "id", b.ID)
	}
	now := time.Now().UTC()
	r.readings, r.lastSuccess, r.err, r.failures, r.nextAttempt = out.rd, &now, nil, 0, 0
}

func (r *Runner) State() model.PowerSourceState {
	r.mu.Lock()
	defer r.mu.Unlock()
	b := r.Source.Info()
	status := "unreachable"
	if r.failures == 0 && r.lastSuccess != nil {
		status = "ok"
	} else if r.lastSuccess != nil && time.Since(*r.lastSuccess).Seconds() < r.offlineAfter {
		status = "stale"
	}
	st := model.PowerSourceState{ID: b.ID, Type: b.Type, Name: b.DisplayName(), Status: status,
		IncludeInTotal: b.IncludeInTotal == nil || *b.IncludeInTotal, Readings: []model.PowerReading{},
		LastSuccess: r.lastSuccess, Error: r.err}
	if status != "unreachable" && r.readings != nil {
		st.Readings = r.readings
	}
	if wait := r.nextAttempt - r.clock(); wait > 0 {
		st.NextAttemptInS = model.P(math.Round(wait*10) / 10)
	}
	return st
}

// ------------------------------------------------------------------ manager
type Manager struct {
	Runners      []*Runner
	IntervalS    float64
	Timeout      time.Duration
	offlineAfter float64

	mu         sync.Mutex
	history    []model.PowerPoint
	maxHistory int
	remote     map[string]model.PowerSourceState // reported by agents for devices on their LAN
	remoteKeys []string
}

func NewManager(configs []map[string]any, interval float64, timeout time.Duration, offlineAfter float64,
	historyS int, agentPower AgentPowerFn, clock func() float64) (*Manager, error) {
	m := &Manager{IntervalS: interval, Timeout: timeout, offlineAfter: offlineAfter,
		maxHistory: max(1, int(float64(historyS)/interval)), remote: map[string]model.PowerSourceState{}}
	for _, raw := range configs {
		s, err := Build(raw, agentPower)
		if err != nil {
			return nil, err
		}
		iv := s.DefaultInterval()
		if b := s.Info(); b.Interval != nil {
			iv = *b.Interval
		}
		m.Runners = append(m.Runners, &Runner{Source: s, Interval: iv, offlineAfter: offlineAfter, clock: clock})
	}
	return m, nil
}

// UpdateRemote merges agent-reported devices (`power_sources` in the agent's /metrics). rows=nil means the agent
// itself was unreachable this cycle. Device credentials never leave the agent.
func (m *Manager) UpdateRemote(agentID string, rows []map[string]any, unreachable bool, links map[string]string, exclude []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	aged := func(last *time.Time) string {
		if last != nil && now.Sub(*last).Seconds() < m.offlineAfter {
			return "stale"
		}
		return "unreachable"
	}
	if unreachable {
		for k, st := range m.remote {
			if st.Via != nil && *st.Via == agentID {
				st.Status = aged(st.LastSuccess)
				st.Error = model.P("agent unreachable")
				if st.Status != "stale" {
					st.Readings = []model.PowerReading{}
				}
				m.remote[k] = st
			}
		}
		return
	}
	seen := map[string]bool{}
	for _, row := range rows {
		rid := str(row["id"])
		if rid == "" {
			continue
		}
		key := agentID + "/" + rid
		seen[key] = true
		prev, hasPrev := m.remote[key]
		var link *string
		if l, ok := links[rid]; ok && l != "" {
			link = model.P(l)
		} else if h := str(row["system_hint"]); h == "self" || h == agentID {
			link = model.P(agentID)
		}
		name := str(row["name"])
		if name == "" {
			name = rid
		}
		var status string
		var last *time.Time
		readings := []model.PowerReading{}
		var errp *string
		if ok, _ := row["ok"].(bool); ok {
			status, last = "ok", &now
			readings = []model.PowerReading{{SourceID: key, Name: name, Watts: f(row["watts"]),
				Voltage: f(row["voltage"]), Current: f(row["current"]), EnergyKWhTotal: f(row["energy_kwh_total"]),
				SystemID: link, Timestamp: now}}
		} else {
			if hasPrev {
				last = prev.LastSuccess
			}
			status = aged(last)
			if hasPrev && status == "stale" {
				readings = prev.Readings
			}
			e := str(row["error"])
			if e == "" {
				e = "error"
			}
			if len(e) > 120 {
				e = e[:120]
			}
			errp = &e
		}
		typ := str(row["type"])
		if typ == "" {
			typ = "unknown"
		}
		incl := true
		for _, x := range exclude {
			if x == rid {
				incl = false
			}
		}
		if !hasPrev {
			m.remoteKeys = append(m.remoteKeys, key)
		}
		m.remote[key] = model.PowerSourceState{ID: key, Type: typ, Name: name, Status: status, IncludeInTotal: incl,
			Readings: readings, LastSuccess: last, Error: errp, Via: model.P(agentID)}
	}
	keys := m.remoteKeys[:0]
	for _, k := range m.remoteKeys {
		st := m.remote[k]
		if st.Via != nil && *st.Via == agentID && !seen[k] {
			delete(m.remote, k)
			continue
		}
		keys = append(keys, k)
	}
	m.remoteKeys = keys
}

func str(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		if x == math.Trunc(x) {
			return fmt.Sprintf("%d", int64(x))
		}
	}
	return fmt.Sprint(v)
}

func f(v any) *float64 {
	if x, ok := v.(float64); ok {
		return &x
	}
	return nil
}

// PollAll polls every source once, then samples history (tests / first poll).
func (m *Manager) PollAll(ctx context.Context) {
	var wg sync.WaitGroup
	for _, r := range m.Runners {
		wg.Add(1)
		go func(r *Runner) { defer wg.Done(); r.Poll(ctx, m.Timeout) }(r)
	}
	wg.Wait()
	m.SampleHistory()
}

func sumW(rs []model.PowerReading) *float64 {
	if len(rs) == 0 {
		return nil
	}
	s := 0.0
	for _, r := range rs {
		if r.Watts != nil {
			s += *r.Watts
		}
	}
	return &s
}

func (m *Manager) SampleHistory() {
	m.mu.Lock()
	empty := len(m.Runners) == 0 && len(m.remote) == 0
	m.mu.Unlock()
	if empty {
		return
	}
	ov := m.Overview()
	p := model.PowerPoint{T: ov.GeneratedAt, TotalW: ov.TotalW, Sources: map[string]*float64{}}
	for _, s := range ov.Sources {
		p.Sources[s.ID] = sumW(s.Readings)
	}
	m.mu.Lock()
	m.history = append(m.history, p)
	if len(m.history) > m.maxHistory {
		m.history = m.history[len(m.history)-m.maxHistory:]
	}
	m.mu.Unlock()
}

func (m *Manager) Overview() model.PowerOverview {
	states := make([]model.PowerSourceState, 0, len(m.Runners))
	for _, r := range m.Runners {
		states = append(states, r.State())
	}
	m.mu.Lock()
	for _, k := range m.remoteKeys {
		states = append(states, m.remote[k])
	}
	m.mu.Unlock()
	var total float64
	n := 0
	for _, s := range states {
		if !s.IncludeInTotal || s.Status == "unreachable" {
			continue
		}
		for _, rd := range s.Readings {
			if rd.Watts != nil {
				total += *rd.Watts
				n++
			}
		}
	}
	ov := model.PowerOverview{GeneratedAt: time.Now().UTC(), Sources: states}
	if n > 0 {
		ov.TotalW = model.P(normalise.Round(total, 1))
	}
	return ov
}

// LinkedWatts: system id -> watts from linked sources.
func (m *Manager) LinkedWatts() map[string]float64 {
	out := map[string]float64{}
	for _, s := range m.Overview().Sources {
		if s.Status == "unreachable" {
			continue
		}
		for _, rd := range s.Readings {
			if rd.SystemID != nil && rd.Watts != nil {
				out[*rd.SystemID] = normalise.Round(out[*rd.SystemID]+*rd.Watts, 1)
			}
		}
	}
	return out
}

func (m *Manager) HistoryView(source string) model.PowerHistory {
	m.mu.Lock()
	pts := append([]model.PowerPoint{}, m.history...)
	m.mu.Unlock()
	if source != "" {
		for i, p := range pts {
			v := p.Sources[source]
			pts[i] = model.PowerPoint{T: p.T, TotalW: v, Sources: map[string]*float64{source: v}}
		}
	}
	return model.PowerHistory{IntervalS: m.IntervalS, Points: pts}
}

// SourceIDs lists local + remote ids (sorted remote) for tests.
func (m *Manager) SourceIDs() []string {
	var out []string
	for _, r := range m.Runners {
		out = append(out, r.ID())
	}
	m.mu.Lock()
	rk := append([]string{}, m.remoteKeys...)
	m.mu.Unlock()
	sort.Strings(rk)
	return append(out, rk...)
}

// SystemOf returns the linked system of a local source (for scheduler job membership).
func (r *Runner) SystemOf() string { return r.Source.Info().System }

// Kind returns the source type.
func (r *Runner) Kind() string { return r.Source.Info().Type }

func (m *Manager) Close() {
	for _, r := range m.Runners {
		r.Source.Close()
	}
}
