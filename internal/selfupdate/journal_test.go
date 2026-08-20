package selfupdate

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// The helper script is the only thing standing between an interrupted self-update and a host with
// no Belay on it, and it runs unsupervised in a container that is about to delete its parent. These
// tests pin the three properties that make it recoverable.
func TestRecreateScript_KeepsThePreviousContainerAsRollback(t *testing.T) {
	s := script(t)
	if strings.Contains(s, "docker rm -f belay\ndocker run") {
		t.Fatal("the old container is destroyed before the new one has proven it runs")
	}
	if !strings.Contains(s, "docker rename belay belay-previous") {
		t.Error("old container should be renamed aside, not removed")
	}
	// and the rollback path must put it back
	for _, want := range []string{"docker rename belay-previous belay", "docker start belay"} {
		if !strings.Contains(s, want) {
			t.Errorf("rollback path missing %q", want)
		}
	}
}

func TestRecreateScript_IsIdempotent(t *testing.T) {
	// Docker restarts the helper if it dies, so a second run must not tear down a healthy result.
	s := script(t)
	if !strings.Contains(s, `if [ "$target" = "$current" ]`) || !strings.Contains(s, "exit 0") {
		t.Errorf("script has no early exit for work already done:\n%s", s)
	}
}

func TestRecreateScript_GatesOnTheNewContainerStayingUp(t *testing.T) {
	s := script(t)
	if !strings.Contains(s, "sleep 30") {
		t.Error("no gate window before declaring success")
	}
	if !strings.Contains(s, "{{.State.Running}}") {
		t.Error("gate does not check that the replacement is actually running")
	}
}

func script(t *testing.T) string {
	t.Helper()
	var docs []inspectDoc
	if err := json.Unmarshal([]byte(sample), &docs); err != nil {
		t.Fatal(err)
	}
	return recreateScript(docs[0], "belay-previous", docs[0].Config.Image, true)
}

// --- journal reconciliation -------------------------------------------------------------------
//
// Reconcile is what turns an interrupted update into something the user is told about. Its whole
// judgement rests on which image the running process is, so these cover each combination.

func TestReconcile_NothingInFlight(t *testing.T) {
	dir := t.TempDir()
	m := NewForTest("belay", "belay:latest", "")
	if out := m.Reconcile(t.Context(), dir); out.Kind != "" {
		t.Fatalf("clean start reported %q", out.Kind)
	}
}

func TestLoadState_CorruptJournalIsNotFatal(t *testing.T) {
	dir := t.TempDir()
	if err := writeRaw(dir, "{not json"); err != nil {
		t.Fatal(err)
	}
	// A corrupt journal must read as "nothing in flight" — Belay still has to boot.
	if st := LoadState(dir); st.Phase != "" {
		t.Fatalf("corrupt journal produced phase %q", st.Phase)
	}
}

func TestState_RoundTrips(t *testing.T) {
	dir := t.TempDir()
	want := State{Phase: PhaseApplying, Container: "belay", Backup: "belay-previous",
		FromImage: "sha256:old", ToImage: "belay:0.2.9"}
	if err := saveState(dir, want); err != nil {
		t.Fatal(err)
	}
	got := LoadState(dir)
	if got.Phase != want.Phase || got.FromImage != want.FromImage || got.Backup != want.Backup {
		t.Fatalf("round trip lost data: %+v", got)
	}
	clearState(dir)
	if LoadState(dir).Phase != "" {
		t.Error("clearState left the journal behind")
	}
}

func TestSaveState_NoDirIsANoop(t *testing.T) {
	// Belay can run without a data dir; self-update still works, it just cannot report afterwards.
	if err := saveState("", State{Phase: PhaseApplying}); err != nil {
		t.Fatalf("saving without a data dir should be a no-op, got %v", err)
	}
	if LoadState("").Phase != "" {
		t.Error("LoadState with no dir should report nothing in flight")
	}
}

func writeRaw(dir, content string) error {
	return os.WriteFile(journalPath(dir), []byte(content), 0o600)
}

// The bug that made the VPS agent's update silently do nothing: the "already done?" check ran
// against the LOCAL copy of the tag. An agent never pulls on its own, so on a moving tag like
// :latest it compared yesterday's image with itself, concluded the work was done, and exited 0.
func TestRecreateScript_PullsBeforeDecidingItIsAlreadyDone(t *testing.T) {
	s := script(t)
	pull := strings.Index(s, "docker pull")
	check := strings.Index(s, `if [ "$target" = "$current" ]`)
	if pull < 0 || check < 0 {
		t.Fatalf("script missing pull or check:\n%s", s)
	}
	if pull > check {
		t.Error("the image is pulled AFTER the up-to-date check, so a stale local tag reads as done")
	}
}

// A rollback deliberately re-points the tag at the older image, so pulling would immediately undo
// it and the helper would "roll back" onto the very build being rolled back.
func TestRecreateScript_RollbackDoesNotPull(t *testing.T) {
	var docs []inspectDoc
	if err := json.Unmarshal([]byte(sample), &docs); err != nil {
		t.Fatal(err)
	}
	s := recreateScript(docs[0], "belay-previous", "belay:latest", false)
	if strings.Contains(s, "docker pull") {
		t.Errorf("rollback script pulls, which would fetch the image it is rolling back from:\n%s", s)
	}
}

func TestSettle_KeepsTheRollbackPointButEndsThePhase(t *testing.T) {
	dir := t.TempDir()
	rb := &RollbackPoint{Image: "sha256:old", Tag: previousTag, Version: "0.2.9",
		Until: time.Now().Add(time.Hour)}
	settle(dir, State{Phase: PhaseApplied, Rollback: rb})
	got := LoadState(dir)
	if got.Phase != "" {
		t.Errorf("phase should be over, got %q", got.Phase)
	}
	if got.Rollback == nil || got.Rollback.Version != "0.2.9" {
		t.Fatal("the way back must outlive the update that created it")
	}
}

func TestFromLabel_PrefersTheVersionOverADigest(t *testing.T) {
	withVer := State{FromVersion: "0.2.9", FromImage: "sha256:cd1ea4999c8f41383bc37fefa77469e1"}
	if got := withVer.fromLabel(); got != "0.2.9" {
		t.Errorf("got %q, want the version", got)
	}
	// A journal written before the version was recorded still has to render sanely.
	noVer := State{FromImage: "sha256:cd1ea4999c8f41383bc37fefa77469e1"}
	if got := noVer.fromLabel(); got != "sha256:cd1ea4999c8f" {
		t.Errorf("got %q, want a short digest", got)
	}
}
