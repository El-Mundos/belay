// Package store keeps a record of update attempts so the UI can show Failed and History tabs,
// plus the retained "rollback points" that back the manual-rollback button.
// In-memory for now (capped ring); a persistent backend can implement the same shape later.
package store

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
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

	// Dismissed takes a failure off the Failed worklist without destroying it. Failed answers
	// "what still needs me?", History answers "what happened" -- so acknowledging a failure you
	// are not going to act on should change the first and never the second.
	Dismissed bool `json:",omitempty"`

	// Freed lists the images this attempt left unused that Belay has since removed, and PruneNote
	// says why one was kept instead. Both arrive long after the attempt itself: removal waits for
	// the rollback window, so the record is where the answer to "what did this cost me?" lands.
	Freed     []FreedImage `json:",omitempty"`
	PruneNote string       `json:",omitempty"`
}

// FreedImage is one image Belay removed after an update replaced it.
type FreedImage struct {
	ImageID string
	Refs    []string // the tags removed with it; empty for a build left dangling by a rebase
	Bytes   int64    // free space gained on the image store (0 = could not be measured)
	At      time.Time
}

// PendingPrune is an image an update left unused, waiting for its rollback window to pass before
// Belay removes it. Keyed by host + image ID rather than by service: a server and its worker share
// one image (authentik), and both updates have to land on the same entry or it is removed twice.
type PendingPrune struct {
	Host      string // "" = this host; otherwise the agent that must do the removal
	ImageID   string // sha256:... — an ID, because by the time this is due the tag may have moved
	Repo      string // registry/repository of the replaced ref; tags outside it are not Belay's
	RecordIDs []int  // the update records the outcome is reported on
	DueAt     time.Time
	Note      string // the latest reason it was kept, while it is being retried

	// CommandID / SentAt track a removal handed to an agent and not yet answered.
	CommandID string    `json:",omitempty"`
	SentAt    time.Time `json:",omitempty"`
}

// RollbackPoint is a retained snapshot + previous image that lets the user manually roll a
// successful update back for a limited window. One per service; a newer update replaces it.
type RollbackPoint struct {
	Project   string
	Service   string
	File      string
	FromImage string // the previous image — what a rollback reverts TO
	// FromImageID is the image ID FromImage resolved to when the update ran. The ref alone cannot
	// protect the old image from removal: its tag may have moved, or been shared, since then.
	FromImageID string `json:",omitempty"`
	ToImage     string // the image currently running (the update that was applied)
	Snapshot    string // snapshot id kept alive in the belay-snapshots volume ("" if none)
	CreatedAt   time.Time
	ExpiresAt   time.Time
}

type Store struct {
	mu        sync.Mutex
	recs      []Record
	seq       int
	max       int
	rollbacks map[string]RollbackPoint // key: project + service
	prunes    map[string]PendingPrune  // key: host + image id
	path      string                   // JSON file for persistence ("" = in-memory only)
}

func key(project, service string) string { return project + "\x00" + service }

func New() *Store {
	return &Store{max: 500, rollbacks: map[string]RollbackPoint{}, prunes: map[string]PendingPrune{}}
}

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
	Prunes    map[string]PendingPrune  `json:"prunes,omitempty"`
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
	if p.Prunes != nil {
		s.prunes = p.Prunes
	}
}

// save persists the current state; callers already hold s.mu.
func (s *Store) save() {
	if s.path == "" {
		return
	}
	b, err := json.Marshal(persisted{Seq: s.seq, Recs: s.recs, Rollbacks: s.rollbacks, Prunes: s.prunes})
	if err != nil {
		log.Printf("store: encode: %v", err)
		return
	}
	if dir := filepath.Dir(s.path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	// A failed write used to vanish without a word. The state is still in memory and the next
	// successful save carries it, but a restart before then loses it, and on a full disk (galaxy,
	// 2026-10-22) every save fails until someone frees space — so say so in the log.
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		_ = os.Remove(tmp) // a half-written tmp only holds on to more of the full disk
		log.Printf("store: save %s: %v (history is kept in memory until a save succeeds)", s.path, err)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		log.Printf("store: save %s: %v", s.path, err)
	}
}

// Add stamps and stores a record (newest kept, oldest dropped past max) and returns its ID.
func (s *Store) Add(r Record) int {
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
	return r.ID
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
		if r.Dismissed {
			continue // acknowledged; it stays in History but is no longer asking for attention
		}
		if r.Outcome == "rolled_back" || r.Outcome == "error" {
			out = append(out, r)
		}
	}
	return out
}

