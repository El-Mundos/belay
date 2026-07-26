package agent

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/El-Mundos/belay/internal/compose"
	"github.com/El-Mundos/belay/internal/engine"
)

const (
	snapVolume  = "belay-snapshots" // docker volume where snapshot tarballs live
	helperImage = "alpine:3.20"     // tiny helper for tar in/out of volumes
)

// Snapshotter snapshots and restores a service's volumes (named volumes + bind mounts) via helper
// containers, so a rollback restores data — not just the image tag. Implements engine.Snapshotter.
type Snapshotter struct{}

// mounts returns the snapshot-able mount sources (volume name or bind host-path) of the service's
// container, in a stable order. `all` includes a stopped/failed container.
func (Snapshotter) mounts(ctx context.Context, file, service string, all bool) ([]string, error) {
	ps := []string{"compose", "-f", file, "ps", "-q", service}
	if all {
		ps = []string{"compose", "-f", file, "ps", "-aq", service}
	}
	out, err := exec.CommandContext(ctx, "docker", ps...).Output()
	if err != nil {
		return nil, err
	}
	cid := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if cid == "" {
		return nil, nil
	}
	const f = `{{range .Mounts}}{{.Type}}|{{if eq .Type "volume"}}{{.Name}}{{else}}{{.Source}}{{end}}` + "\n" + `{{end}}`
	mo, err := exec.CommandContext(ctx, "docker", "inspect", "-f", f, cid).Output()
	if err != nil {
		return nil, err
	}
	var srcs []string
	for _, line := range strings.Split(strings.TrimSpace(string(mo)), "\n") {
		p := strings.SplitN(line, "|", 2)
		if len(p) != 2 || p[1] == "" {
			continue
		}
		typ, src := p[0], p[1]
		if typ != "volume" && typ != "bind" {
			continue // tmpfs etc.
		}
		if strings.HasPrefix(src, "/var/run") || strings.HasPrefix(src, "/run") {
			continue // don't snapshot sockets
		}
		srcs = append(srcs, src)
	}
	return srcs, nil
}

func (s Snapshotter) Snapshot(ctx context.Context, r engine.Request) (string, error) {
	file, err := compose.FileFor(r.Project)
	if err != nil {
		return "", err
	}
	srcs, err := s.mounts(ctx, file, r.Service, false)
	if err != nil || len(srcs) == 0 {
		return "", err
	}
	id := fmt.Sprintf("%s-%d", sanitize(r.Service), time.Now().UnixNano())
	for i, src := range srcs {
		script := fmt.Sprintf("mkdir -p /snap/%s && tar czf /snap/%s/%d.tgz -C /v . 2>/dev/null || true", id, id, i)
		if err := run(ctx, "docker", "run", "--rm",
			"-v", src+":/v:ro", "-v", snapVolume+":/snap", helperImage, "sh", "-c", script); err != nil {
			return "", err
		}
	}
	return id, nil
}

func (s Snapshotter) Restore(ctx context.Context, r engine.Request, snap string) error {
	file, err := compose.FileFor(r.Project)
	if err != nil {
		return err
	}
	_ = run(ctx, "docker", "compose", "-f", file, "stop", r.Service) // free the volumes first
	srcs, err := s.mounts(ctx, file, r.Service, true)
	if err != nil {
		return err
	}
	for i, src := range srcs {
		script := fmt.Sprintf("[ -f /snap/%s/%d.tgz ] && { find /v -mindepth 1 -delete 2>/dev/null; tar xzf /snap/%s/%d.tgz -C /v; } || true", snap, i, snap, i)
		_ = run(ctx, "docker", "run", "--rm",
			"-v", src+":/v", "-v", snapVolume+":/snap", helperImage, "sh", "-c", script)
	}
	return nil
}

func (Snapshotter) Discard(ctx context.Context, r engine.Request, snap string) {
	_ = run(ctx, "docker", "run", "--rm", "-v", snapVolume+":/snap", helperImage, "rm", "-rf", "/snap/"+snap)
}

func sanitize(s string) string {
	return strings.Map(func(c rune) rune {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
			return c
		default:
			return '-'
		}
	}, s)
}
