package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestSchedulePruneMergesSharedImage(t *testing.T) {
	s := New()
	t0 := time.Now()
	server := s.Add(Record{Project: "authentik", Service: "server", Outcome: "updated"})
	worker := s.Add(Record{Project: "authentik", Service: "worker", Outcome: "updated"})
	s.SchedulePrune(PendingPrune{ImageID: "sha256:old", RecordIDs: []int{server}, DueAt: t0.Add(time.Hour)})
	s.SchedulePrune(PendingPrune{ImageID: "sha256:old", RecordIDs: []int{worker}, DueAt: t0.Add(2 * time.Hour)})

	if due := s.DuePrunes(t0.Add(90 * time.Minute)); len(due) != 0 {
		t.Fatalf("due before the later window ended: %+v", due)
	}
	due := s.DuePrunes(t0.Add(2 * time.Hour))
	if len(due) != 1 || len(due[0].RecordIDs) != 2 {
		t.Fatalf("want one entry covering both updates, got %+v", due)
	}

	s.ResolvePrune("", "sha256:old", &FreedImage{ImageID: "sha256:old", Refs: []string{"goauthentik/server:2026.8.4"}, Bytes: 2e9}, "", true)
	for _, id := range []int{server, worker} {
		if r, _ := s.ByID(id); len(r.Freed) != 1 {
			t.Errorf("record %d: Freed = %+v, want the removal reported", id, r.Freed)
		}
	}
	if len(s.DuePrunes(t0.Add(time.Hour*24))) != 0 {
		t.Error("a finished prune must leave the queue")
	}
}

func TestResolvePruneRetryKeepsEntry(t *testing.T) {
	s := New()
	id := s.Add(Record{Outcome: "updated"})
	s.SchedulePrune(PendingPrune{Host: "vps", ImageID: "sha256:x", RecordIDs: []int{id}})
	s.MarkPruneSent("vps", "sha256:x", "cmd1", time.Now())
	if _, ok := s.PruneByCommand("cmd1"); !ok {
		t.Fatal("sent prune not found by command id")
	}
	s.ResolvePrune("vps", "sha256:x", nil, "kept: in use", false)
	due := s.DuePrunes(time.Now())
	if len(due) != 1 || due[0].CommandID != "" || due[0].Note != "kept: in use" {
		t.Fatalf("retry should stay queued, unsent, with its reason: %+v", due)
	}
	if r, _ := s.ByID(id); r.PruneNote != "kept: in use" {
		t.Errorf("PruneNote = %q", r.PruneNote)
	}
}

func TestPrunesPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	s := Open(path)
	s.SchedulePrune(PendingPrune{ImageID: "sha256:p", Repo: "registry-1.docker.io/prom/prometheus"})
	if due := Open(path).DuePrunes(time.Now()); len(due) != 1 || due[0].Repo == "" {
		t.Fatalf("pending prunes lost across a restart: %+v", due)
	}
}

func TestRollbacksOnlyLive(t *testing.T) {
	s := New()
	now := time.Now()
	s.SetRollback(RollbackPoint{Project: "p", Service: "a", ExpiresAt: now.Add(time.Hour)})
	s.SetRollback(RollbackPoint{Project: "p", Service: "b", ExpiresAt: now.Add(-time.Hour)})
	if got := s.Rollbacks(now); len(got) != 1 || got[0].Service != "a" {
		t.Errorf("Rollbacks = %+v, want only the unexpired one", got)
	}
}
