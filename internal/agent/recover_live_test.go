package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/El-Mundos/belay/internal/compose"
	"github.com/El-Mundos/belay/internal/engine"
	"github.com/El-Mundos/belay/internal/journal"
)

// Replays galaxy's authentik-server against real Docker: compose tag rewritten, container not
// recreated, process killed. Recover must put the tag back.
func TestRecover_LiveInterruptedUpdate(t *testing.T) {
	if os.Getenv("BELAY_PRUNE_LIVE") == "" {
		t.Skip("set BELAY_PRUNE_LIVE=1 to run against the local Docker daemon (pulls busybox)")
	}
	ctx := context.Background()
	dir := t.TempDir()
	file := filepath.Join(dir, "compose.yaml")
	const from, to = "busybox:1.36.1", "busybox:1.37.0"
	if err := os.WriteFile(file, []byte("name: belay-recover-test\nservices:\n  app:\n    image: "+from+"\n    command: [sleep, \"300\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run(ctx, "docker", "compose", "-f", file, "up", "-d", "app"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run(ctx, "docker", "compose", "-f", file, "down", "-t", "0") })

	e := journal.Entry{File: file, Service: "app", From: from, To: to, FromImageID: ServiceImageID(ctx, file, "app")}
	if err := compose.SetImage(file, "app", to); err != nil { // the step that ran before the kill
		t.Fatal(err)
	}
	r, ok := Recover(ctx, e)
	if !ok || !r.PutBack || r.Outcome != engine.OutcomeRolledBack {
		t.Fatalf("Recover = %+v, %v", r, ok)
	}
	if got, _ := compose.FindImage(file, "app"); got != from {
		t.Fatalf("compose file says %q after recovery, want %q", got, from)
	}
	t.Log(r.Note)
}
