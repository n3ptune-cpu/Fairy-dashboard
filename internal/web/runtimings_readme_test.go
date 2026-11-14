package web

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
)

// The README's part about "Run timings" is held to the code: the table has every timing under the name the
// dialog and the panel use, with the key and the range the panel checks, and the rule of its confirmation;
// and the sentences an owner acts on are there: what is stored where, that a run keeps its numbers, what
// going back to an older panel does.
func TestREADMESaysTheRunTimingsAsThePanelHasThem(t *testing.T) {
	b, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	readme := strings.Join(strings.Fields(string(b)), " ")
	asks := map[string]string{"": "inside the usual window", rtConfirmAlways: "by itself, **every time** it is changed", rtConfirmLower: "by itself, when it gets shorter", rtConfirmHigher: "by itself, when it gets longer"}
	for _, tm := range config.RunTimings() {
		unit := " s"
		if tm.Key == config.KeyRequestsPerMin {
			unit = ""
		}
		row := fmt.Sprintf("| %s | `%s.%s` | %d-%d%s | %s |", rtLabel[tm.Key], tm.Block, tm.Key, tm.Min, tm.Max, unit, asks[rtConfirm[tm.Key]])
		if strings.Count(readme, row) != 1 {
			t.Errorf("the README's table lacks the row %q", row)
		}
	}
	for _, want := range []string{
		"the Updates page, **Run timings**, next to the start button. Admins, from a browser session.",
		"(`state.json`, `run_timings`) as one decision per timing",
		"A value that is config.yaml's own is not stored.",
		"nothing below 300 s for \"counts as off\" or below 10 s for the countdown can be stored, typed or come into force through an edited `state.json`",
		"(\"" + rtConsequence(config.KeyOffAfter, 360) + "\")", // the sentence of the confirmation, as the panel makes it
		"**A change counts from the next run.** A run that is going keeps the numbers it started with",
		"when a timing is changed between your click on Start and your passkey, nothing is started",
		"(`wakeupdate.timings`, each timing with its old and its new value)",
		"API tokens cannot read or change the timings.",
		"The same goes for the timings changed with **Run timings** (`run_timings` in `state.json`), back to build 15 or older: the older panel runs with config.yaml's timings, says nothing about the key and leaves it in the file as it is",
	} {
		if !strings.Contains(readme, want) {
			t.Errorf("the README lacks %q", want)
		}
	}
	if !strings.Contains(wuTimingsChanged, "The run timings were changed after you pressed the button") || !strings.Contains(readme, "(\"The run timings were changed after you pressed the button ...\")") {
		t.Errorf("the README quotes the refusal of a start in other words than the panel's: %q", wuTimingsChanged)
	}
}
