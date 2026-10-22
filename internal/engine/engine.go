// Package engine implements Belay's core: a health-gated update with automatic rollback.
//
// The flow (see README): record current tag -> rewrite tag -> up -d -> health-gate ladder ->
// keep on healthy, else capture logs + revert to the previous tag + report ROLLED_BACK.
//
// The engine is deliberately Docker-agnostic: it drives the Deployer and HealthGate interfaces,
// which the agent implements against `docker compose`. That keeps the core logic unit-testable
// without a Docker daemon.
package engine

import (
	"context"
	"time"
)

// Outcome is the terminal state of an update attempt.
type Outcome string

const (
	OutcomeUpdated    Outcome = "updated"     // new version came up healthy
	OutcomeRolledBack Outcome = "rolled_back" // new version failed; reverted to the old one
	OutcomeSkipped    Outcome = "skipped"     // nothing to do (already on target)
	OutcomeError      Outcome = "error"       // could not complete safely (e.g. rollback failed)
)

// Request describes a single service update.
type Request struct {
	Project    string             // compose project name / directory
	Service    string             // compose service name
	FromImage  string             // current image ref, e.g. "grafana/grafana-oss:13.0.2"
	ToImage    string             // target image ref, e.g. "grafana/grafana-oss:13.1.0"
	Rebase     bool               // same tag, new build behind it: force a pull instead of a tag rewrite
	PullBytes  int64              // compressed size of ToImage's layers (0 = unknown: skip the disk check)
	OnPhase    func(phase string) // optional: reports the current stage (snapshotting/pulling/health…)
	OnSnapshot func(id string)    // optional: called once the volume snapshot exists, so it can be journaled
}

func (r Request) phase(p string) {
	if r.OnPhase != nil {
		r.OnPhase(p)
	}
}

// Result is the outcome of an update attempt, including the logs captured during it.
type Result struct {
	Request  Request
	Outcome  Outcome
	Logs     string // container logs captured during the update (success or failure)
	Snapshot string // retained volume snapshot id (caller owns cleanup) on success, or when a rollback failed; "" if none
	Duration time.Duration
	Err      error
}

// Preflighter is an optional Deployer capability: a dry run answering "could this deployment even
// be rendered?". Kept optional so a Deployer that has no such notion simply does not implement it.
type Preflighter interface {
	Preflight(ctx context.Context, r Request) error
}

// Deployer applies a desired image tag to a service and brings it up. Implemented by the agent
// against docker compose.
type Deployer interface {
	// SetImage rewrites the service's image tag in the compose source
	// (edit-in-place; commits if the project dir is a git repo).
	SetImage(ctx context.Context, r Request, image string) error
	// Up (re)creates the service with its current compose definition.
	Up(ctx context.Context, r Request) error
	// Logs returns the last `tail` lines of the service's container logs (for failure reports).
	Logs(ctx context.Context, r Request, tail int) (string, error)
}

// HealthGate decides whether a freshly-deployed service is healthy within a timeout.
// Implementations form the ladder: docker HEALTHCHECK -> configured probe -> stayed-running-N-seconds.
type HealthGate interface {
	// Wait blocks until the service is healthy (nil) or the timeout/deadline fails it (error).
	Wait(ctx context.Context, r Request) error
}

// Snapshotter snapshots a service's volumes before an update so a rollback restores DATA, not just
// the image tag. Optional: a nil Snapshotter means image-only rollback. Snapshot returns an opaque
// id ("" if nothing to snapshot); Restore puts the data back; Discard cleans up.
type Snapshotter interface {
	Snapshot(ctx context.Context, r Request) (string, error)
	Restore(ctx context.Context, r Request, snapshot string) error
	Discard(ctx context.Context, r Request, snapshot string)
}

// RollbackTimeout bounds an automatic rollback, which runs on its own deadline (see rollback). The
// previous image is normally still on disk, so this is a recreate rather than a pull.
var RollbackTimeout = 5 * time.Minute

// Engine performs safe updates: snapshot, deploy, health-gate, and roll back (image + data) on failure.
type Engine struct {
	Deployer Deployer
	Health   HealthGate
	Snapshot Snapshotter // optional; when set, volumes are snapshotted and restored on rollback
}

