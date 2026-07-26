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

func (s *Server) handlePin(w http.ResponseWriter, r *http.Request) {
	p, ok := s.project(r.FormValue("p"))
	if !ok {
		http.Error(w, "unknown project", http.StatusBadRequest)
		return
	}
	svc := r.FormValue("s")
	pinned := r.FormValue("pinned") == "1"
	_ = s.set.SetPin(p.File, svc, pinned)
	// return the toggled button so htmx can swap it in place
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	writePinButton(w, p.ID, svc, pinned)
}

// writePinButton renders the pin/unpin toggle for a service.
func writePinButton(w http.ResponseWriter, pid int, svc string, pinned bool) {
	next := "1"
	label, cls := "📌 pin", "btn ghost"
	if pinned {
		next = "0"
		label, cls = "📌 pinned", "btn ghost pinned-on"
	}
	fmt.Fprintf(w, `<button class="%s" hx-post="/pin" hx-vals='{"p":"%d","s":"%s","pinned":"%s"}' hx-swap="outerHTML" title="Pinned services are skipped by update-all and auto-check">%s</button>`,
		cls, pid, template.JSEscapeString(svc), next, label)
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
