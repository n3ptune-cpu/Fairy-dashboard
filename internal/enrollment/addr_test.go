package enrollment

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func ifc(name string, up bool, addrs ...string) Iface {
	i := Iface{Name: name, Up: up}
	for _, a := range addrs {
		i.Addrs = append(i.Addrs, netip.MustParseAddr(a))
	}
	return i
}

func detector(ifs []Iface, api []string, apiErr error) (Detector, *int) {
	calls := 0
	d := Detector{Interfaces: func() ([]Iface, error) { return ifs, nil }, Timeout: time.Second,
		LocalAPI: func(context.Context) ([]netip.Addr, error) {
			calls++
			var out []netip.Addr
			for _, a := range api {
				out = append(out, netip.MustParseAddr(a))
			}
			return out, apiErr
		}}
	return d, &calls
}

func TestDetectPanelAddress(t *testing.T) {
	lan := ifc("eth0", true, "10.0.0.20", "fe80::1")
	for _, tc := range []struct {
		name    string
		ifs     []Iface
		api     []string
		apiErr  error
		want    Detected
		wantAPI bool // the LocalAPI was asked
	}{
		{"tailscale0 IPv4", []Iface{lan, ifc("tailscale0", true, "100.64.0.10", "fd7a:115c:a1e0::1")}, nil, nil,
			Detected{"100.64.0.10", "interface tailscale0"}, false},
		{"the tailscale interface wins over another CGNAT address", []Iface{ifc("wg0", true, "100.100.1.1"), ifc("tailscale0", true, "100.64.0.10")}, nil, nil,
			Detected{"100.64.0.10", "interface tailscale0"}, false},
		{"one CGNAT address on another name", []Iface{lan, ifc("ts-custom", true, "100.70.0.3")}, nil, nil,
			Detected{"100.70.0.3", "interface ts-custom"}, false},
		{"interface down is ignored", []Iface{ifc("tailscale0", false, "100.64.0.10")}, nil, errors.New("no socket"),
			Detected{}, true},
		{"several, none named tailscale: ask tailscaled", []Iface{ifc("a", true, "100.64.0.1"), ifc("b", true, "100.64.0.2")}, []string{"100.64.0.2", "fd7a:115c:a1e0::2"}, nil,
			Detected{"100.64.0.2", "Tailscale LocalAPI"}, true},
		{"userspace networking: no interface, LocalAPI", []Iface{lan}, []string{"fd7a:115c:a1e0::9", "100.101.102.103"}, nil,
			Detected{"100.101.102.103", "Tailscale LocalAPI"}, true},
		{"IPv6 only on the interface", []Iface{lan, ifc("tailscale0", true, "fd7a:115c:a1e0:ab12::5")}, nil, errors.New("no socket"),
			Detected{"fd7a:115c:a1e0:ab12::5", "interface tailscale0"}, true},
		{"IPv6 only from the LocalAPI", []Iface{lan}, []string{"fd7a:115c:a1e0::7"}, nil,
			Detected{"fd7a:115c:a1e0::7", "Tailscale LocalAPI"}, true},
		{"IPv4 from the LocalAPI beats IPv6 on an interface", []Iface{ifc("tailscale0", true, "fd7a:115c:a1e0::5")}, []string{"100.90.0.1"}, nil,
			Detected{"100.90.0.1", "Tailscale LocalAPI"}, true},
		{"nothing at all", []Iface{lan}, nil, errors.New("dial unix: no such file"), Detected{}, true},
		{"addresses outside the tailnet ranges are ignored", []Iface{ifc("tailscale0", true, "192.168.1.5", "fd00::1")}, []string{"8.8.8.8"}, nil,
			Detected{}, true},
	} {
		d, calls := detector(tc.ifs, tc.api, tc.apiErr)
		got := d.Detect(context.Background())
		if got != tc.want || (*calls > 0) != tc.wantAPI {
			t.Errorf("%s: got %+v (LocalAPI asked %d times), want %+v", tc.name, got, *calls, tc.want)
		}
	}
}

// LocalAPIAddrs speaks plain HTTP over tailscaled's unix socket and reads Self.TailscaleIPs.
func TestLocalAPIAddrsOverUnixSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "tailscaled.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip("no unix sockets here:", err)
	}
	var gotHost, gotPath string
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost, gotPath = r.Host, r.URL.String()
		_, _ = w.Write([]byte(`{"Version":"1.80.0","BackendState":"Running","Self":{"HostName":"fairy","TailscaleIPs":["100.64.0.10","fd7a:115c:a1e0::1"]},"Peer":null}`))
	})}
	go func() { _ = srv.Serve(l) }()
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	addrs, err := LocalAPIAddrs(ctx, sock)
	if err != nil || len(addrs) != 2 || addrs[0].String() != "100.64.0.10" {
		t.Fatal(addrs, err)
	}
	if gotHost != "local-tailscaled.sock" || gotPath != "/localapi/v0/status?peers=false" {
		t.Fatal(gotHost, gotPath)
	}
	if _, err := LocalAPIAddrs(ctx, filepath.Join(t.TempDir(), "missing.sock")); err == nil {
		t.Fatal("a missing socket must fail")
	}
}

func TestParseStatus(t *testing.T) {
	if _, err := parseStatus(strings.NewReader(`{"BackendState":"NeedsLogin","Self":null}`)); err == nil {
		t.Fatal("no Self must fail")
	}
	if a, err := parseStatus(strings.NewReader(`{"Self":{"TailscaleIPs":["bogus","100.64.0.9"]}}`)); err != nil || len(a) != 1 {
		t.Fatal(a, err)
	}
}
