package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The update flow itself (check, question, download, verify, swap, rollback) is release.CLI, tested in
// Fairy-agent's pkg/release; here only the panel's own parts.

func TestHealthAddr(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, "fairy.env")
	if got := healthAddr(nil, env); got != "127.0.0.1:8080" {
		t.Fatal(got)
	}
	_ = os.WriteFile(env, []byte("FAIRY_LISTEN=0.0.0.0:9000\n"), 0o600)
	if got := healthAddr(nil, env); got != "127.0.0.1:9000" {
		t.Fatal(got)
	}
	_ = os.WriteFile(env, []byte("FAIRY_LISTEN=\"10.0.0.20:8080\"\n"), 0o600)
	if got := healthAddr(nil, env); got != "10.0.0.20:8080" {
		t.Fatal(got)
	}
}

func TestUpdateUsage(t *testing.T) {
	// a local build has no release key: refuse before touching anything
	if code := update([]string{"--check", "--config", filepath.Join(t.TempDir(), "none.yaml")}); code != 1 {
		t.Fatal(code)
	}
}
