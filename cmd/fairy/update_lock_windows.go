//go:build windows

package main

// lockUpdate: `fairy update` installs on the Linux panel host only (on Windows it can only --check, which
// takes no lock), so there is nothing to keep apart here.
func lockUpdate(string) (func(), error) { return func() {}, nil }
