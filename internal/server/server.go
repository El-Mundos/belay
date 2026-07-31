// Package server is Belay's web UI + controller: three tabs (Updates / Failed / History) backed by
// the safe-update engine (health-gated, auto-rollback). Every update attempt is recorded.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/El-Mundos/belay/internal/agent"
	"github.com/El-Mundos/belay/internal/compose"
	"github.com/El-Mundos/belay/internal/config"
	"github.com/El-Mundos/belay/internal/engine"
	"github.com/El-Mundos/belay/internal/health"
	"github.com/El-Mundos/belay/internal/notify"
	"github.com/El-Mundos/belay/internal/registry"
	"github.com/El-Mundos/belay/internal/selfupdate"
	"github.com/El-Mundos/belay/internal/store"
	"github.com/El-Mundos/belay/web"
	"path/filepath"
)

type Project struct {
	ID   int
	Name string
	File string
}

type Config struct {
	Addr           string
	Projects       []Project
	Password       string
	ForwardHeader  string
	NotifyWebhook  string
	Snapshot       bool
	Timeout        time.Duration
	MinUptime      time.Duration
	RollbackWindow time.Duration // seed for a fresh install; the settings page owns it thereafter
	DataDir        string        // where settings.json + store.json live ("" = in-memory only)
	AgentToken     string        // shared bearer token for remote agents ("" = multi-host disabled)
}

type Server struct {
	cfg     Config
	reg     *registry.Client
	eng     *engine.Engine
	store   *store.Store
	set     *config.Store
	notify  *notify.Notifier
	tpl     map[string]*template.Template
	mu      sync.Mutex
	sess    map[string]struct{}
	checks  map[string]checkResult // auto-check cache, key project\x00service
	jobs    *jobManager            // live activity panel (SSE)
	su      *selfupdate.Manager    // belay self-update (nil-safe if not in a container)
	suAvail bool                   // cached: a newer belay image is available

	agentsMu sync.Mutex
	agents   map[string]*agentConn // connected remote agents, by host
}

// checkResult is a cached "is there a newer version" answer from auto-check.
type checkResult struct {
	Latest, Target, Changelog, Err string
	Count                          int
	When                           time.Time
}

func New(cfg Config) *Server {
	if cfg.Timeout == 0 {
		cfg.Timeout = 90 * time.Second
	}
	if cfg.MinUptime == 0 {
		cfg.MinUptime = 10 * time.Second
	}

	// settings + persistent record store live under DataDir; seed a fresh install from flags/env
	storePath, setPath := "", ""
	if cfg.DataDir != "" {
		storePath = filepath.Join(cfg.DataDir, "store.json")
		setPath = filepath.Join(cfg.DataDir, "settings.json")
	}
	set, loaded := config.Open(setPath)
	if !loaded {
		_ = set.Update(func(s *config.Settings) {
			if cfg.RollbackWindow > 0 {
				s.RollbackWindowHours = int(cfg.RollbackWindow / time.Hour)
			}
			if cfg.NotifyWebhook != "" {
				s.Notify.URL = cfg.NotifyWebhook
			}
		})
	}

	funcs := template.FuncMap{"sub": func(a, b int) int { return a - b }}
	page := func(files ...string) *template.Template {
		return template.Must(template.New("").Funcs(funcs).ParseFS(web.FS, files...))
	}
	// health ladder: docker HEALTHCHECK -> the service's configured probe (from settings) -> stayed-running
	gate := health.Gate{
		Timeout:   cfg.Timeout,
		MinUptime: cfg.MinUptime,
		ProbeFor: func(r engine.Request) (health.Probe, bool) {
			p, ok := set.ProbeFor(r.Project, r.Service)
			return health.Probe{Type: p.Type, Target: p.Target, Expect: p.Expect}, ok
		},
	}
	eng := &engine.Engine{Deployer: agent.Local{}, Health: gate}
	if cfg.Snapshot {
		eng.Snapshot = agent.Snapshotter{} // snapshot volumes -> restore data on rollback
	}
	return &Server{
		cfg:    cfg,
		reg:    registry.New(),
		eng:    eng,
		store:  store.Open(storePath),
		set:    set,
		notify: notify.New(func() config.Notify { return set.Get().Notify }),
		tpl: map[string]*template.Template{
			"dashboard":    page("templates/layout.html", "templates/dashboard.html"),
			"failed":       page("templates/layout.html", "templates/failed.html"),
			"history":      page("templates/layout.html", "templates/history.html"),
			"login":        page("templates/layout.html", "templates/login.html"),
			"settings":     page("templates/layout.html", "templates/settings.html"),
			"status":       page("templates/status.html"),
			"result":       page("templates/result.html"),
			"activity":     page("templates/activity.html"),
			"hosts":        page("templates/layout.html", "templates/hosts.html"),
			"review":       page("templates/layout.html", "templates/review.html"),
			"reviewstatus": page("templates/reviewstatus.html"),
		},
		sess:   map[string]struct{}{},
		checks: map[string]checkResult{},
		jobs:   newJobManager(),
		su:     selfupdate.Detect(context.Background()),
		agents: map[string]*agentConn{},
	}
}

