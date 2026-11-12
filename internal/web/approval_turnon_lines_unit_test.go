package web

import (
	"fmt"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/approval"
)

// The lines themselves: machines with the same list stand together above it, and a list that has no room is
// never cut silently.
func TestTurnOnLines(t *testing.T) {
	alice, bob := approval.User{Sub: "u-alice", Name: "Alice", Why: "admin"}, approval.User{Sub: "u-bob", Why: "approval.users"}
	got := turnOnLines([]turnOnAsked{
		{name: "PC-A", by: "agent", users: []approval.User{alice, bob}},
		{name: "PC-B", by: "helper", users: []approval.User{alice}},
		{name: "PC-C", by: "agent", users: []approval.User{alice, bob}},
	}, 80)
	want := []string{"PC-A (checked by its agent); PC-C (checked by its agent)", "2 people may approve on each of these 2 machines:",
		"· Alice [u-alice, admin]", "· (not a user of this panel) [u-bob, approval.users]",
		"PC-B (checked by its helper)", "1 person may approve on it:", "· Alice [u-alice, admin]"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	short := turnOnLines([]turnOnAsked{{name: "PC-A", by: "agent", users: []approval.User{alice, bob}}, {name: "PC-B", by: "helper", users: []approval.User{alice}}}, 4)
	if len(short) != 4 || short[3] != "4 more lines do not fit here. Cancel, and turn on fewer machines at a time to read every list." || short[0] != "PC-A (checked by its agent)" {
		t.Fatalf("a list without room: %q", short)
	}
	// 64 machines with one list of 32 people (the most a machine takes) fit: nothing is left out
	var many []turnOnAsked
	var people []approval.User
	for i := 0; i < 32; i++ {
		people = append(people, approval.User{Sub: fmt.Sprintf("user-%02d", i), Why: "approval.users"})
	}
	for i := 0; i < MaxTurnOn; i++ {
		many = append(many, turnOnAsked{name: fmt.Sprintf("PC-%02d", i), by: "agent", users: people})
	}
	lines := turnOnLines(many, maxPlanItems-4)
	if joined := strings.Join(lines, "\n"); len(lines) > maxPlanItems-4 || strings.Contains(joined, "do not fit") || strings.Count(joined, "PC-") != MaxTurnOn || strings.Count(joined, "· ") != 32 {
		t.Fatalf("%d lines for 64 machines and 32 people:\n%s", len(lines), joined)
	}
	for _, l := range lines {
		if len(l) > maxActionText {
			t.Fatalf("a line the dialog would cut: %q", l)
		}
	}
}
