package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/approval"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/notifications"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/powerctl"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/textsafe"
)

// POST /api/power/socket: switch one socket of a plug on or off (PLUGS-DESIGN.md, stage 1a, section 4.4).
//
// One request is one socket and one direction, named in words ("on", "off"): there is no toggle, and no
// request for more than one socket. Every switch, on and off, needs a passkey approval that the machine which
// reads the plug checks itself, for exactly this socket and this direction; an approval is good for one
// switch. So the route is for admins in a browser only (no API token), behind the kill switch, and it never
// sends anything on the strength of the panel's own "Confirm it's you".
//
// The panel holds no Home Assistant token and no address of one, and names no entity: the request to the
// agent carries the plug's id, the socket's id, the direction and the socket's tag, as the agent's own row
// gave them. Whether a socket can be switched is set on the machine, in the agent's config.yaml.
//
// The order (plugSwitch): who asks; the body; the socket as the panel sees it now (it exists, the tag is the
// one the page showed, the agent says it can be switched, nothing in the panel locks it, the machine has the
// passkey check); "still running" for a switch-off; the machine's challenge and the passkey round trip; the
// rate limit; the one request to the agent; the audit entry. After the round trip the panel replays the kept
// request through this same handler, so everything before the approval is looked at again with what is true
// then: the PC may have been started and the socket changed while the person was at Pocket ID.

// plugSwitchWait is how long the panel waits for the agent's answer to a switch: the agent reads the state
// (3 s), sends the request (5 s) and watches the state (10 s), 18 s in all, and the answer needs its way back.
const plugSwitchWait = 25 * time.Second

// plugAudit is the audit log's name for every attempt to switch a socket.
const plugAudit = "plug.switch"

// codeMachineRunning is the code of the refusal "the machine on this socket still runs": the page then offers
// "Shut down" and "Cut power anyway".
const codeMachineRunning = "machine_running"

type plugSwitchReq struct {
	Source string `json:"source"` // the source's id in the panel: "<agent id>/<device id>"
	Socket string `json:"socket"`
	To     string `json:"to"`  // on | off
	Tag    string `json:"tag"` // the socket's tag, as the page showed it
	// Cut and Confirm are "Cut power anyway": switch off although the machine on the socket still runs, with
	// that machine's name typed.
	Cut     bool   `json:"cut"`
	Confirm string `json:"confirm"`
}

func (b plugSwitchReq) check() error {
	switch {
	case b.Source == "" || len(b.Source) > 200:
		return apierr.New(422, "source must be the id of a power source")
	case api.CheckSocketID(b.Socket) != nil:
		return apierr.New(422, "socket must be the id of a socket")
	case b.To != api.SocketOn && b.To != api.SocketOff:
		return apierr.New(422, "to must be on or off")
	case api.CheckSocketTag(b.Tag) != nil:
		return apierr.New(422, "tag must be the socket's tag, as GET /api/power gives it")
	case len(b.Confirm) > 200:
		return apierr.New(422, "confirm is too long")
	case b.Cut && b.To != api.SocketOff:
		return apierr.New(422, "cut is for switching off only")
	}
	return nil
}

// PlugSwitchResult is the answer to a switch that was sent.
type PlugSwitchResult struct {
	OK     bool   `json:"ok"`
	Source string `json:"source"`
	Socket string `json:"socket"`
	To     string `json:"to"`
	Was    string `json:"was"`   // the state the machine read before it sent the request
	State  string `json:"state"` // the state it read last, after it
	// Confirmed: the socket showed the new state within 10 s. false: Home Assistant took the request and the
	// socket does not show it yet (it may still follow).
	Confirmed bool   `json:"confirmed"`
	Message   string `json:"message"`
}

// plugError is an attempt that did not end in a switch the machine reported: the sentence the page gets, and
// for the audit log a short word and whether the request had left for Home Assistant by then.
//
//   - refused: nothing was sent to Home Assistant. The panel's own "no", or the machine's.
//   - failed: the machine had used the approval up and sent the request, or the panel got no answer of the
//     machine: the socket may have been switched.
type plugError struct {
	err    *apierr.Error
	word   string
	failed bool
}