func (s *Server) Run() error {
	static, _ := fs.Sub(web.FS, "static")
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("GET /logout", s.handleLogout)
	mux.HandleFunc("GET /{$}", s.guard(s.handleDashboard))
	mux.HandleFunc("GET /failed", s.guard(s.handleFailed))
	mux.HandleFunc("GET /history", s.guard(s.handleHistory))
	mux.HandleFunc("GET /check", s.guard(s.handleCheck))
	mux.HandleFunc("GET /review", s.guard(s.handleReview))
	mux.HandleFunc("GET /review/check", s.guard(s.handleReviewCheck))
	mux.HandleFunc("POST /update", s.guard(s.handleUpdate))
	mux.HandleFunc("POST /update-all", s.guard(s.handleUpdateAll))
	mux.HandleFunc("POST /rollback", s.guard(s.handleRollback))
	mux.HandleFunc("GET /settings", s.guard(s.handleSettings))
	mux.HandleFunc("POST /settings", s.guard(s.handleSaveSettings))
	mux.HandleFunc("POST /settings/test", s.guard(s.handleTestNotify))
	mux.HandleFunc("POST /pin", s.guard(s.handlePin))
	mux.HandleFunc("GET /activity", s.guard(s.handleActivity))
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("POST /self-update", s.guard(s.handleSelfUpdate))
	mux.HandleFunc("GET /hosts", s.guard(s.handleHosts))
	mux.HandleFunc("POST /remote-update", s.guard(s.handleRemoteUpdate))
	// remote-agent endpoints (token-authed, not session-guarded)
	mux.HandleFunc("POST /agent/register", s.handleAgentRegister)
	mux.HandleFunc("GET /agent/poll", s.handleAgentPoll)
	mux.HandleFunc("POST /agent/result", s.handleAgentResult)

	go s.sweepRollbacks()  // discard snapshots whose retention window has passed
	go s.autoCheckLoop()   // periodic version checks (cache + notify), per settings
	go s.selfUpdateWatch() // cache whether a newer belay image is available

	if !s.authEnabled() {
		log.Printf("WARNING: no auth configured (set --password or --forward-header); bind to localhost only.")
	}
	log.Printf("belay server on http://%s  (%d project(s))", s.cfg.Addr, len(s.cfg.Projects))
	return http.ListenAndServe(s.cfg.Addr, mux)
}

// ---- auth ----

func (s *Server) authEnabled() bool { return s.cfg.Password != "" || s.cfg.ForwardHeader != "" }

func (s *Server) user(r *http.Request) string {
	if s.cfg.ForwardHeader != "" {
		if u := r.Header.Get(s.cfg.ForwardHeader); u != "" {
			return u
		}
	}
	if c, err := r.Cookie("belay_session"); err == nil {
		s.mu.Lock()
		_, ok := s.sess[c.Value]
		s.mu.Unlock()
		if ok {
			return "admin"
		}
	}
	return ""
}

