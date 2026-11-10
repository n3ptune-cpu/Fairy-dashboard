package normalise

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/textsafe"
)

// hostile is a text with one of everything a machine's text must not carry into the panel: an ANSI colour
// sequence, a NUL, a line break, a direction override (U+202E), a one-character CSI (U+009B) that clears the
// screen and a sequence that sets the window title. hostileClean is what is left of it.
var hostile = "a\x1b[31m\x00b\r\n" + string(rune(0x202e)) + string(rune(0x9b)) + "2Jc\x1b]0;title\x07"

const hostileClean = "ab  c"

// dirtyTexts returns every text in v (as the API would send it: through JSON) that textsafe.Clean would change.
func dirtyTexts(t *testing.T, v any) (out []string) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var x any
	if err := json.Unmarshal(b, &x); err != nil {
		t.Fatal(err)
	}
	var walk func(any)
	walk = func(v any) {
		switch v := v.(type) {
		case string:
			if textsafe.Clean(v) != v {
				out = append(out, v)
			}
		case map[string]any:
			for k, e := range v {
				walk(k)
				walk(e)
			}
		case []any:
			for _, e := range v {
				walk(e)
			}
		}
	}
	walk(x)
	return out
}

// N6: every text an agent reports arrives in the system entry without control characters, escape sequences
// and direction overrides. The text itself is kept (it is cleaned, not dropped), so a real name with an odd
// character in it still shows.
func TestEveryTextOfAnAgentIsCleaned(t *testing.T) {
	h := hostile
	m := jsonx.M{"hostname": h, "platform": jsonx.M{"system": h, "pretty": h},
		"disks":    []any{jsonx.M{"mount": "/mnt/" + h, "fstype": h, "total": 2.0, "used": 1.0}},
		"temps":    []any{jsonx.M{"sensor": h, "label": h, "current": 40.0}},
		"fans":     []any{jsonx.M{"name": h, "rpm": 900.0}},
		"voltages": []any{jsonx.M{"name": h, "volts": 12.0}},
		"gpus":     []any{jsonx.M{"name": h, "vendor": h, "source": h}},
		"hardware": jsonx.M{"system_vendor": h, "system_model": h, "board_vendor": h, "board_model": h, "bios_version": h, "cpu_model": h},
		"power": jsonx.M{"battery_state": h, "bmc_note": h, "sensors": []any{jsonx.M{"name": h, "watts": 1.0}},
			"source": []any{h}},
		"power_control": jsonx.M{"sleep": false, "sleep_note": h,
			"pending": jsonx.M{"action": "reboot", "at": 1.7e9, "delay_s": 60.0, "requested_by": h}},
		"helper":    jsonx.M{"state": h, "version": h, "since": h, "error": h, "features": []any{h}},
		"bmc_relay": jsonx.M{"targets": []any{jsonx.M{"id": h}, jsonx.M{"id": "bmc1"}}},
		"battery":   jsonx.M{"state": h, "percent": 50.0}}
	s := NormaliseAgent(config.Agent{ID: "box"}, m, time.Now(), 0, NewRateTracker())
	if d := dirtyTexts(t, s); len(d) > 0 {
		t.Fatalf("texts kept as sent: %q", d)
	}
	got := map[string]string{"name": s.Name, "os": model.Deref(s.OS), "platform": model.Deref(s.Platform),
		"mount": s.Disks[0].Mount, "fstype": model.Deref(s.Disks[0].FSType), "sensor": s.Temps[0].Sensor, "label": model.Deref(s.Temps[0].Label),
		"fan": s.Fans[0].Name, "voltage": s.Voltages[0].Name, "gpu": s.GPUs[0].Name, "cpu": model.Deref(s.CPUModel),
		"vendor": model.Deref(s.Hardware.SystemVendor), "bios": model.Deref(s.Hardware.BIOSVersion),
		"battery_state": model.Deref(s.ComponentPower.BatteryState), "bmc_note": model.Deref(s.ComponentPower.BMCNote),
		"power sensor": s.ComponentPower.Sensors[0].Name, "power source": s.ComponentPower.Source[0],
		"sleep_note": model.Deref(s.SleepNote), "requested_by": model.Deref(s.PowerPending.RequestedBy), "helper error": s.Helper.Error}
	for k, v := range got {
		want := hostileClean
		if k == "mount" {
			want = "/mnt/" + hostileClean
		}
		if v != want {
			t.Errorf("%s: %q, want %q", k, v, want)
		}
	}
	// what is an id or one of a few known words is not cleaned into one: it is dropped
	if len(s.BMCRelayTargets) != 1 || s.BMCRelayTargets[0] != "bmc1" || s.Helper.State != "unknown" || s.Helper.Version != "" ||
		len(s.Helper.Features) != 0 || s.GPUs[0].Vendor != "other" {
		t.Fatalf("relay targets %q, helper %+v, GPU %+v", s.BMCRelayTargets, s.Helper, s.GPUs[0])
	}
	// a cut never leaves a broken character or half an escape sequence behind
	long := ""
	for len(long) < 400 {
		long += "é\x1b[31m"
	}
	m2 := jsonx.M{"hostname": long}
	if s := NormaliseAgent(config.Agent{ID: "box"}, m2, time.Now(), 0, NewRateTracker()); len(dirtyTexts(t, s)) > 0 || len(s.Name) > maxLong+4 {
		t.Fatalf("%q", s.Name)
	}
	// and a clean text is the same text, not a copy with something changed
	if p := model.P("Ubuntu 24.04 (☃, 日本語, Größe)"); clipP(p, maxLong) != p || ClipText(*p, maxLong) != *p {
		t.Fatal("a clean text was changed")
	}
}

