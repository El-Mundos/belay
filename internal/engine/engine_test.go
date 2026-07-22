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

func TestSafeUpdate_SameImageSkips(t *testing.T) {
	d := &fakeDeployer{current: "app:1.0"}
	e := &Engine{Deployer: d, Health: fakeGate{healthy: true}}
	r := req()
	r.ToImage = r.FromImage
	if res := e.SafeUpdate(context.Background(), r); res.Outcome != OutcomeSkipped {
		t.Fatalf("outcome = %q, want skipped", res.Outcome)
	}
}
