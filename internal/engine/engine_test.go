package engine

import (
	"context"
	"errors"
	"testing"
)

// fakeDeployer records the sequence of image tags it was told to run, and can simulate an
// "up" failure. It lets us test the engine's rollback logic with no Docker.
type fakeDeployer struct {
	current string   // the image currently "running"
	history []string // every image SetImage+Up brought live, in order
	failUp  bool     // if true, Up returns an error (container won't start)
	logs    string
}

func (f *fakeDeployer) SetImage(_ context.Context, _ Request, image string) error {
	f.current = image
	return nil
}
func (f *fakeDeployer) Up(_ context.Context, _ Request) error {
	if f.failUp {
		return errors.New("container exited")
	}
	f.history = append(f.history, f.current)
	return nil
}
func (f *fakeDeployer) Logs(_ context.Context, _ Request, _ int) (string, error) {
	return f.logs, nil
}

type fakeGate struct{ healthy bool }

func (g fakeGate) Wait(_ context.Context, _ Request) error {
	if g.healthy {
		return nil
	}
	return errors.New("never became healthy")
}

func req() Request {
	return Request{Project: "p", Service: "s", FromImage: "app:1.0", ToImage: "app:2.0"}
}

func TestSafeUpdate_HealthyKeepsNewVersion(t *testing.T) {
	d := &fakeDeployer{current: "app:1.0"}
	e := &Engine{Deployer: d, Health: fakeGate{healthy: true}}
	res := e.SafeUpdate(context.Background(), req())
	if res.Outcome != OutcomeUpdated {
		t.Fatalf("outcome = %q, want updated", res.Outcome)
	}
	if d.current != "app:2.0" {
		t.Fatalf("running image = %q, want app:2.0", d.current)
	}
}

func TestSafeUpdate_UnhealthyRollsBackWithLogs(t *testing.T) {
	d := &fakeDeployer{current: "app:1.0", logs: "panic: bad config"}
	e := &Engine{Deployer: d, Health: fakeGate{healthy: false}}
	res := e.SafeUpdate(context.Background(), req())
	if res.Outcome != OutcomeRolledBack {
		t.Fatalf("outcome = %q, want rolled_back", res.Outcome)
	}
	if d.current != "app:1.0" {
		t.Fatalf("running image = %q, want reverted to app:1.0", d.current)
	}
	if res.Logs != "panic: bad config" {
		t.Fatalf("logs not captured on rollback: %q", res.Logs)
	}
	// history should end on the old image (new tried, then reverted)
	if got := d.history[len(d.history)-1]; got != "app:1.0" {
		t.Fatalf("final deploy = %q, want app:1.0", got)
	}
}

func TestSafeUpdate_FailedStartRollsBack(t *testing.T) {
	d := &fakeDeployer{current: "app:1.0", failUp: true}
	e := &Engine{Deployer: d, Health: fakeGate{healthy: true}}
	res := e.SafeUpdate(context.Background(), req())
	// Up always fails here, so even the rollback Up fails -> OutcomeError (needs a human).
	if res.Outcome != OutcomeError {
		t.Fatalf("outcome = %q, want error (rollback couldn't start either)", res.Outcome)
	}
}

type fakeSnap struct{ snapshotted, restored, discarded bool }

func (f *fakeSnap) Snapshot(context.Context, Request) (string, error) {
	f.snapshotted = true
	return "s1", nil
}
func (f *fakeSnap) Restore(context.Context, Request, string) error { f.restored = true; return nil }
func (f *fakeSnap) Discard(context.Context, Request, string)       { f.discarded = true }

func TestSafeUpdate_SnapshotRetainedOnSuccess(t *testing.T) {
	d := &fakeDeployer{current: "app:1.0"}
	fs := &fakeSnap{}
	e := &Engine{Deployer: d, Health: fakeGate{healthy: true}, Snapshot: fs}
	res := e.SafeUpdate(context.Background(), req())
	if res.Outcome != OutcomeUpdated {
		t.Fatalf("outcome = %q", res.Outcome)
	}
	// On success the snapshot is kept (for manual rollback) and handed back to the caller —
	// NOT discarded here and never restored.
	if !fs.snapshotted || fs.discarded || fs.restored {
		t.Fatalf("snapshot=%v discard=%v restore=%v (want snapshot, no discard, no restore)", fs.snapshotted, fs.discarded, fs.restored)
	}
	if res.Snapshot != "s1" {
		t.Fatalf("res.Snapshot = %q, want s1 (handed to caller)", res.Snapshot)
	}
}

