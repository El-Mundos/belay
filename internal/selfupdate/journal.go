package selfupdate

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A self-update is the one operation Belay cannot supervise, because it is the thing being
// replaced. The helper container can die (host reboot, OOM, docker restart) at any point, including
// the instant between removing the old container and starting the new one — and a helper that dies
// there used to leave no Belay running at all, with nothing on the host that knew a recovery was
// owed.
//
// Two mechanisms cover that, and they are deliberately independent:
//
//  1. The HELPER is restartable and its script is idempotent and rollback-capable (see
//     recreateScript). It keeps the previous container instead of destroying it, so there is
//     always something to go back to, and Docker's restart policy re-runs it after a crash.
//  2. This JOURNAL, written to the data volume before the helper is launched, so it survives the
//     container being replaced. Whichever Belay comes up next — the new one, or the old one after
//     a rollback — reads it and can say what happened. Without it, a rollback is silent: you get
//     the version you already had and no reason why.
//
// The journal is the only part that outlives every process involved, so it is what turns "belay is
// mysteriously still on the old version" into a recorded, visible outcome.

const journalName = "selfupdate.json"

// Phases of an in-flight self-update.
const (
	PhaseApplying = "applying" // helper launched, outcome unknown
	PhaseApplied  = "applied"  // a new Belay booted and claimed success; the helper may still roll it back
)

// State is the on-disk journal.
type State struct {
	Phase       string        `json:"phase"`
	Container   string        `json:"container"`
	Backup      string        `json:"backup"`       // the renamed previous container, gate-window rollback material
	FromImage   string        `json:"from_image"`   // image ID that was running when Apply was called
	FromVersion string        `json:"from_version"` // the version string that image was, e.g. "0.2.9"
	ToImage     string        `json:"to_image"`     // image ref being deployed
	Window      time.Duration `json:"window"`       // how long a manual rollback stays offered
	Started     time.Time     `json:"started"`

	// FollowUp is work the caller asked to happen AFTER this update succeeds. It lives in the
	// journal because the process that asked for it is the process being replaced -- the only way
	// to carry an intention across a self-update is to write it down somewhere that outlives the
	// container. Opaque here on purpose: this package has no business knowing what agents are.
	FollowUp string `json:"follow_up,omitempty"`

	// Rollback outlives the update itself: it is what a MANUAL rollback needs, hours later, once
	// the previous container has been reaped and the tag has moved off the previous image.
	Rollback *RollbackPoint `json:"rollback,omitempty"`
}

// RollbackPoint is the retained way back from a completed self-update.
type RollbackPoint struct {
	Image   string    `json:"image"`   // image ID of the build we came from
	Tag     string    `json:"tag"`     // local tag holding that image against `docker image prune`
	Version string    `json:"version"` // human version, for the History row and the confirm text
	Until   time.Time `json:"until"`   // end of the retention window
}

func journalPath(dir string) string { return filepath.Join(dir, journalName) }

// fromLabel is what to show as the previous version. Once a moving tag like :latest has advanced,
// the build you were running has no name left, so fall back to a short image id rather than
// printing 71 characters of digest at the user.
func (s State) fromLabel() string {
	if s.FromVersion != "" {
		return s.FromVersion
	}
	if algo, hex, ok := strings.Cut(s.FromImage, ":"); ok && len(hex) > 12 {
		return algo + ":" + hex[:12]
	}
	return s.FromImage
}

// settle ends an in-flight update while KEEPING any retained rollback point: the phase is over,
// but the way back is supposed to outlive it. Deleting the journal here is what would silently
// remove the manual-rollback offer the moment the update finished.
func settle(dir string, st State) {
	if st.Rollback == nil {
		clearState(dir)
		return
	}
	_ = saveState(dir, State{Rollback: st.Rollback})
}

// LoadState reads the journal. A missing or unreadable file is "nothing in flight", never an error:
// a corrupt journal must not stop Belay from booting.
func LoadState(dir string) State {
	if dir == "" {
		return State{}
	}
	b, err := os.ReadFile(journalPath(dir))
	if err != nil {
		return State{}
	}
	var st State
	if json.Unmarshal(b, &st) != nil {
		return State{}
	}
	return st
}

