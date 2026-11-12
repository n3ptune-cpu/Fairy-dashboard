//go:build !fairytest

package main

import "github.com/n3ptune-cpu/Fairy-dashboard/internal/web"

// The test build (a panel without sign-in for trying things on one's own PC, TESTING-LOCAL.md) is compiled in
// only with `go build -tags fairytest`: testmode.go here, internal/web/testmode.go, release/key.go. This file
// is what every other build gets instead, releases included: the places that ask (main.go, serve.go, update.go)
// find nothing.
const (
	testBuild     = false
	testNote      = ""
	testUpdateOff = ""
	testAdminOff  = ""
)

func testDefaults(*serveFlags)              {}
func setupTest(serveFlags) (*panel, int)    { return nil, 1 }
func testServer(*web.Server, *panel) func() { return func() {} }
