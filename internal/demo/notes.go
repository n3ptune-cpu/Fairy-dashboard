package demo

import (
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/notifications"
)

// SeedNotes fills an empty demo Notifications page with a day of typical events (the demo's simulated data
// changes too little to make many by itself).
func SeedNotes(st *notifications.Store) {
	if st == nil || len(st.List("", func(notifications.Event) bool { return true }, notifications.Filter{Limit: 1}).Events) > 0 {
		return
	}
	now := time.Now().UTC()
	at := func(d time.Duration) time.Time { return now.Add(-d) }
	for _, e := range []notifications.Event{
		{T: at(26 * time.Hour), Kind: "host_added", Type: "enroll", Severity: notifications.Info, SystemID: "straw-pc", Host: "Straw's Ryzen PC", Text: "added with Add host and online"},
		{T: at(20 * time.Hour), Kind: "ups_on_battery", Type: "ups", Severity: notifications.Warning, UPSID: "asus-i7-agent/cyberpower", Host: "Straw's CyberPower", Text: "on battery (mains power lost)", Key: "ups:asus-i7-agent/cyberpower", Alarm: true},
		{T: at(20*time.Hour - 4*time.Minute), Kind: "ups_power_restored", Type: "ups", Severity: notifications.Info, UPSID: "asus-i7-agent/cyberpower", Host: "Straw's CyberPower", Text: "back on mains power after 4:02", Key: "ups:asus-i7-agent/cyberpower"},
		{T: at(9 * time.Hour), Kind: "update_done", Type: "update", Severity: notifications.Info, SystemID: "nsx-cyrene", Host: "NSX-CYRENE", Text: "agent updated to build 42 (1a2b3c4)"},
		{T: at(5 * time.Hour), Kind: "temp_high", Type: "temperature", Severity: notifications.Warning, SystemID: "node-nsx-snezhnaya", Host: "NSX-SNEZHNAYA", Text: "Tctl 87 °C (limit 85 °C)", Key: "temp:node-nsx-snezhnaya", Alarm: true},
		{T: at(5*time.Hour - 6*time.Minute), Kind: "temp_ok", Type: "temperature", Severity: notifications.Info, SystemID: "node-nsx-snezhnaya", Host: "NSX-SNEZHNAYA", Text: "temperature back to normal (76 °C)", Key: "temp:node-nsx-snezhnaya"},
		{T: at(3 * time.Hour), Kind: "power_shutdown", Type: "power", Severity: notifications.Info, SystemID: "straw-pc", Host: "Straw's Ryzen PC", Text: "shut down in 1 min, requested by straw@example.com"},
		{T: at(3*time.Hour - 2*time.Minute), Kind: "host_off", Type: "host", Severity: notifications.Info, SystemID: "straw-pc", Host: "Straw's Ryzen PC", Text: "off (shut down from the panel)"},
		{T: at(2 * time.Hour), Kind: "power_wake", Type: "power", Severity: notifications.Info, SystemID: "straw-pc", Host: "Straw's Ryzen PC", Text: "woken with Wake-on-LAN by straw@example.com"},
		{T: at(2*time.Hour - 3*time.Minute), Kind: "host_online", Type: "host", Severity: notifications.Info, SystemID: "straw-pc", Host: "Straw's Ryzen PC", Text: "on again", Key: "offline:straw-pc"},
		{T: at(40 * time.Minute), Kind: "host_offline", Type: "host", Severity: notifications.Warning, SystemID: "straw-laptop", Host: "Straw's laptop", Text: "offline · timeout", Key: "offline:straw-laptop", Alarm: true},
	} {
		st.Add(e)
	}
}
