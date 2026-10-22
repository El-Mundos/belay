package agent

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/El-Mundos/belay/internal/engine"
)

// Runs Prune against the real Docker daemon: a shared-ID image with two tags, kept while a stopped
// container uses it and while a foreign tag sits on it, then removed with both tags.
func TestPrune_LiveDocker(t *testing.T) {
	if os.Getenv("BELAY_PRUNE_LIVE") == "" {
		t.Skip("set BELAY_PRUNE_LIVE=1 to run against the local Docker daemon (pulls busybox)")
	}
	ctx := context.Background()
	const src, a, b, foreign = "busybox:1.36.1", "belay-prune-test:a", "belay-prune-test:b", "belay-prune-other:x"
	must := func(args ...string) {
		t.Helper()
		if err := run(ctx, "docker", args...); err != nil {
			t.Fatal(err)
		}
	}
	must("pull", "-q", src)
	must("tag", src, a)
	must("tag", src, b)
	must("image", "rm", src) // leave only the test repository's tags on the ID
	id := ImageID(ctx, a)
	t.Cleanup(func() {
		_ = run(ctx, "docker", "rm", "-f", "belay-prune-test")
		_ = run(ctx, "docker", "image", "rm", a, b, foreign)
	})
	repo := RepoOf(a)

	must("create", "--name", "belay-prune-test", a, "true")
	if r := Prune(ctx, id, repo, nil); !r.Retry || !strings.Contains(r.Kept, "belay-prune-test") {
		t.Fatalf("stopped container: got %+v, want kept for retry naming the container", r)
	}
	must("rm", "belay-prune-test")

	if r := Prune(ctx, id, repo, map[string]string{id: "rollback"}); r.Kept != "rollback" {
		t.Fatalf("keep set: got %+v", r)
	}

	must("tag", a, foreign)
	if r := Prune(ctx, id, repo, nil); r.Kept == "" || r.Retry {
		t.Fatalf("foreign tag: got %+v, want kept for good", r)
	}
	must("image", "rm", foreign)

	r := Prune(ctx, id, repo, nil)
	if r.Kept != "" || len(r.Removed) != 2 {
		t.Fatalf("removal: got %+v, want both tags removed", r)
	}
	if ImageID(ctx, id) != "" {
		t.Fatal("image still present after Prune")
	}
	if again := Prune(ctx, id, repo, nil); !again.Gone {
		t.Fatalf("second prune: got %+v, want Gone", again)
	}
	free, err := FreeBytes(ctx)
	if err != nil || free <= 0 {
		t.Fatalf("FreeBytes = %d, %v", free, err)
	}
	err = checkSpace(ctx, engine.Request{ToImage: "huge:1", PullBytes: free})
	if err == nil || !strings.Contains(err.Error(), "not enough disk space") {
		t.Fatalf("checkSpace with a pull 7x the free space = %v, want a refusal", err)
	}
	t.Log(err)
	t.Logf("removed %v, freed %s, image store has %s free", r.Removed, HumanBytes(r.Bytes), HumanBytes(free))
}
