package approval

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	agentapproval "github.com/n3ptune-cpu/Fairy-agent/pkg/approval"
)

// The keys and users of the protocol's trust-hash vectors (part1-passkey-protocol.md, 6.2).
func vectorN(f func(i int) byte) string {
	b := make([]byte, 256)
	for i := range b {
		b[i] = f(i)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

var (
	vKey1  = api.TrustKey{Kty: "RSA", Kid: "abc123", Alg: "RS256", E: "AQAB", N: vectorN(func(i int) byte { return byte(128 + 3*i) })}
	vKey2  = api.TrustKey{Kty: "RSA", Kid: "next-key", Alg: "RS256", E: "AQAB", N: vectorN(func(i int) byte { return byte(255 - 2*i) })}
	vUser1 = User{Sub: "11111111-2222-4333-8444-555555555555"}
	vUser2 = User{Sub: "user-b"}
)

const (
	vIssuer = "https://id.example.org"
	vClient = "3f6c0c0e-0000-4000-8000-000000000000"
)

// The proposal the panel builds hashes to the protocol's vectors, and its request and nonce are the ones a
// machine computes: built here through Proposal and TrustBound, as "Turn on" builds them.
func TestTrustHashVectors(t *testing.T) {
	if !strings.HasPrefix(vKey1.N, "gIOGiYyPkpWYm56h") || !strings.HasSuffix(vKey1.N, "5xdHd6fQ") || len(vKey1.N) != 342 ||
		!strings.HasPrefix(vKey2.N, "__37-ff18_Hv7evp") || !strings.HasSuffix(vKey2.N, "sJBwUDAQ") {
		t.Fatal("the vector keys are not the protocol's")
	}
	withUse := vKey1
	withUse.Use = "sig"
	for _, c := range []struct {
		name   string
		issuer string
		keys   []api.TrustKey
		users  []User
		want   string
	}{
		{"keys [1], users [1]", vIssuer, []api.TrustKey{vKey1}, []User{vUser1}, "dRtwOuCOmn_fN8xutDqtllZabcZQcFGbT4ShJzPoXpQ"},
		{"keys [1, 2], users [1, 2]", vIssuer, []api.TrustKey{vKey1, vKey2}, []User{vUser1, vUser2}, "e778SBt_90yBRtGMhF_KR8mLGBMsZXr_NypZnETljAE"},
		{"keys [2, 1], users [1, 2]", vIssuer, []api.TrustKey{vKey2, vKey1}, []User{vUser1, vUser2}, "2TNOVELbekzaS5Pl4-s5zUYxWvSs-EHAsTHHJkR6BCE"},
		{"keys [1, 2], users [2, 1]", vIssuer, []api.TrustKey{vKey1, vKey2}, []User{vUser2, vUser1}, "P23yiKxxpRQrQ-oXDqJCfWNms527gd8ZKP3xtuWcMDo"},
		{"issuer with a trailing slash", vIssuer + "/", []api.TrustKey{vKey1}, []User{vUser1}, "qdHOQEj89t-ZvuxetnIT7sxSZgUnKAUoWgGsms9QjIM"},
		{"a key with use: sig", vIssuer, []api.TrustKey{withUse}, []User{vUser1}, "dRtwOuCOmn_fN8xutDqtllZabcZQcFGbT4ShJzPoXpQ"},
	} {
		in := Proposal(c.issuer, vClient, c.keys, c.users)
		if got := api.TrustHash(in); got != c.want {
			t.Errorf("%s: trust hash %s, the protocol says %s", c.name, got, c.want)
		}
	}
	// the request and the nonce of the first row, with challenge A of the nonce vectors
	in := Proposal(vIssuer, vClient, []api.TrustKey{vKey1}, []User{vUser1})
	if _, err := NewPlan([]Bound{TrustBound("pc-a", "PC-A", in, vectorChallenge(0))}); err == nil {
		t.Fatal("a Turn on without a machine id of the panel's own was planned: the panel never leaves the id to the machine")
	}
	in.MachineID = vectorIDA // the id the panel makes up for the machine; it is not part of the hash (6.1)
	if got := api.TrustHash(in); got != "dRtwOuCOmn_fN8xutDqtllZabcZQcFGbT4ShJzPoXpQ" {
		t.Fatal("the machine id changed the trust hash:", got)
	}
	b := TrustBound("pc-a", "PC-A", in, vectorChallenge(0))
	if b.Canonical != "trust_set hash=dRtwOuCOmn_fN8xutDqtllZabcZQcFGbT4ShJzPoXpQ" || len(b.Canonical) != 58 {
		t.Fatalf("the request: %q", b.Canonical)
	}
	plan, err := NewPlan([]Bound{b})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Nonce() != "Nw43E0SfBmQd6YKupw1cRh0KU6h0UIifvU7Vc-fHqxE" { // section 6.2: the set proposes id A
		t.Fatalf("the nonce of the first-time set: %s", plan.Nonce())
	}
	// the approval is handed out for exactly this proposal, not for one that differs in a user
	if _, err := For(plan, "t.o.k", "pc-a", in.MachineID, api.TrustSetRequest(in)); err != nil {
		t.Fatal(err)
	}
	other := Proposal(vIssuer, vClient, []api.TrustKey{vKey1}, []User{vUser1, vUser2})
	if _, err := For(plan, "t.o.k", "pc-a", in.MachineID, api.TrustSetRequest(other)); err == nil {
		t.Fatal("an approval was handed out for another proposal")
	}
	if _, err := For(plan, "t.o.k", "pc-a", vectorIDB, api.TrustSetRequest(in)); err == nil {
		t.Fatal("an approval was handed out for another machine id than the proposal's")
	}
}

func jwk(kid string, pub *rsa.PublicKey, extra map[string]any) map[string]any {
	b64 := base64.RawURLEncoding.EncodeToString
	m := map[string]any{"kty": "RSA", "kid": kid, "n": b64(pub.N.Bytes()), "e": b64(big.NewInt(int64(pub.E)).Bytes())}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

// The keys a machine is given are the issuer's RSA signing keys, reduced to what a machine takes; the result
// passes the machine's own check and has the thumbprint the machine reports.
func TestTrustKeysFromJWKS(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	doc := func(keys ...any) []byte {
		b, _ := json.Marshal(map[string]any{"keys": keys})
		return b
	}
	good := jwk("k1", &key.PublicKey, map[string]any{"use": "sig", "alg": "RS256", "x5c": []string{"ignored"}, "key_ops": []string{"verify"}})
	keys, err := TrustKeys(doc(good,
		map[string]any{"kty": "EC", "kid": "ec1", "crv": "P-256", "x": "AA", "y": "AA"},
		jwk("enc", &key.PublicKey, map[string]any{"use": "enc"}),
		jwk("ps", &key.PublicKey, map[string]any{"alg": "PS256"}),
		jwk("", &key.PublicKey, nil)))
	if err != nil || len(keys) != 1 {
		t.Fatal(keys, err)
	}
	k := keys[0]
	if k.Kty != "RSA" || k.Kid != "k1" || k.Alg != "RS256" || k.Use != "" || k.E != "AQAB" {
		t.Fatalf("%+v", k)
	}
	if got, want := Thumbprints(keys)[0], agentapproval.Thumbprint(agentapproval.KeyFromRSA("k1", &key.PublicKey)); got != want || len(got) != 43 {
		t.Fatalf("thumbprint %s, the machine computes %s", got, want)
	}
	in := Proposal("https://id.example.org", "pc-actions", keys, []User{{Sub: "user-1"}})
	if err := CheckProposal(in); err != nil {
		t.Fatal("a proposal with the issuer's key must pass the machines' rules:", err)
	}
	for name, raw := range map[string][]byte{
		"no usable key":    doc(map[string]any{"kty": "EC", "kid": "ec1"}),
		"not JSON":         []byte("<html>"),
		"five keys":        doc(jwk("a", &key.PublicKey, nil), jwk("b", &key.PublicKey, nil), jwk("c", &key.PublicKey, nil), jwk("d", &key.PublicKey, nil), jwk("e", &key.PublicKey, nil)),
		"a key id twice":   doc(jwk("a", &key.PublicKey, nil), jwk("a", &key.PublicKey, nil)),
		"too large":        append(doc(good), make([]byte, MaxJWKS)...),
		"an empty key set": doc(),
	} {
		if keys, err := TrustKeys(raw); err == nil {
			t.Errorf("%s: accepted (%d keys)", name, len(keys))
		}
	}
	// what a machine would refuse is refused before anybody is asked for a passkey
	for name, bad := range map[string]api.TrustInit{
		"an http issuer":      Proposal("http://id.example.org", "pc-actions", keys, []User{{Sub: "user-1"}}),
		"no users":            Proposal("https://id.example.org", "pc-actions", keys, nil),
		"a user with a space": Proposal("https://id.example.org", "pc-actions", keys, []User{{Sub: "two words"}}),
		"a made-up modulus":   Proposal("https://id.example.org", "pc-actions", []api.TrustKey{{Kty: "RSA", Kid: "x", Alg: "RS256", N: "AQAB", E: "AQAB"}}, []User{{Sub: "user-1"}}),
	} {
		if err := CheckProposal(bad); err == nil {
			t.Errorf("%s: the proposal passed", name)
		}
	}
}

// The list of users has a fixed order (it is part of the hash) and holds each user once.
func TestUsersOrder(t *testing.T) {
	admins := []User{{Sub: "zed", Name: "Zed"}, {Sub: "amy", Name: "Amy"}}
	owner := &User{Sub: "straw", Name: "Straw", Why: "owner of Straw"}
	got := Users(admins, owner, []string{"extra-1", "amy", "straw"})
	var subs, whys []string
	for _, u := range got {
		subs, whys = append(subs, u.Sub), append(whys, u.Why)
	}
	if strings.Join(subs, " ") != "amy zed straw extra-1" || strings.Join(whys, "|") != "admin|admin|owner of Straw|approval.users" {
		t.Fatalf("%v %v", subs, whys)
	}
	if again := Users([]User{{Sub: "amy"}, {Sub: "zed"}}, owner, []string{"extra-1"}); api.TrustHash(Proposal("i", "c", nil, again)) != api.TrustHash(Proposal("i", "c", nil, got)) {
		t.Fatal("the same people in another order of the admins must give the same proposal")
	}
	if len(Users(nil, nil, nil)) != 0 {
		t.Fatal("nobody")
	}
}
