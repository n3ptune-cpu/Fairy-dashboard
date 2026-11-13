package web

import (
	"context"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/approval"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// The approval a machine verifies itself (release item 1.1; internal/approval says what it is).
//
// For the panel it is "Confirm it's you" with another Pocket ID client and a nonce that is not random: the
// refused request is kept as always (pending.go), together with its plan (which machine was asked for which
// challenge, for which exact request). "Continue" in the dialog starts ONE round trip to Pocket ID with the
// client "Fairy PC actions" and the plan's nonce. The answer comes back to its own callback address, is
// verified like a sign-in (the same code: exchangeCode, verifyIDToken) and then checked for this plan
// (approval.Check). Only then the kept request runs, once, with the raw ID token next to it, and hands the
// token to the machines of the plan.
//
// What it is not: a sign-in. No session starts or changes, and the confirmation opens no window: it counts
// for the one kept request and nothing else. The panel's 3-minute window does not count for a request a
// machine wants an approval for.
//
// The raw token exists in three places only: the answer of Pocket ID's token endpoint, the context of the one
// replayed request (approvalGrant), and the body of the requests to the machines of the plan. It is never
// logged, never written to a file, never put into an answer to the browser and never kept after the replay.

// The panel knows the approval client's ID (approval.client_id), never its name: the name is Pocket ID's, and
// an id of another client (the sign-in client's, put there by mistake) has another name. So no answer and no
// text of the panel names the client; they show the Client ID, and the page says what it must be.

// approvalPlan is what a kept action carries when it needs an approval: the plan, and the PKCE verifier of the
// round trip that will ask for it. It lives in memory with the kept action and is gone with it.
type approvalPlan struct {
	plan     *approval.Plan
	verifier string
	// trust is set for "Turn on" only: what each machine of the plan is proposed, by agent id. The hash in a
	// machine's entry is the hash of exactly this value.
	trust map[string]api.TrustInit
	// items, when set, are the dialog's lines for this plan (what each machine would trust); message is the
	// dialog's own sentence instead of the usual one; skipped are the chosen machines that are not part of the
	// plan, with the reason (Turn on: a machine that did not answer the question).
	items   []string
	message string
	skipped map[string]turnOnResult
	// refused are the machines of a batch that could not be asked for an approval, with the reason the dialog
	// showed (by agent id): the replay records that reason for them, not "this is not what was approved".
	refused map[string]error
	// others are the machines without the check that the same request also goes to, as the dialog listed them:
	// agent id -> the canonical text of the request. After the approval the request is sent to the machines of
	// the plan and to these, never to a machine the dialog did not show (shown; audit 6, r4-x1-04).
	others map[string]string
}

// newApprovalPlan makes the kept form of plan.
func newApprovalPlan(plan *approval.Plan) *approvalPlan {
	return &approvalPlan{plan: plan, verifier: oauth2.GenerateVerifier(), others: map[string]string{}}
}

// shown reports whether the dialog showed this request to this agent as one the confirmation starts: it is a
// request of the plan, or one to a machine without the check that the dialog listed.
func (a *approvalPlan) shown(agentID string, req api.ApprovalRequest) bool {
	canon, err := req.Canonical()
	if err != nil {
		return false
	}
	if a.others[agentID] == canon {
		return true
	}
	for _, b := range a.plan.Bound() {
		if b.AgentID == agentID && b.Canonical == canon {
			return true
		}
	}
	return false
}

// machines are the names of the plan's machines, each once, in the plan's order.
func (a *approvalPlan) machines() []string {
	var out []string
	seen := map[string]bool{}
	for _, b := range a.plan.Bound() {
		if !seen[b.AgentID] {
			seen[b.AgentID] = true
			out = append(out, b.Name)
		}
	}
	return out
}

// approvalGrant is a verified approval, for the one replay of the request it was kept with.
type approvalGrant struct {
	plan  *approvalPlan
	token string // the raw ID token: only For hands it out, into a request to a machine of the plan
	sub   string // who approved (Pocket ID user id)
}

// String and LogValue: a grant that ends up in a log line or an error text by mistake shows no token.
func (g *approvalGrant) String() string       { return "[approval]" }
func (g *approvalGrant) LogValue() slog.Value { return slog.StringValue("[approval]") }

// For is the approval to send with req to the agent, whose machine id on record is machineID now: refused
// unless the plan covers exactly that request, made for exactly that id.
func (g *approvalGrant) For(agentID, machineID string, req api.ApprovalRequest) (*api.Approval, error) {
	return approval.For(g.plan.plan, g.token, agentID, machineID, req)
}

// approved returns the approval this request runs with: only the panel's own replay of a kept request, right
// after the callback verified the approval that was asked for it, has one.
func (c *reqCtx) approved() *approvalGrant {
	g, _ := c.r.Context().Value(approvalKey).(*approvalGrant)
	return g
}

// SetIDPClient sets the HTTP client the panel talks to Pocket ID with, for the sign-in and for approvals
// (default: a plain client with a 10 s timeout). For a panel whose Pocket ID has a certificate of a private CA.
func (s *Server) SetIDPClient(hc *http.Client) {
	s.oidc.http, s.approval.http = hc, hc
}

// askApproval refuses this request because a machine it goes to wants an approval, keeps it with its plan and
// answers like "Confirm it's you" does (401 step_up_required), with what the dialog needs to say that the
// machine itself will check: "approval": {"client", "machines"}. A request that cannot be kept (an API token,
// a replay, another client than the session's) is refused with the reason.
func (c *reqCtx) askApproval(u *model.User, plan *approvalPlan) error {
	if u.Auth == "token" {
		return c.deny(u, 403, "this machine checks a passkey approval itself before it does this: an API token cannot give one. "+
			"Use the panel in a browser.", "API token on an action a machine wants an approval for")
	}
	if !c.s.Cfg.ApprovalOn() {
		return apierr.New(409, approvalNotConfigured)
	}
	if c.resumed() {
		// the replay of a request that was confirmed another way (the machine did not ask when it was kept)
		return apierr.New(409, "the machine asks for a passkey approval now (it did not when you pressed the button). Nothing was done: press the button again.")
	}
	if c.bodyKept {
		if err := c.strictStepUpBody(c.body); err != nil {
			return err
		}
	}
	age := math.Inf(1)
	if a := c.authAge(u); a != nil {
		age = *a
	}
	names := plan.machines()
	c.audit(u, stepUpAction, c.r.URL.Path, map[string]any{"approval": names}, stepUpAsked)
	kept, why := c.keepPending(u, plan)
	msg := "Confirm it's you: " + andNames(names) + " checks this approval itself, so it needs its own passkey, every time."
	if len(names) > 1 {
		msg = "Confirm it's you: " + andNames(names) + " each check this approval themselves, so it needs its own passkey, every time."
	}
	if plan.message != "" {
		msg = plan.message
	}
	detail := map[string]any{"code": "step_up_required", "window_s": c.s.Cfg.StepUpWindowS, "pending": kept != nil, "per_action": true,
		"message": msg, "approval": map[string]any{"client_id": c.s.Cfg.Approval.ClientID, "machines": names}}
	if b := plan.plan.Bound(); len(b) == 1 && len(plan.items) == 0 && b[0].Request.Verb != api.ApproveTrustSet {
		// A single action (audit 5, round 3, r3-x1-03): what the passkey signs, with its numbers, in one line.
		// The words are made from the request the machine was asked about and bound its challenge to, the
		// same ones a batch's lines are made from. (A batch and a Turn on have their own lines: items.)
		detail["signs"] = requestWords(b[0].Request)
	}
	if !math.IsInf(age, 1) {
		detail["auth_age_s"] = math.Round(age)
	}
	if kept != nil {
		detail["pending_id"], detail["action"] = kept.ID, kept.Action
		if len(kept.Items) > 0 {
			detail["items"] = kept.Items
		}
	} else if why != "" {
		detail["not_kept"] = why
	}
	return apierr.New(401, detail)
}

// approvalNotConfigured is the answer when a machine wants an approval and the panel has no client to ask
// Pocket ID with.
const approvalNotConfigured = "this machine checks a passkey approval itself before it does this, and this panel has no Pocket ID client " +
	"for that (approval: in config.yaml, with the Client ID of a separate client for PC actions). Nothing was done."

// andNames is a list of names in a sentence ("A", "A and B", "A, B and C"); more than four are counted.
func andNames(names []string) string {
	switch n := len(names); {
	case n == 0:
		return "the machine"
	case n == 1:
		return names[0]
	case n > 4:
		return names[0] + ", " + names[1] + " and " + strconv.Itoa(n-2) + " more machines"
	default:
		out := names[0]
		for i := 1; i < n-1; i++ {
			out += ", " + names[i]
		}
		return out + " and " + names[n-1]
	}
}

// startApproval is "Continue" in the dialog for a kept action that needs an approval (confirmStepUp armed it
// for the round trip `round`): the address of the one request to Pocket ID, with the client "Fairy PC
// actions", PKCE, the plan's nonce, and a fresh authentication asked for. The round trip's state is the kept
// action's round, so the answer finds the action it belongs to and no other.
func (s *Server) startApproval(c *reqCtx, u *model.User, kept *pendingAction, target, round string) error {
	key := c.pendingKey()
	p, err := s.approval.provider(c.r.Context())
	if err != nil {
		slog.Error("approval: Pocket ID discovery failed", "issuer", s.Cfg.ApprovalIssuer(), "error", safeErr(err))
		s.codec.live.dropPending(key)
		return apierr.New(502, "identity provider unreachable")
	}
	s.codec.live.setPendingNext(key, round, target)
	opts := []oauth2.AuthCodeOption{oauth2.S256ChallengeOption(kept.Approval.verifier), oidc.Nonce(kept.Approval.plan.Nonce()),
		oauth2.SetAuthURLParam("max_age", "0")}
	if s.Cfg.StepUpPromptLogin {
		opts = append(opts, oauth2.SetAuthURLParam("prompt", "login"))
	}
	loc := s.approval.oauth(p).AuthCodeURL(round, opts...)
	c.auditAnon("auth.approval", "redirected to Pocket ID", actorOf(u), &u.Sub,
		map[string]any{"for": kept.Action, "machines": kept.Approval.machines()})
	c.json(200, map[string]any{"url": loc, "action": kept.Action})
	return nil
}

// approvalCallback finishes the round trip startApproval began (GET /auth/approval/callback): it takes the
// kept action of this round trip out of the store (so it can run at most once, whatever happens next),
// exchanges the code as the client "Fairy PC actions", verifies the ID token against the issuer's keys and
// checks that it is the approval of this plan, by the signed-in person, fresh, with a passkey. Then the kept
// request runs with the token. Every refusal is audited with its reason and answered with a page that says
// what happened; nothing runs.
func (s *Server) approvalCallback(c *reqCtx) error {
	if c.r.Header.Get("Authorization") != "" {
		return apierr.New(403, "API tokens are not accepted on /auth")
	}
	sess := c.session()
	q := c.r.URL.Query()
	var kept *pendingAction
	var foreign bool
	// round: this callback's record for a repeat of it (approvalrepeat.go). Only the callback that took the
	// kept action has one; it is forgotten again when the approval is not accepted.
	var round *stepRun
	if sess.SID != "" {
		kept, foreign = s.codec.live.takePending(sess.SID, q.Get("state"), c.client(), s.now())
		if kept != nil && kept.Approval != nil {
			round = s.codec.live.beginApproval(sess.SID, q.Get("state"), q.Get("code"), c.client(), s.now())
			defer s.codec.live.endApproval(round)
		}
	}
	if !s.login.allow(c.loginKey()) {
		// The limit on sign-in attempts counts this way back from Pocket ID too. A browser comes here: it gets
		// the page the sign-in's own callback gives over the same limit, with the time to wait and Retry-After
		// (audit 6, r4-p1-04; this was the last line of JSON, "try again in a minute", without the header).
		c.auditAnon("auth.login", "denied: rate limited", "", nil, nil)
		return callbackPage{c: c, title: "The approval was not used", back: "/"}.limited(s.login, c.loginKey())
	}
	u := c.sessionUser()
	if u == nil {
		c.auditAnon("auth.approval", "failed: no session", "", nil, nil)
		return c.errorPage(401, "The approval was not used", "You are not signed in to Fairy any more (the session ended while you were at "+
			"Pocket ID). Nothing was done. Sign in and press the button again.", "/")
	}
	target := "/"
	fail := func(reason, text string, attrs ...any) error {
		slog.Warn("approval callback failed", append([]any{"reason", reason}, attrs...)...)
		params := map[string]any{}
		if kept != nil {
			params["for"] = kept.Action
		}
		c.auditAnon("auth.approval", "failed: "+reason, actorOf(u), &u.Sub, params)
		return c.errorPage(400, "The approval was not accepted", text+" Nothing was done.", target)
	}
	switch {
	case foreign:
		kept = nil
		return fail("another client", "The answer from Pocket ID came back from another browser or address than the one that asked for it.")
	case kept == nil || kept.Approval == nil:
		kept = nil
		// The repeat of a callback whose approval was accepted (a reload, the Back button): what came of the
		// action, not "nothing was done" for an action that was carried out (approvalrepeat.go).
		if run := s.codec.live.approvalRound(sess.SID, q.Get("state"), q.Get("code"), c.client()); run != nil && s.repeatedApproval(c, u, run) {
			return nil
		}
		// the two causes people meet come first (audit 5, r1-b2-08): one action waits per session, and a
		// restart of the panel forgets it
		return fail("nothing waiting", "No action is waiting for this approval any more. Another tab of yours started an action meanwhile "+
			"(only one waits at a time), or the panel was restarted; or the action was cancelled, is more than 3 minutes old, or this "+
			"answer was used before.")
	}
	target = stepUpNext(safeNext(kept.Next))
	if e := q.Get("error"); e != "" {
		attrs := []any{}
		if d := q.Get("error_description"); d != "" {
			attrs = append(attrs, "error_description", safeText(d, 200))
		}
		return fail(oauthCode(truncate(e, 60)), "Pocket ID did not give the approval (it answered \""+oauthCode(truncate(e, 60))+"\").", attrs...)
	}
	p, err := s.approval.provider(c.r.Context())
	if err != nil {
		return fail("discovery", "Pocket ID could not be reached.", "error", safeErr(err))
	}
	ctx, cancel := context.WithTimeout(s.approval.ctx(c.r.Context()), 15*time.Second)
	defer cancel()
	rawID, _, redact, lf := s.approval.exchangeCode(ctx, p, q.Get("code"), kept.Approval.verifier)
	if lf != nil {
		return fail(lf.why(), "Pocket ID did not hand out the approval (the exchange of its answer failed; the panel's log says why).", lf.attrs...)
	}
	if f := approval.CheckSize(rawID); f != nil {
		return fail(f.Reason, f.Text)
	}
	idt, lf := s.approval.verifyIDToken(ctx, p, rawID, redact)
	if lf != nil {
		return fail(lf.why(), "Pocket ID's answer could not be verified (signature, issuer, client or expiry; the panel's log says which). "+
			"Is approval.client_id the Client ID of the separate client for PC actions in Pocket ID (not the sign-in client's)?", lf.attrs...)
	}
	var extra struct {
		AuthTime float64  `json:"auth_time"`
		AMR      []string `json:"amr"`
	}
	if err := idt.Claims(&extra); err != nil {
		return fail("invalid claims", "Pocket ID's answer could not be read.", "error", safeErr(err, redact...))
	}
	want := approval.Want{Nonce: kept.Approval.plan.Nonce(), Sub: u.Sub, Now: s.now(), AnyPerson: s.approvalAnyPerson}
	if f := approval.Check(approval.Claims{Nonce: idt.Nonce, Sub: idt.Subject, AuthTime: extra.AuthTime, AMR: extra.AMR}, want); f != nil {
		return fail(f.Reason, f.Text)
	}
	if s.approvalAnyPerson {
		slog.Warn("!!! TEST BUILD: an approval was accepted without checking that the approver is the person signed in (nobody is)", "approver", idt.Subject)
	}
	s.codec.live.touch(sess.SID, s.now()) // approving at Pocket ID is real use; it is no sign-in and opens no window
	c.auditAnon("auth.approval", "ok", actorOf(u), &u.Sub, map[string]any{"runs": kept.Action, "machines": kept.Approval.machines(),
		"entries": len(kept.Approval.plan.Entries()), "approver": idt.Subject})
	// what came of the kept action is kept with the round trip, for a repeat of this callback (approvalrepeat.go)
	s.codec.live.approvalAccepted(round, target, s.now())
	s.codec.live.stepOutcome(round, s.resumePendingWith(c, kept, &approvalGrant{plan: kept.Approval, token: rawID, sub: idt.Subject}))
	c.redirect(target)
	return nil
}
