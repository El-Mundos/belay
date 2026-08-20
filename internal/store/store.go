// Package store keeps a record of update attempts so the UI can show Failed and History tabs,
// plus the retained "rollback points" that back the manual-rollback button.
// In-memory for now (capped ring); a persistent backend can implement the same shape later.
package store

import (
	"encoding/json"
	"os"
	"path/filepath"
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
	path      string                   // JSON file for persistence ("" = in-memory only)
}

func key(project, service string) string { return project + "\x00" + service }

func New() *Store { return &Store{max: 500, rollbacks: map[string]RollbackPoint{}} }

// Open is New plus loading any previously-persisted records + rollback points from path, and
// saving future changes back to it. An empty path is in-memory only (same as New).
func Open(path string) *Store {
	s := New()
	s.path = path
	s.load()
	return s
}

// persisted is the on-disk shape of store.json. The update history lives under "recs" (not
// "records") and rollback points under "rollbacks" — worth stating, because reading the file with
// the wrong key yields an empty list rather than an error, which reads as "belay has no history"
// when in fact it has plenty.
type persisted struct {
	Seq       int                      `json:"seq"`
	Recs      []Record                 `json:"recs"`
	Rollbacks map[string]RollbackPoint `json:"rollbacks"`
}

func (s *Store) load() {
	if s.path == "" {
		return
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var p persisted
	if json.Unmarshal(b, &p) != nil {
		return
	}
	s.seq, s.recs = p.Seq, p.Recs
	if p.Rollbacks != nil {
		s.rollbacks = p.Rollbacks
	}
}

// save persists the current state; callers already hold s.mu.
func (s *Store) save() {
	if s.path == "" {
		return
	}
	b, err := json.Marshal(persisted{Seq: s.seq, Recs: s.recs, Rollbacks: s.rollbacks})
	if err != nil {
		return
	}
	if dir := filepath.Dir(s.path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		_ = os.Rename(tmp, s.path)
	}
}

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
	s.save()
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

// ByID returns a single attempt. The UI lists carry only what fits on a card and fetch the bulky
// parts (logs, full error) per record on demand, which is what this backs.
func (s *Store) ByID(id int) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.recs) - 1; i >= 0; i-- {
		if s.recs[i].ID == id {
			return s.recs[i], true
		}
	}
	return Record{}, false
}

// Totals counts all recorded attempts by outcome (for metrics).
func (s *Store) Totals() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := map[string]int{}
	for _, r := range s.recs {
		m[r.Outcome]++
	}
	return m
}

// SetRollback stores/replaces the rollback point for a service and returns the previous point
// (if any) so the caller can discard its now-superseded snapshot.
func (s *Store) SetRollback(p RollbackPoint) (old RollbackPoint, had bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(p.Project, p.Service)
	old, had = s.rollbacks[k]
	s.rollbacks[k] = p
	s.save()
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
		s.save()
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
	if len(expired) > 0 {
		s.save()
	}
	return expired
}