func (s *Server) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.authEnabled() && s.user(r) == "" {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		h(w, r)
	}
}

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled() || s.user(r) != "" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	s.render(w, "login", map[string]any{})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Password == "" || r.FormValue("password") != s.cfg.Password {
		s.render(w, "login", map[string]any{"Error": "Incorrect password."})
		return
	}
	tok := newToken()
	s.mu.Lock()
	s.sess[tok] = struct{}{}
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "belay_session", Value: tok, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("belay_session"); err == nil {
		s.mu.Lock()
		delete(s.sess, c.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "belay_session", Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusFound)
}

// ---- shared page data ----

func (s *Server) base(r *http.Request, active string) map[string]any {
	m := map[string]any{
		"User": s.user(r), "Active": active,
		"FailedCount": s.store.FailedCount(), "ActiveJobs": s.jobs.active(), "Pending": s.pendingCount(),
	}
	s.mu.Lock()
	m["SelfUpdate"] = s.suAvail
	s.mu.Unlock()
	if s.su != nil {
		m["SelfUpdateImage"] = s.su.Image()
	}
	m["AgentsEnabled"] = s.agentsEnabled()
	m["HostCount"] = s.agentCount()
	return m
}

// ---- tabs ----

// svcRow / projGroup / hostGroup back the collapsible Updates tab (local host + remote agents).
type svcRow struct {
	Name, Image string
	Pinned      bool
	Local       bool
	Host        string // "" for local
	PID         int    // local project id; -1 for remote
	File        string // compose file path (remote check/update + pin key)
}
type projGroup struct {
	Key      string // collapse id
	Name     string
	File     string
	PID      int
	Services []svcRow
}
type hostGroup struct {
	Key      string
	Name     string
	Local    bool
	Online   bool
	Ago      string
	Projects []projGroup
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	var hosts []hostGroup

	// local host
	local := hostGroup{Key: "local", Name: "this host", Local: true, Online: true}
	for _, p := range s.cfg.Projects {
		services, err := compose.Services(p.File)
		if err != nil {
			log.Printf("dashboard: %s: %v", p.File, err)
			continue
		}
		g := projGroup{Key: "local\x00" + p.File, Name: p.Name, File: p.File, PID: p.ID}
		for _, sv := range services {
			g.Services = append(g.Services, svcRow{
				Name: sv.Name, Image: sv.Image, Local: true, PID: p.ID, File: p.File,
				Pinned: s.set.Pinned(p.File, sv.Name),
			})
		}
		local.Projects = append(local.Projects, g)
	}
	hosts = append(hosts, local)

	// remote agents
	s.agentsMu.Lock()
	var agents []*agentConn
	for _, c := range s.agents {
		agents = append(agents, c)
	}
	s.agentsMu.Unlock()
	sort.Slice(agents, func(i, j int) bool { return agents[i].host < agents[j].host })
	for _, c := range agents {
		hg := hostGroup{
			Key: "host\x00" + c.host, Name: c.host,
			Online: time.Since(c.lastSeen) < 90*time.Second,
			Ago:    time.Since(c.lastSeen).Round(time.Second).String(),
		}
		for _, p := range c.projects {
			g := projGroup{Key: c.host + "\x00" + p.File, Name: p.Name, File: p.File, PID: -1}
			for _, sv := range p.Services {
				g.Services = append(g.Services, svcRow{
					Name: sv.Name, Image: sv.Image, Local: false, Host: c.host, PID: -1, File: p.File,
					Pinned: s.set.Pinned(p.File, sv.Name),
				})
			}
			hg.Projects = append(hg.Projects, g)
		}
		hosts = append(hosts, hg)
	}

	data := s.base(r, "updates")
	data["Hosts"] = hosts
	data["MultiHost"] = len(agents) > 0
	s.render(w, "dashboard", data)
}

