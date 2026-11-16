package web

import (
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/access"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/notify"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

// How the panel names a person (internal/access/people.go decides it; these are the web layer's ways in).
//
//   - actorOf is the text of the audit log alone: the full text an entry had before (the address, marked when
//     it is not verified), next to the subject and, new, the user name.
//   - who is what a record keeps for what a person did (who put a machine id on record, who changed a timing):
//     the user name, or what was kept before while the account has none.
//   - whoShown is what a sentence, an agent and a notification service get: the user name, or the words that
//     stand for an address or a name. Never the address, never the name.
//   - shown and shownIn turn what is stored (also by older versions, with the address) into that where it is
//     handed out. Only the Users page and the audit log hand out an address or a name.
//
// While users.json cannot be read nobody is known: a stored address is then hidden, not shown.

func (s *Server) people() *access.People { return s.Access.Load().People() }

func (s *Server) who(u *model.User) string {
	if u.Auth == "token" {
		return actorOf(u)
	}
	p := s.people()
	if _, known := p.Of(u.Sub); !known {
		return notKnown(u)
	}
	return p.Record(u.Sub, actorOf(u))
}

func (s *Server) whoShown(u *model.User) string {
	p := s.people()
	if u.Auth == "token" {
		return p.Shown(actorOf(u))
	}
	if n, ok := p.Of(u.Sub); ok {
		return n
	}
	return notKnown(u)
}

// notKnown names a signed-in person the panel has no account for: users.json cannot be read, or the account
// was removed while the request was on its way (a session without an account is turned away before it gets
// here). Nothing can be looked up for them, so it is the words that stand for what they would have been named
// by, never that text itself: an address or a name that is written down now would stay readable for good.
func notKnown(u *model.User) string {
	switch {
	case model.Deref(u.Email) != "":
		return access.HiddenAddress
	case model.Deref(u.PreferredUsername) != "" || model.Deref(u.Name) != "":
		return access.HiddenName
	}
	return u.Sub
}

// shown is a stored text that names a person (a "by" of a record), as it is handed out.
func (s *Server) shown(stored string) string { return s.people().Shown(stored) }

// shownIn is a stored sentence that may name a person, as it is handed out.
func (s *Server) shownIn(text string) string { return s.people().ShownIn(text) }

// shownP is shown for a text that may be missing.
func shownP(p *access.People, v *string) *string {
	if v == nil || *v == "" {
		return v
	}
	out := p.Shown(*v)
	return &out
}

// runShown is a run as the Updates page gets it (r is a copy, Manager.Snapshot): who started, cancelled and
// confirmed it by their user names, also in the sentences of a run an older version wrote down, and without
// the text the audit log names the starter by.
func runShown(p *access.People, r *wakerun.Run) *wakerun.Run {
	if r == nil {
		return nil
	}
	r.ByActor = ""
	r.By, r.Tried, r.Trying, r.ConfirmedBy = p.Shown(r.By), p.Shown(r.Tried), p.Shown(r.Trying), p.Shown(r.ConfirmedBy)
	r.Note, r.Summary = p.ShownIn(r.Note), p.ShownIn(r.Summary)
	if r.Cancel != nil {
		r.Cancel.By = p.Shown(r.Cancel.By)
	}
	for _, h := range r.Hosts {
		h.Text, h.UpdateText = p.ShownIn(h.Text), p.ShownIn(h.UpdateText)
		// (a confirmation the machine did not accept is kept as a sentence with who gave it; the two others
		// are what a machine answered, which may repeat what it was told)
		h.ConfirmTried, h.ConfirmText, h.ArmText = p.ShownIn(h.ConfirmTried), p.ShownIn(h.ConfirmText), p.ShownIn(h.ArmText)
		for i := range h.Log {
			h.Log[i].Text = p.ShownIn(h.Log[i].Text)
		}
	}
	return r
}

// auditWho is how the Audit page names the person of an entry, in front of the entry's own text: the user name
// the entry was written with; for an entry of an older version the user name its account has now (by the
// subject, which cannot be mistaken); for an entry without a subject what its actor text is shown as anywhere
// else (a failed sign-in's address is hidden, "root" from the command line stays).
func auditWho(p *access.People, e map[string]any) string {
	if n, _ := e["username"].(string); n != "" {
		return n
	}
	actor, _ := e["actor"].(string)
	if sub, _ := e["sub"].(string); sub != "" && sub != actor {
		if n, ok := p.Of(sub); ok {
			return n
		}
	}
	return p.Shown(actor)
}

// byShown turns every "by" and "total_by" in an answer made from stored changes ("Edit host", "Run timings")
// into the name the panel shows for that person. The stores keep what they were written with: the user name
// since build 17, the address before. v is the answer's own data (maps and lists made for it); nil p: as stored.
func byShown(p *access.People, v any) {
	if p == nil {
		return
	}
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if s, ok := e.(string); ok && (k == "by" || k == "total_by") {
				x[k] = p.Shown(s)
			} else {
				byShown(p, e)
			}
		}
	case []map[string]any:
		for _, e := range x {
			byShown(p, e)
		}
	case []any:
		for _, e := range x {
			byShown(p, e)
		}
	}
}

// accountShown names an account in a sentence of the Users page's answers: by its user name, or as "This
// account" while it has none (never by its address or name; the page has the row and its id).
func (s *Server) accountShown(u access.User) string {
	if n := s.people().Username(u.Sub); n != "" {
		return n
	}
	return "This account"
}

// outside sends a message to the notification service (ntfy or Discord: FAIRY_NOTIFY_URL). It is the one way
// out for both kinds of message, the audited control actions and the warnings of the Notifications list, and
// every e-mail address in the title or the text is replaced on the way (ShownIn): by the user name of the
// account that has it, else by the words that stand for it. The sentences written now name people by their
// user names already; this is for a sentence made from what an older version stored, and for whatever names
// an account by its address in the details of an audited action.
func (s *Server) outside(title, message string) {
	who := s.people()
	send := s.sendOutside
	if send == nil {
		send = notify.Notify
	}
	send(who.ShownIn(title), who.ShownIn(message))
}
