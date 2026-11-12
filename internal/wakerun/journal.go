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
// ones. It is written before a run sends anything, before every single request of a run (Manager.ask: the
// request is entered as "going out" first) and at every change of a machine's state, so that after a restart
// the panel can say which machines an interrupted run had woken and what it was doing with each.
//
// A run needs its journal: when a write fails (a full disk), nothing further is sent, and the run stops and
// says so. Because every request is entered before it leaves, the journal on the disk then still knows of
// every request that may have left, and the report after a restart never says "nothing was sent" about a
// machine that got a wake.
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
		_ = m.save() // if it cannot be written, the run is reported again after the next restart
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
		asking := h.Asking // a request that was going out when the journal was last written
		h.State, h.Since, h.Asking = StInterrupted, now, ""
		switch {
		case asking == "shutdown" && h.ShutdownAt == nil:
			h.On = OnUnknown
			h.Text = "The panel restarted while the run was asking it to shut down. The run had woken it. Fairy does not know whether the request arrived: " +
				"if it did, it switches off by itself after its countdown; if not, it is ON. Check it." + updateTail(h)
		case was == StShutting && h.ShutdownAt != nil:
			h.On = OnUnknown
			h.Text = fmt.Sprintf("The panel restarted while its shutdown was counting down (due at %s). Fairy stopped watching: check that it went off.", clock(*h.ShutdownAt))
		case h.NotFresh:
			h.On = OnYes
			h.Text = "The panel restarted during the run. " + notFreshText(h) + updateTail(h)
		case h.AlreadyOn || h.Mode == ModeUpdateOnly:
			h.On = OnYes
			h.Text = "The panel restarted during the run. It was already on and stays on." + updateTail(h)
		case h.OnlineAt != nil:
			h.On = OnYes
			h.Text = "The panel restarted during the run. The run had woken it and it was ON (" + was + "): it is still on unless someone switched it off." + updateTail(h)
		case h.WakeSentAt != nil:
			h.On = OnUnknown
			h.Text = fmt.Sprintf("The panel restarted during the run. A wake was sent to it at %s and it had not answered yet: if it switched on, it is ON.", clock(*h.WakeSentAt))
		case asking == "wake":
			h.On = OnUnknown
			h.Text = "The panel restarted while the run was sending a wake to it. Fairy does not know whether the wake went out: if it switched on, it is ON."
		default:
			h.On = OnUntouched
			h.Text = "The panel restarted before the run reached it. Nothing was sent to it."
		}
		if asking == "update" && h.UpdateSentAt == nil {
			h.Text += " An update request was going out to it; if it arrived, the agent finishes the update by itself."
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

// save writes the journal: locked, to a temporary file, renamed (statefile). A failure is logged (once, until
// a write works again), kept in m.werr, and stops the run that is going at its next step: a run sends nothing
// that it could not write down first.
func (m *Manager) save() error {
	if m.path == "" {
		return nil
	}
	m.mu.Lock()
	if m.jerr != nil { // never overwrite a file that could not be read
		m.mu.Unlock()
		return nil
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
	m.mu.Lock()
	was := m.werr
	m.werr, m.werrAt = err, m.now()
	if err != nil && m.active != nil && m.active.journalErr == "" {
		m.active.journalErr = journalErrText(err)
	}
	m.mu.Unlock()
	switch {
	case err != nil && was == nil:
		slog.Error("wake-update: cannot write the run journal; a run that is going stops, and no run starts until it can be written", "path", m.path, "error", err)
	case err == nil && was != nil:
		slog.Info("wake-update: the run journal can be written again", "path", m.path)
	}
	return err
}

// journalErrText is a write error of the journal for a sentence: the system's own reason, without the path
// (the report names the file).
func journalErrText(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Op + ": " + pe.Err.Error()
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return le.Op + ": " + le.Err.Error()
	}
	return "the write failed"
}

// ask enters, in the journal, that a request (what: "wake", "update", "shutdown") is about to go out to the
// machine, and reports whether the run may send it. The entry is written before the request leaves, with
// everything else the run knows by then. If it cannot be written, the request is not sent, and the run stops
// at its next step. The caller holds m.mu and calls done after the request came back.
func (m *Manager) ask(r *Run, h *Host, what string) bool {
	if m.path == "" {
		return true
	}
	if r.journalErr != "" {
		return false
	}
	h.Asking = what
	var err error
	m.io(func() { err = m.save() })
	if err != nil {
		h.Asking = ""
		return false
	}
	return true
}

// done ends what ask began: the request came back, and what came of it is in the machine's record.
func (m *Manager) done(h *Host) {
	if h.Asking != "" {
		h.Asking = ""
		m.dirty = true
	}
}
