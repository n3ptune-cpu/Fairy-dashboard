package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/n3ptune-cpu/Fairy/internal/model"
)

var watchRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// stream: Server-Sent Events, pushed every `intervals.stream` (1 s): `systems` (full, first), then
// `systems_delta` {changed, removed}; `summary`, `ups`, `power` only when they change; `ups_event` on UPS
// transitions; a `: keepalive` comment every 15 s regardless of traffic (proxy idle timers).
// `?watch=<id>` boosts that system to 1 s polling while the stream is open.
func (s *Server) stream(c *reqCtx) error {
	if _, err := c.requireUser(); err != nil {
		return err
	}
	watch := c.r.URL.Query().Get("watch")
	if watch != "" && (len(watch) > 80 || !watchRe.MatchString(watch)) {
		return herr(422, "invalid watch id")
	}
	boost := ""
	if watch != "" && s.Poller.HasSystem(watch) {
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
	upsSeq := s.Poller.UPS.Seq() // only push UPS events that happen after connecting
	var sent map[string][]byte
	var sentOrder []string
	last := map[string][]byte{}
	quiet := time.Now()
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
		var out bytes.Buffer
		now := time.Now().UTC().Format(time.RFC3339Nano)
		snap, order := s.Poller.SnapshotJSON()
		if sent == nil {
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
				if !bytes.Equal(sent[id], snap[id]) {
					changed = append(changed, snap[id])
				}
			}
			removed := []string{}
			for _, id := range sentOrder {
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
		sent, sentOrder = snap, order
		sum := s.Poller.Summary()
		upsOv := s.Poller.UPS.Overview(false)
		pow := s.Poller.Power.Overview()
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
			if !bytes.Equal(last[ev.name], key) {
				last[ev.name] = key
				full, _ := json.Marshal(ev.full)
				fmt.Fprintf(&out, "event: %s\ndata: %s\n\n", ev.name, full)
			}
		}
		for _, e := range s.Poller.UPS.EventsSince(upsSeq) {
			b, _ := json.Marshal(e)
			fmt.Fprintf(&out, "event: ups_event\nid: ups-%d\ndata: %s\n\n", e.Seq, b)
			upsSeq = e.Seq
		}
		if time.Since(quiet) > 15*time.Second { // always, even while data flows
			quiet = time.Now()
			out.WriteString(": keepalive\n\n")
		}
		if out.Len() > 0 {
			_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
			if _, err := c.w.Write(out.Bytes()); err != nil {
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

func withoutTime(s model.Summary) model.Summary {
	s.GeneratedAt = time.Time{}
	return s
}
