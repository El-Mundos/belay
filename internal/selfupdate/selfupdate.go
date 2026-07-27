// Package selfupdate lets Belay update its own container (Watchtower-style): it detects the belay
// container it runs in, notices when the image tag it was started from now resolves to a newer image
// (e.g. after a `docker load` or registry pull), and recreates itself from its own inspected config.
//
// A container can't cleanly replace itself, so Apply launches a short-lived detached helper container
// (the same belay image, run as `sh`) that outlives belay: it pulls, removes the old container, and
// re-runs it with identical env/mounts/ports/networks but the new image.
package selfupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Manager performs self-update for the belay container it is running inside.
type Manager struct {
	container  string // own container name (no leading slash)
	image      string // own image ref, e.g. "belay:latest"
	network    string // first docker network (helper joins it to reach the socket-proxy)
	dockerHost string // DOCKER_HOST from own env ("" => use the mounted /var/run/docker.sock)
}

// inspectDoc is the subset of `docker inspect` we need to faithfully recreate the container.
type inspectDoc struct {
	Name   string
	Image  string
	Config struct {
		Env   []string
		Cmd   []string
		Image string
	}
	HostConfig struct {
		RestartPolicy struct {
			Name              string
			MaximumRetryCount int
		}
		PortBindings map[string][]struct{ HostIp, HostPort string }
	}
	Mounts []struct {
		Type, Name, Source, Destination string
		RW                              bool
	}
	NetworkSettings struct {
		Networks map[string]struct{ Aliases []string }
	}
}

// Detect finds the belay container we're running in (its hostname is the container id by default).
// It returns a disabled (nil) manager without error when we can't self-identify (e.g. not in Docker).
func Detect(ctx context.Context) *Manager {
	host, err := os.Hostname()
	if err != nil || host == "" {
		return nil
	}
	raw, err := exec.CommandContext(ctx, "docker", "inspect", host).Output()
	if err != nil {
		return nil
	}
	var docs []inspectDoc
	if json.Unmarshal(raw, &docs) != nil || len(docs) == 0 {
		return nil
	}
	d := docs[0]
	m := &Manager{
		container: strings.TrimPrefix(d.Name, "/"),
		image:     d.Config.Image,
	}
	for name := range d.NetworkSettings.Networks {
		m.network = name
		break
	}
	for _, e := range d.Config.Env {
		if strings.HasPrefix(e, "DOCKER_HOST=") {
			m.dockerHost = strings.TrimPrefix(e, "DOCKER_HOST=")
		}
	}
	return m
}

func (m *Manager) Enabled() bool { return m != nil && m.container != "" }
func (m *Manager) Image() string { return m.image }

// Available reports whether the image tag now resolves to a different image than the running one.
// For registry images it first attempts a pull; a failed pull (e.g. a local-only tag) is ignored.
func (m *Manager) Available(ctx context.Context) (bool, error) {
	if !m.Enabled() {
		return false, nil
	}
	_ = exec.CommandContext(ctx, "docker", "pull", m.image).Run() // best-effort; local-only tags fail
	tagID, err := dockerOut(ctx, "image", "inspect", m.image, "--format", "{{.Id}}")
	if err != nil {
		return false, err
	}
	runningID, err := dockerOut(ctx, "inspect", m.container, "--format", "{{.Image}}")
	if err != nil {
		return false, err
	}
	return tagID != "" && tagID != runningID, nil
}

// Apply recreates belay from a fresh inspection, using the current image tag. It returns once the
// detached helper has been launched — belay itself is torn down and replaced moments later.
func (m *Manager) Apply(ctx context.Context) error {
	if !m.Enabled() {
		return fmt.Errorf("self-update not available (not running in a detectable container)")
	}
	raw, err := exec.CommandContext(ctx, "docker", "inspect", m.container).Output()
	if err != nil {
		return err
	}
	var docs []inspectDoc
	if err := json.Unmarshal(raw, &docs); err != nil || len(docs) == 0 {
		return fmt.Errorf("inspect self: %v", err)
	}
	script := recreateScript(docs[0])

	args := []string{"run", "-d", "--rm"}
	if m.network != "" {
		args = append(args, "--network", m.network)
	}
	if m.dockerHost != "" {
		args = append(args, "-e", "DOCKER_HOST="+m.dockerHost)
	} else {
		args = append(args, "-v", "/var/run/docker.sock:/var/run/docker.sock")
	}
	// same belay image, run as a shell; sleep briefly so belay can answer the HTTP request first
	args = append(args, "--entrypoint", "sh", m.image, "-c", "sleep 3; "+script)
	return exec.CommandContext(ctx, "docker", args...).Run()
}

// recreateScript builds the shell that removes the old container and re-runs it from d with the
// current image tag. Exported so it can be unit-tested without Docker.
func recreateScript(d inspectDoc) string {
	name := strings.TrimPrefix(d.Name, "/")
	image := d.Config.Image

	run := []string{"docker", "run", "-d", "--name", name}
	if p := d.HostConfig.RestartPolicy.Name; p != "" && p != "no" {
		run = append(run, "--restart", p)
	}
	for _, e := range d.Config.Env {
		run = append(run, "-e", e)
	}
	for _, mnt := range d.Mounts {
		src := mnt.Source
		if mnt.Type == "volume" {
			src = mnt.Name
		}
		spec := src + ":" + mnt.Destination
		if !mnt.RW {
			spec += ":ro"
		}
		run = append(run, "-v", spec)
	}
	for cp, binds := range d.HostConfig.PortBindings {
		for _, b := range binds {
			hostpart := b.HostPort
			if b.HostIp != "" {
				hostpart = b.HostIp + ":" + b.HostPort
			}
			run = append(run, "-p", hostpart+":"+cp)
		}
	}
	// first network on `run`, the rest connected afterwards
	var first string
	var rest []string
	for n := range d.NetworkSettings.Networks {
		if first == "" {
			first = n
		} else {
			rest = append(rest, n)
		}
	}
	if first != "" {
		run = append(run, "--network", first)
	}
	run = append(run, image)
	run = append(run, d.Config.Cmd...)

	var b strings.Builder
	b.WriteString("set -e\n")
	b.WriteString("docker pull " + shq(image) + " 2>/dev/null || true\n")
	b.WriteString("docker rm -f " + shq(name) + " 2>/dev/null || true\n")
	b.WriteString(shellJoin(run) + "\n")
	for _, n := range rest {
		b.WriteString("docker network connect " + shq(n) + " " + shq(name) + " || true\n")
	}
	return b.String()
}

func dockerOut(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", args...).Output()
	return strings.TrimSpace(string(out)), err
}

func shellJoin(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = shq(a)
	}
	return strings.Join(q, " ")
}

// shq quotes a shell argument only when it contains anything outside a safe set — keeps the common
// `docker run -d --name belay …` readable while still quoting values with spaces/specials.
func shq(s string) string {
	if s != "" && strings.IndexFunc(s, unsafeShell) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func unsafeShell(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return false
	case strings.ContainsRune("_@%+=:,./-", r):
		return false
	}
	return true
}
