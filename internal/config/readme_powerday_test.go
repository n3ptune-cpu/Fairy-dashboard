package config

import (
	"os"
	"strings"
	"testing"
)

// The README about the power sources' last 24 hours: its own section for the user, the file in the list of
// what the data folder holds (security model) and in the map of the code, and what going back to an older
// panel means for the file (the paragraph TestReadmeGoingBackNamesWhatTheOlderPanelRefuses reads).
func TestReadmeHasTheLast24Hours(t *testing.T) {
	b, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	readme := string(b)
	_, sec, ok := strings.Cut(readme, "\n### Last 24 hours\n")
	if !ok {
		t.Fatal("README.md has no section \"Last 24 hours\"")
	}
	sec, _, _ = strings.Cut(sec, "\n### ")
	flat := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	for _, want := range []string{
		"click the small chart at the end of the source's row", "in the **Power** box",
		"**A gap stays a gap.**", "nothing is drawn at 0 W",
		"a counter that starts again at 0 after an outage is not counted backwards", "**estimated**", "**partly estimated**",
		"judged stretch by stretch", "to within 5 %: what it shows less is added",
		"**Time without readings** is counted where the counter went on through it by an amount the plug could have used", "that time is left out", "is left out and is not taken for a restart", "and at most 16 of one host",
		"**A wrong clock.**", "reads it as soon as the clock has passed the time the file was saved at", "within an hour", "saved more than a day before the clock's time is left alone for that hour",
		"a write that fails (a full disk) is said once in the log", "This source is no longer reported",
		"voltage_entity: sensor.corner_plug_voltage", "current_entity:", "energy_entity:", "an agent up to v0.8.0 does not start with one of these lines",
		"`power-day.json`", "every five minutes and when the panel stops", "no key in config.yaml", "At most 64 sources",
		"takes its whole day along", "**Remove** of a host added with Add host empties the graphs of the plugs that host relays", "**Re-pair** does not",
		"`GET /api/power/day?source=<id>`",
	} {
		if !strings.Contains(flat(sec), want) && !strings.Contains(sec, want) {
			t.Errorf("the section lacks %q", want)
		}
	}
	for _, want := range []string{
		"* `power-day.json` in the data folder is the other file that decides nothing",
		"(`day.go`), with their file `power-day.json` (`dayfile.go`)",
	} {
		if !strings.Contains(flat(readme), want) {
			t.Errorf("README.md lacks %q", want)
		}
	}
	_, back, ok := strings.Cut(readme, "\nGoing back to the older panel:")
	if !ok {
		t.Fatal("README.md has no paragraph \"Going back to the older panel\"")
	}
	back, _, _ = strings.Cut(back, "\n\n")
	if want := "`power-day.json` (the power sources' last 24 hours) is known to no build before the one that has the graph: build 15, 14 and 13 leave it alone, there is nothing to take out of config.yaml for it"; !strings.Contains(flat(back), want) {
		t.Errorf("the paragraph about going back lacks %q", want)
	}
}