// The same for the Proxmox API: guest and node names, the node a guest runs on, tags.
func TestEveryTextOfAProxmoxRowIsCleaned(t *testing.T) {
	rows := []jsonx.M{
		{"type": "node", "node": "pve1" + hostile, "status": "online"},
		{"type": "lxc", "vmid": 100.0, "node": "pve1" + hostile, "status": "running", "name": hostile, "tags": "a;" + hostile + ";\x1b[0m;b"},
		{"type": "qemu", "vmid": 101.0, "node": "pve1", "status": hostile, "name": "vm"},
	}
	out := NormaliseProxmox(rows, time.Now(), 0, NewRateTracker(), ProxmoxOpts{})
	if len(out) != 3 {
		t.Fatalf("%d systems", len(out))
	}
	if d := dirtyTexts(t, out); len(d) > 0 {
		t.Fatalf("texts kept as sent: %q", d)
	}
	node, guest := out[0], out[1]
	// "a", the cleaned text (its line break was a tag separator: two tags), and "b"; the tag that was only an
	// escape sequence is gone
	if node.Name != "pve1"+hostileClean || guest.Name != hostileClean || model.Deref(guest.Parent) != "pve1"+hostileClean ||
		len(guest.Tags) < 3 || guest.Tags[0] != "a" || guest.Tags[len(guest.Tags)-1] != "b" || out[2].Status != "offline" {
		t.Fatalf("node %q, guest %q on %q, tags %q, status %q", node.Name, guest.Name, model.Deref(guest.Parent), guest.Tags, out[2].Status)
	}
	for _, tag := range guest.Tags {
		if tag == "" {
			t.Fatalf("an empty tag: %q", guest.Tags)
		}
	}
}

// The answer to a power request carries the same `pending` block as the report and gets the same checks (the
// page shows it at once): a known action and a time the page can show, or nothing.
func TestPendingOfAPowerAnswer(t *testing.T) {
	ok := Pending(jsonx.M{"pending": jsonx.M{"action": "shutdown", "at": 1.7e9, "delay_s": 60.0, "requested_by": hostile}})
	if ok == nil || ok.Action != "shutdown" || ok.DelayS != 60 || *ok.RequestedBy != hostileClean {
		t.Fatalf("%+v", ok)
	}
	for _, bad := range []jsonx.M{
		{"action": hostile, "at": 1.7e9}, {"action": "<b>format</b>", "at": 1.7e9}, {"action": "reboot", "at": 1e300},
		{"action": "reboot", "at": -1.0}, {"action": "reboot"}, nil,
	} {
		if p := Pending(jsonx.M{"pending": bad}); p != nil {
			t.Errorf("%v: %+v", bad, p)
		}
	}
	if Pending(nil) != nil || Pending(jsonx.M{}) != nil {
		t.Fatal("no block is no pending action")
	}
}
