// Package ups: UPS as a first-class feature: NUT polling (backend or agent-relayed), state, transitions, events,
// history.
//
// Read-only by design: only LIST UPS / LIST VAR are ever sent (no INSTCMD, no SET VAR).
package ups

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/n3ptune-cpu/Fairy/internal/config"
	"github.com/n3ptune-cpu/Fairy/internal/devices"
	"github.com/n3ptune-cpu/Fairy/internal/model"
	"github.com/n3ptune-cpu/Fairy/internal/normalise"
	"github.com/n3ptune-cpu/Fairy/internal/power"
)

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Powers: a list applies to every UPS of this source; a map maps NUT UPS name -> system ids.
type Powers struct {
	All    []string
	ByName map[string][]string
}

func (p Powers) For(upsName string) []string {
	if p.ByName != nil {
		return slices.Clone(p.ByName[upsName])
	}
	return slices.Clone(p.All)
}

type NutConfig struct {
	ID          string   `yaml:"id"`
	Type        string   `yaml:"type"`
	Name        string   `yaml:"name"`
	PowersRaw   any      `yaml:"powers"`
	Host        string   `yaml:"host"`
	Port        int      `yaml:"port"`
	UPS         []string `yaml:"ups"` // empty = every UPS that upsd lists
	Username    string   `yaml:"username"`
	PasswordEnv string   `yaml:"password_env"`
	powers      Powers
}

func parsePowers(id string, raw any) (Powers, error) {
	ids := func(v any) ([]string, error) {
		l, ok := v.([]any)
		if !ok && v != nil {
			return nil, fmt.Errorf("ups %s: powers must be a list of system ids or a mapping", id)
		}
		out := []string{}
		for _, x := range l {
			s, _ := x.(string)
			if !idRe.MatchString(s) {
				return nil, fmt.Errorf("ups %s: invalid system id in powers", id)
			}
			out = append(out, s)
		}
		return out, nil
	}
	switch x := raw.(type) {
	case nil:
		return Powers{All: []string{}}, nil
	case map[string]any:
		p := Powers{ByName: map[string][]string{}}
		for k, v := range x {
			l, err := ids(v)
			if err != nil {
				return p, err
			}
			p.ByName[k] = l
		}
		return p, nil
	default:
		l, err := ids(raw)
		return Powers{All: l}, err
	}
}

// Source is one configured UPS source (a NUT daemon, or a registered extra type).
type Source struct {
	ID, Name string
	Powers   Powers
	Read     func(ctx context.Context, timeout time.Duration) ([]map[string]any, error)
}

// Extra holds additional source types (the demo simulator registers "demo").
var Extra = map[string]func(raw map[string]any) (*Source, error){}

// ParsePowers is exported for Extra types.
func ParsePowers(id string, raw any) (Powers, error) { return parsePowers(id, raw) }

// Build validates one `ups:` entry.
func Build(raw map[string]any) (*Source, error) {
	if t, _ := raw["type"].(string); Extra[t] != nil {
		return Extra[t](raw)
	}
	c, err := buildNut(raw)
	if err != nil {
		return nil, err
	}
	return &Source{ID: c.ID, Name: c.Name, Powers: c.powers, Read: c.read}, nil
}

func buildNut(raw map[string]any) (*NutConfig, error) {
	c := &NutConfig{Type: "nut", Port: 3493}
	if err := power.DecodeStrict(raw, c); err != nil {
		return nil, fmt.Errorf("ups %v: %v", raw["id"], err)
	}
	if c.Type != "nut" {
		return nil, fmt.Errorf("ups %q: unknown type %q (known: nut)", c.ID, c.Type)
	}
	if !idRe.MatchString(c.ID) {
		return nil, fmt.Errorf("ups %q: id must be lowercase letters, digits and dashes", c.ID)
	}
	if c.Host == "" {
		return nil, fmt.Errorf("ups %s: host is required", c.ID)
	}
	p, err := parsePowers(c.ID, c.PowersRaw)
	if err != nil {
		return nil, err
	}
	c.powers = p
	return c, nil
}

