package server

import (
	"context"
	"log"
	"time"

	"github.com/El-Mundos/belay/internal/agent"
	"github.com/El-Mundos/belay/internal/journal"
	"github.com/El-Mundos/belay/internal/notify"
	"github.com/El-Mundos/belay/internal/store"
)

// recoverInterrupted settles the updates the previous process was killed in the middle of, and puts
// each on the record as a failure so it shows up where a retry is one click away. Docker is often
// still coming back when this runs (a daemon restart is what killed us), so it waits for it.
func (s *Server) recoverInterrupted() {
	ctx := context.Background()
	agent.RecoverAll(ctx, s.journal, 3*time.Minute, func(e journal.Entry, r agent.Recovery) {
		note := r.Note
		if e.Snapshot != "" {
			// Its data may be mid-restore, or the only copy of what came before: keep it for the
			// rollback window like any other rollback point, rather than guess.
			s.retainRollback(ctx, Project{Name: e.Project, File: e.File}, e.Service, e.From, e.FromImageID, e.To, e.Snapshot)
			note += " Its pre-update data snapshot is kept for the rollback window."
		}
		log.Printf("recovered %s/%s: %s", e.Project, e.Service, note)
		s.store.Add(store.Record{
			Project: e.Project, Service: e.Service, From: e.From, To: e.To,
			Outcome: string(r.Outcome), Err: note, Duration: "-",
		})
		s.notify.Failure(notify.Event{
			Project: e.Project, Service: e.Service, From: e.From, To: e.To,
			Outcome: string(r.Outcome), Error: note,
		})
	})
}
