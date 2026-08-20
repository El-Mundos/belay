// Package agent implements engine.Deployer against `docker compose` on the local host.
package agent

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/El-Mundos/belay/internal/compose"
	"github.com/El-Mundos/belay/internal/engine"
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
//
// The pull policy is "missing" normally — the tag was just rewritten, so the image is either present
// or fetched once. A rebase keeps the same tag, so nothing looks missing and Docker would happily
// reuse the stale local image; those runs must pull unconditionally or the update is a no-op.
func (l Local) Up(ctx context.Context, r engine.Request) error {
	f, err := l.file(r)
	if err != nil {
		return err
	}
	pull := "missing"
	if r.Rebase {
		pull = "always"
	}
	return run(ctx, "docker", "compose", "-f", f, "up", "-d", "--pull", pull, r.Service)
}

// ImageDigest returns the repository digest of the image currently present locally for a reference,
// i.e. what was actually pulled. Empty (with no error) when the image has no repo digest — a locally
// built image that was never pulled from a registry, which simply cannot be digest-tracked.
func ImageDigest(ctx context.Context, image string) (string, error) {
	out, err := output(ctx, "docker", "image", "inspect", image, "--format", "{{join .RepoDigests \"\\n\"}}")
	if err != nil {
		return "", fmt.Errorf("inspect %s: %w: %s", image, err, strings.TrimSpace(out))
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if _, dig, ok := strings.Cut(strings.TrimSpace(line), "@"); ok {
			return dig, nil
		}
	}
	return "", nil
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
