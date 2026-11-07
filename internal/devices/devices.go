// Package devices: power-device and UPS protocol code shared by the panel and the agent (agent-side devices on
// a friend's LAN use the same code). Errors are safe to show: they never contain credentials or URLs.
package devices

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Error is a failure whose message is safe to show in the API.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func Errf(f string, a ...any) error { return &Error{Msg: fmt.Sprintf(f, a...)} }

// Reading: watts, volts, amps, kWh total (nil = unknown).
type Reading struct {
	Watts, Voltage, Current, EnergyKWh *float64
}

func f64(v any) *float64 {
	switch x := v.(type) {
	case float64:
		return &x
	case int:
		f := float64(x)
		return &f
	case int64:
		f := float64(x)
		return &f
	case json.Number:
		if f, err := x.Float64(); err == nil {
			return &f
		}
	}
	return nil
}

func div(p *float64, d float64) *float64 {
	if p == nil {
		return nil
	}
	v := *p / d
	return &v
}

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}

func obj(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// ------------------------------------------------------------------ Shelly
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

// ------------------------------------------------------------------ Tasmota
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

// ------------------------------------------------------------------ Redfish
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

// ------------------------------------------------------------------ Tuya DPS mapping
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

// ------------------------------------------------------------------ HTTP helpers
var lanTransport = &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 2, IdleConnTimeout: 90 * time.Second,
	DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext}

// LANClient never uses an HTTP proxy (devices live on the local network / tailnet).
func LANClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: lanTransport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// TLSClient for BMCs: optional custom CA / verification off (owner's choice in config).
func TLSClient(timeout time.Duration, cfg *tls.Config) *http.Client {
	tr := lanTransport.Clone()
	tr.TLSClientConfig = cfg
	return &http.Client{Timeout: timeout, Transport: tr}
}

