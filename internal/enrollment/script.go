package enrollment

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/release"
)

// The install command never runs "whatever is latest on GitHub". It names one Fairy-agent release (pinned
// here: the agent this panel is built and tested against), and the panel downloads that release's SHA256SUMS,
// SHA256SUMS.sig, VERSION, install.sh and install.ps1 and checks them with the release key built into the panel
// (release/signing.pub, set with -ldflags) before it shows any command. The command then refuses to run a
// script whose SHA-256 differs from the one the panel checked, and points the script at that same release
// (FAIRY_RELEASE_URL / -ReleaseUrl), whose files it checks against SHA256SUMS in turn. The installers take the
// SHA-256 of the SHA256SUMS the panel verified (--sums-sha256 / -SumsSha256, Fairy-agent v0.4.3 on) and
// refuse any other before trusting a line of it, so even a first install rests on the panel's key, not on
// the downloaded binary's own.
//
// The command has one form per system (Command), and that form needs an install script that takes the
// pairing v2 option, takes the join code from standard input (Linux) or the environment (Windows) instead of
// its command line, and, on Windows, can be run from memory. What a signed script can do is read from the
// script itself (Installers.Lacks); a release whose script cannot is refused, never served an older form.

// AgentModule is the Fairy-agent version in go.mod; AgentRelease is its GitHub release (tag build-<run>, VERSION
// "<run>-<short commit>"). Bump both together (a test compares AgentModule with go.mod).
const (
	AgentModule         = "v0.7.0"
	AgentReleaseTag     = "build-16"
	AgentReleaseVersion = "16-a791c55"
)

// Installer file names in a Fairy-agent release.
const (
	InstallSH  = "install.sh"
	InstallPS1 = "install.ps1"
)

// Installers is a checked release: where its files are and the SHA-256 of both install scripts as listed in
// its signed SHA256SUMS.
type Installers struct {
	Repo    string `json:"repo"`
	Tag     string `json:"tag"`
	Version string `json:"version"`
	Base    string `json:"base"` // https://github.com/<repo>/releases/download/<tag>
	SH      string `json:"install_sh_sha256"`
	PS1     string `json:"install_ps1_sha256"`
	// Sums is the SHA-256 of the verified SHA256SUMS (its exact bytes). PinSH / PinPS1: the checked script
	// accepts it (--sums-sha256 / -SumsSha256, Fairy-agent v0.4.3+), so the command passes it.
	Sums   string `json:"sha256sums_sha256"`
	PinSH  bool   `json:"install_sh_takes_sums"`
	PinPS1 bool   `json:"install_ps1_takes_sums"`
	// PairV2SH / PairV2PS1: the checked script takes the pairing v2 option (FlagPairV2SH / FlagPairV2PS1) and
	// hands it to `fairy-agent join --pair-v2`. The panel only pairs v2, so a release without it is refused.
	PairV2SH  bool `json:"install_sh_pair_v2"`
	PairV2PS1 bool `json:"install_ps1_pair_v2"`
	// StdinSH: install.sh reads the join code from its standard input (FlagJoinStdinSH), so the code is on no
	// command line. EnvPS1: install.ps1 takes it from the environment variable JoinCodeEnv, set in the
	// PowerShell window by the command.
	StdinSH bool `json:"install_sh_join_stdin"`
	EnvPS1  bool `json:"install_ps1_join_env"`
	// MemoryPS1: install.ps1 can be run from memory as a script block (no #Requires line, no use of its own
	// path), so the command needs no file between checking the script's hash and running it.
	MemoryPS1 bool `json:"install_ps1_from_memory"`
}

// The pairing v2 option of the install scripts, as the command passes it. The agent then stores a key made
// from the join code instead of the old verifier (`fairy-agent join --pair-v2`) and answers only a panel that
// proves it has the code, with its tokens encrypted.
const (
	FlagPairV2SH  = "--pair-v2"
	FlagPairV2PS1 = "-PairV2"
)

