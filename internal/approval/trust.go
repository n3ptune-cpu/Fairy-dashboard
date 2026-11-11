package approval

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	agentapproval "github.com/n3ptune-cpu/Fairy-agent/pkg/approval"
)

// The trust a machine is proposed when the check is switched on for it ("Turn on"): the issuer, the client
// "Fairy PC actions", the issuer's signing key(s) as the panel fetched them, and the users who may approve.
// A machine takes it once, and only together with an approval that verifies against exactly these values
// (its request is "trust_set hash=<api.TrustHash of the proposal>"); afterwards it is changed only on the
// machine. The hash and the request are made with the agent module's own functions.

// User is one user of a proposal, with the reason they are on the list (for the dialog and the audit log).
type User struct {
	Sub  string `json:"sub"`
	Name string `json:"name"` // what the panel calls them ("" when it does not know them)
	Why  string `json:"why"`  // "admin", "owner of <label>", "approval.users"
}

// MaxJWKS is how much of an issuer's key set the panel reads.
const MaxJWKS = 256 << 10

// TrustKeys reads an issuer's JWKS document and returns the keys a machine can pin, in the document's order:
// RSA keys for signing (use absent or "sig") with RS256 (alg absent or "RS256"), reduced to exactly the
// members a machine takes (kty, kid, alg = RS256, n, e). Other keys are left out. It is an error when none is
// left, when there are more than a machine takes, or when a key id is there twice.
func TrustKeys(jwks []byte) ([]api.TrustKey, error) {
	if len(jwks) > MaxJWKS {
		return nil, errors.New("the issuer's key set is too large")
	}
	var doc struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			Alg string `json:"alg"`
			Use string `json:"use"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(jwks, &doc); err != nil {
		return nil, errors.New("the issuer's key set is not JSON")
	}
	var out []api.TrustKey
	seen := map[string]bool{}
	for _, k := range doc.Keys {
		if k.Kty != "RSA" || (k.Use != "" && k.Use != "sig") || (k.Alg != "" && k.Alg != "RS256") || k.Kid == "" {
			continue
		}
		if seen[k.Kid] {
			return nil, fmt.Errorf("the issuer's key set has the key id %q twice", k.Kid)
		}
		seen[k.Kid] = true
		out = append(out, api.TrustKey{Kty: "RSA", Kid: k.Kid, Alg: "RS256", N: k.N, E: k.E})
	}
	switch {
	case len(out) == 0:
		return nil, errors.New("the issuer publishes no RSA signing key for RS256 (the machines check RS256 only; Pocket ID's default)")
	case len(out) > api.MaxTrustKeys:
		return nil, fmt.Errorf("the issuer publishes %d RSA signing keys; a machine pins at most %d", len(out), api.MaxTrustKeys)
	}
	return out, nil
}

// Thumbprints are the RFC 7638 thumbprints of keys, as a machine reports the keys it has pinned.
func Thumbprints(keys []api.TrustKey) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = agentapproval.Thumbprint(k)
	}
	return out
}

// Users puts the list of a proposal together, in a fixed order (the order is part of the hash): the panel's
// admins sorted by id, then the host's owner, then the extra users of the configuration; each id once.
func Users(admins []User, owner *User, extra []string) []User {
	admins = slices.Clone(admins)
	slices.SortFunc(admins, func(a, b User) int {
		switch {
		case a.Sub < b.Sub:
			return -1
		case a.Sub > b.Sub:
			return 1
		}
		return 0
	})
	var out []User
	seen := map[string]bool{}
	add := func(u User) {
		if u.Sub != "" && !seen[u.Sub] {
			seen[u.Sub] = true
			out = append(out, u)
		}
	}
	for _, u := range admins {
		u.Why = "admin"
		add(u)
	}
	if owner != nil {
		add(*owner)
	}
	for _, sub := range extra {
		add(User{Sub: sub, Why: "approval.users"})
	}
	return out
}

// Proposal is the trust to propose: the four values in the order they are hashed and sent.
func Proposal(issuer, clientID string, keys []api.TrustKey, users []User) api.TrustInit {
	in := api.TrustInit{Issuer: issuer, ClientID: clientID, Keys: slices.Clone(keys)}
	for _, u := range users {
		in.Users = append(in.Users, u.Sub)
	}
	return in
}

// CheckProposal applies the machines' own rules to a proposal (the agent module's check of a first-time set):
// an https issuer, the client id's characters, 1-4 RSA keys of 2048-8192 bits, 1-32 users. A proposal that
// fails here would be refused by every machine, so nobody is asked for a passkey for it.
func CheckProposal(in api.TrustInit) error {
	if len(in.Users) == 0 {
		return errors.New("nobody would be allowed to approve: the list of users is empty")
	}
	_, err := agentapproval.FromInit(in, time.Unix(0, 0))
	return err
}

// TrustBound is the request a machine must approve for the proposal in, as the plan keeps it: the machine's
// challenge for "trust_set hash=<hash of in>".
func TrustBound(agentID, name string, in api.TrustInit, challenge string) Bound {
	req := api.TrustSetRequest(in)
	canon, _ := req.Canonical()
	return Bound{Target: Target{AgentID: agentID, Name: name, Request: req}, Canonical: canon, Challenge: challenge}
}
