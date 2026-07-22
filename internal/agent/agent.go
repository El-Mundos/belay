// Package agent implements engine.Deployer against `docker compose` on the local host.
package agent

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"

	"github.com/belay-sh/belay/internal/compose"
	"github.com/belay-sh/belay/internal/engine"
)

// Local drives docker compose on the machine it runs on.
// engine.Request.Project is a compose file path or a directory containing one.
type Local struct{}

func (Local) file(r engine.Request) (string, error) { return compose.FileFor(r.Project) }

// SetImage rewrites the service's image tag in the compose file (formatting-preserving).
func (l Local) SetImage(_ context.Context, r engine.Request, image string) error {
	f, err := l.file(r)
	if err != nil {
		return err
	}
	return compose.SetImage(f, r.Service, image)
}

// Up recreates the single service with its current compose definition.
func (l Local) Up(ctx context.Context, r engine.Request) error {
	f, err := l.file(r)
	if err != nil {
		return err
	}
	return run(ctx, "docker", "compose", "-f", f, "up", "-d", "--pull", "missing", r.Service)
}

// Logs returns the last `tail` lines of the service's container logs.
func (l Local) Logs(ctx context.Context, r engine.Request, tail int) (string, error) {
	f, err := l.file(r)
	if err != nil {
		return "", err
	}
	out, _ := output(ctx, "docker", "compose", "-f", f, "logs",
		"--no-color", "--no-log-prefix", "--tail", strconv.Itoa(tail), r.Service)
	return out, nil
}

func run(ctx context.Context, name string, args ...string) error {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %v: %w: %s", name, args, err, stderr.String())
	}
	return nil
}

func output(ctx context.Context, name string, args ...string) (string, error) {
	var buf bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	return buf.String(), err
}
