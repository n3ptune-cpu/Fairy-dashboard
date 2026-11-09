package poller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/configutil"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/power"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/redact"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

type httpBackend struct {
	p             *Poller
	tokMu         sync.RWMutex // agentTokens / controlTokens change when hosts are added in the panel
	pveMu         sync.RWMutex // pve / pveSecrets / powerSecrets change when Proxmox nodes are added in the panel
	pve           map[string]*http.Client
	pveSecrets    map[string]string
	powerSecrets  map[string]string
	agentTokens   map[string]string
	controlTokens map[string]string
	agents        *http.Client
}

func newHTTPBackend(p *Poller) (*httpBackend, error) {
	cfg := p.Cfg
	b := &httpBackend{p: p, pve: map[string]*http.Client{}, pveSecrets: map[string]string{},
		powerSecrets: map[string]string{}, agentTokens: map[string]string{}, controlTokens: map[string]string{},
		agents: devices.LANClient(0)} // never via a proxy; per-request timeouts via context
	// Resolve every agent token at startup: a missing/short token is a config error.
	for _, a := range cfg.Agents {
		if err := b.SetAgentTokens(a); err != nil {
			return nil, err
		}
	}
	for _, c := range cfg.Proxmox { // one client per cluster: each has its own CA / verify setting and token
		if err := b.SetProxmox(c); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// SetProxmox builds a cluster's HTTP client (its CA, verify setting and certificate name) and resolves its
// tokens: from the environment, or for a node added in the panel from its managed secret.
func (b *httpBackend) SetProxmox(c config.Proxmox) error {
	tc, err := power.TLSConfig(c.Verify(), c.CAFile)
	if err != nil {
		return fmt.Errorf("proxmox %s: %w", c.ID, err)
	}
	if c.TLSServerName != "" {
		tc.ServerName = c.TLSServerName
	}
	var secret, powerSecret string
	switch {
	case c.Managed:
		secret = c.TokenSecret
	case c.TokenSecretEnv != "":
		if secret, err = configutil.EnvSecret(c.TokenSecretEnv, "proxmox "+c.ID, 1); err != nil {
			return err
		}
	default:
		secret = b.p.Secrets.ProxmoxTokenSecret
	}
	if c.PowerTokenID != "" && c.PowerTokenSecretEnv != "" { // separate token: VM.PowerMgmt only
		if powerSecret, err = configutil.EnvSecret(c.PowerTokenSecretEnv, "proxmox "+c.ID+" power", 1); err != nil {
			return err
		}
	}
	b.pveMu.Lock()
	defer b.pveMu.Unlock()
	b.pve[c.ID], b.pveSecrets[c.ID] = devices.TLSClient(0, tc), secret
	if powerSecret != "" {
		b.powerSecrets[c.ID] = powerSecret
	}
	return nil
}

// ForgetProxmox drops a cluster's client and tokens (a node removed in the panel).
func (b *httpBackend) ForgetProxmox(id string) {
	b.pveMu.Lock()
	defer b.pveMu.Unlock()
	delete(b.pve, id)
	delete(b.pveSecrets, id)
	delete(b.powerSecrets, id)
}

// pveClient returns a cluster's client and read / power secrets.
func (b *httpBackend) pveClient(id string) (*http.Client, string, string) {
	b.pveMu.RLock()
	defer b.pveMu.RUnlock()
	return b.pve[id], b.pveSecrets[id], b.powerSecrets[id]
}

func getJSON(ctx context.Context, c *http.Client, u string, hdr map[string]string, timeout time.Duration) (jsonx.M, int, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, devices.Errf("invalid url")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	var out jsonx.M
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode >= 400 {
		_ = json.Unmarshal(body, &out)
		return out, resp.StatusCode, &devices.HTTPStatusError{Code: resp.StatusCode}
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &out); err != nil {
			return nil, resp.StatusCode, devices.Errf("invalid response")
		}
	}
	return out, resp.StatusCode, nil
}

func (b *httpBackend) pveAuth(c config.Proxmox) map[string]string {
	_, secret, _ := b.pveClient(c.ID)
	return map[string]string{"Authorization": "PVEAPIToken=" + c.TokenID + "=" + secret}
}

func (b *httpBackend) FetchProxmox(ctx context.Context, pve config.Proxmox, timeout time.Duration) ([]jsonx.M, error) {
	var last error = errors.New("no proxmox urls")
	cl, _, _ := b.pveClient(pve.ID)
	if cl == nil {
		return nil, errors.New("proxmox " + pve.ID + " is not configured")
	}
	for _, base := range pve.URLs { // failover across cluster nodes
		d, _, err := getJSON(ctx, cl, strings.TrimRight(base, "/")+"/api2/json/cluster/resources", b.pveAuth(pve), timeout)
		if err != nil {
			last = err
			continue
		}
		rows, ok := d["data"].([]any)
		if !ok {
			last = devices.Errf("invalid response")
			continue
		}
		return jsonx.Objs(rows), nil
	}
	return nil, last
}

func (b *httpBackend) FetchNodeStatus(ctx context.Context, pve config.Proxmox, node string) (jsonx.M, error) {
	u := strings.TrimRight(pve.URLs[0], "/") + "/api2/json/nodes/" + url.PathEscape(node) + "/status"
	cl, _, _ := b.pveClient(pve.ID)
	if cl == nil {
		return nil, errors.New("proxmox " + pve.ID + " is not configured")
	}
	d, _, err := getJSON(ctx, cl, u, b.pveAuth(pve), 0)
	if err != nil {
		return nil, err
	}
	return jsonx.Obj(d, "data"), nil
}

// PVEPower: POST with the separate power token. Fails over across the cluster's URLs only on connection errors,
// so an action is never sent twice.
func (b *httpBackend) PVEPower(ctx context.Context, clusterID, path string, data url.Values) (any, error) {
	pve, _ := b.p.ClusterCfg(clusterID)
	cl, _, secret := b.pveClient(clusterID)
	if secret == "" || pve.PowerTokenID == "" || cl == nil {
		return nil, apierr.New(409, "no power token configured")
	}
	segs := strings.Split(path, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	safe := strings.Join(segs, "/")
	last := "no urls"
	timeout := time.Duration((b.p.Cfg.RequestTimeout + 6) * float64(time.Second))
	for _, base := range pve.URLs {
		cctx, cancel := context.WithTimeout(ctx, timeout)
		req, err := http.NewRequestWithContext(cctx, http.MethodPost, strings.TrimRight(base, "/")+"/api2/json"+safe,
			strings.NewReader(data.Encode()))
		if err != nil { // a malformed URL in config.yaml: try the next one (used to be a nil-pointer panic)
			cancel()
			last = "invalid url"
			continue
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Authorization", "PVEAPIToken="+pve.PowerTokenID+"="+secret)
		resp, err := cl.Do(req)
		if err != nil {
			cancel()
			var ne interface{ Timeout() bool }
			if errors.As(err, &ne) && ne.Timeout() {
				return nil, apierr.New(502, "proxmox: timeout") // may have been delivered: never resend
			}
			last = devices.Describe(err)
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		cancel()
		if resp.StatusCode == 403 { // Proxmox answered: 424, not 502 (see apierr.Upstream)
			return nil, apierr.New(apierr.Upstream, "proxmox refused the power token (needs VM.PowerMgmt / Sys.PowerMgmt)")
		}
		if resp.StatusCode >= 400 {
			return nil, apierr.Errf(apierr.Upstream, "proxmox: http %d", resp.StatusCode)
		}
		var out jsonx.M
		_ = json.Unmarshal(body, &out)
		return out["data"], nil
	}
	return nil, apierr.New(502, "proxmox: "+last)
}

// SetAgentTokens resolves an agent's tokens: from the environment (config.yaml's token_env /
// control_token_env), or for a host added in the panel from its managed secrets.
func (b *httpBackend) SetAgentTokens(a config.Agent) error {
	var t, ct string
	if a.Managed {
		t, ct = a.Token, a.ControlToken
		if len(t) < 24 || (ct != "" && len(ct) < 24) {
			return fmt.Errorf("agent %s: managed tokens too short", a.ID)
		}
	} else {
		var err error
		if t, err = configutil.EnvSecret(a.TokenEnv, "agent "+a.ID, 24); err != nil {
			return err
		}
		if env := a.ControlEnv(); env != "" {
			if ct, err = configutil.EnvSecret(env, "agent "+a.ID+" control", 24); err != nil {
				return err
			}
		}
	}
	if ct != "" && ct == t {
		return fmt.Errorf("agent %s: control token must differ from the read token", a.ID)
	}
	b.tokMu.Lock()
	defer b.tokMu.Unlock()
	b.agentTokens[a.ID] = t
	if ct != "" {
		b.controlTokens[a.ID] = ct
	} else {
		delete(b.controlTokens, a.ID)
	}
	return nil
}

// ForgetAgent drops a removed agent's tokens.
func (b *httpBackend) ForgetAgent(id string) {
	b.tokMu.Lock()
	defer b.tokMu.Unlock()
	delete(b.agentTokens, id)
	delete(b.controlTokens, id)
}

func (b *httpBackend) readToken(id string) string {
	b.tokMu.RLock()
	defer b.tokMu.RUnlock()
	return b.agentTokens[id]
}

func (b *httpBackend) controlToken(id string) string {
	b.tokMu.RLock()
	defer b.tokMu.RUnlock()
	return b.controlTokens[id]
}

func (b *httpBackend) agentURL(a config.Agent, path string) string {
	return strings.TrimRight(a.URL, "/") + path
}

func (b *httpBackend) FetchAgentMetrics(ctx context.Context, a config.Agent, timeout time.Duration) (jsonx.M, error) {
	d, _, err := getJSON(ctx, b.agents, b.agentURL(a, "/metrics"), map[string]string{"Authorization": "Bearer " + b.readToken(a.ID)}, timeout)
	return d, err
}

func (b *httpBackend) FetchProcesses(ctx context.Context, a config.Agent, sid string, limit int, sortBy string) (model.ProcessList, error) {
	q := url.Values{"limit": {strconv.Itoa(limit)}, "sort": {sortBy}}
	d, code, err := getJSON(ctx, b.agents, b.agentURL(a, "/processes?"+q.Encode()),
		map[string]string{"Authorization": "Bearer " + b.readToken(a.ID)}, time.Duration((b.p.Cfg.RequestTimeout+2)*float64(time.Second)))
	if code == 403 && jsonx.S(d, "code") == "processes_disabled" {
		return model.ProcessList{SystemID: sid, DisabledByHost: true, Sort: sortBy, Processes: []model.Process{}}, nil
	}
	if err != nil {
		return model.ProcessList{}, err
	}
	return DecodeProcessList(d, sid)
}

// DecodeProcessList validates an agent /processes payload.
func DecodeProcessList(d jsonx.M, sid string) (model.ProcessList, error) {
	b, _ := json.Marshal(d)
	var pl model.ProcessList
	if err := json.Unmarshal(b, &pl); err != nil {
		return pl, devices.Errf("invalid response")
	}
	pl.SystemID = sid
	if pl.Sort == "" {
		pl.Sort = "cpu"
	}
	for i := range pl.Processes { // defence in depth: agents redact too, but some are not ours to trust
		if p := pl.Processes[i].Cmdline; p != nil {
			v := redact.Cmdline(*p, 300)
			pl.Processes[i].Cmdline = &v
		}
	}
	model.Normalize(&pl)
	return pl, nil
}

func (b *httpBackend) AgentRequest(ctx context.Context, a config.Agent, method, path string, body any) (int, jsonx.M, error) {
	tok := b.readToken(a.ID)
	if method == http.MethodPost {
		tok = b.controlToken(a.ID)
	}
	var rd io.Reader
	if body != nil {
		bb, _ := json.Marshal(body)
		rd = strings.NewReader(string(bb))
	}
	cctx, cancel := context.WithTimeout(ctx, time.Duration((b.p.Cfg.RequestTimeout+8)*float64(time.Second))) // ipmi via powershell is slow
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, method, b.agentURL(a, path), rd)
	if err != nil {
		return 0, nil, devices.Errf("invalid url")
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := b.agents.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	out := jsonx.M{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			return resp.StatusCode, nil, devices.Errf("invalid response")
		}
	}
	return resp.StatusCode, out, nil
}
