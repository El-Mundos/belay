package server

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/El-Mundos/belay/internal/compose"
	"github.com/El-Mundos/belay/internal/config"
	"github.com/El-Mundos/belay/internal/registry"
)

// probeRow is one service on the settings page (pin state + optional health probe).
type probeRow struct {
	Idx     int
	PID     int
	Project string
	Service string
	Image   string
	Pinned  bool
	Type    string
	Target  string
	Expect  int
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	cur := s.set.Get()
	var rows []probeRow
	i := 0
	for _, p := range s.cfg.Projects {
		services, err := compose.Services(p.File)
		if err != nil {
			continue
		}
		for _, sv := range services {
			pr := cur.Probes[config.Key(p.File, sv.Name)]
			rows = append(rows, probeRow{
				Idx: i, PID: p.ID, Project: p.Name, Service: sv.Name, Image: sv.Image,
				Pinned: cur.Pins[config.Key(p.File, sv.Name)],
				Type:   pr.Type, Target: pr.Target, Expect: pr.Expect,
			})
			i++
		}
	}
	data := s.base(r, "settings")
	data["S"] = cur
	data["Services"] = rows
	data["N"] = i
	s.render(w, "settings", data)
}

func (s *Server) handleSaveSettings(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if r.FormValue("section") == "probes" {
		n, _ := strconv.Atoi(r.FormValue("n"))
		_ = s.set.Update(func(st *config.Settings) {
			for i := 0; i < n; i++ {
				pid, _ := strconv.Atoi(r.FormValue(fmt.Sprintf("pid_%d", i)))
				svc := r.FormValue(fmt.Sprintf("svc_%d", i))
				file := s.fileByID(pid)
				if file == "" || svc == "" {
					continue
				}
				key := config.Key(file, svc)
				typ := r.FormValue(fmt.Sprintf("ptype_%d", i))
				if typ == "" || typ == "none" {
					delete(st.Probes, key)
					continue
				}
				expect, _ := strconv.Atoi(r.FormValue(fmt.Sprintf("pexpect_%d", i)))
				st.Probes[key] = config.Probe{Type: typ, Target: strings.TrimSpace(r.FormValue(fmt.Sprintf("ptarget_%d", i))), Expect: expect}
			}
		})
	} else {
		_ = s.set.Update(func(st *config.Settings) {
			st.Notify.Enabled = r.FormValue("notify_enabled") == "on"
			st.Notify.URL = strings.TrimSpace(r.FormValue("notify_url"))
			st.Notify.Kind = r.FormValue("notify_kind")
			st.Notify.OnFailure = r.FormValue("on_failure") == "on"
			st.Notify.OnNewVersion = r.FormValue("on_new_version") == "on"
			st.AutoCheckHours = atoiDefault(r.FormValue("auto_check_hours"), 0)
			st.RollbackWindowHours = atoiDefault(r.FormValue("rollback_window_hours"), 24)
		})
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// handleTestNotify sends a test notification using the CURRENTLY SAVED settings and returns a snippet.
func (s *Server) handleTestNotify(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.notify.Test(s.set.Get().Notify); err != nil {
		fmt.Fprintf(w, `<span class="err">❌ %s</span>`, template.HTMLEscapeString(err.Error()))
		return
	}
	fmt.Fprint(w, `<span class="ok">✅ test notification sent — check your device</span>`)
}

// handlePin pins/unpins a service (keyed by its compose file, so it works for local + remote).
func (s *Server) handlePin(w http.ResponseWriter, r *http.Request) {
	file := r.FormValue("file")
	if file == "" { // back-compat: local project id
		if p, ok := s.project(r.FormValue("p")); ok {
			file = p.File
		}
	}
	svc := r.FormValue("s")
	if file == "" || svc == "" {
		http.Error(w, "missing file/service", http.StatusBadRequest)
		return
	}
	pinned := r.FormValue("pinned") == "1"
	_ = s.set.SetPin(file, svc, pinned)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	writePinButton(w, file, svc, pinned)
}

// writePinButton renders the pin/unpin toggle for a service (proper button + short confirm).
func writePinButton(w http.ResponseWriter, file, svc string, pinned bool) {
	next, label, cls, confirm := "1", "Pin", "pin-btn", "Pin "+svc+" at its current version?"
	if pinned {
		next, label, cls, confirm = "0", "Pinned", "pin-btn on", "Unpin "+svc+"?"
	}
	fmt.Fprintf(w, `<button class="%s" hx-post="/pin" hx-vals='{"file":%q,"s":%q,"pinned":"%s"}' hx-swap="outerHTML" hx-confirm="%s" data-ok-label="%s" title="Pinned services are skipped by update-all and auto-check"><svg class="pin-ico" viewBox="0 0 16 16" width="12" height="12" aria-hidden="true"><path d="M9.5 1.5l5 5-2 .5-3 3-.5 3-1.5-1.5L2.5 14l3.5-4.5L4.5 8l3-3 .5-2z" fill="currentColor"/></svg>%s</button>`,
		cls, file, svc, next, template.HTMLEscapeString(confirm), label, label)
}

// ---- auto-check ----

func (s *Server) autoCheckLoop() {
	for {
		every := s.set.AutoCheckEvery()
		if every <= 0 {
			time.Sleep(time.Minute) // disabled; re-poll settings
			continue
		}
		s.runAutoCheck()
		slept := time.Duration(0)
		for slept < every { // sleep in chunks so interval/toggle changes apply within ~a minute
			time.Sleep(time.Minute)
			slept += time.Minute
			if s.set.AutoCheckEvery() != every {
				break
			}
		}
	}
}

func (s *Server) runAutoCheck() {
	ctx := context.Background()
	for _, p := range s.cfg.Projects {
		services, err := compose.Services(p.File)
		if err != nil {
			continue
		}
		for _, sv := range services {
			if sv.Image == "" || !strings.Contains(sv.Image, ":") || s.set.Pinned(p.File, sv.Name) {
				continue
			}
			ref := registry.ParseRef(sv.Image)
			newer, comparable, err := s.reg.Newer(ctx, ref)
			key := config.Key(p.File, sv.Name)
			prev := s.getCheck(key)
			cr := checkResult{When: time.Now()}
			switch {
			case err != nil:
				cr.Err = err.Error()
			case comparable && len(newer) > 0:
				cr.Latest = newer[len(newer)-1]
				cr.Count = len(newer)
				cr.Target = strings.TrimSuffix(sv.Image, ":"+ref.Tag) + ":" + cr.Latest
			}
			s.setCheck(key, cr)
			if cr.Target != "" && cr.Target != prev.Target { // a newly-available version
				s.notify.NewVersion(p.Name, sv.Name, ref.Tag, cr.Latest)
			}
		}
	}
}

// ---- self-update ----

// selfUpdateWatch caches whether a newer belay image is available (checked at startup + every 30m).
func (s *Server) selfUpdateWatch() {
	if s.su == nil || !s.su.Enabled() {
		return
	}
	for {
		avail, err := s.su.Available(context.Background())
		if err == nil {
			s.mu.Lock()
			s.suAvail = avail
			s.mu.Unlock()
		}
		time.Sleep(30 * time.Minute)
	}
}

// handleSelfUpdate recreates the belay container from the current image tag. Belay is torn down and
// replaced by a detached helper moments after this responds, so we render a "reconnecting" page first.
func (s *Server) handleSelfUpdate(w http.ResponseWriter, r *http.Request) {
	if s.su == nil || !s.su.Enabled() {
		http.Error(w, "self-update not available (belay is not running in a detectable container)", http.StatusBadRequest)
		return
	}
	if err := s.su.Apply(r.Context()); err != nil {
		http.Error(w, "self-update failed to launch: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<!doctype html><meta charset=utf-8><title>Belay updating…</title>
<meta http-equiv="refresh" content="15;url=/">
<style>body{font:15px/1.6 -apple-system,sans-serif;background:#0e1014;color:#e6e8ee;display:grid;place-items:center;height:100vh;margin:0}</style>
<div style="text-align:center"><h2>⚓ Belay is updating itself…</h2>
<p>The container is being recreated on the new image. This page will reconnect in ~15s.</p></div>`)
}

// ---- metrics ----

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	totals := s.store.Totals()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprint(w, "# HELP belay_pending_updates Services with a newer stable version available (from auto-check).\n")
	fmt.Fprint(w, "# TYPE belay_pending_updates gauge\n")
	fmt.Fprintf(w, "belay_pending_updates %d\n", s.pendingCount())
	fmt.Fprint(w, "# HELP belay_failed_current Services whose latest update attempt is still failed.\n")
	fmt.Fprint(w, "# TYPE belay_failed_current gauge\n")
	fmt.Fprintf(w, "belay_failed_current %d\n", s.store.FailedCount())
	fmt.Fprint(w, "# HELP belay_active_jobs Updates currently in progress.\n")
	fmt.Fprint(w, "# TYPE belay_active_jobs gauge\n")
	fmt.Fprintf(w, "belay_active_jobs %d\n", s.jobs.active())
	fmt.Fprint(w, "# HELP belay_update_attempts_total Update attempts recorded, by outcome.\n")
	fmt.Fprint(w, "# TYPE belay_update_attempts_total counter\n")
	for _, o := range []string{"updated", "rolled_back", "error", "reverted", "skipped"} {
		fmt.Fprintf(w, "belay_update_attempts_total{outcome=%q} %d\n", o, totals[o])
	}
}

// ---- small helpers ----

func (s *Server) fileByID(id int) string {
	for _, p := range s.cfg.Projects {
		if p.ID == id {
			return p.File
		}
	}
	return ""
}

func (s *Server) getCheck(key string) checkResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checks[key]
}

func (s *Server) setCheck(key string, cr checkResult) {
	s.mu.Lock()
	s.checks[key] = cr
	s.mu.Unlock()
}

func (s *Server) pendingCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.checks {
		if c.Target != "" {
			n++
		}
	}
	return n
}

func atoiDefault(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return n
	}
	return def
}
