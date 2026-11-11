//go:build fairytest

// Package releasekey gives a test build (-tags fairytest, TESTING-LOCAL.md) the public key Fairy's releases are
// signed with, so its Add host checks the Fairy-agent release the way a released panel does. A release gets the
// same key from its workflow (-ldflags) and is built without this file.
package releasekey

import _ "embed"

// Pub is release/signing.pub.
//
//go:embed signing.pub
var Pub string
