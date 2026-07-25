// Package health implements the engine.HealthGate ladder for a freshly-deployed compose service:
//
//  1. Docker HEALTHCHECK, if the image defines one (authoritative)
//  2. otherwise: the container stayed running (not exited/crash-looping) for MinUptime
//
// A configured HTTP/TCP/log-line probe is a planned middle rung.
package health

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/El-Mundos/belay/internal/compose"
	"github.com/El-Mundos/belay/internal/engine"
)

type Gate struct {
	Timeout   time.Duration // overall deadline
	MinUptime time.Duration // "stayed running" window when the image has no healthcheck
	Poll      time.Duration // poll interval
}

func (g Gate) Wait(ctx context.Context, r engine.Request) error {
	if g.Poll <= 0 {
		g.Poll = time.Second
	}
	if g.Timeout <= 0 {
		g.Timeout = 60 * time.Second
	}
	if g.MinUptime <= 0 {
		g.MinUptime = 10 * time.Second
	}
	deadline := time.Now().Add(g.Timeout)

	file, err := compose.FileFor(r.Project)
	if err != nil {
		return err
	}
	cid, err := containerID(ctx, file, r.Service)
	if err != nil {
		return err
	}
	if cid == "" {
		return fmt.Errorf("no container for service %q after up", r.Service)
	}

	var firstRunning time.Time
	for {
		status, hstatus, err := inspect(ctx, cid)
		if err != nil {
			return err
		}
		switch hstatus {
		case "healthy":
			return nil
		case "unhealthy":
			return fmt.Errorf("healthcheck reported unhealthy")
		case "starting":
			// image has a healthcheck; keep waiting for it
		default: // no healthcheck -> "stayed running" rung
			switch status {
			case "running":
				if firstRunning.IsZero() {
					firstRunning = time.Now()
				}
				if time.Since(firstRunning) >= g.MinUptime {
					return nil
				}
			case "restarting":
				return fmt.Errorf("container is restarting (crash loop)")
			case "exited", "dead":
				return fmt.Errorf("container %s", status)
			default: // created/paused/etc — not up yet
				firstRunning = time.Time{}
			}
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("not healthy within %s (status=%s health=%s)", g.Timeout, status, hstatus)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(g.Poll):
		}
	}
}

func containerID(ctx context.Context, file, service string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", "compose", "-f", file, "ps", "-q", service).Output()
	if err != nil {
		return "", fmt.Errorf("compose ps: %w", err)
	}
	return strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0]), nil
}

// inspect returns (State.Status, health status or "" when the image has no healthcheck).
func inspect(ctx context.Context, cid string) (status, health string, err error) {
	const f = `{{.State.Status}}|{{if .State.Health}}{{.State.Health.Status}}{{end}}`
	out, err := exec.CommandContext(ctx, "docker", "inspect", "-f", f, cid).Output()
	if err != nil {
		return "", "", fmt.Errorf("inspect: %w", err)
	}
	parts := strings.SplitN(strings.TrimSpace(string(out)), "|", 2)
	if len(parts) == 2 {
		return parts[0], parts[1], nil
	}
	return parts[0], "", nil
}
