package server

import (
	"testing"

	"github.com/El-Mundos/belay/internal/selfupdate"
)

// The 2026-08-20 outage in miniature: Belay updated the socket-proxy it reaches Docker through,
// stopped it, and then could not finish or revert because the connection it was travelling over was
// the thing it had just stopped.
func TestProtectedReason(t *testing.T) {
	s := &Server{su: selfupdate.NewForTest("belay", "ghcr.io/el-mundos/belay:0.2.7", "tcp://belay-sockproxy:2375")}

	for _, tc := range []struct {
		name, service, image string
		protected            bool
	}{
		{"the docker transport", "belay-sockproxy", "tecnativa/docker-socket-proxy:latest", true},
		{"belay's own container", "belay", "ghcr.io/el-mundos/belay:0.2.7", true},
		{"belay by image under another service name", "belay-renamed", "ghcr.io/el-mundos/belay:0.2.7", true},
		{"an ordinary service", "grafana", "grafana/grafana-oss:13.1.0", false},
		// A service that merely looks similar must still be updatable.
		{"similarly named but not the transport", "belay-sockproxy-backup", "tecnativa/docker-socket-proxy:latest", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			why := s.protectedReason(tc.service, tc.image)
			if (why != "") != tc.protected {
				t.Errorf("protectedReason(%q, %q) = %q; want protected=%v", tc.service, tc.image, why, tc.protected)
			}
		})
	}
}

// With no DOCKER_HOST (the mounted-socket deployment) there is no transport container to protect,
// and nothing should be excluded on that basis.
func TestProtectedReason_MountedSocket(t *testing.T) {
	s := &Server{su: selfupdate.NewForTest("belay", "belay:latest", "")}
	if why := s.protectedReason("some-proxy", "tecnativa/docker-socket-proxy:latest"); why != "" {
		t.Errorf("nothing should be transport-protected without DOCKER_HOST, got %q", why)
	}
	if why := s.protectedReason("belay", "belay:latest"); why == "" {
		t.Error("belay's own container must still be protected")
	}
}

// Belay may not be running in a container at all (a bare `belay server`); the guard must be inert
// rather than panicking on a nil manager.
func TestProtectedReason_NotContainerised(t *testing.T) {
	s := &Server{su: nil}
	if why := s.protectedReason("anything", "nginx:1.27.1"); why != "" {
		t.Errorf("want no protection when not containerised, got %q", why)
	}
}
