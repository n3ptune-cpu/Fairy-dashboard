package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/n3ptune-cpu/Fairy/internal/apierr"
	"github.com/n3ptune-cpu/Fairy/internal/model"
)

var watchRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// keepaliveEvery is how often a `: keepalive` comment is sent, even while data flows (proxy idle timers).
const keepaliveEvery = 15 * time.Second

// stream: Server-Sent Events, pushed every `intervals.stream` (1 s): `systems` (full, first), then
// `systems_delta` {changed, removed}; `summary`, `ups`, `power` only when they change; `ups_event` on UPS
// transitions; a `: keepalive` comment every 15 s regardless of traffic (proxy idle timers).
// `?watch=<id>` boosts that system to 1 s polling while the stream is open.
//
// Everything sent is filtered to what the user may see (see view); access is re-checked every tick and the
// stream ends when it changes (the browser reconnects and gets a fresh, re-filtered `systems` list).
func (s *Server) stream(c *reqCtx) error {
	v, err := c.view()
	if err != nil {
		return err
	}
	grant := *c.g
	watch := c.r.URL.Query().Get("watch")
	if watch != "" && (len(watch) > 80 || !watchRe.MatchString(watch)) {
		return apierr.New(422, "invalid watch id")
	}
	boost := ""
	if watch != "" && s.Poller.HasSystem(watch) && v.Has(watch) {
		boost = watch
	}
	c.flush()
	h := c.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	c.w.WriteHeader(200)
	rc := http.NewResponseController(c.w)
	if boost != "" {
		s.Poller.Scheduler.BoostAcquire(boost)
		defer s.Poller.Scheduler.BoostRelease(boost)
	}
	st := &streamState{s: s, v: v, upsSeq: s.Poller.UPS.Seq(), last: map[string][]byte{}, quiet: time.Now()}
	iv := time.Duration(s.Cfg.Intervals.Stream * float64(time.Second))
	if iv <= 0 {
		iv = time.Second
	}
	tick := time.NewTicker(iv)
	defer tick.Stop()
	isSession := c.user != nil && c.user.Auth == "session"
	for {
		if isSession && c.sessionUser() == nil { // revoked / expired while streaming
			return nil
		}
		if g := s.grantOf(c, c.user); !g.Same(grant) { // role or sites changed: start over, filtered anew
			return nil
		} else if st.ticks > 0 {
			st.v = s.newView(g)
		}
		st.ticks++
		if out := st.next(); len(out) > 0 {
			_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
			if _, err := c.w.Write(out); err != nil {
				return nil
			}
			if err := rc.Flush(); err != nil {
				return nil
			}
		}
		select {
		case <-c.r.Context().Done():
			return nil
		case <-tick.C:
		}
	}
}

// streamState is what one SSE client has already been sent.
type streamState struct {
	s         *Server
	v         *view // what this client may see (rebuilt every tick)
	ticks     int
	sent      map[string][]byte // system id -> JSON last sent (nil before the first `systems` event)
	sentOrder []string
	last      map[string][]byte // event name -> comparison key of the last summary / ups / power sent
	upsSeq    int               // last UPS event sent (starts at "now": only events after connecting)
	quiet     time.Time         // last keepalive
}

// next returns the events due in this tick (possibly none).
func (st *streamState) next() []byte {
	var out bytes.Buffer
	st.writeSystems(&out)
	st.writeChanged(&out)
	st.writeUPSEvents(&out)
	if time.Since(st.quiet) > keepaliveEvery { // always, even while data flows
		st.quiet = time.Now()
		out.WriteString(": keepalive\n\n")
	}
	return out.Bytes()
}

// writeSystems sends the full `systems` list the first time, then `systems_delta` with the systems whose
// JSON changed and the ids that disappeared.
func (st *streamState) writeSystems(out *bytes.Buffer) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	snap, order := st.v.SystemsJSON()
	if st.sent == nil {
		out.WriteString(`event: systems` + "\n" + `data: {"generated_at":"` + now + `","systems":[`)
		for i, id := range order {
			if i > 0 {
				out.WriteByte(',')
			}
			out.Write(snap[id])
		}
		out.WriteString("]}\n\n")
	} else {
		var changed [][]byte
		for _, id := range order {
			if !bytes.Equal(st.sent[id], snap[id]) {
				changed = append(changed, snap[id])
			}
		}
		removed := []string{}
		for _, id := range st.sentOrder {
			if _, ok := snap[id]; !ok {
				removed = append(removed, id)
			}
		}
		if len(changed) > 0 || len(removed) > 0 {
			rb, _ := json.Marshal(removed)
			out.WriteString(`event: systems_delta` + "\n" + `data: {"generated_at":"` + now + `","changed":[`)
			out.Write(bytes.Join(changed, []byte(",")))
			out.WriteString(`],"removed":`)
			out.Write(rb)
			out.WriteString("}\n\n")
		}
	}
	st.sent, st.sentOrder = snap, order
}

// writeChanged sends `summary`, `ups` and `power` when their content changed. Timestamps and other fields that
// change on every read are left out of the comparison key, so an unchanged panel sends nothing.
func (st *streamState) writeChanged(out *bytes.Buffer) {
	sum := st.v.Summary()
	upsOv := st.v.UPS(false)
	pow := st.v.Power()
	for _, ev := range []struct {
		name string
		full any
		key  any
	}{
		{"summary", sum, withoutTime(sum)},
		{"ups", upsOv, struct {
			U []model.UpsState            `json:"ups"`
			H map[string][]model.UpsPoint `json:"history"`
		}{upsOv.UPS, upsOv.History}},
		{"power", pow, struct {
			T *float64                 `json:"total_w"`
			S []model.PowerSourceState `json:"sources"`
		}{pow.TotalW, pow.Sources}},
	} {
		key, _ := json.Marshal(ev.key)
		if !bytes.Equal(st.last[ev.name], key) {
			st.last[ev.name] = key
			full, _ := json.Marshal(ev.full)
			fmt.Fprintf(out, "event: %s\ndata: %s\n\n", ev.name, full)
		}
	}
}

// writeUPSEvents sends every UPS transition since the last one sent, as `ups_event` with id `ups-<seq>`.
func (st *streamState) writeUPSEvents(out *bytes.Buffer) {
	for _, e := range st.s.Poller.UPS.EventsSince(st.upsSeq) {
		st.upsSeq = e.Seq
		if !st.v.all && !st.v.upsVis[e.UpsID] {
			continue
		}
		b, _ := json.Marshal(e)
		fmt.Fprintf(out, "event: ups_event\nid: ups-%d\ndata: %s\n\n", e.Seq, b)
	}
}

func withoutTime(s model.Summary) model.Summary {
	s.GeneratedAt = time.Time{}
	return s
}