func (e *plugError) Error() string { return e.err.Error() }
func (e *plugError) Unwrap() error { return e.err }

// plugErr is a refusal: nothing was sent to Home Assistant.
func plugErr(status int, word, text string) error {
	return &plugError{err: apierr.New(status, text), word: word}
}

// plugFailed is a failure after the request was (or may have been) sent.
func plugFailed(status int, word, text string) error {
	return &plugError{err: apierr.New(status, text), word: word, failed: true}
}

// plugOutcome is the audit log's outcome of an attempt that did not end in "ok": "refused (<word>): <sentence>"
// or "failed (<word>): <sentence>" (plugError), else the usual "error <status>: <detail>".
func plugOutcome(err error) string {
	var pe *plugError
	if errors.As(err, &pe) {
		text, _ := pe.err.Detail.(string)
		if d, ok := pe.err.Detail.(map[string]any); ok {
			text, _ = d["message"].(string)
		}
		kind := "refused"
		if pe.failed {
			kind = "failed"
		}
		return textsafe.Clip(fmt.Sprintf("%s (%s): %s", kind, pe.word, text), 400)
	}
	return errorOutcome(err)
}

// plugFind is the socket the request names, as the panel sees it now. It answers 404 for a source or a socket
// that is not there, the same as for one the caller may not see.
func (c *reqCtx) plugFind(b plugSwitchReq) (socketLook, *plugLooks, error) {
	v, err := c.view()
	if err != nil {
		return socketLook{}, nil, err
	}
	none := apierr.New(404, "there is no such socket")
	for _, st := range c.s.Poller.Power.Overview().Sources {
		if st.ID != b.Source {
			continue
		}
		if !v.all && !v.powerVisible(st) {
			return socketLook{}, nil, none
		}
		looks := c.s.newPlugLooks(v.snap)
		for _, sock := range st.Sockets {
			if sock.ID == b.Socket {
				if k, ok := looks.look(st, sock); ok {
					return k, looks, nil
				}
			}
		}
	}
	return socketLook{}, nil, none
}

// plugWords says one switch in words: the direction, the socket by the name the machine gives it, the plug,
// and what hangs on the socket. They are the dialog's line "Your passkey signs" and the text of the kept
// action.
func plugWords(k socketLook, to string, cut bool) string {
	w := fmt.Sprintf("switch %s %s (%s)", to, k.sock.Name, k.src.Name)
	name, running := k.running()
	switch {
	case name == "":
	case cut && running:
		w += ": cut the power of " + name + " while it is running"
	default:
		w += ", which powers " + name
	}
	return w
}

// plugDetail is what the description of a kept switch says about it (pending.go, actionDetail).
func (c *reqCtx) plugDetail(_ *model.User) string {
	var b plugSwitchReq
	if !decodeStrictly(c.body, &b) || b.check() != nil {
		return ""
	}
	k, _, err := c.plugFind(b)
	if err != nil {
		return ""
	}
	return plugWords(k, b.To, b.Cut)
}