func saveState(dir string, st State) error {
	if dir == "" {
		return nil
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(journalPath(dir), b, 0o600)
}

func clearState(dir string) {
	if dir != "" {
		_ = os.Remove(journalPath(dir))
	}
}

// Outcome is what Reconcile concluded about a self-update that was in flight when this process
// started. Kind is "" when there was nothing to reconcile.
type Outcome struct {
	Kind      string // "updated" | "rolled_back" | "error"
	From, To  string
	Detail    string
	ReapAfter string // container to remove once the helper's gate window has passed ("" = none)
	FollowUp  string // work the previous process asked to run if this update succeeded
}

// Reconcile inspects the journal against reality and reports what became of an in-flight update.
// It runs at startup, before serving.
//
// The test is simply which image WE are: the journal records the image ID that was running when the
// helper was launched, so a Belay that finds itself still on that ID is the old one — either the
// helper never got the new container up, or it got it up and then rolled it back.
func (m *Manager) Reconcile(ctx context.Context, dir string) Outcome {
	st := LoadState(dir)
	if st.Phase == "" || !m.Enabled() {
		return Outcome{}
	}
	running, err := dockerOut(ctx, "inspect", m.container, "--format", "{{.Image}}")
	if err != nil {
		// We cannot tell which image we are, so we cannot judge. Leave the journal alone rather
		// than reporting a guess; the next boot can still settle it.
		return Outcome{}
	}
	isOld := running == st.FromImage

	switch {
	case st.Phase == PhaseApplying && !isOld:
		// We are the new image and this is our first boot: the update took. Retain the way back.
		window := st.Window
		if window <= 0 {
			window = DefaultRollbackWindow
		}
		// Hold the image we came from under a name. Apply does this too, but a journal written by an
		// older Belay never did — and an untagged image is dangling the moment the tag moves on.
		_ = runDocker(ctx, "tag", st.FromImage, previousTag)
		next := st
		next.Phase = PhaseApplied
		next.Rollback = &RollbackPoint{
			Image: st.FromImage, Tag: previousTag, Version: st.FromVersion,
			Until: time.Now().Add(window),
		}
		saveState(dir, next)
		return Outcome{Kind: "updated", From: st.fromLabel(), To: st.ToImage,
			Detail: "Belay restarted on the new image", ReapAfter: st.Backup, FollowUp: st.FollowUp}

	case st.Phase == PhaseApplying && isOld:
		// The helper never got the new container running — it died mid-flight, or the new image
		// failed to start and it put us back.
		settle(dir, st)
		return Outcome{Kind: "error", From: st.fromLabel(), To: st.ToImage,
			Detail: "self-update did not take: Belay is still running the previous image"}

	case st.Phase == PhaseApplied && isOld:
		// We reported success on a previous boot and are now the OLD image again: the helper's
		// health gate failed after we came up, and it rolled us back.
		settle(dir, st)
		return Outcome{Kind: "rolled_back", From: st.fromLabel(), To: st.ToImage,
			Detail: "the new image did not stay healthy; Belay was rolled back to the previous container"}

	default:
		// Phase applied and we are the new image: a normal restart after a settled update.
		settle(dir, st)
		return Outcome{ReapAfter: st.Backup}
	}
}

// Reap removes a container left behind as rollback material. Called once the helper's gate window
// has passed, so it cannot pull the rug out from under a rollback that is still possible.
func (m *Manager) Reap(ctx context.Context, dir, container string) {
	if container == "" {
		return
	}
	_ = runDocker(ctx, "rm", "-f", container)
	_ = runDocker(ctx, "rm", "-f", helperName) // the finished helper, exited 0
	if st := LoadState(dir); st.Phase == PhaseApplied {
		settle(dir, st)
	}
}

// DefaultRollbackWindow is used when the journal carries no window of its own. A journal written by
// a Belay from before that field existed decodes it as zero, and zero would mean "expired the
// instant it was created" -- the worst possible reading for a safety feature, and one that silently
// removes the rollback offer on exactly the upgrade that introduces it.
const DefaultRollbackWindow = 24 * time.Hour

// GateWindow is how long a new Belay must stay up before its predecessor is discarded. It matches
// the wait inside the helper script: reaping earlier would destroy the only rollback target while
// the helper could still decide to use it.
const GateWindow = 30 * time.Second
