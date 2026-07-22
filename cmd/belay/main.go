// Command belay is a lightweight, self-hosted safe-updater for Docker Compose stacks:
// it updates images with a health gate and automatically rolls back failures.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/belay-sh/belay/internal/agent"
	"github.com/belay-sh/belay/internal/compose"
	"github.com/belay-sh/belay/internal/engine"
	"github.com/belay-sh/belay/internal/health"
	"github.com/belay-sh/belay/internal/version"
)

func usage() {
	fmt.Fprintf(os.Stderr, `belay %s — safe Docker Compose updates, with automatic rollback.

usage:
  belay update [flags] <compose-file|dir> <service> <new-image>
                   update one service, health-check it, and roll back on failure
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
	case "update":
		runUpdate(os.Args[2:])
	case "server":
		fmt.Println("belay server: not implemented yet") // TODO
	case "agent":
		fmt.Println("belay agent: not implemented yet") // TODO
	case "version", "-v", "--version":
		fmt.Println("belay", version.Version)
	default:
		usage()
		os.Exit(2)
	}
}

func runUpdate(args []string) {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	timeout := fs.Duration("timeout", 60*time.Second, "overall health-gate timeout")
	minUptime := fs.Duration("min-uptime", 10*time.Second, "stayed-running window when the image has no healthcheck")
	noCommit := fs.Bool("no-commit", false, "do not git-commit the change even if the compose dir is a repo")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: belay update [flags] <compose-file|dir> <service> <new-image>")
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if fs.NArg() != 3 {
		fs.Usage()
		os.Exit(2)
	}
	project, service, newImage := fs.Arg(0), fs.Arg(1), fs.Arg(2)

	file, err := compose.FileFor(project)
	if err != nil {
		fatal(err)
	}
	current, err := compose.FindImage(file, service)
	if err != nil {
		fatal(err)
	}

	fmt.Printf("→ %s: %s → %s\n", service, current, newImage)
	e := &engine.Engine{
		Deployer: agent.Local{},
		Health:   health.Gate{Timeout: *timeout, MinUptime: *minUptime},
	}
	res := e.SafeUpdate(context.Background(), engine.Request{
		Project: project, Service: service, FromImage: current, ToImage: newImage,
	})

	dur := res.Duration.Round(time.Millisecond)
	switch res.Outcome {
	case engine.OutcomeUpdated:
		fmt.Printf("✅ updated to %s (%s)\n", newImage, dur)
		if !*noCommit {
			if err := compose.CommitIfRepo(file, fmt.Sprintf("belay: update %s %s -> %s", service, current, newImage)); err != nil {
				fmt.Fprintf(os.Stderr, "warn: git commit failed: %v\n", err)
			}
		}
	case engine.OutcomeSkipped:
		fmt.Println("• already on that image; nothing to do")
	case engine.OutcomeRolledBack:
		fmt.Printf("↩️  rolled back to %s — update failed: %v (%s)\n", current, res.Err, dur)
		printLogs(res.Logs)
		os.Exit(1)
	case engine.OutcomeError:
		fmt.Printf("❌ error — service may need attention: %v\n", res.Err)
		printLogs(res.Logs)
		os.Exit(1)
	}
}

func printLogs(logs string) {
	if strings.TrimSpace(logs) == "" {
		return
	}
	fmt.Println("---- logs ----")
	fmt.Println(strings.TrimRight(logs, "\n"))
	fmt.Println("--------------")
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "belay:", err)
	os.Exit(1)
}