func (s *Server) plugSwitch(c *reqCtx) error {
	u, err := c.controlAllowed(true)
	if err != nil {
		return err
	}
	var b plugSwitchReq
	if err := c.decodeBody(&b); err != nil {
		return err
	}
	if err := b.check(); err != nil {
		return err
	}
	// From here on every attempt is in the audit log, refused ones too; only "confirm first" is not an outcome.
	// What the panel refuses itself, before it asks the machine anything, can be sent without limit: those
	// entries share the per-subject budget of refused requests (auditRefusal). An attempt that got as far as
	// the machine is always on record (fail).
	target := b.Source + "/" + b.Socket
	params := map[string]any{"to": b.To}
	fail := func(err error) error {
		c.audit(u, plugAudit, target, params, plugOutcome(err))
		return err
	}
	refuse := func(err error) error {
		c.auditRefusal(u, plugAudit, target, params, plugOutcome(err))
		return err
	}
	k, looks, err := c.plugFind(b)
	if err != nil {
		return refuse(err)
	}
	machine := s.machineNames([]string{k.agent.ID})[0] // the machine that reads the plug
	params["name"], params["plug"], params["agent"], params["was"] = k.sock.Name, k.src.Name, k.agent.ID, k.sock.State
	if k.link != "" {
		params["linked"] = k.shown
	}
	// The socket as the panel sees it now: the tag the page showed is still the socket's, the agent's own
	// config lets it be switched, nothing in the panel locks it, and the machine has the passkey check.
	if b.Tag != k.sock.Tag || k.sock.Tag == "" {
		return refuse(plugErr(409, api.CodeSocketChanged, "This socket was changed on the machine. Nothing was switched. Look again."))
	}
	if why := s.socketWhy(k); why != "" {
		return refuse(plugErr(409, why, socketWhyText(why, machine)+" Nothing was switched."))
	}
	if k.sock.State != api.SocketStateOn && k.sock.State != api.SocketStateOff {
		return refuse(plugErr(409, api.CodeSocketUnavailable, "Home Assistant has no state for this socket right now. Nothing was switched."))
	}
	// "Still running": a socket whose machine runs is not switched off, unless the request says "cut power
	// anyway" with that machine's name typed. "On" has no such rule.
	name, running := k.running()
	cut := false
	switch {
	case b.Cut && name == "":
		return refuse(plugErr(422, "cut_unlinked", "Nothing the panel knows of hangs on this socket, so there is no machine whose power would be cut. Nothing was switched."))
	case b.To == api.SocketOff && running && !b.Cut:
		detail := map[string]any{"code": codeMachineRunning, "message": looks.runningWords(k), "machine": name, "state": k.runningState()}
		if k.row != nil {
			detail["system_id"] = k.row.ID
		}
		return refuse(&plugError{err: apierr.New(409, detail), word: codeMachineRunning})
	case b.To == api.SocketOff && running:
		if !strings.EqualFold(strings.TrimSpace(b.Confirm), strings.TrimSpace(name)) {
			return refuse(plugErr(422, "cut_name", fmt.Sprintf("Type %s to confirm cutting its power. Nothing was switched.", name)))
		}
		cut = true
		params["cut"] = true
	}
	// The machine's challenge for exactly this request, then the passkey. No other way leads on from here.
	req := api.SocketSetRequest(k.dev, k.sock.ID, b.To, k.sock.Tag)
	ap, err := c.confirmOne(u, "control actions", &approvalTarget{agent: k.agent, name: machine, req: req, words: plugWords(k, b.To, cut)})
	if err != nil {
		if isStepUp(err) {
			return err
		}
		if c.approved() != nil {
			return fail(err) // after an approval: always on record
		}
		return refuse(err) // the machine's "no" to the question, or the limit on questions: nothing was sent
	}
	if ap == nil {
		// confirmOne lets a request through without an approval where a machine checks nothing. A socket is
		// never switched that way.
		return fail(plugErr(409, "no_approval", "No passkey approval was given for this switch, and a socket is switched only with one. Nothing was sent."))
	}
	if !s.control.allow(u.Sub) {
		params["approval"] = "not sent"
		c.audit(u, plugAudit, target, params, "refused (rate_limit): too many control requests")
		return c.limited(u, s.control, "too many control requests")
	}
	params["approval"] = "sent" // the machine checks it; the token itself is never written anywhere
	path := api.PathPlugSwitch
	status, data, err := s.Poller.AgentRequestWithin(c.r.Context(), k.agent, "POST", path,
		api.PlugSwitch{Source: k.dev, Socket: k.sock.ID, To: b.To, Tag: k.sock.Tag, RequestedBy: actorOf(u), Approval: ap}, plugSwitchWait)
	s.Poller.Scheduler.Kick("agent:" + k.agent.ID) // the socket as it is now, not at the next poll
	if e := unanswered(path, status, data, err); e != nil {
		var lost *powerctl.Unanswered
		if errors.As(e, &lost) {
			// no answer of the agent is never its "no": the request may have arrived
			return fail(plugFailed(502, "no_answer_agent", fmt.Sprintf("No answer from %s. It may have been switched: look at the socket.", machine)))
		}
		return fail(plugErr(502, "not_sent", fmt.Sprintf("%s could not be reached. Nothing was sent.", machine)))
	}
	if status != 200 {
		return fail(s.plugAnswerRefusal(machine, k, status, data))
	}
	var got api.PlugSwitched
	raw, _ := json.Marshal(data)
	if json.Unmarshal(raw, &got) != nil || got.Source != k.dev || got.Socket != k.sock.ID || got.To != b.To {
		// a good status that is not the answer to this request: something else answered at the agent's address
		return fail(plugFailed(502, "other_answer", fmt.Sprintf("%s answered for another request than the one sent. It may have been switched: look at the socket.", machine)))
	}
	res := PlugSwitchResult{OK: true, Source: b.Source, Socket: b.Socket, To: b.To, Was: socketState(got.Was), State: socketState(got.State)}
	res.Confirmed = got.Confirmed && res.State == b.To // only a state read afterwards says that it switched
	params["was"], params["confirmed"] = res.Was, res.Confirmed
	outcome := "ok"
	if res.Confirmed {
		res.Message = fmt.Sprintf("%s is %s.", k.sock.Name, b.To)
	} else {
		outcome = "ok (not confirmed)"
		res.Message = fmt.Sprintf("Sent. %s does not show %s yet.", k.sock.Name, b.To)
	}
	c.audit(u, plugAudit, target, params, outcome)
	e := notifications.Event{Type: "power", Severity: notifications.Info, Kind: "plug_switched", Host: machine,
		Text: fmt.Sprintf("%s (%s) switched %s by %s", k.sock.Name, k.src.Name, b.To, actorOf(u))}
	if !res.Confirmed {
		e.Text += " (sent; the socket did not show it yet)"
	}
	if cut {
		e.Text += ": power cut while " + name + " was running"
	}
	if k.row != nil {
		e.SystemID = k.row.ID
	}
	s.Notes.Add(e)
	c.json(200, res)
	return nil
}

