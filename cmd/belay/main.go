// Command belay is a lightweight, self-hosted safe-updater for Docker Compose stacks:
// it updates images with a health gate and automatically rolls back failures.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/El-Mundos/belay/internal/agent"
	"github.com/El-Mundos/belay/internal/compose"
	"github.com/El-Mundos/belay/internal/discover"
	"github.com/El-Mundos/belay/internal/engine"
	"github.com/El-Mundos/belay/internal/health"
	"github.com/El-Mundos/belay/internal/registry"
	"github.com/El-Mundos/belay/internal/server"
	"github.com/El-Mundos/belay/internal/version"
)

func usage() {
	fmt.Fprintf(os.Stderr, `belay %s — safe Docker Compose updates, with automatic rollback.

usage:
  belay check <compose-file|dir>
                   list services that have newer stable versions available
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
	case "check":
		runCheck(os.Args[2:])
	case "update":
		runUpdate(os.Args[2:])
	case "server":
		runServer(os.Args[2:])
	case "agent":
		fmt.Println("belay agent: not implemented yet") // TODO
	case "version", "-v", "--version":
		fmt.Println("belay", version.Version)
	default:
		usage()
		os.Exit(2)
	}
}

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func runServer(args []string) {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8080", "listen address")
	var projects stringList
	fs.Var(&projects, "project", "compose project path (file or dir); repeatable")
	password := fs.String("password", os.Getenv("BELAY_PASSWORD"), "built-in login password (env BELAY_PASSWORD)")
	forward := fs.String("forward-header", os.Getenv("BELAY_FORWARD_HEADER"), "trusted reverse-proxy user header (e.g. X-authentik-username)")
	notifyURL := fs.String("notify-webhook", os.Getenv("BELAY_NOTIFY_WEBHOOK"), "webhook URL to POST on failed updates (ntfy/Discord/Slack/…)")
	timeout := fs.Duration("timeout", 90*time.Second, "health-gate timeout")
	minUptime := fs.Duration("min-uptime", 10*time.Second, "stayed-running window when the image has no healthcheck")
	fs.Parse(args)

	var pl []server.Project
	if len(projects) > 0 {
		// explicit projects
		for i, p := range projects {
			file, err := compose.FileFor(p)
			if err != nil {
				fatal(err)
			}
			pl = append(pl, server.Project{ID: i, Name: filepath.Base(filepath.Dir(file)), File: file})
		}
	} else {
		// auto-discover running compose stacks from the Docker daemon
		found, err := discover.RunningProjects(context.Background())
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: docker auto-discovery failed (%v); starting with no projects\n", err)
		} else {
			for i, d := range found {
				pl = append(pl, server.Project{ID: i, Name: d.Name, File: d.File})
			}
			fmt.Fprintf(os.Stderr, "auto-discovered %d compose project(s) from Docker\n", len(found))
		}
	}
	srv := server.New(server.Config{
		Addr: *addr, Projects: pl, Password: *password, ForwardHeader: *forward,
		NotifyWebhook: *notifyURL, Timeout: *timeout, MinUptime: *minUptime,
	})
	if err := srv.Run(); err != nil {
		fatal(err)
	}
}

func runCheck(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: belay check <compose-file|dir>")
		os.Exit(2)
	}
	file, err := compose.FileFor(args[0])
	if err != nil {
		fatal(err)
	}
	services, err := compose.Services(file)
	if err != nil {
		fatal(err)
	}
	rc := registry.New()
	ctx := context.Background()
	updates := 0
	for _, s := range services {
		if s.Image == "" || !strings.Contains(s.Image, ":") {
			continue // build-only or untagged
		}
		ref := registry.ParseRef(s.Image)
		newer, comparable, err := rc.Newer(ctx, ref)
		switch {
		case err != nil:
			fmt.Printf("  %-16s %-32s  error: %v\n", s.Name, ref.Tag, err)
		case !comparable:
			fmt.Printf("  %-16s %-32s  (non-semver tag — skipped)\n", s.Name, ref.Tag)
		case len(newer) == 0:
			fmt.Printf("  %-16s %-32s  up to date\n", s.Name, ref.Tag)
		default:
			updates++
			latest := newer[len(newer)-1]
			extra := ""
			if len(newer) > 1 {
				extra = fmt.Sprintf("  (+%d more)", len(newer)-1)
			}
			fmt.Printf("  %-16s %-32s→ %s%s\n", s.Name, ref.Tag, latest, extra)
		}
	}
	fmt.Printf("\n%d service(s) with updates available.\n", updates)
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
