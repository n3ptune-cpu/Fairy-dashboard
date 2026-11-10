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
		{T: at(26 * time.Hour), Kind: "host_added", Type: "enroll", Severity: notifications.Info, SystemID: "bob-pc", Host: "Bob's Ryzen PC", Text: "added with Add host and online"},
		{T: at(20 * time.Hour), Kind: "ups_on_battery", Type: "ups", Severity: notifications.Warning, UPSID: "mini-i7-agent/cyberpower", Host: "Bob's CyberPower", Text: "on battery (mains power lost)", Key: "ups:mini-i7-agent/cyberpower", Alarm: true},
		{T: at(20*time.Hour - 4*time.Minute), Kind: "ups_power_restored", Type: "ups", Severity: notifications.Info, UPSID: "mini-i7-agent/cyberpower", Host: "Bob's CyberPower", Text: "back on mains power after 4:02", Key: "ups:mini-i7-agent/cyberpower"},
		{T: at(9 * time.Hour), Kind: "update_done", Type: "update", Severity: notifications.Info, SystemID: "srv-tower", Host: "SRV-TOWER", Text: "agent updated to build 42 (1a2b3c4)"},
		{T: at(5 * time.Hour), Kind: "temp_high", Type: "temperature", Severity: notifications.Warning, SystemID: "node-srv-pveb", Host: "SRV-PVEB", Text: "Tctl 87 °C (limit 85 °C)", Key: "temp:node-srv-pveb", Alarm: true},
		{T: at(5*time.Hour - 6*time.Minute), Kind: "temp_ok", Type: "temperature", Severity: notifications.Info, SystemID: "node-srv-pveb", Host: "SRV-PVEB", Text: "temperature back to normal (76 °C)", Key: "temp:node-srv-pveb"},
		{T: at(3 * time.Hour), Kind: "power_shutdown", Type: "power", Severity: notifications.Info, SystemID: "bob-pc", Host: "Bob's Ryzen PC", Text: "shut down in 1 min, requested by bob@example.com"},
		{T: at(3*time.Hour - 2*time.Minute), Kind: "host_off", Type: "host", Severity: notifications.Info, SystemID: "bob-pc", Host: "Bob's Ryzen PC", Text: "off (shut down from the panel)"},
		{T: at(2 * time.Hour), Kind: "power_wake", Type: "power", Severity: notifications.Info, SystemID: "bob-pc", Host: "Bob's Ryzen PC", Text: "woken with Wake-on-LAN by bob@example.com"},
		{T: at(2*time.Hour - 3*time.Minute), Kind: "host_online", Type: "host", Severity: notifications.Info, SystemID: "bob-pc", Host: "Bob's Ryzen PC", Text: "on again", Key: "offline:bob-pc"},
		{T: at(40 * time.Minute), Kind: "host_offline", Type: "host", Severity: notifications.Warning, SystemID: "bob-laptop", Host: "Bob's laptop", Text: "offline · timeout", Key: "offline:bob-laptop", Alarm: true},
	} {
		st.Add(e)
	}
}
