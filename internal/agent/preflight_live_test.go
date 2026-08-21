package agent

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/El-Mundos/belay/internal/engine"
)

// Exercises the real `docker compose config` against a project shaped exactly like traefik-edge:
// unguarded variables supplied by a deploy wrapper Belay does not run.
func TestPreflight_LiveCompose(t *testing.T) {
	dir := os.Getenv("BELAY_PREFLIGHT_DIR")
	if dir == "" {
		t.Skip("set BELAY_PREFLIGHT_DIR to run against a real docker compose")
	}
	err := Local{}.Preflight(context.Background(),
		engine.Request{Project: dir + "/docker-compose.yml", Service: "proxy"})
	if os.Getenv("BELAY_PREFLIGHT_WANT") == "ok" {
		if err != nil {
			t.Fatalf("expected a clean render, got: %v", err)
		}
		return
	}
	if err == nil {
		t.Fatal("expected preflight to REFUSE a project with unset variables")
	}
	if !strings.Contains(err.Error(), "IONOS_API_PREFIX") {
		t.Errorf("the refusal should name the missing variable, got: %v", err)
	}
	t.Logf("refused as intended: %v", err)
}
