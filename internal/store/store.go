// Package store keeps a record of update attempts so the UI can show Failed and History tabs,
// plus the retained "rollback points" that back the manual-rollback button.
// In-memory for now (capped ring); a persistent backend can implement the same shape later.
package store

import (
	"sync"
	"time"
)

// Record is one update attempt.
type Record struct {
	ID       int
	Time     time.Time
	Project  string
	Service  string
	From     string
	To       string
	Outcome  string // updated | rolled_back | error | skipped
	Err      string
	Logs     string
	Duration string
}

// RollbackPoint is a retained snapshot + previous image that lets the user manually roll a
// successful update back for a limited window. One per service; a newer update replaces it.
type RollbackPoint struct {
	Project   string
	Service   string
	File      string
	FromImage string // the previous image — what a rollback reverts TO
	ToImage   string // the image currently running (the update that was applied)
	Snapshot  string // snapshot id kept alive in the belay-snapshots volume ("" if none)
	CreatedAt time.Time
	ExpiresAt time.Time
}

type Store struct {
	mu        sync.Mutex
	recs      []Record
	seq       int
	max       int
	rollbacks map[string]RollbackPoint // key: project + service
}

func key(project, service string) string { return project + "\x00" + service }

func New() *Store { return &Store{max: 500, rollbacks: map[string]RollbackPoint{}} }

// Add stamps and stores a record (newest kept, oldest dropped past max).
func (s *Store) Add(r Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	r.ID = s.seq
	r.Time = time.Now()
	s.recs = append(s.recs, r)
	if len(s.recs) > s.max {
		s.recs = s.recs[len(s.recs)-s.max:]
	}
}

// Failed returns the most recent attempt for each service whose LATEST attempt failed
// (rolled_back or error), newest first. A service drops off the list — and out of the badge
// count — as soon as a later attempt for it succeeds.
func (s *Store) Failed() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	var out []Record
	for i := len(s.recs) - 1; i >= 0; i-- { // newest first
		r := s.recs[i]
		k := key(r.Project, r.Service)
		if seen[k] {
			continue // only the latest attempt per service counts
		}
		seen[k] = true
		if r.Outcome == "rolled_back" || r.Outcome == "error" {
			out = append(out, r)
		}
	}
	return out
}

// FailedCount is the number of services currently in a failed state (the red badge).
func (s *Store) FailedCount() int { return len(s.Failed()) }

// Succeeded returns successful updates and manual reverts, newest first (the History tab /
// log of good upgrades).
func (s *Store) Succeeded() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Record
	for i := len(s.recs) - 1; i >= 0; i-- {
		if o := s.recs[i].Outcome; o == "updated" || o == "reverted" {
			out = append(out, s.recs[i])
		}
	}
	return out
}

// SetRollback stores/replaces the rollback point for a service and returns the previous point
// (if any) so the caller can discard its now-superseded snapshot.
func (s *Store) SetRollback(p RollbackPoint) (old RollbackPoint, had bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(p.Project, p.Service)
	old, had = s.rollbacks[k]
	s.rollbacks[k] = p
	return
}

// RollbackFor returns the active rollback point for a service, if one exists.
func (s *Store) RollbackFor(project, service string) (RollbackPoint, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.rollbacks[key(project, service)]
	return p, ok
}

// TakeRollback removes and returns a rollback point — used when the user triggers a rollback,
// so it can't be triggered twice.
func (s *Store) TakeRollback(project, service string) (RollbackPoint, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(project, service)
	p, ok := s.rollbacks[k]
	if ok {
		delete(s.rollbacks, k)
	}
	return p, ok
}

// SweepExpired removes rollback points past their window and returns them so the caller can
// discard the underlying snapshots.
func (s *Store) SweepExpired(now time.Time) []RollbackPoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	var expired []RollbackPoint
	for k, p := range s.rollbacks {
		if now.After(p.ExpiresAt) {
			expired = append(expired, p)
			delete(s.rollbacks, k)
		}
	}
	return expired
}