func (s *Server) handleFailed(w http.ResponseWriter, r *http.Request) {
	data := s.base(r, "failed")
	data["Records"] = s.store.Failed()
	s.render(w, "failed", data)
}

// histRow is a successful/reverted record plus whether the manual-rollback button is live for it.
type histRow struct {
	store.Record
	CanRollback bool   // newest success for this service, still within the retention window
	Superseded  bool   // an older success whose rollback point was replaced by a newer update
	PID         int    // project id, for the rollback POST
	Expires     string // human "expires in …" for the button tooltip
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	recs := s.store.Succeeded()
	now := time.Now()
	seen := map[string]bool{} // project+service already shown a newer row
	rows := make([]histRow, 0, len(recs))
	for _, rec := range recs {
		row := histRow{Record: rec}
		k := rec.Project + "\x00" + rec.Service
		if pt, ok := s.store.RollbackFor(rec.Project, rec.Service); ok && now.Before(pt.ExpiresAt) {
			if !seen[k] && pt.ToImage == rec.To {
				row.CanRollback = true
				row.PID = s.pidByName(rec.Project)
				row.Expires = "expires " + pt.ExpiresAt.Format("Jan 2, 15:04")
			} else {
				row.Superseded = true // a live point exists, but for a newer update
			}
		}
		seen[k] = true
		rows = append(rows, row)
	}
	data := s.base(r, "history")
	data["Records"] = rows
	s.render(w, "history", data)
}

// handleRollback reverts a previously-successful update (data + image) on user request.
func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request) {
	p, ok := s.project(r.FormValue("p"))
	if !ok {
		http.Error(w, "unknown project", http.StatusBadRequest)
		return
	}
	service := r.FormValue("s")
	pt, ok := s.store.TakeRollback(p.Name, service)
	if !ok {
		http.Error(w, "no rollback point for this service", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Timeout+30*time.Second)
	defer cancel()
	req := engine.Request{Project: pt.File, Service: service, FromImage: pt.FromImage, ToImage: pt.ToImage}
	res := s.eng.ManualRollback(ctx, req, pt.Snapshot)

	outcome, errStr := "reverted", ""
	if res.Err != nil {
		errStr = res.Err.Error()
	}
	if res.Outcome == engine.OutcomeError {
		outcome = "error" // couldn't restore — needs attention, so let it light the badge
	} else {
		_ = compose.CommitIfRepo(pt.File, fmt.Sprintf("belay: rollback %s %s -> %s", service, pt.ToImage, pt.FromImage))
	}
	s.store.Add(store.Record{
		Project: p.Name, Service: service, From: pt.ToImage, To: pt.FromImage,
		Outcome: outcome, Err: errStr, Logs: strings.TrimSpace(res.Logs),
		Duration: res.Duration.Round(time.Millisecond).String(),
	})
	http.Redirect(w, r, "/history", http.StatusSeeOther)
}

// sweepRollbacks discards snapshots whose retention window has elapsed.
func (s *Server) sweepRollbacks() {
	if s.eng.Snapshot == nil {
		return
	}
	for range time.Tick(5 * time.Minute) {
		for _, pt := range s.store.SweepExpired(time.Now()) {
			if pt.Snapshot != "" {
				s.eng.Snapshot.Discard(context.Background(), engine.Request{Project: pt.File, Service: pt.Service}, pt.Snapshot)
			}
		}
	}
}

// handleCheck reports whether a service has a newer version. It works for a local project
// (?p=&s=) or a remote agent service (?host=&file=&s=&image=), rendering the right update button.
func (s *Server) handleCheck(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	host, service := q.Get("host"), q.Get("s")
	var image, file string
	var pid int
	if host == "" {
		p, ok := s.project(q.Get("p"))
		if !ok {
			http.Error(w, "unknown project", http.StatusBadRequest)
			return
		}
		pid, file = p.ID, p.File
		var err error
		if image, err = compose.FindImage(p.File, service); err != nil {
			s.render(w, "status", map[string]any{"Err": err.Error()})
			return
		}
	} else {
		pid, file, image = -1, q.Get("file"), q.Get("image")
	}

	ref := registry.ParseRef(image)
	newer, comparable, err := s.reg.Newer(r.Context(), ref)
	view := map[string]any{
		"PID": pid, "Service": service, "Comparable": comparable,
		"Remote": host != "", "Host": host, "File": file, "Image": image,
	}
	switch {
	case err != nil:
		view["Err"] = err.Error()
	case comparable && len(newer) > 0:
		latest := newer[len(newer)-1]
		view["Latest"] = latest
		view["Count"] = len(newer)
		view["Target"] = strings.TrimSuffix(image, ":"+ref.Tag) + ":" + latest
		if src, e := s.reg.SourceRepo(r.Context(), ref); e == nil && src != "" {
			view["Changelog"] = registry.ChangelogURL(src, latest)
		}
	}
	s.render(w, "status", view)
}

func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	p, ok := s.project(r.FormValue("p"))
	if !ok {
		http.Error(w, "unknown project", http.StatusBadRequest)
		return
	}
	service, target := r.FormValue("s"), r.FormValue("image")
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Timeout+30*time.Second)
	defer cancel()
	res, current := s.applyUpdate(ctx, p, service, target)

	shownImage, newTag := current, ""
	if res.Outcome == engine.OutcomeUpdated {
		shownImage = target
		if i := strings.LastIndex(target, ":"); i >= 0 {
			newTag = target[i+1:]
		}
	}
	errStr := ""
	if res.Err != nil {
		errStr = res.Err.Error()
	}
	s.render(w, "result", map[string]any{
		"PID": p.ID, "Service": service, "Image": shownImage,
		"Outcome": string(res.Outcome), "NewTag": newTag, "Err": errStr,
		"Logs": strings.TrimSpace(res.Logs), "Duration": res.Duration.Round(time.Millisecond).String(),
	})
}