// SafeUpdate runs the core loop for one service and never leaves it broken:
// on a failed start or health gate it reverts to FromImage and returns OutcomeRolledBack + logs.
func (e *Engine) SafeUpdate(ctx context.Context, r Request) Result {
	start := time.Now()
	res := Result{Request: r}
	finish := func(o Outcome, err error) Result {
		res.Outcome, res.Err, res.Duration = o, err, time.Since(start)
		return res
	}

	// A rebase deliberately keeps the same tag, so equal refs are real work there, not a no-op.
	if r.ToImage == "" || (r.FromImage == r.ToImage && !r.Rebase) {
		return finish(OutcomeSkipped, nil)
	}

	// 0. Refuse to deploy something we cannot fully render. This runs before the snapshot and
	// before the tag is touched, so a project whose variables are missing is left exactly as it
	// was — no rollback needed, because nothing happened.
	if p, ok := e.Deployer.(Preflighter); ok {
		r.phase("checking the deployment renders and fits on disk")
		if err := p.Preflight(ctx, r); err != nil {
			return finish(OutcomeError, err)
		}
	}

	// 0. snapshot the service's volumes so a rollback can restore data too
	snap := ""
	if e.Snapshot != nil {
		r.phase("snapshotting volumes")
		snap, _ = e.Snapshot.Snapshot(ctx, r)
		if snap != "" && r.OnSnapshot != nil {
			r.OnSnapshot(snap)
		}
	}

	// 1. move to the new tag and bring it up
	r.phase("pulling & starting new version")
	if err := e.Deployer.SetImage(ctx, r, r.ToImage); err != nil {
		e.discard(ctx, r, snap)
		return finish(OutcomeError, err)
	}
	if err := e.Deployer.Up(ctx, r); err != nil {
		return e.rollback(ctx, r, &res, start, err, snap) // couldn't even start -> revert
	}

	// 2. health gate
	r.phase("checking it came up healthy")
	if err := e.Health.Wait(ctx, r); err != nil {
		return e.rollback(ctx, r, &res, start, err, snap)
	}

	// success: record the update's logs, and hand the snapshot to the caller so it can offer a
	// manual rollback for a retention window. The caller is responsible for discarding it later.
	if logs, err := e.Deployer.Logs(ctx, r, 200); err == nil {
		res.Logs = logs
	}
	res.Snapshot = snap
	return finish(OutcomeUpdated, nil)
}

// ManualRollback reverts a previously-successful update on user request: it restores the retained
// volume snapshot and the previous image, then health-gates. r.FromImage is the image to revert TO
// (the old one); r.ToImage is what's currently running. On success it discards the snapshot and
// reports OutcomeRolledBack; if it can't complete it reports OutcomeError (needs a human).
func (e *Engine) ManualRollback(ctx context.Context, r Request, snapshot string) Result {
	start := time.Now()
	res := Result{Request: r}
	fail := func(err error) Result {
		res.Outcome, res.Err, res.Duration = OutcomeError, err, time.Since(start)
		return res
	}
	if e.Snapshot != nil && snapshot != "" {
		e.Snapshot.Restore(ctx, r, snapshot) // restore pre-update data (stops the service first)
	}
	if err := e.Deployer.SetImage(ctx, r, r.FromImage); err != nil {
		return fail(err)
	}
	if err := e.Deployer.Up(ctx, r); err != nil {
		return fail(err)
	}
	if err := e.Health.Wait(ctx, r); err != nil {
		return fail(err)
	}
	e.discard(ctx, r, snapshot)
	res.Outcome, res.Duration = OutcomeRolledBack, time.Since(start)
	return res
}

func (e *Engine) discard(ctx context.Context, r Request, snap string) {
	if e.Snapshot != nil && snap != "" {
		e.Snapshot.Discard(ctx, r, snap)
	}
}

// rollback captures logs, restores the previous image AND (if snapshotted) the volume data, and
// reports. If the rollback itself fails, that's an OutcomeError — the one case that needs a human now.
//
// The rollback gets a fresh deadline of its own. The commonest reason to be here is that the update
// ran out of time — a slow pull hitting the caller's deadline — and a rollback run on that same,
// already-expired context fails at its first docker call, turning a recoverable timeout into an
// error that needs a human (and, before this, a leaked snapshot each time it happened).
//
// When it does fail, the snapshot is handed back in res.Snapshot rather than discarded: it is the
// only copy of the pre-update data, and the caller decides how long to keep it.
func (e *Engine) rollback(ctx context.Context, r Request, res *Result, start time.Time, cause error, snap string) Result {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), RollbackTimeout)
	defer cancel()
	fail := func(err error) Result {
		res.Outcome, res.Err, res.Duration = OutcomeError, err, time.Since(start)
		res.Snapshot = snap
		return *res
	}
	r.phase("unhealthy — rolling back")
	if logs, err := e.Deployer.Logs(ctx, r, 200); err == nil {
		res.Logs = logs
	}
	if e.Snapshot != nil && snap != "" {
		e.Snapshot.Restore(ctx, r, snap) // restores volume data (stops the service first)
	}
	if err := e.Deployer.SetImage(ctx, r, r.FromImage); err != nil {
		return fail(err)
	}
	if err := e.Deployer.Up(ctx, r); err != nil {
		return fail(err)
	}
	e.discard(ctx, r, snap)
	res.Outcome, res.Err, res.Duration = OutcomeRolledBack, cause, time.Since(start)
	return *res
}
