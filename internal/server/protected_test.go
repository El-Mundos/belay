package server

import (
	"testing"

	"github.com/El-Mundos/belay/internal/cluster"
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

// --- remote hosts -----------------------------------------------------------------------------
//
// The server cannot work out which container is the agent on a machine it only reaches over the
// wire, so the agent reports the verdict and the server must honour it. Before this, an agent host
// had NO protection at all: update-all could recreate the very container executing the update, on
// the host least convenient to go and fix by hand.

func agentServer(t *testing.T) *Server {
	t.Helper()
	return &Server{
		su: selfupdate.NewForTest("belay", "belay:latest", ""),
		agents: map[string]*agentConn{
			"vps": {host: "vps", version: "0.2.9", projects: []cluster.Project{{
				Name: "belay", File: "/srv/belay/docker-compose.yml",
				Services: []cluster.Service{
					{Name: "belay-agent", Image: "ghcr.io/el-mundos/belay:0.2.9", Protected: "Belay itself — use self-update"},
					{Name: "belay-sockproxy", Image: "tecnativa/docker-socket-proxy:latest", Protected: "Belay reaches Docker through this service"},
					{Name: "wg-easy", Image: "ghcr.io/wg-easy/wg-easy:15"},
				},
			}}},
		},
	}
}

func TestRemoteProtected_HonoursTheAgentsVerdict(t *testing.T) {
	s := agentServer(t)
	file := "/srv/belay/docker-compose.yml"
	if why := s.remoteProtected("vps", file, "belay-agent"); why == "" {
		t.Error("the agent's own container must be protected on the agent host")
	}
	if why := s.remoteProtected("vps", file, "belay-sockproxy"); why == "" {
		t.Error("the agent's docker transport must be protected")
	}
	if why := s.remoteProtected("vps", file, "wg-easy"); why != "" {
		t.Errorf("an ordinary remote service must stay updatable, got %q", why)
	}
}

func TestRemoteProtected_UnknownHostOrService(t *testing.T) {
	s := agentServer(t)
	if why := s.remoteProtected("nowhere", "/srv/x.yml", "belay-agent"); why != "" {
		t.Errorf("unknown host should report nothing, got %q", why)
	}
	if why := s.remoteProtected("vps", "/srv/other.yml", "belay-agent"); why != "" {
		t.Errorf("a same-named service in another stack is not the agent, got %q", why)
	}
}

// A local service named like the agent's container is a DIFFERENT machine's container, and vice
// versa: protection must not leak across hosts in either direction.
func TestProtectionFor_DoesNotLeakAcrossHosts(t *testing.T) {
	s := agentServer(t) // local Belay is the container "belay"
	file := "/srv/belay/docker-compose.yml"
	if why := s.protectionFor(false, "vps", file, "belay", "belay:latest"); why != "" {
		t.Errorf("the LOCAL container name must not protect a remote service, got %q", why)
	}
	if why := s.protectionFor(true, "", file, "belay-agent", "ghcr.io/el-mundos/belay:0.2.9"); why != "" {
		t.Errorf("a REMOTE agent's name must not protect a local service, got %q", why)
	}
	if why := s.protectionFor(true, "", file, "belay", "belay:latest"); why == "" {
		t.Error("the local Belay is still protected locally")
	}
}

// An agent too old to report protection sends nothing. The server cannot invent the verdict, so
// the honest behaviour is to allow — and to make the skew visible on the Hosts tab, which is why
// the version field exists.
func TestRemoteProtected_PreFieldAgentReportsNothing(t *testing.T) {
	s := &Server{agents: map[string]*agentConn{
		"old": {host: "old", version: "", projects: []cluster.Project{{
			File:     "/srv/belay/docker-compose.yml",
			Services: []cluster.Service{{Name: "belay-agent", Image: "belay:0.2.6"}},
		}}},
	}}
	if why := s.remoteProtected("old", "/srv/belay/docker-compose.yml", "belay-agent"); why != "" {
		t.Errorf("a pre-field agent cannot report protection, got %q", why)
	}
}
