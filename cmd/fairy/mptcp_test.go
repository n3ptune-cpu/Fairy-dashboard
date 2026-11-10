package main

import (
	"net"
	"testing"
)

// Multipath TCP is off for everything this program listens on and dials: go.mod says "godebug multipathtcp=0".
// Go otherwise switches it on for listeners on Linux. The line sets the default for the programs and tests of
// this module only; it does not come with Fairy-agent's packages, whose go.mod has its own.
//
// In a workspace build (a go.work file, GOWORK) Go takes the godebug lines of go.work and ignores go.mod's: a
// panel built that way has Multipath TCP on unless go.work says "godebug multipathtcp=0" too, and this test
// says so. Releases are built without a workspace.
func TestMultipathTCPIsOff(t *testing.T) {
	var lc net.ListenConfig
	if lc.MultipathTCP() {
		t.Error("a listener of this program would use Multipath TCP: go.mod has lost its line `godebug multipathtcp=0`, " +
			"or this is a workspace build (go.work), where only go.work's godebug lines count")
	}
	var d net.Dialer
	if d.MultipathTCP() {
		t.Error("a connection this program opens would use Multipath TCP")
	}
}
