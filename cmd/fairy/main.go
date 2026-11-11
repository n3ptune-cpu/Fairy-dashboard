// Command fairy is the Fairy homelab panel: `fairy serve`, `fairy admin ...`, `fairy version`.
package main

import (
	"fmt"
	"os"
	"runtime"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/admin"
)

// Set by the release workflow with -ldflags "-X main.version=... -X main.releaseKey=...". releaseKey is the
// base64 ed25519 public key that signs releases; `fairy update` refuses to install anything without it.
var (
	version    = "dev"
	releaseKey = ""
)

const usage = `Fairy homelab panel

usage:
  fairy serve [--config FILE] [--env-file FILE] [--listen 127.0.0.1:8080]
  fairy serve --demo [--listen 127.0.0.1:8090]   simulated data, NO login, loopback only
        [--logo FILE]                            ...with your own logo (PNG/JPEG/WebP/GIF, also for serve)
        [--demo-as viewer:Bob]                 ...previewed as a viewer of one site (or pending)
  fairy admin ...                                break-glass CLI (fairy admin help)
  fairy update [--check] [--force] [--yes]       install the latest signed Fairy-dashboard release (root, on
                                                 the panel host; keeps /usr/local/bin/fairy.old, rolls back)
  fairy version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		os.Exit(serve(os.Args[2:]))
	case "admin":
		if testBuild { // the CLI works on an installed panel's files; a test build keeps to its own folder
			fmt.Fprintln(os.Stderr, "fairy admin:", testAdminOff)
			os.Exit(1)
		}
		os.Exit(admin.Main(os.Args[2:], os.Stdout, os.Stderr))
	case "update":
		os.Exit(update(os.Args[2:]))
	case "version", "--version", "-v":
		key := "none (local build, fairy update refuses)"
		if releaseKey != "" {
			key = "built in"
		}
		fmt.Printf("fairy %s (%s, %s/%s) - release key: %s%s\n", version, runtime.Version(), runtime.GOOS, runtime.GOARCH, key, testNote)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}