// How the join code reaches the install script without being on a command line, where every local user could
// read it while the installer runs: install.sh reads one line from standard input, install.ps1 reads an
// environment variable of the PowerShell window (and removes it).
const (
	FlagJoinStdinSH = "--join-stdin"
	JoinCodeEnv     = "FAIRY_JOIN_CODE"
)

// Lacks lists what the checked install script for this system ("linux", "windows") cannot do that the
// panel's command needs; empty when the release can be installed with it.
func (in Installers) Lacks(os string) []string {
	var out []string
	add := func(has bool, what string) {
		if !has {
			out = append(out, what)
		}
	}
	if os == "windows" {
		add(in.PinPS1, "checking the release's fingerprint (-SumsSha256)")
		add(in.PairV2PS1, "pairing v2 ("+FlagPairV2PS1+")")
		add(in.EnvPS1, "taking the join code from "+JoinCodeEnv+" instead of the command line")
		add(in.MemoryPS1, "running from memory, without a file between the check and the run")
		return out
	}
	add(in.PinSH, "checking the release's fingerprint (--sums-sha256)")
	add(in.PairV2SH, "pairing v2 ("+FlagPairV2SH+")")
	add(in.StdinSH, "taking the join code from standard input ("+FlagJoinStdinSH+") instead of the command line")
	return out
}

// PairsV2 reports whether the checked install script for this system ("linux", "windows") takes the pairing
// v2 option.
func (in Installers) PairsV2(os string) bool {
	if os == "windows" {
		return in.PairV2PS1
	}
	return in.PairV2SH
}

// ScriptSource gives the checked installers for the command (the real one fetches and verifies them once).
type ScriptSource interface {
	Installers(ctx context.Context) (Installers, error)
}

// Fixed is a ScriptSource with a fixed answer (demo mode and tests).
type Fixed Installers

// Installers returns the fixed answer.
func (f Fixed) Installers(context.Context) (Installers, error) { return Installers(f), nil }

// Unavailable means the panel cannot check the install script now (503: no key, GitHub unreachable, a bad
// signature). Add host is refused until it can.
type Unavailable struct{ Msg string }

func (u *Unavailable) Error() string { return u.Msg }

// TooOld means the Fairy-agent release the command would install cannot be used by this panel: its install
// script does not know pairing v2 (the panel never pairs the old, unauthenticated way) or cannot take the join
// code off the command line (409).
type TooOld struct{ Msg string }

func (o *TooOld) Error() string { return o.Msg }

var (
	tagRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	hexRe = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// The installers' own option parsing (signed, so it tells what the script accepts): install.sh's case arm
	// and install.ps1's param() entry. Older scripts refuse unknown options, so the hash is only passed when
	// the checked script takes it.
	shTakesSums  = regexp.MustCompile(`(?m)^\s*--sums-sha256\)`)
	ps1TakesSums = regexp.MustCompile(`(?im)^\s*\[string\]\$SumsSha256\b`)
	// Pairing v2, found the same way: install.sh's `--pair-v2)` case arm and install.ps1's `[switch]$PairV2`
	// parameter.
	shTakesPairV2  = regexp.MustCompile(`(?m)^\s*--pair-v2\)`)
	ps1TakesPairV2 = regexp.MustCompile(`(?im)^\s*\[switch\]\$PairV2\b`)
	// The join code off the command line: install.sh's `--join-stdin)` case arm; install.ps1 reads
	// $env:FAIRY_JOIN_CODE and no longer demands -Join.
	shTakesJoinStdin = regexp.MustCompile(`(?m)^\s*--join-stdin\)`)
	ps1ReadsJoinEnv  = regexp.MustCompile(`(?m)\$env:FAIRY_JOIN_CODE\b`)
	ps1JoinMandatory = regexp.MustCompile(`(?i)Mandatory[^\n]*\$Join\b`)
	// What stops install.ps1 from running as a script block made from the downloaded bytes: a #Requires line
	// (not checked there) and anything that needs the script's own path.
	ps1Requires = regexp.MustCompile(`(?im)^\s*#Requires\b`)
	ps1OwnPath  = regexp.MustCompile(`(?i)\$(PSScriptRoot|PSCommandPath|MyInvocation)\b`)
)