func describeHTTP(err error) error {
	var de *Error
	if errors.As(err, &de) {
		return err
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() || errors.Is(err, context.DeadlineExceeded) {
		return Errf("timeout")
	}
	if strings.Contains(err.Error(), "certificate") {
		return Errf("tls verification failed")
	}
	return Errf("connection failed")
}

// GetJSON fetches a JSON object. auth: "" | "basic" | "digest".
func GetJSON(ctx context.Context, c *http.Client, rawURL, auth, user, pass string) (map[string]any, error) {
	do := func(hdr string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, Errf("invalid url")
		}
		req.Header.Set("Accept", "application/json")
		if auth == "basic" && pass != "" {
			req.SetBasicAuth(user, pass)
		}
		if hdr != "" {
			req.Header.Set("Authorization", hdr)
		}
		return c.Do(req)
	}
	resp, err := do("")
	if err != nil {
		return nil, describeHTTP(err)
	}
	if resp.StatusCode == http.StatusUnauthorized && auth == "digest" && pass != "" {
		chal := resp.Header.Get("WWW-Authenticate")
		resp.Body.Close()
		u, _ := url.Parse(rawURL)
		hdr, derr := DigestAuthorization(chal, http.MethodGet, u.RequestURI(), user, pass)
		if derr != nil {
			return nil, derr
		}
		if resp, err = do(hdr); err != nil {
			return nil, describeHTTP(err)
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, &HTTPStatusError{Code: resp.StatusCode}
	}
	var out map[string]any
	dec := json.NewDecoder(io.LimitReader(resp.Body, 4<<20))
	if err := dec.Decode(&out); err != nil {
		return nil, Errf("invalid JSON from device")
	}
	return out, nil
}

type HTTPStatusError struct{ Code int }

func (e *HTTPStatusError) Error() string { return fmt.Sprintf("http %d", e.Code) }

// ------------------------------------------------------------------ NUT (RFC 9271, read-only)
var UPSFlags = map[string]string{"OL": "online", "OB": "on_battery", "LB": "low_battery", "HB": "high_battery",
	"RB": "replace_battery", "CHRG": "charging", "DISCHRG": "discharging", "BYPASS": "bypass", "CAL": "calibrating",
	"OFF": "off", "OVER": "overload", "TRIM": "trim", "BOOST": "boost", "FSD": "forced_shutdown", "ALARM": "alarm",
	"TEST": "test"}

// ParseUPSStatus: NUT ups.status ("OB DISCHRG LB") -> state (online|on_battery|low_battery|off|unknown), flags.
func ParseUPSStatus(raw string) (string, []string) {
	toks := strings.Fields(strings.ToUpper(raw))
	has := map[string]bool{}
	flags := []string{}
	for _, t := range toks {
		has[t] = true
		if f, ok := UPSFlags[t]; ok {
			flags = append(flags, f)
		}
	}
	switch {
	case has["OB"] && (has["LB"] || has["FSD"]):
		return "low_battery", flags
	case has["OB"]:
		return "on_battery", flags
	case has["OL"]:
		return "online", flags
	case has["OFF"]:
		return "off", flags
	}
	return "unknown", flags
}

func nutUnescape(v string) string {
	return strings.ReplaceAll(strings.ReplaceAll(v, `\"`, `"`), `\\`, `\`)
}

func unquote(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		v = v[1 : len(v)-1]
	}
	return nutUnescape(v)
}

// ParseNUTVarLine: `VAR <ups> <name> "<value>"`.
func ParseNUTVarLine(line, ups string) (string, string, bool) {
	prefix := "VAR " + ups + " "
	if !strings.HasPrefix(line, prefix) {
		return "", "", false
	}
	name, value, _ := strings.Cut(line[len(prefix):], " ")
	return name, unquote(value), true
}

// ParseNUTUPSLine: `UPS <name> "<description>"` from LIST UPS.
func ParseNUTUPSLine(line string) (string, string, bool) {
	if !strings.HasPrefix(line, "UPS ") {
		return "", "", false
	}
	name, desc, _ := strings.Cut(line[4:], " ")
	return name, unquote(desc), true
}

type NUTUPS struct {
	Name, Description string
	Vars              map[string]string
}

type nutConn struct {
	c net.Conn
	r *bufio.Reader
}

func (n *nutConn) cmd(line string) error {
	_, err := n.c.Write([]byte(line + "\n"))
	return err
}

func (n *nutConn) readline() (string, error) {
	raw, err := n.r.ReadString('\n')
	if err != nil {
		if raw == "" {
			return "", Errf("nut: connection closed")
		}
	}
	text := strings.TrimRight(raw, "\r\n")
	if strings.HasPrefix(text, "ERR") {
		return "", Errf("nut: %s", text)
	}
	return text, nil
}

func (n *nutConn) listing(what string) ([]string, error) {
	if err := n.cmd("LIST " + what); err != nil {
		return nil, Errf("connection failed")
	}
	first, err := n.readline()
	if err != nil {
		return nil, err
	}
	if first != "BEGIN LIST "+what {
		return nil, Errf("nut: unexpected reply")
	}
	var lines []string
	for {
		l, err := n.readline()
		if err != nil {
			return nil, err
		}
		if l == "END LIST "+what {
			return lines, nil
		}
		lines = append(lines, l)
		if len(lines) > 5000 {
			return nil, Errf("nut: reply too long")
		}
	}
}

// NUTReadAll: one connection, optional login, LIST UPS (unless names given), LIST VAR for each. Read-only:
// never sends INSTCMD / SET VAR / FSD.
func NUTReadAll(ctx context.Context, host string, port int, username, password string, timeout time.Duration, names []string) ([]NUTUPS, error) {
	d := net.Dialer{Timeout: timeout}
	c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, describeHTTP(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(timeout + 2*time.Second))
	n := &nutConn{c: c, r: bufio.NewReaderSize(c, 8192)}
	if username != "" {
		if err := n.cmd("USERNAME " + username); err != nil {
			return nil, Errf("connection failed")
		}
		if _, err := n.readline(); err != nil {
			return nil, err
		}
		if err := n.cmd("PASSWORD " + password); err != nil {
			return nil, Errf("connection failed")
		}
		if _, err := n.readline(); err != nil {
			return nil, err
		}
	}
	type pair struct{ name, desc string }
	var upses []pair
	if len(names) > 0 {
		for _, nm := range names {
			upses = append(upses, pair{nm, ""})
		}
	} else {
		lines, err := n.listing("UPS")
		if err != nil {
			return nil, err
		}
		for _, l := range lines {
			if nm, desc, ok := ParseNUTUPSLine(l); ok {
				upses = append(upses, pair{nm, desc})
			}
		}
	}
	var out []NUTUPS
	for _, u := range upses {
		lines, err := n.listing("VAR " + u.name)
		if err != nil {
			return nil, err
		}
		vars := map[string]string{}
		for _, l := range lines {
			if k, v, ok := ParseNUTVarLine(l, u.name); ok {
				vars[k] = v
			}
		}
		out = append(out, NUTUPS{u.name, u.desc, vars})
	}
	_ = n.cmd("LOGOUT")
	return out, nil
}

func numVar(v map[string]string, keys ...string) *float64 {
	for _, k := range keys {
		if s, ok := v[k]; ok {
			if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
				return &f
			}
		}
	}
	return nil
}

func strVar(v map[string]string, keys ...string) *string {
	for _, k := range keys {
		if s, ok := v[k]; ok && s != "" {
			return &s
		}
	}
	return nil
}

// NUTWatts: (watts, input volts, output amps); estimated from load% x nominal when realpower is missing.
func NUTWatts(v map[string]string) (watts, volts, amps *float64) {
	watts = numVar(v, "ups.realpower")
	if watts == nil {
		if load, nom := numVar(v, "ups.load"), numVar(v, "ups.realpower.nominal"); load != nil && nom != nil && *nom != 0 {
			w := round(*load**nom/100, 1)
			watts = &w
		}
	}
	return watts, numVar(v, "input.voltage"), numVar(v, "output.current")
}

// UPSRow is the normalised UPS shape (also the agent's /metrics `ups` rows).
type UPSRow struct {
	Name            string   `json:"name"`
	Description     *string  `json:"description"`
	StatusRaw       *string  `json:"status_raw"`
	State           string   `json:"state"`
	Flags           []string `json:"flags"`
	BatteryCharge   *float64 `json:"battery_charge"`
	BatteryRuntimeS *int     `json:"battery_runtime_s"`
	BatteryVoltage  *float64 `json:"battery_voltage"`
	LoadPercent     *float64 `json:"load_percent"`
	Watts           *float64 `json:"watts"`
	WattsEstimated  bool     `json:"watts_estimated"`
	NominalW        *float64 `json:"nominal_w"`
	InputVoltage    *float64 `json:"input_voltage"`
	OutputVoltage   *float64 `json:"output_voltage"`
	InputFrequency  *float64 `json:"input_frequency"`
	Model           *string  `json:"model"`
	Mfr             *string  `json:"mfr"`
	Serial          *string  `json:"serial"`
	BatteryDate     *string  `json:"battery_date"`
	TestResult      *string  `json:"test_result"`
	TransferReason  *string  `json:"transfer_reason"`
	OK              bool     `json:"ok"`
	Error           *string  `json:"error"`
}

// UPSFromVars normalises a NUT variable set. CyberPower/usbhid-ups often has no ups.realpower: watts are then
// estimated as ups.load % x ups.realpower.nominal (WattsEstimated).
func UPSFromVars(name string, v map[string]string, description string) UPSRow {
	raw := v["ups.status"]
	state, flags := ParseUPSStatus(raw)
	load, nominal := numVar(v, "ups.load"), numVar(v, "ups.realpower.nominal")
	watts, est := numVar(v, "ups.realpower"), false
	if watts == nil && load != nil && nominal != nil && *nominal != 0 {
		w := round(*load**nominal/100, 1)
		watts, est = &w, true
	}
	var runtime *int
	if r := numVar(v, "battery.runtime"); r != nil {
		x := int(*r)
		runtime = &x
	}
	row := UPSRow{Name: name, State: state, Flags: flags, BatteryCharge: numVar(v, "battery.charge"),
		BatteryRuntimeS: runtime, BatteryVoltage: numVar(v, "battery.voltage"), LoadPercent: load, Watts: watts,
		WattsEstimated: est, NominalW: nominal, InputVoltage: numVar(v, "input.voltage"),
		OutputVoltage: numVar(v, "output.voltage"), InputFrequency: numVar(v, "input.frequency", "output.frequency"),
		Model: strVar(v, "ups.model", "device.model"), Mfr: strVar(v, "ups.mfr", "device.mfr"),
		Serial: strVar(v, "ups.serial", "device.serial"), BatteryDate: strVar(v, "battery.date", "battery.mfr.date"),
		TestResult: strVar(v, "ups.test.result"), TransferReason: strVar(v, "input.transfer.reason"), OK: true}
	if raw != "" {
		row.StatusRaw = &raw
	}
	if description != "" {
		row.Description = &description
	}
	return row
}

// PollUPS never fails: on error it returns error rows (named after `names`, or "upsd").
func PollUPS(ctx context.Context, host string, port int, user, pass string, names []string, timeout time.Duration) []UPSRow {
	upses, err := NUTReadAll(ctx, host, port, user, pass, timeout, names)
	if err != nil {
		msg := err.Error()
		if len(names) == 0 {
			names = []string{"upsd"}
		}
		out := make([]UPSRow, 0, len(names))
		for _, n := range names {
			out = append(out, UPSRow{Name: n, OK: false, Error: &msg, Flags: []string{}})
		}
		return out
	}
	out := make([]UPSRow, 0, len(upses))
	for _, u := range upses {
		out = append(out, UPSFromVars(u.Name, u.Vars, u.Description))
	}
	return out
}
