package agentcontract_test

import (
	"os"
	"strings"
	"testing"
	"time"

	agentapi "github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
)

// Audit 5, round 3, r3-x1-02: the guide said of the machine id "nobody else [sees it], and no API answer
// carries it", and "a machine cannot see the other machines". For one approval that covers several machines
// that is not so: every machine of the batch is sent the whole list of entries, with the other machines' ids
// (each needs the list to work out the nonce). The protocol is not changed here; the guide is, and names it
// as a known limit. This test holds the two together: what a batch sends (the auditor's
// TestR3X1_ABatchHandsEveryMachineTheOthersMachineIDs), and what the guide says about it. When a later
// protocol sends the other entries as hashes, this test fails and the paragraph goes.
func TestABatchHandsEveryMachineTheOthersIDsAndTheGuideSaysSo(t *testing.T) {
	pn := newApprovalPanel(t)
	pn.turnOn("pc-a", "pc-b")
	idA, idB := pn.a.machineID(t), pn.b.machineID(t)
	if trustB := pn.b.sent(agentapi.PathApprovalTrust); len(trustB) != 1 || !strings.Contains(string(trustB[0]), idA) {
		t.Fatal("Turn on for two: pc-b's POST /approval/trust does not carry pc-a's id any more: the guide's \"Known limits\" is out of date")
	}
	st, out := pn.call("POST", "/api/admin/updates", map[string]any{})
	id, _, _, ok := asked(st, out)
	if !ok {
		t.Fatalf("Update all: %d %v", st, out)
	}
	pn.approve(id)
	// Both machines are updating now, each in the background, into a folder of this test. The test must not
	// end before they are done: a folder an updater still writes to cannot be removed, and the test then
	// failed with "TempDir RemoveAll cleanup: ... update: directory not empty" (about one run in fifteen; it is
	// the failure of this package that the report of passkey-h could not name). Registered after the folders,
	// so it runs before they are removed.
	t.Cleanup(func() {
		for deadline := time.Now().Add(5 * time.Second); (pn.a.applied.Load() < 1 || pn.b.applied.Load() < 1) && time.Now().Before(deadline); {
			time.Sleep(10 * time.Millisecond)
		}
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
			pn.poll()
			_, page := pn.call("GET", "/api/admin/updates", nil)
			idle := 0
			for _, r := range page["agents"].([]any) {
				if r.(map[string]any)["can_update"] == true {
					idle++
				}
			}
			if idle == 2 {
				return
			}
		}
	})
	if updA := pn.a.sent("/update"); len(updA) != 1 || !strings.Contains(string(updA[0]), idB) {
		t.Fatal("Update all: pc-a's POST /update does not carry pc-b's id any more: the guide's \"Known limits\" is out of date")
	}
	// the panel itself still hands an id to an admin in a browser only (TestBindingViewersAndTokensLearnNoMachineIDs)
	raw, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	guide := strings.Join(strings.Fields(string(raw)), " ")
	for _, want := range []string{
		"**Known limits.**",
		"so every machine of that batch receives the machine ids of the others",
		// audit 6, r4-x1-05: the limit is wider than the ids, and the guide says so
		"so it also holds a usable approval for the other machines' entries",
		"whoever has that copy and another machine's control token can start the action that was approved for that machine",
		"as long as that machine has not used its entry itself and its challenge lives (15 minutes for an update)",
		"sending the other machines' entries as hashes, which is a change of the protocol and needs a later agent release",
		"It is a name, not a secret",
		"A machine cannot tell whether another machine has the same id",
	} {
		if !strings.Contains(guide, want) {
			t.Errorf("README.md lacks %q", want)
		}
	}
	for _, gone := range []string{"no API answer carries it", "A machine cannot see the other machines", "Machine ids are shown to admins in a browser only;"} {
		if strings.Contains(guide, gone) {
			t.Errorf("README.md still promises %q", gone)
		}
	}
}