// verStep is one newer version and its changelog link.
type verStep struct{ Version, Changelog string }

// pendingUpd is a service with a newer version available (local or remote), with the version steps
// in between (each with a changelog) for the pre-update review.
type pendingUpd struct {
	Host, Project, Service string
	Local                  bool
	File                   string
	PID                    int
	From, To, Latest       string
	Steps                  []verStep
}

// pendingUpdates scans every non-pinned service across the local host and all agents and returns the
// ones with a newer stable version, including the changelog for each intermediate version.
func (s *Server) pendingUpdates(ctx context.Context) []pendingUpd {
	var out []pendingUpd
	add := func(local bool, host, projName, file string, pid int, svcName, image string) {
		if image == "" || !strings.Contains(image, ":") || s.set.Pinned(file, svcName) {
			return
		}
		ref := registry.ParseRef(image)
		newer, comparable, err := s.reg.Newer(ctx, ref)
		if err != nil || !comparable || len(newer) == 0 {
			return
		}
		latest := newer[len(newer)-1]
		u := pendingUpd{
			Host: host, Project: projName, Service: svcName, Local: local, File: file, PID: pid,
			From: image, To: strings.TrimSuffix(image, ":"+ref.Tag) + ":" + latest, Latest: latest,
		}
		src, _ := s.reg.SourceRepo(ctx, ref)
		for _, v := range newer {
			st := verStep{Version: v}
			if src != "" {
				st.Changelog = registry.ChangelogURL(src, v)
			}
			u.Steps = append(u.Steps, st)
		}
		out = append(out, u)
	}
	for _, p := range s.cfg.Projects {
		if services, err := compose.Services(p.File); err == nil {
			for _, sv := range services {
				add(true, "", p.Name, p.File, p.ID, sv.Name, sv.Image)
			}
		}
	}
	s.agentsMu.Lock()
	agents := make([]*agentConn, 0, len(s.agents))
	for _, c := range s.agents {
		agents = append(agents, c)
	}
	s.agentsMu.Unlock()
	sort.Slice(agents, func(i, j int) bool { return agents[i].host < agents[j].host })
	for _, c := range agents {
		for _, p := range c.projects {
			for _, sv := range p.Services {
				add(false, c.host, p.Name, p.File, -1, sv.Name, sv.Image)
			}
		}
	}
	return out
}

