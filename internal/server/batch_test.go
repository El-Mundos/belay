package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/El-Mundos/belay/internal/config"
)

func testServer(t *testing.T, groups []config.ServiceGroup) *Server {
	t.Helper()
	set, _ := config.Open("") // in-memory
	if len(groups) > 0 {
		if err := set.Update(func(s *config.Settings) { s.Groups = groups }); err != nil {
			t.Fatal(err)
		}
	}
	return &Server{set: set}
}

// Yesterday's incident in miniature: authentik's server and worker share a compose file, and
// updating the worker after the server had just failed is what produced the version split.
func TestBatches_SameFileGroupsTogether(t *testing.T) {
	s := testServer(t, nil)
	ups := []pendingUpd{
		{Host: "local", Project: "authentik", File: "/srv/authentik/dc.yml", Service: "server"},
		{Host: "local", Project: "authentik", File: "/srv/authentik/dc.yml", Service: "worker"},
		{Host: "local", Project: "monitoring", File: "/srv/monitoring/dc.yml", Service: "prometheus"},
	}

	got := s.batches(ups)
	if len(got) != 2 {
		t.Fatalf("got %d batches, want 2 (one per compose file)", len(got))
	}
	if len(got[0].ups) != 2 || got[0].lockstep {
		t.Errorf("authentik batch = %d services, lockstep=%v; want 2 services and lockstep=false",
			len(got[0].ups), got[0].lockstep)
	}
	if len(got[1].ups) != 1 {
		t.Errorf("monitoring batch = %d services, want 1", len(got[1].ups))
	}
}

// The same compose file on two different hosts must not share a batch — a failure on the VPS says
// nothing about whether the identical stack on galaxy can be updated.
func TestBatches_SplitsByHost(t *testing.T) {
	s := testServer(t, nil)
	ups := []pendingUpd{
		{Host: "local", Project: "traefik", File: "/srv/traefik/dc.yml", Service: "traefik"},
		{Host: "vps", Project: "traefik", File: "/srv/traefik/dc.yml", Service: "traefik"},
	}
	if got := s.batches(ups); len(got) != 2 {
		t.Fatalf("got %d batches, want 2 (one per host)", len(got))
	}
}

// An explicit group is lockstep and outranks the compose-file grouping, including across files.
func TestBatches_ExplicitGroupIsLockstepAndSpansFiles(t *testing.T) {
	s := testServer(t, []config.ServiceGroup{{
		Name: "authentik",
		Members: []string{
			config.Key("/srv/authentik/dc.yml", "server"),
			config.Key("/srv/extra/dc.yml", "outpost"),
		},
	}})
	ups := []pendingUpd{
		{Host: "local", Project: "authentik", File: "/srv/authentik/dc.yml", Service: "server"},
		{Host: "local", Project: "extra", File: "/srv/extra/dc.yml", Service: "outpost"},
		{Host: "local", Project: "extra", File: "/srv/extra/dc.yml", Service: "unrelated"},
	}

	got := s.batches(ups)
	if len(got) != 2 {
		t.Fatalf("got %d batches, want 2 (the group, plus the ungrouped service)", len(got))
	}
	grp := got[0]
	if !grp.lockstep || grp.key != "authentik" {
		t.Errorf("first batch lockstep=%v key=%q; want true / %q", grp.lockstep, grp.key, "authentik")
	}
	if len(grp.ups) != 2 {
		t.Fatalf("group batch = %d services, want 2 spanning both files", len(grp.ups))
	}
	if got[1].lockstep || len(got[1].ups) != 1 {
		t.Errorf("ungrouped remainder = %d services lockstep=%v; want 1 / false", len(got[1].ups), got[1].lockstep)
	}
}

func writeCompose(t *testing.T, body string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "docker-compose.yml")
	if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

// A rolled-back service that shares a stack with a database gets the "state may still be changed"
// caveat, because belay's snapshot never covered that sibling's volume.
func TestRollbackCaveat_FlagsSiblingDatabase(t *testing.T) {
	f := writeCompose(t, `services:
  server:
    image: ghcr.io/goauthentik/server:2026.5.6
  worker:
    image: ghcr.io/goauthentik/server:2026.5.6
  postgresql:
    image: docker.io/library/postgres:16-alpine
`)
	got := rollbackCaveat(f, "server")
	if got == "" {
		t.Fatal("want a caveat when a sibling datastore exists, got none")
	}
	if !strings.Contains(got, "postgresql") {
		t.Errorf("caveat should name the datastore service, got: %q", got)
	}
}

func TestRollbackCaveat_QuietWithoutDatastore(t *testing.T) {
	f := writeCompose(t, `services:
  traefik:
    image: traefik:v3.7.9
  whoami:
    image: traefik/whoami:latest
`)
	if got := rollbackCaveat(f, "traefik"); got != "" {
		t.Errorf("want no caveat for a stateless stack, got: %q", got)
	}
}

// The service being rolled back must not flag itself — updating postgres alone is exactly the case
// belay's own volume snapshot does cover.
func TestRollbackCaveat_IgnoresSelf(t *testing.T) {
	f := writeCompose(t, `services:
  postgresql:
    image: postgres:16-alpine
`)
	if got := rollbackCaveat(f, "postgresql"); got != "" {
		t.Errorf("want no caveat when the datastore is the updated service itself, got: %q", got)
	}
}