// Limits for what the panel downloads to check a release.
const (
	maxSmall  = 64 << 10  // SHA256SUMS, SHA256SUMS.sig, VERSION
	maxScript = 512 << 10 // install.sh, install.ps1
	retryWait = 30 * time.Second
)

// Verifier fetches and checks a pinned release's install scripts once and keeps the result (a release's files
// never change; the signature proves it). After a failure it waits retryWait before asking GitHub again.
type Verifier struct {
	Repo, Tag, Version string // Version "" = any VERSION (a tag set in config.yaml)
	Key                ed25519.PublicKey
	HTTP               *http.Client
	// DownloadBase is https://github.com and Hosts release.DefaultHosts; only tests change them.
	DownloadBase string
	Hosts        []string
	Now          func() time.Time

	mu      sync.Mutex
	ok      *Installers
	err     error
	errTime time.Time
}

// NewVerifier checks the release tag of repo with key (nil key: every check fails, the panel was built without
// the release key). version "" accepts any VERSION in that release.
func NewVerifier(repo, tag, version string, key ed25519.PublicKey) *Verifier {
	tr := &http.Transport{Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true, MaxIdleConns: 2,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 20 * time.Second, IdleConnTimeout: 30 * time.Second}
	return &Verifier{Repo: repo, Tag: tag, Version: version, Key: key, HTTP: &http.Client{Transport: tr},
		DownloadBase: "https://github.com", Hosts: release.DefaultHosts, Now: time.Now}
}

// Base is the release's download folder.
func (v *Verifier) Base() string {
	return v.DownloadBase + "/" + v.Repo + "/releases/download/" + url.PathEscape(v.Tag)
}

// Installers returns the checked installers, fetching and verifying them the first time.
func (v *Verifier) Installers(ctx context.Context) (Installers, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.ok != nil {
		return *v.ok, nil
	}
	if v.err != nil && v.Now().Sub(v.errTime) < retryWait {
		return Installers{}, v.err
	}
	in, err := v.check(ctx)
	if err != nil {
		v.err, v.errTime = &Unavailable{Msg: fmt.Sprintf("cannot check the Fairy-agent install script (release %s of %s): %v; "+
			"Add host shows no command until it can", v.Tag, v.Repo, err)}, v.Now()
		return Installers{}, v.err
	}
	v.ok, v.err = &in, nil
	return in, nil
}