// socketState is a state word of an agent's answer as the panel repeats it: one of the four, else "unknown".
func socketState(s string) string {
	switch s {
	case api.SocketStateOn, api.SocketStateOff, api.SocketStateUnavailable:
		return s
	}
	return api.SocketStateUnknown
}

// socketWhyText is the sentence for a socket that cannot be switched, by its reason (model.PowerSocket.Why);
// machine is the machine that reads the plug. The page has the same sentences for the lock's hover text.
func socketWhyText(why, machine string) string {
	switch why {
	case model.SocketLockAgent:
		return "Locked. This socket powers the machine that reads the plugs."
	case model.SocketLockHomeAssistant:
		return "Locked. This socket powers Home Assistant."
	case model.SocketLockHAUnknown:
		return "Locked. The panel cannot tell which machine Home Assistant runs on (home_assistant_system names no row)."
	case model.SocketLockPanel:
		return "Locked. This socket powers the panel."
	case api.WhyFeedsSelf:
		return "Locked. On " + machine + " this socket is marked as one that is never switched."
	case api.WhyNotAllowed:
		return "Not switchable. It is not in the allowed list on the machine that reads this plug."
	case api.WhySwitchingOff:
		return "Switching is turned off on " + machine + "."
	case api.WhyNoToken:
		return machine + " has no Home Assistant token to switch with."
	case api.WhyApprovalOff:
		return machine + " has no passkey check. Plugs are switched only with one."
	case api.WhyApprovalUnavailable:
		return machine + " cannot check passkey approvals right now."
	case model.SocketAgentUnreachable:
		return machine + " does not answer."
	case model.SocketNoControl:
		return "The panel has no control token for " + machine + "."
	case model.SocketNoTag:
		return machine + " gave this socket no tag, so no request can name it."
	}
	return "This socket cannot be switched."
}

