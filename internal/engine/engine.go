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
	Project   string // compose project name / directory
	Service   string // compose service name
	FromImage string // current image ref, e.g. "grafana/grafana-oss:13.0.2"
	ToImage   string // target image ref, e.g. "grafana/grafana-oss:13.1.0"
}

// Result is the outcome of an update attempt, including logs captured on failure.
type Result struct {
	Request  Request
	Outcome  Outcome
	Logs     string // container logs captured on failure, surfaced in the UI
	Duration time.Duration
	Err      error
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

// Engine performs safe updates: deploy, health-gate, and roll back on failure.
type Engine struct {
	Deployer Deployer
	Health   HealthGate
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

	if r.FromImage == r.ToImage || r.ToImage == "" {
		return finish(OutcomeSkipped, nil)
	}

	// 1. move to the new tag and bring it up
	if err := e.Deployer.SetImage(ctx, r, r.ToImage); err != nil {
		return finish(OutcomeError, err)
	}
	if err := e.Deployer.Up(ctx, r); err != nil {
		return e.rollback(ctx, r, &res, start, err) // couldn't even start -> revert
	}

	// 2. health gate
	if err := e.Health.Wait(ctx, r); err != nil {
		return e.rollback(ctx, r, &res, start, err)
	}

	return finish(OutcomeUpdated, nil)
}

// rollback captures logs (before reverting, so the user sees why it failed), restores the previous
// image, and reports. If the rollback itself fails, that's an OutcomeError — the one case that needs
// a human now.
func (e *Engine) rollback(ctx context.Context, r Request, res *Result, start time.Time, cause error) Result {
	if logs, err := e.Deployer.Logs(ctx, r, 200); err == nil {
		res.Logs = logs
	}
	if err := e.Deployer.SetImage(ctx, r, r.FromImage); err != nil {
		res.Outcome, res.Err, res.Duration = OutcomeError, err, time.Since(start)
		return *res
	}
	if err := e.Deployer.Up(ctx, r); err != nil {
		res.Outcome, res.Err, res.Duration = OutcomeError, err, time.Since(start)
		return *res
	}
	res.Outcome, res.Err, res.Duration = OutcomeRolledBack, cause, time.Since(start)
	return *res
}
