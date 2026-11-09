package ups

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// FmtRuntime formats seconds as m:ss ("?" when unknown).
func FmtRuntime(s *int) string {
	if s == nil {
		return "?"
	}
	return fmt.Sprintf("%d:%02d", *s/60, *s%60)
}

func (m *Manager) event(st *model.UpsState, kind, severity, msg string) {
	now := time.Now()
	r := m.evRate[st.ID]
	if now.Sub(r.since) >= time.Hour {
		r = evRate{since: now}
	}
	r.n++
	m.evRate[st.ID] = r
	if r.n > MaxEventsPerHour {
		if r.n == MaxEventsPerHour+1 {
			slog.Warn("UPS events: too many for one UPS, dropping the rest of this hour", "ups", st.Name)
		}
		return
	}
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

type evRate struct {
	since time.Time
	n     int
}

// maxEventsFile: the event log is rotated to <file>.1 past this size (two files at most).
const maxEventsFile = 4 << 20

func appendLine(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if st, err := os.Stat(path); err == nil && st.Size()+int64(len(b)) > maxEventsFile {
		if err := os.Rename(path, path+".1"); err != nil {
			return err
		}
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

// EventsSince returns the events after sequence number seq, oldest first.
func (m *Manager) EventsSince(seq int) []model.UpsEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.newEvents(seq)
}

// Seq is the latest event's sequence number.
func (m *Manager) Seq() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.seq
}