func TestSafeUpdate_VolumeRestoredOnRollback(t *testing.T) {
	d := &fakeDeployer{current: "app:1.0", logs: "boom"}
	fs := &fakeSnap{}
	e := &Engine{Deployer: d, Health: fakeGate{healthy: false}, Snapshot: fs}
	res := e.SafeUpdate(context.Background(), req())
	if res.Outcome != OutcomeRolledBack {
		t.Fatalf("outcome = %q, want rolled_back", res.Outcome)
	}
	if !fs.restored {
		t.Fatal("volume data was NOT restored on rollback")
	}
	if d.current != "app:1.0" {
		t.Fatalf("image not reverted: %q", d.current)
	}
}

func TestManualRollback_RestoresDataAndImage(t *testing.T) {
	d := &fakeDeployer{current: "app:2.0"} // currently on the new version
	fs := &fakeSnap{}
	e := &Engine{Deployer: d, Health: fakeGate{healthy: true}, Snapshot: fs}
	// FromImage = the old image to revert TO; ToImage = what's running now.
	r := Request{Project: "p", Service: "s", FromImage: "app:1.0", ToImage: "app:2.0"}
	res := e.ManualRollback(context.Background(), r, "s1")
	if res.Outcome != OutcomeRolledBack {
		t.Fatalf("outcome = %q, want rolled_back", res.Outcome)
	}
	if d.current != "app:1.0" {
		t.Fatalf("image = %q, want reverted to app:1.0", d.current)
	}
	if !fs.restored {
		t.Fatal("data snapshot was not restored")
	}
	if !fs.discarded {
		t.Fatal("snapshot should be discarded after a successful manual rollback")
	}
}

func TestManualRollback_UnhealthyIsError(t *testing.T) {
	d := &fakeDeployer{current: "app:2.0"}
	e := &Engine{Deployer: d, Health: fakeGate{healthy: false}, Snapshot: &fakeSnap{}}
	r := Request{Project: "p", Service: "s", FromImage: "app:1.0", ToImage: "app:2.0"}
	if res := e.ManualRollback(context.Background(), r, "s1"); res.Outcome != OutcomeError {
		t.Fatalf("outcome = %q, want error when the reverted version won't come up healthy", res.Outcome)
	}
}

func TestSafeUpdate_SameImageSkips(t *testing.T) {
	d := &fakeDeployer{current: "app:1.0"}
	e := &Engine{Deployer: d, Health: fakeGate{healthy: true}}
	r := req()
	r.ToImage = r.FromImage
	if res := e.SafeUpdate(context.Background(), r); res.Outcome != OutcomeSkipped {
		t.Fatalf("outcome = %q, want skipped", res.Outcome)
	}
}

// A rebase deploys the same ref it is already on — the tag was re-pointed at a new build. The engine
// must treat that as real work rather than the no-op it looks like, or rolling tags can never move.
func TestSafeUpdate_RebaseIsNotSkipped(t *testing.T) {
	d := &fakeDeployer{current: "app:15"}
	e := &Engine{Deployer: d, Health: fakeGate{healthy: true}}

	r := Request{Project: "p", Service: "s", FromImage: "app:15", ToImage: "app:15"}
	if res := e.SafeUpdate(context.Background(), r); res.Outcome != OutcomeSkipped {
		t.Fatalf("without Rebase, equal refs should skip; got %q", res.Outcome)
	}

	r.Rebase = true
	res := e.SafeUpdate(context.Background(), r)
	if res.Outcome != OutcomeUpdated {
		t.Fatalf("outcome = %q, want updated", res.Outcome)
	}
	if len(d.history) != 1 || d.history[0] != "app:15" {
		t.Errorf("history = %v, want one deploy of app:15", d.history)
	}
}

// A failed rebase cannot roll back "to the previous tag" — the tag now means the broken build. The
// caller pins FromImage to the digest that was running, and the engine must restore exactly that.
func TestSafeUpdate_RebaseRollsBackToPinnedDigest(t *testing.T) {
	const old = "app@sha256:oldbuild"
	d := &fakeDeployer{current: "app:15", logs: "panic: bad build"}
	e := &Engine{Deployer: d, Health: fakeGate{healthy: false}}

	res := e.SafeUpdate(context.Background(), Request{
		Project: "p", Service: "s", FromImage: old, ToImage: "app:15", Rebase: true,
	})
	if res.Outcome != OutcomeRolledBack {
		t.Fatalf("outcome = %q, want rolled_back", res.Outcome)
	}
	if d.current != old {
		t.Errorf("running image = %q, want the pinned digest %q — reverting to the tag would "+
			"re-pull the build that just failed", d.current, old)
	}
}
