//go:build windows

package statefile

// checkOwner: on Windows the data directory's ACL decides; nothing to check here.
func checkOwner(string) error { return nil }