// reviewRow is one service on the progressive review page; its update status loads per-row via htmx.
type reviewRow struct {
	Local          bool
	Host, Project  string
	Service, Image string
	PID            int
	File           string
}

// handleReview renders the review page INSTANTLY (no registry calls) — a list of every candidate
// service; each row then checks itself via /review/check, and the "Update all" button stays disabled
// until every row has reported.
func (s *Server) handleReview(w http.ResponseWriter, r *http.Request) {
	var rows []reviewRow
	for _, p := range s.cfg.Projects {
		services, err := compose.Services(p.File)
		if err != nil {
			continue
		}
		for _, sv := range services {
			if sv.Image == "" || !strings.Contains(sv.Image, ":") || s.set.Pinned(p.File, sv.Name) {
				continue
			}
			rows = append(rows, reviewRow{Local: true, Project: p.Name, Service: sv.Name, Image: sv.Image, PID: p.ID, File: p.File})
		}
	}
	s.agentsMu.Lock()
	agents := make([]*agentConn, 0, len(s.agents))
	for _, c := range s.agents {
		agents = append(agents, c)
	}
	s.agentsMu.Unlock()
	sort.Slice(agents, func(i, j int) bool { return agents[i].host < agents[j].host })
	for _, c := range agents {
		for _, p := range c.projects {
			for _, sv := range p.Services {
				if sv.Image == "" || !strings.Contains(sv.Image, ":") || s.set.Pinned(p.File, sv.Name) {
					continue
				}
				rows = append(rows, reviewRow{Host: c.host, Project: p.Name, Service: sv.Name, Image: sv.Image, PID: -1, File: p.File})
			}
		}
	}
	data := s.base(r, "updates")
	data["Services"] = rows
	data["Count"] = len(rows)
	s.render(w, "review", data)
}

// handleReviewCheck checks one service for the review page (local ?p=&s= or remote ?host=&file=&s=&image=).
func (s *Server) handleReviewCheck(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	host, service := q.Get("host"), q.Get("s")
	var image string
	if host == "" {
		p, ok := s.project(q.Get("p"))
		if !ok {
			http.Error(w, "unknown project", http.StatusBadRequest)
			return
		}
		var err error
		if image, err = compose.FindImage(p.File, service); err != nil {
			s.render(w, "reviewstatus", map[string]any{"Err": err.Error()})
			return
		}
	} else {
		image = q.Get("image")
	}
	ref := registry.ParseRef(image)
	newer, comparable, err := s.reg.Newer(r.Context(), ref)
	view := map[string]any{"Service": service, "From": ref.Tag}
	switch {
	case err != nil:
		view["Err"] = err.Error()
	case comparable && len(newer) > 0:
		view["Updatable"] = true
		view["Latest"] = newer[len(newer)-1]
		if src, e := s.reg.SourceRepo(r.Context(), ref); e == nil && src != "" {
			type step struct{ Version, Changelog string }
			steps := make([]step, 0, len(newer))
			for _, v := range newer {
				steps = append(steps, step{Version: v, Changelog: registry.ChangelogURL(src, v)})
			}
			view["Steps"] = steps
		}
	}
	s.render(w, "reviewstatus", view)
}

