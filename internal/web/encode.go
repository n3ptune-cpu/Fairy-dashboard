package web

import (
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
)

// The web layer's last line of defence against a value that cannot be written as JSON (a number that is not a
// number, a date outside the years 0 to 9999). Numbers and dates are checked where they enter the panel and
// the poller hands out only systems it could write, so none of this should ever run. If it does, the answer
// stays whole: the one system, power source or UPS that cannot be written is replaced by a stub that says so
// (a system gets status "error", a power source or UPS "unreachable"), everything else in the answer is
// untouched, and nothing is sent that is not valid JSON.

const cannotShow = "the panel cannot show this data (a value that is not a number, or a date out of range)"

var encodeWarned sync.Map // kind of data -> time of the last log line (one line a minute per kind)

func encodeFailed(what, id string, err error) {
	now := time.Now()
	if last, ok := encodeWarned.Load(what); ok && now.Sub(last.(time.Time)) < time.Minute {
		return
	}
	encodeWarned.Store(what, now)
	slog.Warn("data that cannot be written as JSON was replaced by a stub in an answer", "what", what, "id", id, "error", err)
}

func writable(v any) bool {
	_, err := json.Marshal(v)
	return err == nil
}

// systemJSON is one system as JSON; a system that cannot be written is its error entry (status "error").
func systemJSON(x model.System) []byte {
	_, b, err := poller.EncodeSystem(x)
	if err != nil {
		encodeFailed("system", x.ID, err)
	}
	return b
}

// reduced returns v with every part that cannot be written as JSON replaced by its stub. ok is false for a
// type this file does not know. Only called after writing v failed; v itself is never changed (lists are
// copied), because it may share memory with the poller's state.
func reduced(v any) (any, bool) {
	switch x := v.(type) {
	case model.System:
		e, _, _ := poller.EncodeSystem(x)
		return e, true
	case model.SystemList:
		out := make([]model.System, len(x.Systems))
		for i, s := range x.Systems {
			out[i], _, _ = poller.EncodeSystem(s)
		}
		x.Systems = out
		return x, true
	case model.PowerOverview:
		if !writable(x.TotalW) {
			x.TotalW = nil
		}
		out := make([]model.PowerSourceState, len(x.Sources))
		for i, s := range x.Sources {
			if !writable(s) {
				s = model.PowerSourceState{ID: s.ID, Type: s.Type, Name: s.Name, Status: "unreachable",
					IncludeInTotal: s.IncludeInTotal, Readings: []model.PowerReading{}, Error: model.P(cannotShow), Via: s.Via}
			}
			out[i] = s
		}
		x.Sources = out
		return x, true
	case model.UpsOverview:
		out := make([]model.UpsState, len(x.UPS))
		for i, u := range x.UPS {
			if !writable(u) {
				u = model.UpsState{ID: u.ID, Name: u.Name, UpsName: u.UpsName, Via: u.Via, Status: "unreachable", Stale: true,
					Flags: []string{}, Powers: append([]string{}, u.Powers...), Error: model.P(cannotShow)}
			}
			out[i] = u
		}
		x.UPS = out
		if x.History != nil {
			hist := make(map[string][]model.UpsPoint, len(x.History))
			for id, pts := range x.History {
				if !writable(pts) {
					pts = []model.UpsPoint{}
				}
				hist[id] = pts
			}
			x.History = hist
		}
		return x, true
	case model.Summary:
		if x.ControlDisabled != nil && !writable(x.ControlDisabled) {
			cd := *x.ControlDisabled
			cd.At = time.Time{}
			x.ControlDisabled = &cd
		}
		if x.Cluster != nil && !writable(x.Cluster) {
			c := *x.Cluster
			c.CPUPercent = nil
			x.Cluster = &c
		}
		clusters := make([]model.ClusterTotals, len(x.Clusters))
		for i, c := range x.Clusters {
			if !writable(c) {
				c.CPUPercent = nil
			}
			clusters[i] = c
		}
		x.Clusters = clusters
		if !writable(x.Totals) {
			x.Totals.CPUPercent = nil
		}
		sources := make([]model.SourceStatus, len(x.Sources))
		for i, s := range x.Sources {
			if !writable(s) {
				s.LastSuccess, s.LatencyMS = nil, nil
			}
			sources[i] = s
		}
		x.Sources = sources
		return x, true
	case model.UpsEvent:
		x.T = time.Now().UTC() // the only field that can fail to be written
		return x, true
	}
	return nil, false
}

// safeJSON writes v as JSON; when that fails, v with the parts that cannot be written replaced (see reduced).
// It returns what was written (v or its reduced form). ok is false when nothing valid can be written: the
// caller then sends nothing instead of broken JSON.
func safeJSON(what string, v any) (b []byte, written any, ok bool) {
	b, err := json.Marshal(v)
	if err == nil {
		return b, v, true
	}
	encodeFailed(what, "", err)
	r, known := reduced(v)
	if !known {
		return nil, nil, false
	}
	if b, err = json.Marshal(r); err != nil {
		return nil, nil, false
	}
	return b, r, true
}
