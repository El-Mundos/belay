// Package agent implements engine.Deployer against `docker compose` on the local host.
package agent

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
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

// Preflight reports whether Compose can fully render this project, without touching anything.
//
// `docker compose config` runs the SAME interpolation the real deployment would, as a dry run. Two
// failures matter, and the second is the dangerous one:
//
//   - a guarded variable is missing -> compose errors, and the deployment would fail loudly;
//   - an UNGUARDED variable is missing -> compose warns and substitutes a blank string, so
//     "${A}.${B}" becomes "." and the container starts perfectly healthy with a useless value.
//
// The second is how a credential supplied by a deploy wrapper disappears when anything else
// recreates the container: nothing errors, nothing looks wrong, and the damage surfaces weeks later.
// Refusing to deploy a project we cannot fully render is the only way to catch it in time.
func (l Local) Preflight(ctx context.Context, r engine.Request) error {
	f, err := l.file(r)
	if err != nil {
		return err
	}
	out, err := output(ctx, "docker", "compose", "-f", f, "config", "--quiet")
	if err != nil {
		if line := firstInterpolationError(out); line != "" {
			return fmt.Errorf("compose cannot render this project: %s", line)
		}
		return fmt.Errorf("compose cannot render this project: %s", strings.TrimSpace(lastLine(out)))
	}
	if blank := blankVars(out); len(blank) > 0 {
		return fmt.Errorf("compose would substitute a blank string for %s — this project expects "+
			"variables Belay does not have, and deploying would silently drop them. Supply them "+
			"(an .env beside the compose file) or pin this service",
			strings.Join(blank, ", "))
	}
	return nil
}

// blankVars extracts the variable names compose warned it would blank out.
func blankVars(out string) []string {
	// Compose emits its warnings as JSON-quoted log lines, so the variable name arrives wrapped in
	// escaped quotes. Unescape first and match plain quotes, rather than encoding that detail into
	// the pattern where it is easy to get subtly wrong.
	out = strings.ReplaceAll(out, `\"`, `"`)
	var names []string
	seen := map[string]bool{}
	for _, m := range blankVarRe.FindAllStringSubmatch(out, -1) {
		if n := m[1]; !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	return names
}

// e.g. level=warning msg="The "IONOS_API_KEY" variable is not set. Defaulting to a blank string."
var blankVarRe = regexp.MustCompile(`The "([A-Za-z_][A-Za-z0-9_]*)" variable is not set`)

func firstInterpolationError(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "error while interpolating") || strings.Contains(line, "required variable") {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

func lastLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return lines[len(lines)-1]
}

// HostName is the name of the MACHINE Belay manages, which is not the same as this process's
// hostname: inside a container os.Hostname() returns the container ID, so a containerised Belay
// would call itself "e450a426f5bb". The Docker daemon knows the real name, so ask it, and fall
// back to the process hostname when it can't be reached (no Docker, or a socket-proxy with INFO=0).
// BELAY_HOST_NAME overrides both.
func HostName(ctx context.Context) string {
	if n := strings.TrimSpace(os.Getenv("BELAY_HOST_NAME")); n != "" {
		return n
	}
	if out, err := output(ctx, "docker", "info", "--format", "{{.Name}}"); err == nil {
		if n := strings.TrimSpace(out); n != "" {
			return n
		}
	}
	if n, err := os.Hostname(); err == nil && n != "" {
		return n
	}
	return "local"
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
