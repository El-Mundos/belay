package agent

import (
	"strings"
	"testing"

	"github.com/El-Mundos/belay/internal/engine"
	"github.com/El-Mundos/belay/internal/journal"
)

func authentikEntry() journal.Entry {
	return journal.Entry{File: "/srv/infra/authentik/docker-compose.yml", Service: "server",
		From: "ghcr.io/goauthentik/server:2026.8.4", To: "ghcr.io/goauthentik/server:2026.8.5",
		FromImageID: "sha256:old"}
}

// galaxy, 2026-10-22: the tag was rewritten, the container never recreated.
func TestRecoveryPutsBackTagNeverDeployed(t *testing.T) {
	e := authentikEntry()
	r := decideRecovery(e, "sha256:old", true, "sha256:new", e.To)
	if !r.PutBack || r.Outcome != engine.OutcomeRolledBack {
		t.Fatalf("got %+v; want the tag put back and the attempt shown as rolled back", r)
	}
}

func TestRecoveryNothingChanged(t *testing.T) {
	e := authentikEntry()
	if r := decideRecovery(e, "sha256:old", true, "", e.From); r.PutBack || r.Outcome != engine.OutcomeRolledBack {
		t.Fatalf("killed before the tag moved: got %+v", r)
	}
	e.Rebase, e.To = true, e.From // a rebase never rewrites the tag
	if r := decideRecovery(e, "sha256:old", true, "sha256:new", e.From); r.PutBack {
		t.Fatalf("rebase must not rewrite the compose file: %+v", r)
	}
}

func TestRecoveryNewVersionUnverified(t *testing.T) {
	e := authentikEntry()
	r := decideRecovery(e, "sha256:new", true, "sha256:new", e.To)
	if r.PutBack || r.Outcome != engine.OutcomeError || !strings.Contains(r.Note, "never finished") {
		t.Fatalf("new version up without a health check: got %+v", r)
	}
}

func TestRecoveryLeavesOddStatesAlone(t *testing.T) {
	e := authentikEntry()
	for name, c := range map[string]struct {
		cur     string
		running bool
	}{
		"no container":      {"", false},
		"stopped (restore)": {"sha256:old", false},
		"some other image":  {"sha256:else", true},
	} {
		if r := decideRecovery(e, c.cur, c.running, "sha256:new", e.To); r.PutBack || r.Outcome != engine.OutcomeError {
			t.Errorf("%s: got %+v; want an error and nothing touched", name, r)
		}
	}
}
