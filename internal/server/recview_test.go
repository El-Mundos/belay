package server

import (
	"strings"
	"testing"

	"github.com/El-Mundos/belay/internal/store"
)

func TestNewRecView_CollapsesTheSharedImageName(t *testing.T) {
	v := newRecView(store.Record{From: "prom/prometheus:v3.13.2", To: "prom/prometheus:v3.14.0"})
	if v.Repo != "prom/prometheus" || v.FromRef != "v3.13.2" || v.ToRef != "v3.14.0" {
		t.Fatalf("got repo=%q %q -> %q", v.Repo, v.FromRef, v.ToRef)
	}
}

func TestNewRecView_KeepsFullRefsWhenTheImageDiffers(t *testing.T) {
	// Nothing normally changes a service's image, but if it did, hiding the name would be a lie.
	v := newRecView(store.Record{From: "nginx:1.27", To: "ghcr.io/nginx/nginx:1.28"})
	if v.Repo != "" || v.FromRef != "nginx:1.27" || v.ToRef != "ghcr.io/nginx/nginx:1.28" {
		t.Fatalf("got repo=%q %q -> %q", v.Repo, v.FromRef, v.ToRef)
	}
}

func TestSplitRef(t *testing.T) {
	cases := []struct{ in, repo, tag string }{
		{"nginx:1.27", "nginx", "1.27"},
		{"ghcr.io/el-mundos/belay:0.2.8", "ghcr.io/el-mundos/belay", "0.2.8"},
		{"registry.example.com:5000/app:v1", "registry.example.com:5000/app", "v1"},
		{"nginx", "nginx", ""}, // no tag
		// a digest ref must split at "@": its "sha256:" would otherwise read as a tag separator
		{"nginx@sha256:0123456789abcdef0123456789abcdef", "nginx", "sha256:0123456789ab"},
	}
	for _, c := range cases {
		repo, tag := splitRef(c.in)
		if repo != c.repo || tag != c.tag {
			t.Errorf("splitRef(%q) = %q, %q; want %q, %q", c.in, repo, tag, c.repo, c.tag)
		}
	}
}

func TestErrPreview_OneClippedLine(t *testing.T) {
	long := "Error response from daemon: " + strings.Repeat("x", 400)
	prev, more := errPreview(long + "\nstack line\nstack line")
	if !more {
		t.Error("want more=true for a clipped multi-line error")
	}
	if n := len([]rune(prev)); n > errPreviewMax+1 { // +1 for the ellipsis
		t.Errorf("preview is %d runes, want <= %d", n, errPreviewMax+1)
	}
	if !strings.HasSuffix(prev, "…") {
		t.Errorf("clipped preview should say so: %q", prev)
	}
}

func TestErrPreview_ShortSingleLineIsComplete(t *testing.T) {
	prev, more := errPreview("\n  health gate failed  \n\n")
	if prev != "health gate failed" || more {
		t.Fatalf("got %q, more=%v", prev, more)
	}
}

func TestErrPreview_Empty(t *testing.T) {
	if prev, more := errPreview(""); prev != "" || more {
		t.Fatalf("got %q, more=%v", prev, more)
	}
}

func TestListRev_ChangesOnlyWhenTheListDoes(t *testing.T) {
	rows := []recView{newRecView(store.Record{ID: 1, Outcome: "updated", From: "a:1", To: "a:2"})}
	base := listRev(rows)
	if listRev(rows) != base {
		t.Fatal("same rows must fingerprint the same, or the UI swaps on every poll")
	}

	// Logs are not part of the fingerprint (they are not rendered in the list) but the rollback
	// button's state is — it is the thing that silently goes stale.
	withLogs := []recView{newRecView(store.Record{ID: 1, Outcome: "updated", From: "a:1", To: "a:2", Logs: "noise"})}
	if listRev(withLogs) != base {
		t.Error("logs are not shown in the list; they must not force a swap")
	}

	rows[0].CanRollback = true
	if listRev(rows) == base {
		t.Error("rollback availability changed; the list must swap")
	}
}

// A compose interpolation failure is about Belay's environment, not the stack: the same file works
// when the deploy wrapper supplies the variable. The raw compose error does not say that.
func TestEnvCaveat(t *testing.T) {
	real := `docker [compose -f /srv/infra/kalos-cobra/docker-compose.yml up -d --pull always db]: ` +
		`exit status 15: error while interpolating services.db.environment.POSTGRES_PASSWORD: ` +
		`required variable POSTGRES_PASSWORD is missing a value: missing — run via deploy.sh`
	if envCaveat(real) == "" {
		t.Error("a missing-variable failure should be explained")
	}
	if got := envCaveat("health gate failed: probe returned 502"); got != "" {
		t.Errorf("an ordinary failure must not be given an environment excuse: %q", got)
	}
}

// Two updates must never run against one service. The window this closes is ordinary: "Update all"
// scans registries for several seconds with every Update button still live.
func TestJobs_StartRefusesASecondUpdateForTheSameService(t *testing.T) {
	m := newJobManager()
	first := m.start("monitoring", "prometheus", "v3.13.2", "v3.14.0")
	if first == nil {
		t.Fatal("the first update should be allowed")
	}
	if second := m.start("monitoring", "prometheus", "v3.13.2", "v3.14.0"); second != nil {
		t.Error("a second update for the same service was allowed to start")
	}
	// a different service is unaffected
	if other := m.start("monitoring", "alertmanager", "v0.33", "v0.34"); other == nil {
		t.Error("an unrelated service should still be startable")
	}
	// and once the first finishes, the service is free again
	m.finish(first, "updated", "")
	if again := m.start("monitoring", "prometheus", "v3.14.0", "v3.15.0"); again == nil {
		t.Error("a finished job must not block the next update")
	}
}
