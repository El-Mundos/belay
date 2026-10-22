// Package journal records updates while they are in flight, so one that dies with the process — a
// Docker upgrade restarting every container mid-batch, an OOM kill, a reboot — is noticed on the
// next start instead of vanishing.
//
// Without it an interrupted update leaves no record at all, and can leave worse: on galaxy
// (2026-10-22) `apt upgrade` restarted Docker 20 seconds into an update-all, after authentik-server's
// compose tag had been rewritten but before its container was recreated. Belay read the file, saw the
// new version, and treated the server as done — leaving it a version behind its worker.
package journal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Entry is one update that has started and not yet been recorded.
type Entry struct {
	Project     string // display name (the record's Project)
	File        string // compose file
	Service     string
	From        string // ref in the compose file before the update (a digest ref for a rebase)
	To          string // target ref
	FromImageID string // image the container ran when the update began
	Rebase      bool
	Snapshot    string // volume snapshot taken for it, once there is one
	CommandID   string `json:",omitempty"` // agent: the server command this came from
	Started     time.Time
}

// Key identifies an entry: one update per service at a time is all Belay ever runs.
func (e Entry) Key() string { return e.File + "\x00" + e.Service }

// Journal is a small JSON file of in-flight entries. An empty path makes every call a no-op, for
// runs without a data directory.
type Journal struct {
	mu   sync.Mutex
	path string
}

// Open returns the journal kept at dir/inflight.json ("" dir = disabled).
func Open(dir string) *Journal {
	if dir == "" {
		return &Journal{}
	}
	return &Journal{path: filepath.Join(dir, "inflight.json")}
}

// Begin records that an update is starting. It is written before the compose file is touched; if
// the write fails the update still runs, it just could not be recovered — so the error is returned
// for the caller to log, never to block on.
func (j *Journal) Begin(e Entry) error {
	if e.Started.IsZero() {
		e.Started = time.Now()
	}
	return j.change(func(m map[string]Entry) { m[e.Key()] = e })
}

// SetSnapshot attaches the volume snapshot an update took, so recovery can keep it.
func (j *Journal) SetSnapshot(key, snapshot string) error {
	return j.change(func(m map[string]Entry) {
		if e, ok := m[key]; ok {
			e.Snapshot = snapshot
			m[key] = e
		}
	})
}

// End removes an entry once its outcome is recorded.
func (j *Journal) End(key string) error {
	return j.change(func(m map[string]Entry) { delete(m, key) })
}

// Pending returns the entries left by a process that did not finish them.
func (j *Journal) Pending() []Entry {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []Entry
	for _, e := range j.load() {
		out = append(out, e)
	}
	return out
}

func (j *Journal) change(fn func(map[string]Entry)) error {
	if j.path == "" {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	m := j.load()
	fn(m)
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_ = os.MkdirAll(filepath.Dir(j.path), 0o755)
	tmp := j.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("update journal: %w", err)
	}
	return os.Rename(tmp, j.path)
}

func (j *Journal) load() map[string]Entry {
	m := map[string]Entry{}
	if j.path == "" {
		return m
	}
	if b, err := os.ReadFile(j.path); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}
