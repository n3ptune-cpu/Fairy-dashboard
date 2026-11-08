package poller

import (
	"slices"
	"strconv"
	"strings"

	"github.com/n3ptune-cpu/Fairy/internal/model"
	"github.com/n3ptune-cpu/Fairy/internal/scheduler"
)

func (p *Poller) HasSystem(id string) bool {
	for _, s := range p.Snapshot() {
		if s.ID == id {
			return true
		}
	}
	return false
}

func (p *Poller) SetPoll(id string, v *scheduler.Override) error {
	err := p.Scheduler.SetOverride(id, v)
	p.mu.Lock()
	p.snapVer, p.snapJSONVer = -1, -1
	if v == nil || !v.Paused {
		delete(p.fingerprints, id)
	}
	p.mu.Unlock()
	p.Bump()
	return err
}

func fmtG(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }

func (p *Poller) PollStatus(id string) model.PollStatus {
	sch := p.Scheduler
	req, has := sch.Overrides()[id]
	jobs := sch.Describe(id)
	var effs []float64
	for _, j := range jobs {
		if j.EffectiveS != nil {
			effs = append(effs, *j.EffectiveS)
		}
	}
	var notes []string
	for _, j := range jobs {
		if j.Clamped {
			kind, _, _ := strings.Cut(j.Job, ":")
			notes = append(notes, kind+" source polls at most every "+fmtG(j.FloorS)+" s")
		}
	}
	shared := false
	for _, j := range jobs {
		shared = shared || j.Shared
	}
	if shared && has && !req.Paused && len(effs) > 0 && req.Seconds > slices.Min(effs) {
		notes = append(notes, "shares one Proxmox API call with its cluster, which runs at the fastest rate any member asks for")
	}
	ps := model.PollStatus{SystemID: id, Mode: "default", Boosted: sch.Boosted(id), BoostEnabled: sch.BoostEnabled, Jobs: jobs}
	if has {
		if req.Paused {
			ps.Mode = "paused"
		} else {
			ps.Mode = "override"
			ps.RequestedS = model.P(req.Seconds)
		}
	}
	if len(effs) > 0 {
		ps.EffectiveS = model.P(slices.Min(effs))
	}
	if len(notes) > 0 {
		ps.Note = model.P(strings.Join(notes, "; "))
	}
	return ps
}
