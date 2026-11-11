package wakerun

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"slices"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/statefile"
)

// The journal (wake-update.json in the data folder, mode 0600) holds the active run and the last finished
// ones. It is written before a run sends anything and at every change of a machine's state, so that after a
// restart the panel can say which machines an interrupted run had woken and what it was doing with each.
//
// That is all it is for. Nothing is ever sent because of what the file says: a run that was going when the
// panel stopped becomes "interrupted", and the proof a run needs to switch a machine off (that this run woke
// it) is not taken from a file. Whoever can write the file can make the panel show a false report, nothing
// more. It holds machine ids and names, states, times and who started the run; no token and no secret.

const (
	journalVersion = 1
	journalMax     = 1 << 20 // a journal of ten runs of sixteen machines is a tenth of this
)

type journalFile struct {
	Version int    `json:"version"`
	Active  *Run   `json:"active"`
	History []*Run `json:"history"`
}

// Load reads the journal. A run that was active when the panel stopped is marked interrupted, every machine
// of it that had not ended gets the sentence that says where it was, and the run is reported once (an
// EvInterrupted event). A journal that cannot be read is left as it is, and no run starts until it is fixed
// or moved away.
func (m *Manager) Load() error {
	if m.path == "" {
		return nil
	}
	jf, err := readJournal(m.path)
	m.mu.Lock()
	if err != nil {
		m.jerr = err
		m.mu.Unlock()
		slog.Error("wake-update: the run journal cannot be used; no run starts until it is fixed or moved away", "path", m.path, "error", err)
		return err
	}
	m.jerr, m.active = nil, nil
	m.hist = jf.History
	var report *Run
	now := m.now()
	if r := jf.Active; r != nil {
		interrupt(r, now)
		m.hist = append([]*Run{r}, m.hist...)
	}
	if len(m.hist) > historyKept {
		m.hist = m.hist[:historyKept]
	}
	for _, r := range m.hist {
		if r.State == RunInterrupted && !r.Reported {
			r.Reported = true
			report = r.clone()
			break
		}
	}
	m.mu.Unlock()
	if report != nil {
		m.save()
		slog.Warn("wake-update: the panel restarted during a run; it is not continued", "run", report.ID, "started_by", report.By)
		if m.Event != nil {
			m.Event(Event{Kind: EvInterrupted, Run: report})
		}
	}
	return nil
}

// interrupt turns a run that was going into the record of where it stopped.
func interrupt(r *Run, now time.Time) {
	r.State, r.EndedAt = RunInterrupted, &now
	r.Note = "the panel restarted during this run; a run is never continued"
	for _, h := range r.Hosts {
		if h.Final() {
			continue
		}
		was := h.State
		h.State, h.Since = StInterrupted, now
		switch {
		case was == StShutting && h.ShutdownAt != nil:
			h.On = OnUnknown
			h.Text = fmt.Sprintf("The panel restarted while its shutdown was counting down (due at %s). Fairy stopped watching: check that it went off.", clock(*h.ShutdownAt))
		case h.AlreadyOn || h.Mode == ModeUpdateOnly:
			h.On = OnYes
			h.Text = "The panel restarted during the run. It was already on and stays on." + updateTail(h)
		case h.OnlineAt != nil:
			h.On = OnYes
			h.Text = "The panel restarted during the run. The run had woken it and it was ON (" + was + "): it is still on unless someone switched it off." + updateTail(h)
		case h.WakeSentAt != nil:
			h.On = OnUnknown
			h.Text = fmt.Sprintf("The panel restarted during the run. A wake was sent to it at %s and it had not answered yet: if it switched on, it is ON.", clock(*h.WakeSentAt))
		default:
			h.On = OnUntouched
			h.Text = "The panel restarted before the run reached it. Nothing was sent to it."
		}
		h.Log = append(h.Log, LogLine{T: now, Text: h.Text})
	}
	lines := make([]string, 0, len(r.Hosts))
	for _, h := range r.Hosts {
		lines = append(lines, h.Name+": "+h.Text)
	}
	r.Summary = joinLines(lines)
}

func joinLines(l []string) string {
	out := ""
	for i, s := range l {
		if i > 0 {
			out += "\n"
		}
		out += s
	}
	return out
}

func readJournal(path string) (journalFile, error) {
	var jf journalFile
	st, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return jf, nil
	}
	if err != nil {
		return jf, fmt.Errorf("cannot read %s: %w", path, err)
	}
	if st.Size() > journalMax {
		return jf, fmt.Errorf("%s is larger than a run journal ever is (%d bytes)", path, st.Size())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return jf, fmt.Errorf("cannot read %s: %w", path, err)
	}
	if len(b) == 0 {
		return jf, nil
	}
	if err := json.Unmarshal(b, &jf); err != nil {
		return jf, fmt.Errorf("%s is not a run journal (%v)", path, err)
	}
	if jf.Version != journalVersion {
		return jf, fmt.Errorf("%s has version %d, not %d", path, jf.Version, journalVersion)
	}
	if jf.Active != nil && (jf.Active.ID == "" || len(jf.Active.Hosts) > MaxHosts) {
		return jf, fmt.Errorf("%s does not hold a run this panel wrote", path)
	}
	for _, r := range jf.History {
		if r == nil || len(r.Hosts) > MaxHosts {
			return jf, fmt.Errorf("%s does not hold runs this panel wrote", path)
		}
	}
	// A machine's entry that is null (the panel never writes one; a file edited by hand can have it) is left
	// out, and the log says so once. It says nothing about any machine, so it is no reason to stop new runs,
	// and it must never keep the panel from starting. The rest of the run is reported as it is.
	dropped := 0
	for _, r := range append([]*Run{jf.Active}, jf.History...) {
		if r == nil {
			continue
		}
		n := len(r.Hosts)
		r.Hosts = slices.DeleteFunc(r.Hosts, func(h *Host) bool { return h == nil })
		dropped += n - len(r.Hosts)
	}
	if dropped > 0 {
		slog.Warn("wake-update: the run journal has empty (null) machine entries; they are left out", "path", path, "entries", dropped)
	}
	return jf, nil
}

// save writes the journal: locked, to a temporary file, renamed (statefile). A failure is logged and does not
// stop the run that is going: the machines matter more than the record.
func (m *Manager) save() {
	if m.path == "" {
		return
	}
	m.mu.Lock()
	if m.jerr != nil { // never overwrite a file that could not be read
		m.mu.Unlock()
		return
	}
	jf := journalFile{Version: journalVersion, History: []*Run{}}
	if m.active != nil {
		jf.Active = m.active.clone()
	}
	for _, r := range m.hist {
		jf.History = append(jf.History, r.clone())
	}
	m.mu.Unlock()
	_, err := statefile.Update(m.path, func(d statefile.Data) error {
		clear(d)
		d["version"], d["active"], d["history"] = jf.Version, jf.Active, jf.History
		return nil
	})
	if err != nil {
		slog.Error("wake-update: cannot write the run journal", "path", m.path, "error", err)
	}
}
