package poller

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/textsafe"
)

// certFor makes a self-signed certificate with its own key for these DNS names.
func certFor(t *testing.T, names []string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), DNSNames: names,
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, IsCA: true, BasicConstraintsValid: true,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// hostileName is a DNS name of a certificate that is no host name: an escape sequence that sets the window
// title, one that clears the screen and a line break (a certificate made by Go takes ASCII only).
const hostileName = "evil\x1b]0;title\x07\x1b[2J-\r\n-injected-line"

// certPoller is a poller with one Proxmox cluster (verify_tls on, ca_file, tls_server_name pve-good) at a
// fake API that lists one node and `guests` guests. The API shows a good certificate for pve-good until evil
// is set, then a certificate of another key (not signed by the ca_file) that lists `names`.
func certPoller(t *testing.T, guests int, names []string) (p *Poller, evil *atomic.Bool) {
	t.Helper()
	good, bad := certFor(t, []string{"pve-good"}), certFor(t, names)
	evil = &atomic.Bool{}
	var list bytes.Buffer
	list.WriteString(`{"data":[{"type":"node","node":"pve-good","status":"online","maxcpu":8,"maxmem":1000,"mem":10}`)
	for i := range guests {
		fmt.Fprintf(&list, `,{"type":"qemu","vmid":%d,"node":"pve-good","status":"running","name":"vm%d","maxmem":1000,"mem":10}`, 100+i, i)
	}
	list.WriteString("]}")
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/cluster/resources") {
			_, _ = w.Write(list.Bytes())
			return
		}
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	srv.TLS = &tls.Config{GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		if evil.Load() {
			return &bad, nil
		}
		return &good, nil
	}}
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // the refused handshakes
	srv.Config.SetKeepAlivesEnabled(false)           // every poll makes a new handshake
	srv.StartTLS()
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "pve-ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: good.Certificate[0]}), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := testutil.Config(t, []string{srv.URL}, deadURL(t), deadURL(t))
	cfg.Proxmox[0].VerifyTLS, cfg.Proxmox[0].CAFile, cfg.Proxmox[0].TLSServerName = model.P(true), ca, "pve-good"
	cfg.RequestTimeout = 20
	p, err := New(cfg, config.Secrets{SessionSecret: "x", ProxmoxTokenSecret: "s"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p, evil
}

// T-06: whoever answers the TLS handshake at a Proxmox address chooses the DNS names of the certificate it
// shows; no trusted certificate is needed for that. The names went, as many and as long as they were and
// with their control characters, into the error text of the cluster's source, of every node and guest, the
// log line and every snapshot. Now an error text is short and clean wherever it is kept, and of a
// certificate's names at most three that are host names are repeated.
func TestCertificateNamesAtAProxmoxAddressStayOutOfTheErrorText(t *testing.T) {
	const guests = 40
	names := []string{hostileName, "pve1", "pve1.lan"}
	for n := 0; n < 200_000; n += 201 {
		names = append(names, strings.Repeat("a", 200))
	}
	p, evil := certPoller(t, guests, names)
	ctx := context.Background()
	pve := p.Cfg.Proxmox[0]
	p.pollProxmox(ctx, pve, 0)
	if n, g := pveSystems(p); n != 1 || g != guests {
		t.Fatalf("with the good certificate: %d nodes, %d guests, source %+v", n, g, pveSource(p))
	}

	var log bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&log, nil)))
	evil.Store(true)
	p.pollProxmox(ctx, pve, 0)
	slog.SetDefault(old)

	check := func(what, e string) {
		t.Helper()
		if len(e) > textsafe.MaxError+len("…") {
			t.Fatalf("%s: an error text of %d bytes (limit %d): %.120q...", what, len(e), textsafe.MaxError, e)
		}
		if textsafe.Clean(e) != e || strings.Contains(e, "injected-line") || strings.Contains(e, "aaaaaaaa") {
			t.Fatalf("%s: the error text carries what the certificate says: %q", what, e)
		}
	}
	src := pveSource(p)
	if src.OK || src.LastError == nil {
		t.Fatalf("the poll with the other certificate did not fail: %+v", src)
	}
	check("source", *src.LastError)
	// it still says what is wrong and what to do, and names the host names the certificate does list
	for _, want := range []string{`not issued for "pve-good"`, "pvecm updatecerts --force", "tls_server_name", "pve1, pve1.lan"} {
		if !strings.Contains(*src.LastError, want) {
			t.Fatalf("the error text lost %q: %q", want, *src.LastError)
		}
	}
	js, order := p.SnapshotJSON()
	total, failed := 0, 0
	for _, s := range p.Snapshot() {
		if s.Source != "proxmox" {
			continue
		}
		if s.Error == nil || s.Status == "online" {
			t.Fatalf("%s is not marked as failing: %+v", s.ID, s)
		}
		failed++
		check(s.ID, *s.Error)
		total += len(js[s.ID])
	}
	if failed != guests+1 || len(order) < failed {
		t.Fatalf("%d systems of the cluster carry the error, want %d", failed, guests+1)
	}
	if total > (guests+1)*2048 {
		t.Fatalf("the cluster's %d systems are %d bytes of JSON", failed, total)
	}
	if log.Len() > 2048 || !strings.Contains(log.String(), "proxmox poll failed") || strings.Contains(log.String(), "injected-line") {
		t.Fatalf("the log line of the failed poll: %d bytes: %.300s", log.Len(), log.String())
	}
}

