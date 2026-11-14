package web

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
)

// The README's part about a machine's own run timings in "Edit host" is held to the code: the table has the
// two timings under the names the dialog and the panel use, with the range the panel checks; and the
// sentences an owner acts on are there, the one of the confirmation as the panel makes it.
func TestREADMESaysAMachinesOwnRunTimingsAsThePanelHasThem(t *testing.T) {
	b, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	readme := strings.Join(strings.Fields(string(b)), " ")
	if n := len(config.MachineRunTimings()); n != 2 {
		t.Fatalf("%d timings can be a machine's own; the README has two", n)
	}
	for _, tm := range config.MachineRunTimings() {
		row := fmt.Sprintf("| **%s** |", rtLabel[tm.Key])
		end := fmt.Sprintf("| %d to %d s |", tm.Min, tm.Max)
		i := strings.Index(readme, row)
		if i < 0 || strings.Count(readme, row) != 1 || !strings.Contains(readme[i:i+260], "`"+tm.Key+"`") || !strings.Contains(readme[i:i+260], end) {
			t.Errorf("the README's table lacks the row of %s (%q ... %q)", tm.Key, row, end)
		}
	}
	for _, want := range []string{
		"#### The machine's own run timings Between \"Wake-on-LAN\" and the links the dialog has the group **Run timings**",
		"**from config.yaml** (the machine's own block), **from Run timings** (the machine has none of its own and follows the panel's timing",
		"(`state.json`, `host_options`, `hosts`, `<id>`, `wake_update`). The number you save is the number that counts for this machine afterwards.",
		"A value that counts anyway (the block's, or the panel's where the machine has no block) is not stored: typing it is the same as Reset",
		"Nothing below 300 s or below 10 s can be typed, stored, or come into force through a `state.json` edited by hand",
		"**Every change of the two asks you to confirm it's you, by itself**, also right after you confirmed something else, and also a longer countdown and a Reset.",
		"\"" + hoRunConsequence(config.KeyOffAfter, "PC-A", 360) + "\"", // the sentence of the confirmation, as the panel makes it
		"**A change counts from the next run.** A run that is going keeps the two numbers it took for the machine when it started",
		"when a listed machine's own timing is changed in between, nothing is started",
		"A machine that checks passkey approvals itself takes a countdown of at most 10 minutes in a run.",
		"Remove of **another** host that was the sender of this machine's wake packets takes this machine's Wake-on-LAN change out and leaves its run timings and its name as they are.",
		"(\"wake_update.off_after_s 360 (config.yaml: 600)\")",
		"`\"wake_update\": {\"off_after_s\": 600, \"shutdown_delay_s\": 30}`",
		"`{\"field\": \"off_after_s\"}` / `{\"field\": \"shutdown_delay_s\"}` in `reset`",
		"(\"host pc-a: \"wake_update\" cannot be set in the panel: it is not used (and left in the file as it is)\")",
		"One machine's own two timings are changed in **Edit host** on the machine's page",
	} {
		if !strings.Contains(readme, want) {
			t.Errorf("the README lacks %q", want)
		}
	}
	if strings.Contains(readme, "own two timings are still set in config.yaml") {
		t.Error("the README still says that a machine's own timings can only be set in config.yaml")
	}
}
