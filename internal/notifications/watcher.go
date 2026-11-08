package notifications

import (
	"fmt"
	"sync"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// Thresholds for temperature events: hot at HotC, back to normal below CoolC (hysteresis).
const (
	HotC  = 85.0
	CoolC = 80.0
)

// Watcher turns the poller's snapshots into events on state changes only: a host's new state must hold for
// Debounce before it counts, a host that changes state FlapCount times within FlapWindow gets one "unstable"
// warning and stays quiet until it settles, and a host shut down from the panel is "off", not an alarm.
type Watcher struct {
	Store      *Store
	Debounce   time.Duration // default 30 s
	FlapWindow time.Duration // default 10 min
	FlapCount  int           // default 6

	mu       sync.Mutex
	started  bool
	hosts    map[string]*hostState
	panelOff map[string]time.Time // system id -> until when "offline" is the panel's own shutdown / reboot
}

type hostState struct {
	up, hot         bool
	pendUp, pendHot *bool
	pendSince       time.Time
	pendHotSince    time.Time
	flips           []time.Time
	quietUntil      time.Time
	offByPanel      bool
	lastTemp        float64
}

func (w *Watcher) defaults() {
	if w.Debounce == 0 {
		w.Debounce = 30 * time.Second
	}
	if w.FlapWindow == 0 {
		w.FlapWindow = 10 * time.Minute
	}
	if w.FlapCount == 0 {
		w.FlapCount = 6
	}
	if w.hosts == nil {
		w.hosts = map[string]*hostState{}
		w.panelOff = map[string]time.Time{}
	}
}

// NotePanelPower records a shutdown / reboot requested in the panel: the host going offline in the next 30
// minutes (shutdown) or 10 minutes (reboot) is expected.
func (w *Watcher) NotePanelPower(systemID, action string, now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.defaults()
	switch action {
	case "shutdown":
		w.panelOff[systemID] = now.Add(30 * time.Minute)
	case "reboot":
		w.panelOff[systemID] = now.Add(10 * time.Minute)
	case "cancel":
		delete(w.panelOff, systemID)
	}
}

func isHost(s model.System) bool { return s.Kind == "node" || s.Kind == "agent" }

func hottest(s model.System) (float64, string) {
	best, what := -1.0, ""
	for _, t := range s.Temps {
		if t.CurrentC > best {
			best, what = t.CurrentC, model.Deref(t.Label)
			if what == "" {
				what = t.Sensor
			}
		}
	}
	for _, g := range s.GPUs {
		if g.TempC != nil && *g.TempC > best {
			best, what = *g.TempC, "GPU"
		}
	}
	return best, what
}

// Observe compares a snapshot with the last one. The first call only learns the current state (no events at
// panel start). upsEvents are the UPS manager's events since the last call.
func (w *Watcher) Observe(now time.Time, systems []model.System, upsEvents []model.UpsEvent) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.defaults()
	first := !w.started
	w.started = true
	seen := map[string]bool{}
	for _, s := range systems {
		if !isHost(s) {
			continue
		}
		seen[s.ID] = true
		up := s.Status != "offline"
		temp, what := hottest(s)
		st := w.hosts[s.ID]
		if st == nil || first {
			w.hosts[s.ID] = &hostState{up: up, hot: up && temp >= HotC, lastTemp: temp}
			continue
		}
		w.observeStatus(now, s, st, up)
		if up {
			w.observeTemp(now, s, st, temp, what)
		}
	}
	for id := range w.hosts {
		if !seen[id] {
			delete(w.hosts, id)
		}
	}
	for id, until := range w.panelOff {
		if now.After(until) {
			delete(w.panelOff, id)
		}
	}
	if !first {
		for _, e := range upsEvents {
			w.upsEvent(e)
		}
	}
}

