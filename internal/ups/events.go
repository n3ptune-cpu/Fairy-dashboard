package ups

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	upssrc "github.com/n3ptune-cpu/Fairy-agent/pkg/ups"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// FmtRuntime formats seconds as m:ss ("?" when unknown).
func FmtRuntime(s *int) string {
	if s == nil {
		return "?"
	}
	return fmt.Sprintf("%d:%02d", *s/60, *s%60)
}

// maxSourceEvents is how many UPS events one source (a configured UPS server, or an agent that relays UPSes)
// may cause in the limiter's window (10 minutes). The limit per UPS is 10; a source with several UPSes in a real
// power cut needs a few events for each of them, and one that gives its UPSes new names on every poll gets no
// more than this.
const maxSourceEvents = 40

// sourceOf is the source of a UPS id ("<source or agent id>/<UPS name>").
func sourceOf(id string) string {
	src, _, _ := strings.Cut(id, "/")
	return src
}

// event records a transition. The shared limiter (Fairy-agent's pkg/ups, at most 10 events per UPS in 10
// minutes) keeps a flapping or hostile UPS from flooding the list, the notifications and the log, and a second
// one counts per source (maxSourceEvents), because the limit per UPS alone is avoided by renaming the UPS. The
// next event that gets through says how many were dropped.
func (m *Manager) event(st *model.UpsState, kind, severity, msg string) {
	now := time.Now()
	ok, dropped := m.limiter.Allow(st.ID, now)
	if !ok {
		return
	}
	ok, droppedSource := m.sourceLimiter.Allow(sourceOf(st.ID), now)
	if !ok {
		return
	}
	if dropped > 0 {
		msg += fmt.Sprintf(" (%d earlier events of this UPS not shown: too many in a short time)", dropped)
		slog.Warn("UPS events were rate-limited", "ups", st.Name, "dropped", dropped)
	}
	if droppedSource > 0 {
		msg += fmt.Sprintf(" (%d earlier UPS events from the same source not shown: too many in a short time)", droppedSource)
		slog.Warn("UPS events were rate-limited", "source", sourceOf(st.ID), "dropped", droppedSource)
	}
	m.seq++
	ev := model.UpsEvent{Seq: m.seq, T: now.UTC(), UpsID: st.ID, UpsName: st.Name, Kind: kind,
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
		b, err := json.Marshal(ev)
		if err == nil {
			err = upssrc.AppendLine(m.eventsFile, b, maxEventsFile) // rotated to .1: about 2 x 4 MiB on disk
		}
		if err != nil {
			slog.Warn("cannot write UPS event log", "error", err)
		}
	}
}

// maxEventsFile: the event log is rotated to <file>.1 past this size.
const maxEventsFile = 4 << 20

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
