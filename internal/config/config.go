// Package config holds Belay's user-editable settings — notifications, auto-check interval, the
// rollback window, per-service pins and health probes — and persists them to a JSON file on disk so
// they survive restarts. It is safe for concurrent use.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Notify configures where update events are sent. URL is a webhook compatible with ntfy, Discord,
// Slack, Gotify, or any generic receiver.
type Notify struct {
	Enabled      bool   `json:"enabled"`
	URL          string `json:"url"`
	Kind         string `json:"kind"`           // auto | ntfy | discord | slack | gotify | generic
	OnFailure    bool   `json:"on_failure"`     // notify when an update fails / rolls back
	OnNewVersion bool   `json:"on_new_version"` // notify when auto-check finds a newer version
	OnSuccess    bool   `json:"on_success"`     // notify when an update succeeds
}

// Registry holds credentials for a private container registry, used both for the version-check API
// calls and (via a generated docker config.json) for the actual pull. Token is a password or PAT.
type Registry struct {
	Host     string `json:"host"`     // docker.io | ghcr.io | registry.example.com:5000
	Username string `json:"username"`
	Token    string `json:"token"`
}

// Probe is an optional health check for a service, forming the middle rung of the health ladder
// (docker HEALTHCHECK → this probe → stayed-running). Target is reachable from the Belay container.
type Probe struct {
	Type   string `json:"type"`   // "" (none) | http | tcp
	Target string `json:"target"` // http URL (e.g. http://wg-easy:51821) or host:port for tcp
	Expect int    `json:"expect"` // expected HTTP status for http probes (0 => any 2xx/3xx)
}

// Settings is the whole persisted config document.
type Settings struct {
	RollbackWindowHours int              `json:"rollback_window_hours"` // 0 = retention off
	AutoCheckHours      int              `json:"auto_check_hours"`      // 0 = auto-check off
	AllowMajor          bool             `json:"allow_major"`           // include major version bumps in update-all
	QuietHours          bool             `json:"quiet_hours"`           // suppress notifications during the window
	QuietStart          int              `json:"quiet_start"`           // hour 0-23
	QuietEnd            int              `json:"quiet_end"`             // hour 0-23
	Concurrency         int              `json:"concurrency"`           // parallel updates in update-all (min 1)
	MetricsToken        string           `json:"metrics_token"`         // if set, /metrics requires ?token= or Bearer
	Notify              Notify           `json:"notify"`
	Registries          []Registry       `json:"registries"` // private-registry credentials for checks + pulls
	Pins                map[string]bool  `json:"pins"`       // key = project\x00service → ignored for updates
	Probes              map[string]Probe `json:"probes"`     // key = project\x00service
}

// canonicalRegistry normalizes a registry host for matching, collapsing Docker Hub's several spellings
// to a single "docker.io" token.
func canonicalRegistry(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	h = strings.TrimPrefix(h, "https://")
	h = strings.TrimPrefix(h, "http://")
	h = strings.TrimSuffix(h, "/v1")
	h = strings.TrimSuffix(h, "/")
	switch h {
	case "docker.io", "index.docker.io", "registry-1.docker.io":
		return "docker.io"
	}
	return h
}

// RegistryCred returns the configured credentials for a registry host (matching Docker Hub's aliases),
// used by the registry client's auth dance. ok is false when nothing matches or the entry is blank.
func (s Settings) RegistryCred(host string) (user, token string, ok bool) {
	want := canonicalRegistry(host)
	for _, r := range s.Registries {
		if r.Username != "" && canonicalRegistry(r.Host) == want {
			return r.Username, r.Token, true
		}
	}
	return "", "", false
}

// InQuietHours reports whether the given hour (0-23) is inside the notification quiet window.
func (s Settings) InQuietHours(hour int) bool {
	if !s.QuietHours || s.QuietStart == s.QuietEnd {
		return false
	}
	if s.QuietStart < s.QuietEnd {
		return hour >= s.QuietStart && hour < s.QuietEnd
	}
	return hour >= s.QuietStart || hour < s.QuietEnd // window wraps midnight
}

// Store wraps Settings with a file path and a mutex.
type Store struct {
	mu   sync.RWMutex
	path string
	s    Settings
}

// Key builds the map key used for Pins and Probes.
func Key(project, service string) string { return project + "\x00" + service }

// Defaults returns the settings a fresh install starts with.
func Defaults() Settings {
	return Settings{
		RollbackWindowHours: 24,
		AutoCheckHours:      0,
		Concurrency:         1,
		Notify:              Notify{Enabled: true, Kind: "auto", OnFailure: true, OnNewVersion: true},
		Pins:                map[string]bool{},
		Probes:              map[string]Probe{},
	}
}

// Open loads settings from path (falling back to defaults if the file is missing or unreadable).
// The bool reports whether an existing settings file was loaded, so first-run callers can seed
// values from flags/env. An empty path keeps settings in memory only.
func Open(path string) (*Store, bool) {
	st := &Store{path: path, s: Defaults()}
	if path == "" {
		return st, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return st, false
	}
	var s Settings
	if json.Unmarshal(b, &s) != nil {
		return st, false
	}
	st.s = normalize(s)
	return st, true
}

func normalize(s Settings) Settings {
	if s.Pins == nil {
		s.Pins = map[string]bool{}
	}
	if s.Probes == nil {
		s.Probes = map[string]Probe{}
	}
	if s.Notify.Kind == "" {
		s.Notify.Kind = "auto"
	}
	return s
}

// Get returns a copy of the current settings.
func (st *Store) Get() Settings {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.s
}

// Update applies fn to the settings under lock and persists the result.
func (st *Store) Update(fn func(*Settings)) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	fn(&st.s)
	st.s = normalize(st.s)
	return st.save()
}

// Pinned reports whether a service is pinned (ignored for updates).
func (st *Store) Pinned(project, service string) bool {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.s.Pins[Key(project, service)]
}

// SetPin pins/unpins a service and persists.
func (st *Store) SetPin(project, service string, pinned bool) error {
	return st.Update(func(s *Settings) {
		if pinned {
			s.Pins[Key(project, service)] = true
		} else {
			delete(s.Pins, Key(project, service))
		}
	})
}

// ProbeFor returns the configured probe for a service, if any.
func (st *Store) ProbeFor(project, service string) (Probe, bool) {
	st.mu.RLock()
	defer st.mu.RUnlock()
	p, ok := st.s.Probes[Key(project, service)]
	return p, ok && p.Type != ""
}

// RollbackWindow is the retention duration (0 => off).
func (st *Store) RollbackWindow() time.Duration {
	return time.Duration(st.Get().RollbackWindowHours) * time.Hour
}

// AutoCheckEvery is the auto-check interval (0 => off).
func (st *Store) AutoCheckEvery() time.Duration {
	return time.Duration(st.Get().AutoCheckHours) * time.Hour
}

func (st *Store) save() error {
	if st.path == "" {
		return nil
	}
	if dir := filepath.Dir(st.path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	b, err := json.MarshalIndent(st.s, "", "  ")
	if err != nil {
		return err
	}
	tmp := st.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, st.path)
}