func (w *Watcher) observeStatus(now time.Time, s model.System, st *hostState, up bool) {
	if up == st.up {
		st.pendUp = nil
		return
	}
	if st.pendUp == nil || *st.pendUp != up {
		v := up
		st.pendUp, st.pendSince = &v, now
	}
	if now.Sub(st.pendSince) < w.Debounce {
		return
	}
	st.up, st.pendUp = up, nil
	st.flips = append(st.flips, now)
	for len(st.flips) > 0 && now.Sub(st.flips[0]) > w.FlapWindow {
		st.flips = st.flips[1:]
	}
	key := "offline:" + s.ID
	if now.Before(st.quietUntil) {
		return
	}
	if len(st.flips) >= w.FlapCount {
		st.quietUntil = now.Add(w.FlapWindow)
		w.Store.Add(Event{Kind: "host_unstable", Type: "host", Severity: Warning, SystemID: s.ID, Host: s.Name,
			Text: fmt.Sprintf("unstable: went offline and online %d times in %s (quiet for %s)", len(st.flips), w.FlapWindow, w.FlapWindow)})
		return
	}
	if !up {
		if until, ok := w.panelOff[s.ID]; ok && now.Before(until) {
			st.offByPanel = true
			w.Store.Add(Event{Kind: "host_off", Type: "host", Severity: Info, SystemID: s.ID, Host: s.Name,
				Text: "off (shut down from the panel)"})
			return
		}
		why := "offline"
		if s.Error != nil && *s.Error != "" {
			why += " · " + clip(*s.Error, 80)
		}
		w.Store.Add(Event{Kind: "host_offline", Type: "host", Severity: Warning, SystemID: s.ID, Host: s.Name,
			Text: why, Key: key, Alarm: true})
		return
	}
	text := "back online"
	if st.offByPanel {
		text = "on again"
	}
	st.offByPanel = false
	delete(w.panelOff, s.ID)
	w.Store.Add(Event{Kind: "host_online", Type: "host", Severity: Info, SystemID: s.ID, Host: s.Name, Text: text, Key: key})
}

func (w *Watcher) observeTemp(now time.Time, s model.System, st *hostState, temp float64, what string) {
	hot := st.hot
	switch {
	case temp >= HotC:
		hot = true
	case temp >= 0 && temp < CoolC:
		hot = false
	}
	if hot == st.hot {
		st.pendHot = nil
		return
	}
	if st.pendHot == nil || *st.pendHot != hot {
		v := hot
		st.pendHot, st.pendHotSince = &v, now
	}
	if now.Sub(st.pendHotSince) < w.Debounce/3 { // 10 s by default: one hot reading is not an event
		return
	}
	st.hot, st.pendHot = hot, nil
	key := "temp:" + s.ID
	if hot {
		w.Store.Add(Event{Kind: "temp_high", Type: "temperature", Severity: Warning, SystemID: s.ID, Host: s.Name,
			Text: fmt.Sprintf("%s %.0f °C (limit %.0f °C)", what, temp, HotC), Key: key, Alarm: true})
		return
	}
	w.Store.Add(Event{Kind: "temp_ok", Type: "temperature", Severity: Info, SystemID: s.ID, Host: s.Name,
		Text: fmt.Sprintf("temperature back to normal (%.0f °C)", temp), Key: key})
}

func (w *Watcher) upsEvent(e model.UpsEvent) {
	ev := Event{Kind: "ups_" + e.Kind, Type: "ups", Severity: e.Severity, UPSID: e.UpsID, Host: e.UpsName, Text: clip(e.Message, 160)}
	switch e.Kind {
	case "on_battery", "low_battery":
		ev.Key, ev.Alarm = "ups:"+e.UpsID, true
	case "power_restored":
		ev.Key = "ups:" + e.UpsID
	case "communication_lost":
		ev.Key, ev.Alarm = "upscomm:"+e.UpsID, true
	case "communication_restored":
		ev.Key = "upscomm:" + e.UpsID
	}
	w.Store.Add(ev)
}