func (v *Verifier) check(ctx context.Context) (Installers, error) {
	if len(v.Key) != ed25519.PublicKeySize {
		return Installers{}, errors.New("this panel build has no release signing key (not built by the release workflow); install a release build of the panel")
	}
	if !release.ValidRepo(v.Repo) || !tagRe.MatchString(v.Tag) {
		return Installers{}, fmt.Errorf("invalid release %q / %q", v.Repo, v.Tag)
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	files := map[string][]byte{}
	for _, f := range []struct {
		name string
		max  int64
	}{{release.SumsFile, maxSmall}, {release.SigFile, maxSmall}, {release.VersionFile, maxSmall},
		{InstallSH, maxScript}, {InstallPS1, maxScript}} {
		b, err := v.get(ctx, v.Base()+"/"+f.name, f.max)
		if err != nil {
			return Installers{}, err
		}
		files[f.name] = b
	}
	ch, err := release.VerifyFiles(v.Key, release.AgentProgram, files[release.SumsFile], files[release.SigFile], files[release.VersionFile])
	if err != nil {
		return Installers{}, err
	}
	if v.Version != "" && ch.Version != v.Version {
		return Installers{}, fmt.Errorf("release %s is fairy-agent %s, expected %s", v.Tag, ch.Version, v.Version)
	}
	sums := sha256.Sum256(files[release.SumsFile]) // the exact bytes whose signature was just checked
	in := Installers{Repo: v.Repo, Tag: v.Tag, Version: ch.Version, Base: v.Base(), Sums: hex.EncodeToString(sums[:]),
		PinSH: shTakesSums.Match(files[InstallSH]), PinPS1: ps1TakesSums.Match(files[InstallPS1]),
		PairV2SH: shTakesPairV2.Match(files[InstallSH]), PairV2PS1: ps1TakesPairV2.Match(files[InstallPS1]),
		StdinSH:   shTakesJoinStdin.Match(files[InstallSH]),
		EnvPS1:    ps1ReadsJoinEnv.Match(files[InstallPS1]) && !ps1JoinMandatory.Match(files[InstallPS1]),
		MemoryPS1: !ps1Requires.Match(files[InstallPS1]) && !ps1OwnPath.Match(files[InstallPS1])}
	for name, dst := range map[string]*string{InstallSH: &in.SH, InstallPS1: &in.PS1} {
		if err := ch.Sums.Check(name, files[name]); err != nil {
			return Installers{}, err
		}
		sum := sha256.Sum256(files[name])
		*dst = hex.EncodeToString(sum[:])
	}
	return in, nil
}

func (v *Verifier) allowed(u *url.URL) error {
	if u.Scheme != "https" || u.User != nil || !slices.Contains(v.Hosts, strings.ToLower(u.Hostname())) {
		return fmt.Errorf("refusing to fetch from %s://%s (only https from GitHub)", u.Scheme, u.Host)
	}
	return nil
}

// get downloads one file: https from the allowed hosts only (redirects too), at most max bytes.
func (v *Verifier) get(ctx context.Context, u string, max int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if err := v.allowed(req.URL); err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "fairy-panel")
	hc := *v.HTTP
	hc.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		return v.allowed(r.URL)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	name := u[strings.LastIndex(u, "/")+1:]
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub answered HTTP %d for %s", resp.StatusCode, name)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", name, err)
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s is larger than %d bytes", name, max)
	}
	return b, nil
}

// valid reports whether the installers can go into a command (defence in depth: they end up in a shell line).
func (in Installers) valid() bool {
	u, err := url.Parse(in.Base)
	return err == nil && u.Scheme == "https" && u.User == nil && u.RawQuery == "" && !strings.ContainsAny(in.Base, " '\"`$\\;&|<>(){}") &&
		hexRe.MatchString(in.SH) && hexRe.MatchString(in.PS1) && (!(in.PinSH || in.PinPS1) || hexRe.MatchString(in.Sums))
}

// PinnedVerifier checks the release Add host installs: tag (enroll.agent_release in config.yaml, any version)
// or, by default, AgentReleaseTag with exactly AgentReleaseVersion.
func PinnedVerifier(repo, tag string, key ed25519.PublicKey) *Verifier {
	if tag != "" {
		return NewVerifier(repo, tag, "", key)
	}
	return NewVerifier(repo, AgentReleaseTag, AgentReleaseVersion, key)
}

// Label names the release for the Add host dialog: "build-7 (fairy-agent 7-6155fe6)".
func (v *Verifier) Label() string {
	if v.Version != "" {
		return v.Tag + " (fairy-agent " + v.Version + ")"
	}
	return v.Tag
}

// ReleaseLabel names the release the command installs ("" when unknown).
func (m *Manager) ReleaseLabel() string {
	if l, ok := m.Scripts.(interface{ Label() string }); ok {
		return l.Label()
	}
	if f, ok := m.Scripts.(Fixed); ok {
		return f.Tag + " (fairy-agent " + f.Version + ")"
	}
	return ""
}
