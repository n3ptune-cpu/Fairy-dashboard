package tokens

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestTokensHashedScopedExpiringRevocable(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	raw, r, err := s.Create("ha", "system", "read", "cli:root", 0)
	if err != nil || !strings.HasPrefix(raw, "fy_") || len(raw) < 40 || r.Hint != raw[len(raw)-4:] {
		t.Fatal(raw, err)
	}
	b, _ := os.ReadFile(s.Path)
	if strings.Contains(string(b), raw) || !strings.Contains(string(b), HashToken(raw)) {
		t.Fatal("plaintext stored")
	}
	if st, _ := os.Stat(s.Path); st.Mode().Perm() != 0o600 {
		t.Fatal(st.Mode())
	}
	if v := s.Verify(raw); v == nil || v.Scope != "read" {
		t.Fatal("verify")
	}
	if s.Verify("tp_"+raw[3:]) != nil || s.Verify(raw+"x") != nil || s.Verify(strings.Repeat("fy_", 50)) != nil {
		t.Fatal("wrong tokens accepted")
	}
	if _, _, err := s.Create("x", "system", "admin", "cli", 0); err == nil {
		t.Fatal("bad scope")
	}
	// expired
	raw2, r2, _ := s.Create("old", "system", "control", "cli", 1)
	var d map[string]any
	b, _ = os.ReadFile(s.Path)
	_ = json.Unmarshal(b, &d)
	d["tokens"].(map[string]any)[r2.ID].(map[string]any)["expires_at"] = "2001-01-01T00:00:00Z"
	b, _ = json.Marshal(d)
	_ = os.WriteFile(s.Path, b, 0o600)
	if s.Verify(raw2) != nil {
		t.Fatal("expired token accepted")
	}
	if _, err := s.Revoke("ha"); err != nil || s.Verify(raw) != nil {
		t.Fatal("revoke")
	}
	if _, err := s.Revoke("ha"); err != ErrNotFound {
		t.Fatal("double revoke")
	}
	s.Touch(&r)
	if len(s.List()) != 2 {
		t.Fatal(len(s.List()))
	}
}