func ValidateConfigs(raws []map[string]any) error {
	for _, r := range raws {
		if _, err := Build(r); err != nil {
			return err
		}
	}
	return nil
}

func (c *NutConfig) read(ctx context.Context, timeout time.Duration) ([]map[string]any, error) {
	pw, err := config.EnvSecret(c.PasswordEnv, "ups "+c.ID, 1)
	if err != nil {
		return nil, devices.Errf("%s", err.Error())
	}
	all, err := devices.NUTReadAll(ctx, c.Host, c.Port, c.Username, pw, timeout, c.UPS)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(all))
	for _, u := range all {
		out = append(out, RowMap(devices.UPSFromVars(u.Name, u.Vars, u.Description)))
	}
	return out, nil
}

// RowMap converts a typed row to the generic shape agents send.
func RowMap(r devices.UPSRow) map[string]any {
	b, _ := json.Marshal(r)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

func FmtRuntime(s *int) string {
	if s == nil {
		return "?"
	}
	return fmt.Sprintf("%d:%02d", *s/60, *s%60)
}

type Manager struct {
	Sources      []*Source
	timeout      time.Duration
	offlineAfter float64
	eventsFile   string
	maxlen       int

	mu      sync.Mutex
	states  map[string]*model.UpsState
	order   []string
	history map[string][]model.UpsPoint
	events  []model.UpsEvent
	seq     int
	// OnEvent is called (outside the lock) for every new event (notifications).
	OnEvent func(model.UpsEvent)
}

func NewManager(configs []map[string]any, interval float64, timeout time.Duration, offlineAfter float64,
	historyS int, eventsFile string) (*Manager, error) {
	m := &Manager{timeout: timeout, offlineAfter: offlineAfter, eventsFile: eventsFile,
		maxlen: max(1, int(float64(historyS)/interval)), states: map[string]*model.UpsState{},
		history: map[string][]model.UpsPoint{}}
	for _, raw := range configs {
		c, err := Build(raw)
		if err != nil {
			return nil, err
		}
		m.Sources = append(m.Sources, c)
	}
	return m, nil
}

// ------------------------------------------------------------- events
func (m *Manager) event(st *model.UpsState, kind, severity, msg string) {
	m.seq++
	ev := model.UpsEvent{Seq: m.seq, T: time.Now().UTC(), UpsID: st.ID, UpsName: st.Name, Kind: kind,
		Severity: severity, Message: msg, StatusRaw: st.StatusRaw}
	m.events = append(m.events, ev)
	if len(m.events) > 500 {
		m.events = m.events[len(m.events)-500:]
	}
	if severity != "info" {
		slog.Warn("UPS event", "ups", st.Name, "message", msg)
	} else {
		slog.Info("UPS event", "ups", st.Name, "message", msg)
	}
	if m.eventsFile != "" {
		if err := appendLine(m.eventsFile, ev); err != nil {
			slog.Warn("cannot write UPS event log", "error", err)
		}
	}
}

func appendLine(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

func onBatt(s string) bool { return s == "on_battery" || s == "low_battery" }

func fmtCharge(c *float64) string {
	if c == nil || *c == 0 {
		return "?"
	}
	if *c == float64(int64(*c)) {
		return fmt.Sprintf("%.1f", *c) // Python prints floats as 87.0
	}
	return fmt.Sprint(*c)
}

func (m *Manager) transitions(prev, st *model.UpsState) {
	was := ""
	if prev != nil {
		was = prev.Status
	}
	if was == "unreachable" && st.Status != "unreachable" {
		m.event(st, "communication_restored", "info", "communication with UPS restored")
	}
	if onBatt(st.Status) && !onBatt(was) {
		m.event(st, "on_battery", "warning", fmt.Sprintf("power lost: on battery, %s%% charge, %s runtime",
			fmtCharge(st.BatteryCharge), FmtRuntime(st.BatteryRuntimeS)))
	}
	if st.Status == "low_battery" && was != "low_battery" {
		m.event(st, "low_battery", "critical", "LOW BATTERY: "+FmtRuntime(st.BatteryRuntimeS)+" runtime left")
	}
	if st.Status == "online" && onBatt(was) {
		msg := "power restored: back on line"
		if prev != nil && prev.OnBatterySince != nil {
			if d := time.Since(*prev.OnBatterySince).Seconds(); d >= 0.5 {
				msg += fmt.Sprintf(" after %.0f s", d)
			}
		}
		m.event(st, "power_restored", "info", msg)
	}
	for _, x := range [][3]string{{"replace_battery", "warning", "UPS reports: replace battery"},
		{"overload", "warning", "UPS reports: overload"}} {
		if slices.Contains(st.Flags, x[0]) && (prev == nil || !slices.Contains(prev.Flags, x[0])) {
			m.event(st, x[0], x[1], x[2])
		}
	}
}

// ------------------------------------------------------------- updates
var rowSkip = map[string]bool{"name": true, "status": true, "powers": true, "id": true, "via": true,
	"ups_name": true, "last_seen": true, "on_battery_since": true, "stale": true}

func (m *Manager) apply(key string, row map[string]any, name string, via *string, powers []string) {
	now := time.Now().UTC()
	prev := m.states[key]
	fields := map[string]any{}
	for k, v := range row {
		if !rowSkip[k] {
			fields[k] = v
		}
	}
	b, _ := json.Marshal(fields)
	st := &model.UpsState{}
	_ = json.Unmarshal(b, st)
	st.ID, st.Name, st.UpsName, st.Via, st.Powers, st.LastSeen = key, name, fmt.Sprint(row["name"]), via, powers, &now
	st.Status, _ = row["state"].(string)
	if st.Status == "" {
		st.Status = "unknown"
	}
	st.Stale = false
	if st.Flags == nil {
		st.Flags = []string{}
	}
	if st.Powers == nil {
		st.Powers = []string{}
	}
	if onBatt(st.Status) {
		if prev != nil && prev.OnBatterySince != nil {
			st.OnBatterySince = prev.OnBatterySince
		} else {
			st.OnBatterySince = &now
		}
	}
	m.transitions(prev, st)
	if prev == nil {
		m.order = append(m.order, key)
	}
	m.states[key] = st
}

func (m *Manager) fail(keys []string, errMsg string) {
	now := time.Now()
	for _, key := range keys {
		st := m.states[key]
		if st == nil {
			continue
		}
		cp := *st
		gone := st.LastSeen == nil || now.Sub(*st.LastSeen).Seconds() >= m.offlineAfter
		cp.Stale = true
		cp.Error = model.P(errMsg)
		if gone && st.Status != "unreachable" {
			cp.Status = "unreachable"
			m.event(&cp, "communication_lost", "warning", "lost communication with UPS ("+errMsg+")")
		}
		m.states[key] = &cp
	}
}

// PollSource polls one configured NUT source.
func (m *Manager) PollSource(ctx context.Context, c *Source, timeout time.Duration) {
	if timeout <= 0 {
		timeout = m.timeout
	}
	rows, err := c.Read(ctx, timeout)
	m.mu.Lock()
	evBefore := m.seq
	if err != nil {
		var keys []string
		for _, k := range m.order {
			if strings.HasPrefix(k, c.ID+"/") {
				keys = append(keys, k)
			}
		}
		m.fail(keys, normalise.Describe(err))
	} else {
		for _, row := range rows {
			n := fmt.Sprint(row["name"])
			label := n
			if c.Name != "" && len(rows) == 1 {
				label = c.Name
			} else if d, _ := row["description"].(string); d != "" {
				label = d
			}
			m.apply(c.ID+"/"+n, row, label, nil, c.Powers.For(n))
		}
	}
	evs := m.newEvents(evBefore)
	m.mu.Unlock()
	m.emit(evs)
}

func (m *Manager) newEvents(after int) []model.UpsEvent {
	var out []model.UpsEvent
	for _, e := range m.events {
		if e.Seq > after {
			out = append(out, e)
		}
	}
	return out
}

func (m *Manager) emit(evs []model.UpsEvent) {
	if m.OnEvent != nil {
		for _, e := range evs {
			m.OnEvent(e)
		}
	}
}

func (m *Manager) PollAll(ctx context.Context) {
	var wg sync.WaitGroup
	for _, s := range m.Sources {
		wg.Add(1)
		go func(s *Source) { defer wg.Done(); m.PollSource(ctx, s, 0) }(s)
	}
	wg.Wait()
	m.SampleHistory()
}

func (m *Manager) SampleHistory() {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	for _, k := range m.order {
		st := m.states[k]
		if st.Status == "unreachable" {
			continue
		}
		h := append(m.history[k], model.UpsPoint{T: now, Charge: st.BatteryCharge, Load: st.LoadPercent,
			Watts: st.Watts, RuntimeS: st.BatteryRuntimeS})
		if len(h) > m.maxlen {
			h = h[len(h)-m.maxlen:]
		}
		m.history[k] = h
	}
}

// UpdateFromAgent merges `ups` rows relayed by an agent. unreachable: the agent itself was unreachable.
func (m *Manager) UpdateFromAgent(agentID string, rows []map[string]any, unreachable bool, powers map[string][]string, names map[string]string) {
	m.mu.Lock()
	evBefore := m.seq
	var mine []string
	for _, k := range m.order {
		if v := m.states[k].Via; v != nil && *v == agentID {
			mine = append(mine, k)
		}
	}
	if unreachable {
		m.fail(mine, "agent unreachable")
	} else {
		seen := map[string]bool{}
		for _, row := range rows {
			n, _ := row["name"].(string)
			if n == "" {
				continue
			}
			key := agentID + "/" + n
			if ok, isBool := row["ok"].(bool); isBool && !ok {
				var hit []string
				for _, k := range mine {
					if k == key || n == "upsd" {
						hit = append(hit, k)
						seen[k] = true
					}
				}
				e := fmt.Sprint(row["error"])
				if row["error"] == nil || e == "" {
					e = "error"
				}
				if len(e) > 120 {
					e = e[:120]
				}
				m.fail(hit, e)
				continue
			}
			seen[key] = true
			label := names[n]
			for _, k := range []string{"description", "model"} {
				if label == "" {
					label, _ = row[k].(string)
				}
			}
			if label == "" {
				label = n
			}
			p := slices.Clone(powers[n])
			if p == nil {
				p = []string{}
			}
			m.apply(key, row, label, model.P(agentID), p)
		}
		for _, k := range mine {
			if !seen[k] { // agent no longer reports it (UPS removed / relay turned off)
				delete(m.states, k)
				m.order = slices.DeleteFunc(m.order, func(x string) bool { return x == k })
			}
		}
	}
	evs := m.newEvents(evBefore)
	m.mu.Unlock()
	m.emit(evs)
}

// ------------------------------------------------------------- views
func (m *Manager) Overview(history bool) model.UpsOverview {
	m.mu.Lock()
	defer m.mu.Unlock()
	ov := model.UpsOverview{GeneratedAt: time.Now().UTC(), UPS: []model.UpsState{}, History: map[string][]model.UpsPoint{}}
	for _, k := range m.order {
		ov.UPS = append(ov.UPS, *m.states[k])
	}
	if history {
		for k, v := range m.history {
			ov.History[k] = slices.Clone(v)
		}
	}
	return ov
}

func (m *Manager) EventsSince(seq int) []model.UpsEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.newEvents(seq)
}

func (m *Manager) Seq() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.seq
}

// SystemsMap: system id -> UPS ids powering it.
func (m *Manager) SystemsMap() map[string][]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string][]string{}
	for _, k := range m.order {
		st := m.states[k]
		for _, sid := range st.Powers {
			out[sid] = append(out[sid], st.ID)
		}
	}
	return out
}

// States returns a copy (for tests).
func (m *Manager) State(key string) *model.UpsState {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.states[key]; s != nil {
		cp := *s
		return &cp
	}
	return nil
}
