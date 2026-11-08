// Command fairy is the Fairy homelab panel: `fairy serve`, `fairy admin ...`, `fairy version`.
package main

import (
	"fmt"
	"os"
	"runtime"

	"github.com/n3ptune-cpu/Fairy/internal/admin"
)

var version = "dev" // set with -ldflags "-X main.version=..."

const usage = `Fairy homelab panel

usage:
  fairy serve [--config FILE] [--env-file FILE] [--listen 127.0.0.1:8080]
  fairy serve --demo [--listen 127.0.0.1:8090]   simulated data, NO login, loopback only
  fairy admin ...                                break-glass CLI (fairy admin help)
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
		os.Exit(admin.Main(os.Args[2:], os.Stdout, os.Stderr))
	case "version", "--version", "-v":
		fmt.Printf("fairy %s (%s, %s/%s)\n", version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}
