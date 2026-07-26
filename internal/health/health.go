// Package health implements the engine.HealthGate ladder for a freshly-deployed compose service:
//
//  1. Docker HEALTHCHECK, if the image defines one (authoritative)
//  2. otherwise, if the service has a configured probe: poll that HTTP/TCP endpoint until it passes
//  3. otherwise: the container stayed running (not exited/crash-looping) for MinUptime
//
// The probe (rung 2) is what catches "runs but broken" for images without a docker healthcheck.
package health

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/El-Mundos/belay/internal/compose"
	"github.com/El-Mundos/belay/internal/engine"
)

// Probe is an optional HTTP/TCP liveness check reachable from the Belay process.
type Probe struct {
	Type   string // http | tcp
	Target string // http URL, or host:port for tcp
	Expect int    // expected HTTP status (0 => any non-4xx/5xx)
}

type Gate struct {
	Timeout   time.Duration // overall deadline
	MinUptime time.Duration // "stayed running" window when the image has no healthcheck/probe
	Poll      time.Duration // poll interval
	// ProbeFor returns the configured probe for a service (rung 2), if any.
	ProbeFor func(r engine.Request) (Probe, bool)
}

var probeClient = &http.Client{
	Timeout: 5 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse // a redirect still means it's serving
	},
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

	var probe Probe
	if g.ProbeFor != nil {
		if p, ok := g.ProbeFor(r); ok {
			probe = p
		}
	}

	var firstRunning time.Time
	var lastProbeErr error
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
		default: // no docker healthcheck
			switch status {
			case "running":
				if probe.Type != "" { // rung 2: poll the configured probe
					if err := checkProbe(ctx, probe); err == nil {
						return nil
					} else {
						lastProbeErr = err
					}
				} else { // rung 3: stayed running
					if firstRunning.IsZero() {
						firstRunning = time.Now()
					}
					if time.Since(firstRunning) >= g.MinUptime {
						return nil
					}
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
			if probe.Type != "" && lastProbeErr != nil {
				return fmt.Errorf("probe never passed within %s: %v", g.Timeout, lastProbeErr)
			}
			return fmt.Errorf("not healthy within %s (status=%s health=%s)", g.Timeout, status, hstatus)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(g.Poll):
		}
	}
}

// checkProbe returns nil if the configured probe currently passes.
func checkProbe(ctx context.Context, p Probe) error {
	switch p.Type {
	case "http":
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.Target, nil)
		if err != nil {
			return err
		}
		resp, err := probeClient.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if p.Expect != 0 {
			if resp.StatusCode != p.Expect {
				return fmt.Errorf("got HTTP %d, want %d", resp.StatusCode, p.Expect)
			}
			return nil
		}
		if resp.StatusCode >= 400 {
			return fmt.Errorf("got HTTP %d", resp.StatusCode)
		}
		return nil
	case "tcp":
		d := net.Dialer{Timeout: 3 * time.Second}
		c, err := d.DialContext(ctx, "tcp", p.Target)
		if err != nil {
			return err
		}
		c.Close()
		return nil
	}
	return nil
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