// Attempts returns every recorded attempt for one service, newest first. The Failed tab shows the
// current failure with its earlier tries underneath: retries are a sequence against one service,
// and the key they share is the only thing needed to group them.
func (s *Store) Attempts(project, service string) []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Record
	for i := len(s.recs) - 1; i >= 0; i-- {
		if s.recs[i].Project == project && s.recs[i].Service == service {
			out = append(out, s.recs[i])
		}
	}
	return out
}

// Dismiss acknowledges every recorded attempt for a service. A later attempt adds an undismissed
// record, so a service that fails again comes back on its own.
func (s *Store) Dismiss(project, service string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for i := range s.recs {
		if s.recs[i].Project == project && s.recs[i].Service == service && !s.recs[i].Dismissed {
			s.recs[i].Dismissed = true
			n++
		}
	}
	if n > 0 {
		s.save()
	}
	return n
}

// FailedCount is the number of services currently in a failed state (the red badge).
func (s *Store) FailedCount() int { return len(s.Failed()) }

// Succeeded returns what History shows, newest first: successful updates, manual reverts, and
// failures that were DISMISSED.
//
// A dismissed failure has to land somewhere. Failed is a worklist and dismissing removes it from
// there — but dropping it from History too would mean acknowledging a failure quietly deletes it,
// which is the opposite of what dismissing is for.
func (s *Store) Succeeded() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Record
	for i := len(s.recs) - 1; i >= 0; i-- {
		switch o := s.recs[i].Outcome; {
		case o == "updated" || o == "reverted":
			out = append(out, s.recs[i])
		case s.recs[i].Dismissed && (o == "rolled_back" || o == "error"):
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

// Rollbacks returns every rollback point still inside its window. The image prune reads these to
// know which old images a rollback could still need.
func (s *Store) Rollbacks(now time.Time) []RollbackPoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []RollbackPoint
	for _, p := range s.rollbacks {
		if now.Before(p.ExpiresAt) {
			out = append(out, p)
		}
	}
	return out
}

func pruneKey(host, imageID string) string { return host + "\x00" + imageID }

// SchedulePrune queues an image for removal, or folds another update into an entry already queued
// for the same image: the record joins its list and the later due time wins, so the image outlives
// every rollback window that might still want it.
func (s *Store) SchedulePrune(p PendingPrune) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := pruneKey(p.Host, p.ImageID)
	if cur, ok := s.prunes[k]; ok {
		p.RecordIDs = append(cur.RecordIDs, p.RecordIDs...)
		if cur.DueAt.After(p.DueAt) {
			p.DueAt = cur.DueAt
		}
	}
	s.prunes[k] = p
	s.save()
}

// DuePrunes returns the queued removals whose time has come, oldest first.
func (s *Store) DuePrunes(now time.Time) []PendingPrune {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []PendingPrune
	for _, p := range s.prunes {
		if !now.Before(p.DueAt) {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DueAt.Before(out[j].DueAt) })
	return out
}

// MarkPruneSent records that an agent was asked to remove an image, so the next sweep waits for
// its answer instead of asking again.
func (s *Store) MarkPruneSent(host, imageID, cmdID string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := pruneKey(host, imageID)
	if p, ok := s.prunes[k]; ok {
		p.CommandID, p.SentAt = cmdID, at
		s.prunes[k] = p
		s.save()
	}
}

// PruneByCommand finds the queued removal an agent's answer belongs to.
func (s *Store) PruneByCommand(cmdID string) (PendingPrune, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.prunes {
		if cmdID != "" && p.CommandID == cmdID {
			return p, true
		}
	}
	return PendingPrune{}, false
}

// ResolvePrune reports the outcome of a removal attempt on every record that queued it. done
// drops the entry; otherwise it stays queued for the next sweep, with note as the current reason.
// A note is only written when it changes, so an image kept for days does not rewrite the file on
// every sweep.
func (s *Store) ResolvePrune(host, imageID string, freed *FreedImage, note string, done bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := pruneKey(host, imageID)
	p, ok := s.prunes[k]
	if !ok {
		return
	}
	changed := done
	if done {
		delete(s.prunes, k)
	} else {
		if p.Note != note || p.CommandID != "" {
			changed = true
		}
		p.Note, p.CommandID, p.SentAt = note, "", time.Time{}
		s.prunes[k] = p
	}
	for i := range s.recs {
		if !containsInt(p.RecordIDs, s.recs[i].ID) {
			continue
		}
		if freed != nil {
			s.recs[i].Freed = append(s.recs[i].Freed, *freed)
			s.recs[i].PruneNote = ""
			changed = true
		} else if note != "" && s.recs[i].PruneNote != note {
			s.recs[i].PruneNote = note
			changed = true
		}
	}
	if changed {
		s.save()
	}
}

func containsInt(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
