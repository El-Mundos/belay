package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/El-Mundos/belay/internal/compose"
	"github.com/El-Mundos/belay/internal/engine"
	"github.com/El-Mundos/belay/internal/journal"
)

// Recovery is the verdict on an update a previous process left unfinished.
type Recovery struct {
	Outcome engine.Outcome // rolled_back: the service is on its old version; error: someone should look
	Note    string         // goes on the record, so it reads as a sentence to the user
	PutBack bool           // the compose tag must be put back to e.From
}

// Recover settles one interrupted update: it looks at what the service is actually running, puts
// the compose tag back when the new version never started, and says what happened. ok is false
// when Docker is not answering yet — straight after a daemon restart, which is the usual reason we
// are here at all — so the caller can try again rather than misjudge a missing container.
func Recover(ctx context.Context, e journal.Entry) (r Recovery, ok bool) {
	if _, err := output(ctx, "docker", "ps", "-q"); err != nil {
		return Recovery{}, false
	}
	cur, running := serviceState(ctx, e.File, e.Service)
	composeRef, _ := compose.FindImage(e.File, e.Service)
	r = decideRecovery(e, cur, running, ImageID(ctx, e.To), composeRef)
	if r.PutBack {
		if err := compose.SetImage(e.File, e.Service, e.From); err != nil {
			return Recovery{Outcome: engine.OutcomeError,
				Note: r.Note + " — but putting the compose tag back failed: " + err.Error()}, true
		}
		_ = compose.CommitIfRepo(e.File, fmt.Sprintf("belay: revert interrupted update %s %s -> %s", e.Service, e.To, e.From))
	}
	return r, true
}

// decideRecovery is Recover's judgement, apart from Docker so it can be tested. cur is the image ID
// the service's container runs ("" = no container), toID what the target ref resolves to locally.
func decideRecovery(e journal.Entry, cur string, running bool, toID, composeRef string) Recovery {
	lead := "interrupted: Belay was stopped partway through this update"
	if !e.Started.IsZero() {
		lead += " (started " + e.Started.Local().Format("Jan 2, 15:04") + ")"
	}
	switch {
	case cur == "":
		return Recovery{Outcome: engine.OutcomeError,
			Note: lead + ", and the service has no container now. Check it before retrying."}
	case !running:
		return Recovery{Outcome: engine.OutcomeError,
			Note: lead + ", and its container is stopped — it may have died during a rollback or a data " +
				"restore. Check it before retrying."}
	case e.FromImageID != "" && cur == e.FromImageID:
		if !e.Rebase && composeRef != e.From {
			return Recovery{Outcome: engine.OutcomeRolledBack, PutBack: true,
				Note: lead + ". The new version never started, so the service is still on " + e.From +
					"; its compose file named " + composeRef + " and has been put back. Retry when ready."}
		}
		return Recovery{Outcome: engine.OutcomeRolledBack,
			Note: lead + ". Nothing changed: the service is still on " + e.From + ". Retry when ready."}
	case toID != "" && cur == toID:
		return Recovery{Outcome: engine.OutcomeError,
			Note: lead + ". " + e.To + " is running, but its health check never finished, so nobody has " +
				"verified it. Check it, or roll it back."}
	default:
		return Recovery{Outcome: engine.OutcomeError,
			Note: lead + ", and the service now runs " + shortID(cur) + ", which is neither the version " +
				"it came from nor the target. Check it before retrying."}
	}
}

// serviceState is ServiceImageID plus whether that container is running.
func serviceState(ctx context.Context, file, service string) (imageID string, running bool) {
	out, err := output(ctx, "docker", "compose", "-f", file, "ps", "-aq", service)
	if err != nil {
		return "", false
	}
	cid := strings.TrimSpace(strings.SplitN(strings.TrimSpace(out), "\n", 2)[0])
	if cid == "" {
		return "", false
	}
	st, err := output(ctx, "docker", "inspect", "-f", "{{.Image}} {{.State.Running}}", cid)
	if err != nil {
		return "", false
	}
	id, run, _ := strings.Cut(strings.TrimSpace(st), " ")
	return id, run == "true"
}

// RecoverAll waits for Docker (up to wait) and settles every pending entry, calling report for each
// before removing it from the journal. Entries Docker never answered for stay for the next start.
func RecoverAll(ctx context.Context, j *journal.Journal, wait time.Duration, report func(journal.Entry, Recovery)) {
	pending := j.Pending()
	deadline := time.Now().Add(wait)
	for len(pending) > 0 {
		var left []journal.Entry
		for _, e := range pending {
			r, ok := Recover(ctx, e)
			if !ok {
				left = append(left, e)
				continue
			}
			report(e, r)
			_ = j.End(e.Key())
		}
		if pending = left; len(pending) == 0 || time.Now().After(deadline) {
			return
		}
		time.Sleep(5 * time.Second)
	}
}
