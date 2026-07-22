// Command belay is a lightweight, self-hosted safe-updater for Docker Compose stacks:
// it updates images with a health gate and automatically rolls back failures.
package main

import (
	"fmt"
	"os"

	"github.com/belay-sh/belay/internal/version"
)

func usage() {
	fmt.Fprintf(os.Stderr, `belay %s — safe Docker Compose updates, with automatic rollback.

usage:
  belay server     start the web UI + controller (includes a local agent)
  belay agent      start a headless agent that dials out to a server
  belay version    print version
`, version.Version)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "server":
		fmt.Println("belay server: not implemented yet") // TODO: server.Run(os.Args[2:])
	case "agent":
		fmt.Println("belay agent: not implemented yet") // TODO: agent.Run(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Println("belay", version.Version)
	default:
		usage()
		os.Exit(2)
	}
}
