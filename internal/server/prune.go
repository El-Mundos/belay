package server

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/El-Mundos/belay/internal/agent"
	"github.com/El-Mundos/belay/internal/cluster"
	"github.com/El-Mundos/belay/internal/registry"
	"github.com/El-Mundos/belay/internal/selfupdate"
	"github.com/El-Mundos/belay/internal/store"
)

// pruneGiveUp is how long past its due time an image may stay kept (still in use, removal failing,
// agent away) before Belay stops trying and leaves it to a human, saying so on the record.
const pruneGiveUp = 7 * 24 * time.Hour

// pruneResend is how long an agent has to answer a prune before it is asked again. Commands live
// in memory, so a server restart or a dropped poll loses them; the store entry is what survives.
const pruneResend = time.Hour

// pullBytes is the compressed size of an image for the pre-pull disk check; 0 when the registry
// will not say, which skips the check rather than blocking the update.
func (s *Server) pullBytes(ctx context.Context, image string) int64 {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	n, err := s.reg.PullSize(ctx, registry.ParseRef(image))
	if err != nil {
		return 0
	}
	return n
}

// schedulePrune queues the image an update left unused for removal once the rollback window that
// might still want it has passed. With retention off nothing wants it, so it is due at once.
func (s *Server) schedulePrune(host, imageID, repo string, recID int) {
	if s.set.Get().KeepReplacedImages {
		return
	}
	s.store.SchedulePrune(store.PendingPrune{
		Host: host, ImageID: imageID, Repo: repo, RecordIDs: []int{recID},
		DueAt: time.Now().Add(s.set.RollbackWindow()),
	})
}

// sweepImages removes, every five minutes, the queued images whose time has come.
func (s *Server) sweepImages() {
	for range time.Tick(5 * time.Minute) {
		s.pruneDue(context.Background(), time.Now())
	}
}

func (s *Server) pruneDue(ctx context.Context, now time.Time) {
	if s.set.Get().KeepReplacedImages {
		return // turned off: queued entries wait, and nothing new is queued
	}
	var keep map[string]string // built on first local use; it costs a docker call per rollback point
	for _, p := range s.store.DuePrunes(now) {
		if now.Sub(p.DueAt) > pruneGiveUp {
			note := "Belay gave up removing the image this update replaced"
			if p.Note != "" {
				note += " (" + p.Note + ")"
			}
			s.store.ResolvePrune(p.Host, p.ImageID, nil, note+"; remove it by hand if it is not needed", true)
			continue
		}
		if p.Host != "" {
			s.sendPrune(p, now)
			continue
		}
		if s.jobs.anyRunning("") {
			return // something is updating here; its rollback may need any of these
		}
		if keep == nil {
			keep = s.keepImages(ctx, now)
		}
		pr := agent.Prune(ctx, p.ImageID, p.Repo, keep)
		s.finishPrune(p, cluster.PruneReport{ImageID: p.ImageID, Removed: pr.Removed, Bytes: pr.Bytes,
			Gone: pr.Gone, Kept: pr.Kept, Retry: pr.Retry})
	}
}

// keepImages maps the local image IDs a removal must spare to the reason: everything a live
// rollback point could still revert to or from, and the self-update's retained images.
func (s *Server) keepImages(ctx context.Context, now time.Time) map[string]string {
	keep := map[string]string{}
	for _, pt := range s.store.Rollbacks(now) {
		why := "the rollback for " + pt.Service + " still needs it until " + pt.ExpiresAt.Format("Jan 2, 15:04")
		for _, id := range []string{pt.FromImageID, agent.ImageID(ctx, pt.FromImage), agent.ImageID(ctx, pt.ToImage)} {
			if id != "" {
				keep[id] = why
			}
		}
	}
	st := selfupdate.LoadState(s.cfg.DataDir)
	if st.FromImage != "" {
		keep[st.FromImage] = "it is Belay's own previous image (self-update)"
	}
	if st.Rollback != nil && st.Rollback.Image != "" {
		keep[st.Rollback.Image] = "Belay's self-update rollback still needs it"
	}
	return keep
}

// sendPrune hands a due removal to its agent — only to one that said it understands the command,
// because an agent treats an unknown kind as an update.
func (s *Server) sendPrune(p store.PendingPrune, now time.Time) {
	if p.CommandID != "" && now.Sub(p.SentAt) < pruneResend {
		return // asked recently; wait for the answer
	}
	s.agentsMu.Lock()
	c := s.agents[p.Host]
	ready := c != nil && c.can(cluster.CapPrune) && now.Sub(c.lastSeen) <= 90*time.Second
	s.agentsMu.Unlock()
	if !ready {
		return // away, or too old to ask: stays queued until pruneGiveUp
	}
	if s.jobs.anyRunning(p.Host) {
		return
	}
	cmd := cluster.Command{ID: newToken()[:12], Kind: cluster.KindPrune, ImageID: p.ImageID, Repo: p.Repo}
	select {
	case c.queue <- cmd:
		s.store.MarkPruneSent(p.Host, p.ImageID, cmd.ID, now)
	default:
	}
}

// finishPrune records one removal attempt, local or reported by an agent.
func (s *Server) finishPrune(p store.PendingPrune, r cluster.PruneReport) {
	where := p.Host
	if where == "" {
		where = s.hostName
	}
	switch {
	case len(r.Removed) > 0:
		log.Printf("prune %s: removed %s (%s freed)", where, strings.Join(r.Removed, ", "), agent.HumanBytes(r.Bytes))
		s.store.ResolvePrune(p.Host, p.ImageID, &store.FreedImage{
			ImageID: p.ImageID, Refs: r.Removed, Bytes: r.Bytes, At: time.Now(),
		}, "", true)
	case r.Gone:
		s.store.ResolvePrune(p.Host, p.ImageID, nil, "", true) // removed by someone else already
	default:
		log.Printf("prune %s: kept %s: %s", where, p.ImageID, r.Kept)
		note := "kept the image this update replaced: " + r.Kept
		if r.Retry {
			note += " (will retry)"
		}
		s.store.ResolvePrune(p.Host, p.ImageID, nil, note, !r.Retry)
	}
}
