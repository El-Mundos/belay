// Package store keeps a record of update attempts so the UI can show Failed and History tabs.
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

type Store struct {
	mu   sync.Mutex
	recs []Record
	seq  int
	max  int
}

func New() *Store { return &Store{max: 500} }

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

func (s *Store) filter(pred func(Record) bool) []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Record
	for i := len(s.recs) - 1; i >= 0; i-- { // newest first
		if pred(s.recs[i]) {
			out = append(out, s.recs[i])
		}
	}
	return out
}

// Failed returns rolled-back + errored attempts, newest first.
func (s *Store) Failed() []Record {
	return s.filter(func(r Record) bool { return r.Outcome == "rolled_back" || r.Outcome == "error" })
}

// Succeeded returns successful updates, newest first.
func (s *Store) Succeeded() []Record {
	return s.filter(func(r Record) bool { return r.Outcome == "updated" })
}