// T-06, the size of it: 2000 guests and 200 kB of certificate names were 400 MB of error text in every
// snapshot (the unit's MemoryMax is 512M), built again after every poll even with nobody looking.
func TestCertificateNamesTimesGuestsStaySmall(t *testing.T) {
	if testing.Short() {
		t.Skip("2000 guests")
	}
	names := []string{hostileName}
	for n := 0; n < 200_000; n += 201 {
		names = append(names, strings.Repeat("a", 200))
	}
	p, evil := certPoller(t, 2000, names)
	ctx := context.Background()
	pve := p.Cfg.Proxmox[0]
	p.pollProxmox(ctx, pve, 0)
	if _, g := pveSystems(p); g != 2000 {
		t.Fatalf("%d guests", g)
	}
	base := heapMiB()
	evil.Store(true)
	p.pollProxmox(ctx, pve, 0)
	if src := pveSource(p); src.OK {
		t.Fatalf("the poll with the other certificate did not fail: %+v", src)
	}
	js, _ := p.SnapshotJSON() // what the watcher, the health file and every open page ask for
	total := 0
	for _, b := range js {
		total += len(b)
	}
	if total > 4<<20 {
		t.Fatalf("the snapshot's JSON is %d bytes", total)
	}
	if grew := heapMiB() - base; grew > 16 {
		t.Fatalf("the failed poll left %.0f MiB behind", grew)
	}
}

// T-06, the second line: whatever put an error text on a system, the snapshot hands it out short and clean.
func TestSnapshotClipsAnErrorText(t *testing.T) {
	p, _ := certPoller(t, 1, []string{"x"})
	p.pollProxmox(context.Background(), p.Cfg.Proxmox[0], 0)
	long := "boom " + hostileName + "\u202e" + strings.Repeat("b", 100_000) // and a direction override
	p.mu.Lock()
	var id string
	for k, s := range p.systems {
		if s.Source == "proxmox" && s.Kind != "node" {
			id = k
			s.Error = model.P(long)
			p.systems[k] = s
		}
	}
	p.snapVer, p.snapJSONVer = -1, -1
	p.mu.Unlock()
	for _, s := range p.Snapshot() {
		if s.ID != id {
			continue
		}
		if s.Error == nil || len(*s.Error) > textsafe.MaxError+len("…") || textsafe.Clean(*s.Error) != *s.Error ||
			!strings.HasPrefix(*s.Error, "boom evil") {
			t.Fatalf("the snapshot's error text: %d bytes: %.80q", len(model.Deref(s.Error)), model.Deref(s.Error))
		}
		return
	}
	t.Fatal("the guest is not in the snapshot")
}

// T-06: which names of a certificate an error text repeats.
func TestCertNames(t *testing.T) {
	long := strings.Repeat("a", 65)
	for _, tc := range []struct {
		names []string
		want  string
	}{
		{nil, "it lists none"},
		{[]string{"pve1"}, "pve1"},
		{[]string{"pve1", "pve1.lan", "*.example.com"}, "pve1, pve1.lan, *.example.com"},
		{[]string{"a", "b", "c", "d", "e"}, "a, b, c and 2 more"},
		{[]string{hostileName, long, "pve 1", "-x", "x-", "*", "a_b", ""}, "none of its 8 is a host name"},
		{[]string{hostileName, "pve1", long}, "pve1 and 2 more"},
	} {
		if got := certNames(tc.names); got != tc.want {
			t.Errorf("certNames(%.60q) = %q, want %q", tc.names, got, tc.want)
		}
	}
	// the longest text the certificate can make still says what to do, and is not cut
	n := strings.Repeat("n", 64)
	he := x509.HostnameError{Host: strings.Repeat("h", 300), Certificate: &x509.Certificate{DNSNames: []string{n, n, n, "x", "y"}}}
	msg := proxmoxErr(he)
	if len(msg) > textsafe.MaxError+len("…") || !strings.Contains(msg, "pvecm updatecerts --force") || !strings.Contains(msg, "tls_server_name") {
		t.Fatalf("%d bytes: %q", len(msg), msg)
	}
}