// plugRefusal is a machine's "no" to a switch (or to the question about an approval for one), in the panel's
// words, when the code is one of the plug route's own; nil for any other answer. name is the machine, sock the
// socket's name ("": "this socket").
//
// The sentences say whether something was switched, and whether the passkey approval is used up: two of the
// machine's answers come after it sent the request (ha_refused, no_answer). Nothing of what Home Assistant
// said is in an answer of the agent; the agent's own fixed sentence follows in brackets where it says more.
func plugRefusal(name, sock string, status int, data jsonx.M) error {
	code := jsonx.S(data, "code")
	if sock == "" {
		sock = "this socket"
	}
	says := ""
	if said := textsafe.Clip(jsonx.S(data, "error"), maxAgentError); said != "" {
		says = " (" + name + " says: " + said + ")"
	}
	switch code {
	case api.CodePlugSwitchingDisabled:
		return plugErr(409, code, "Switching is turned off on "+name+". Nothing was switched.")
	case api.CodeUnknownSocket:
		return plugErr(409, code, name+" has no such socket any more. Nothing was switched. Look again.")
	case api.CodeNotAllowed:
		return plugErr(409, code, "Not switchable. It is not in the allowed list on the machine that reads this plug. Nothing was switched.")
	case api.CodeFeedsSelf:
		return plugErr(409, code, "Locked. On "+name+" this socket is marked as one that is never switched. Nothing was switched.")
	case api.CodeSocketChanged:
		return plugErr(409, code, "This socket was changed on the machine. Nothing was switched. Look again.")
	case api.CodeSocketUnavailable:
		return plugErr(409, code, "Home Assistant cannot reach this socket right now. Nothing was switched.")
	case api.CodeSwitchInProgress:
		return plugErr(409, code, name+" is switching another socket right now. Nothing was switched: try again in a moment.")
	case api.CodeNoSwitchToken:
		return plugErr(409, code, name+" has no Home Assistant token to switch with. Nothing was switched."+says)
	case api.CodeApprovalOff:
		return plugErr(409, code, name+" has no passkey check. Plugs are switched only with one. Nothing was switched.")
	case api.CodeTooSoon:
		if n := retryAfter(data); n > 0 {
			return plugErr(429, code, fmt.Sprintf("Wait %d s before you switch %s again.", n, sock))
		}
		return plugErr(429, code, "Wait a moment before you switch "+sock+" again.")
	case api.CodeHAUnreachable:
		if n := retryAfter(data); n > 0 {
			// the agent's wait after Home Assistant refused its token: nothing is asked until it is over
			return plugErr(apierr.Upstream, code, fmt.Sprintf("%s does not ask Home Assistant right now: its token was refused there, and it waits before it asks again "+
				"(%d s left; a restart of its agent ends the wait). Nothing was switched.", name, n)+says)
		}
		return plugErr(apierr.Upstream, code, name+" cannot reach Home Assistant. Nothing was switched."+says)
	case api.CodeHARefused:
		return plugFailed(apierr.Upstream, code, "Home Assistant refused the request, so "+sock+" was most likely not switched. "+name+
			" did not look at it again: look at the socket. The passkey approval is used up."+says)
	case api.CodeNoAnswer:
		return plugFailed(apierr.Upstream, code, "No answer from Home Assistant. It may have been switched: look at the socket. The passkey approval is used up.")
	}
	if status == 404 && code == "" {
		return plugErr(409, "agent_too_old", "The agent on "+name+" is too old to switch plugs. Update it.")
	}
	return nil
}

// plugAnswerRefusal is the agent's "no" to the switch request itself: one of the plug route's own codes, one
// of the approval protocol's, the control token, its request budget, or its own words.
func (s *Server) plugAnswerRefusal(machine string, k socketLook, status int, data jsonx.M) error {
	if e := plugRefusal(machine, k.sock.Name, status, data); e != nil {
		return e
	}
	code, said := jsonx.S(data, "code"), textsafe.Clip(jsonx.S(data, "error"), maxAgentError)
	if msg, ok := approval.ErrorText(machine, code, said, s.unavailableWhy(k.agent.ID)); ok {
		st := 409
		if code == api.CodeApprovalRefused || code == api.CodeApprovalRequired {
			st = 403
		}
		return plugErr(st, code, msg)
	}
	switch {
	case status == 401:
		return plugErr(apierr.Upstream, "control_token", "agent rejected the control token (check control_token_env). Nothing was switched.")
	case status == 429:
		return plugErr(429, "agent_busy", approval.Busy(machine, code, retryAfter(data), false))
	}
	if said == "" {
		said = fmt.Sprintf("agent error %d", status)
	}
	if code == "" {
		code = fmt.Sprintf("http_%d", status)
	}
	return plugErr(apierr.Upstream, code, machine+" did not switch the socket: "+said)
}