// handleUpdateAll applies every pending update (local runs the engine; remote is enqueued to its
// agent) in the background, then returns to the dashboard where the Activity tray shows progress.
func (s *Server) handleUpdateAll(w http.ResponseWriter, r *http.Request) {
	go func() {
		ctx := context.Background()
		for _, u := range s.pendingUpdates(ctx) {
			if u.Local {
				if p, ok := s.project(strconv.Itoa(u.PID)); ok {
					s.applyUpdate(ctx, p, u.Service, u.To)
				}
			} else {
				s.enqueue(u.Host, u.File, u.Service, u.To)
			}
		}
	}()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// applyUpdate runs the safe-update engine for one service, commits on success, records the attempt,
// and surfaces the whole thing live in the Activity tray (with streamed logs).
func (s *Server) applyUpdate(ctx context.Context, p Project, service, target string) (engine.Result, string) {
	current, err := compose.FindImage(p.File, service)
	if err != nil {
		return engine.Result{Outcome: engine.OutcomeError, Err: err}, ""
	}

	job := s.jobs.start(p.Name, service, current, target)
	logCtx, stopLogs := context.WithCancel(ctx)
	go s.streamLogs(logCtx, p.File, service, job) // live progress in the Activity tray

	res := s.eng.SafeUpdate(ctx, engine.Request{
		Project: p.File, Service: service, FromImage: current, ToImage: target,
		OnPhase: func(ph string) { s.jobs.setPhase(job, ph) },
	})
	stopLogs()

	if res.Outcome == engine.OutcomeUpdated {
		_ = compose.CommitIfRepo(p.File, fmt.Sprintf("belay: update %s %s -> %s", service, current, target))
		s.retainRollback(ctx, p, service, current, target, res.Snapshot)
	}
	errStr := ""
	if res.Err != nil {
		errStr = res.Err.Error()
	}
	s.jobs.finish(job, string(res.Outcome), strings.TrimSpace(res.Logs))
	s.store.Add(store.Record{
		Project: p.Name, Service: service, From: current, To: target,
		Outcome: string(res.Outcome), Err: errStr, Logs: strings.TrimSpace(res.Logs),
		Duration: res.Duration.Round(time.Millisecond).String(),
	})
	if res.Outcome == engine.OutcomeRolledBack || res.Outcome == engine.OutcomeError {
		s.notify.Failure(notify.Event{
			Project: p.Name, Service: service, From: current, To: target,
			Outcome: string(res.Outcome), Error: errStr, Logs: strings.TrimSpace(res.Logs),
		})
	}
	return res, current
}

// retainRollback keeps a successful update's snapshot + old image as the service's single rollback
// point for the retention window, discarding any point it supersedes. If retention is off, it
// discards the snapshot immediately.
func (s *Server) retainRollback(ctx context.Context, p Project, service, from, to, snapshot string) {
	window := s.set.RollbackWindow()
	if window <= 0 {
		if s.eng.Snapshot != nil && snapshot != "" {
			s.eng.Snapshot.Discard(ctx, engine.Request{Project: p.File, Service: service}, snapshot)
		}
		return
	}
	now := time.Now()
	old, had := s.store.SetRollback(store.RollbackPoint{
		Project: p.Name, Service: service, File: p.File,
		FromImage: from, ToImage: to, Snapshot: snapshot,
		CreatedAt: now, ExpiresAt: now.Add(window),
	})
	if had && old.Snapshot != "" && s.eng.Snapshot != nil {
		s.eng.Snapshot.Discard(ctx, engine.Request{Project: old.File, Service: old.Service}, old.Snapshot)
	}
}

// ---- helpers ----

func (s *Server) pidByName(name string) int {
	for _, p := range s.cfg.Projects {
		if p.Name == name {
			return p.ID
		}
	}
	return -1
}

func (s *Server) project(id string) (Project, bool) {
	n, err := strconv.Atoi(id)
	if err != nil {
		return Project{}, false
	}
	for _, p := range s.cfg.Projects {
		if p.ID == n {
			return p, true
		}
	}
	return Project{}, false
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	root := "layout.html"
	if name == "status" || name == "result" || name == "activity" || name == "reviewstatus" {
		root = name
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tpl[name].ExecuteTemplate(w, root, data); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

func newToken() string {
	b := make([]byte, 24)
	rand.Read(b)
	return hex.EncodeToString(b)
}
