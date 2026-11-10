package testutil

import (
	"embed"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
)

//go:embed all:testdata/v-before
var vBeforeFS embed.FS

//go:embed all:testdata/v-before-managed
var vBeforeManagedFS embed.FS

// AddVBeforeManaged adds the "v-before-managed" fixture to a data directory: managed-hosts.yaml and secrets/ as
// the release before the security fixes wrote them for two hosts added with Add host (NSX-FURINA, a Windows
// PC, and nsx-kachina, a standalone Proxmox node). See testdata/v-before-managed/README.md.
func AddVBeforeManaged(t *testing.T, dataDir string) {
	t.Helper()
	const root = "testdata/v-before-managed"
	err := fs.WalkDir(vBeforeManagedFS, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, filepath.FromSlash(p))
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dataDir, rel), 0o700)
		}
		if rel != "managed-hosts.yaml" && filepath.Dir(rel) != "secrets" {
			return nil // the fixture's own notes
		}
		b, err := vBeforeManagedFS.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dataDir, rel), b, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// VBefore is a private copy of the "v-before" fixture: a panel's config.yaml and data directory exactly as the
// release before the security fixes (commit 05b1019) wrote them. See testdata/v-before/README.md.
type VBefore struct {
	ConfigPath string // config.yaml; its data_dir and audit_log point into this copy, nothing else is changed
	DataDir    string
	Secrets    config.Secrets

	GeneratedAt  time.Time // when the old release wrote the files (its sessions were created then)
	CookieName   string    // the old release's cookie name
	OwnerSub     string    // the admin
	OwnerCookie  string    // "name=value", as the old release set it
	ViewerSub    string
	ViewerCookie string
	TokenSystem  string // a read token owned by "system"
	TokenViewer  string // a read token owned by the viewer, for the site Straw
}

// NewVBefore copies the fixture into a temporary directory (files 0600, as the panel writes them) and sets the
// environment variables its config.yaml names.
func NewVBefore(t *testing.T) *VBefore {
	t.Helper()
	t.Setenv("TOK_LAPTOP", ReadToken)
	t.Setenv("TOK_CYRENE", ReadToken+"x")
	t.Setenv("CTL_CYRENE", ControlToken)
	t.Setenv("PVE_SECRET", "pve-secret-uuid")
	dir := t.TempDir()
	v := &VBefore{ConfigPath: filepath.Join(dir, "config.yaml"), DataDir: filepath.Join(dir, "data")}
	const root = "testdata/v-before"
	err := fs.WalkDir(vBeforeFS, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// The data directory is kept as var-lib-fairy/<name>.old: the repository's .gitignore (rightly) ignores
		// data/, *.log, tokens.json, sessions.json and state.json.
		rel, _ := filepath.Rel(root, filepath.FromSlash(p))
		rel = strings.TrimSuffix(strings.Replace(rel, "var-lib-fairy", "data", 1), ".old")
		dst := filepath.Join(dir, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o700)
		}
		b, err := vBeforeFS.ReadFile(p)
		if err != nil {
			return err
		}
		if rel == "config.yaml" {
			if strings.Count(string(b), "/var/lib/fairy") != 2 {
				t.Fatal("v-before config.yaml: expected data_dir and audit_log under /var/lib/fairy")
			}
			b = []byte(strings.ReplaceAll(string(b), "/var/lib/fairy", v.DataDir))
		}
		return os.WriteFile(dst, b, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"users.json", "sessions.json", "state.json", "settings.json", "notifications.json", "tokens.json",
		"ups-events.jsonl", "audit.log"} {
		if st, err := os.Stat(filepath.Join(v.DataDir, name)); err != nil || st.Size() == 0 {
			t.Fatalf("v-before fixture: %s is missing (%v)", name, err)
		}
	}
	var meta struct {
		GeneratedAt   int64  `json:"generated_at_unix"`
		SessionSecret string `json:"session_secret"`
		CookieName    string `json:"cookie_name"`
		OwnerSub      string `json:"owner_sub"`
		OwnerCookie   string `json:"owner_cookie"`
		ViewerSub     string `json:"viewer_sub"`
		ViewerCookie  string `json:"viewer_cookie"`
		TokenSystem   string `json:"token_system_read"`
		TokenViewer   string `json:"token_viewer_read"`
	}
	b, err := vBeforeFS.ReadFile(root + "/generated.json")
	if err == nil {
		err = json.Unmarshal(b, &meta)
	}
	if err != nil || meta.OwnerSub == "" || meta.SessionSecret == "" {
		t.Fatal("v-before generated.json", err)
	}
	v.Secrets = config.Secrets{SessionSecret: meta.SessionSecret}
	v.GeneratedAt = time.Unix(meta.GeneratedAt, 0)
	v.CookieName, v.OwnerSub, v.OwnerCookie = meta.CookieName, meta.OwnerSub, meta.OwnerCookie
	v.ViewerSub, v.ViewerCookie, v.TokenSystem, v.TokenViewer = meta.ViewerSub, meta.ViewerCookie, meta.TokenSystem, meta.TokenViewer
	return v
}

// Load reads the copy's config.yaml the way `fairy serve` and `fairy admin` do.
func (v *VBefore) Load(t *testing.T) *config.Panel {
	t.Helper()
	cfg, err := config.Load(v.ConfigPath)
	if err != nil {
		t.Fatal("the old config.yaml no longer loads: ", err)
	}
	return cfg
}

// OldExampleConfig is deploy/config.example.yaml as the old release shipped it (many installs started from it).
func OldExampleConfig(t *testing.T) []byte {
	t.Helper()
	b, err := vBeforeFS.ReadFile("testdata/v-before/config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ReadData returns a file of the copy's data directory.
func (v *VBefore) ReadData(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(v.DataDir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
