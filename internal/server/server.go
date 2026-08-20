// Package server is Belay's web UI + controller: three tabs (Updates / Failed / History) backed by
// the safe-update engine (health-gated, auto-rollback). Every update attempt is recorded.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"os"
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
	"github.com/El-Mundos/belay/internal/version"
	"github.com/El-Mundos/belay/web"
	"path/filepath"
)

type Project struct {
	ID   int
	Name string
	File string
}

type Config struct {
	Addr          string
	Projects      []Project
	Password      string
	ForwardHeader string
	// ForwardGroupsHeader carries the caller's groups from the reverse proxy, for the optional
	// Settings group gate (Authentik sends X-authentik-groups, pipe-separated).
	ForwardGroupsHeader string
	NotifyWebhook       string
	Snapshot            bool
	Timeout             time.Duration
	MinUptime           time.Duration
	RollbackWindow      time.Duration // seed for a fresh install; the settings page owns it thereafter
	DataDir             string        // where settings.json + store.json live ("" = in-memory only)
	AgentToken          string        // shared bearer token for remote agents ("" = multi-host disabled)
}

type Server struct {
	cfg      Config
	reg      *registry.Client
	eng      *engine.Engine
	store    *store.Store
	set      *config.Store
	notify   *notify.Notifier
	tpl      map[string]*template.Template
	mu       sync.Mutex
	sess     map[string]struct{}
	checks   map[string]checkResult // auto-check cache, key project\x00service
	jobs     *jobManager            // live activity panel (SSE)
	su       *selfupdate.Manager    // belay self-update (nil-safe if not in a container)
	hostName string                 // the machine Belay manages (not this container's id)
	suAvail  bool                   // cached: a newer belay image is available

	dockerCfgDir string // DOCKER_CONFIG dir holding generated auths for private-registry pulls

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

	funcs := template.FuncMap{
		"sub": func(a, b int) int { return a - b },
		// the services a host refuses to update through the ordinary path (Belay/agent + transport)
		"protected": func(h hostView) []string {
			var out []string
			for _, p := range h.Projects {
				for _, sv := range p.Services {
					if sv.Protected != "" {
						out = append(out, sv.Name)
					}
				}
			}
			return out
		},
	}
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
	s := &Server{
		cfg:   cfg,
		reg:   registry.New(),
		eng:   eng,
		store: store.Open(storePath),
		set:   set,
		notify: notify.New(
			func() config.Notify { return set.Get().Notify },
			func() bool { return set.Get().InQuietHours(time.Now().Hour()) },
		),
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
		sess:     map[string]struct{}{},
		checks:   map[string]checkResult{},
		jobs:     newJobManager(),
		su:       selfupdate.Detect(context.Background()),
		hostName: agent.HostName(context.Background()),
		agents:   map[string]*agentConn{},
	}
	// private registries: the version-check client reads creds live; the pull reads a generated
	// docker config.json (DOCKER_CONFIG) so `docker compose up --pull` can authenticate too.
	s.reg.SetCreds(func(host string) (string, string, bool) { return s.set.Get().RegistryCred(host) })
	if cfg.DataDir != "" {
		s.dockerCfgDir = filepath.Join(cfg.DataDir, "docker")
		os.Setenv("DOCKER_CONFIG", s.dockerCfgDir)
	}
	s.syncDockerAuth()
	return s
}

// syncDockerAuth (re)writes the docker config.json used by pulls from the current registry settings.
func (s *Server) syncDockerAuth() {
	if s.dockerCfgDir == "" {
		return
	}
	var auths []registry.Auth
	for _, r := range s.set.Get().Registries {
		auths = append(auths, registry.Auth{Host: r.Host, Username: r.Username, Token: r.Token})
	}
	if err := registry.WriteDockerConfig(s.dockerCfgDir, auths); err != nil {
		log.Printf("registry: write docker config: %v", err)
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
	mux.HandleFunc("GET /record", s.guard(s.handleRecord))
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
	mux.HandleFunc("POST /agent-update", s.guard(s.handleAgentSelfUpdate))
	// Unguarded liveness probe: it exposes nothing, and a health check that needs a session is
	// useless to Docker, to a reverse proxy, and to Belay's own health gate.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok "+version.Version)
	})
	// remote-agent endpoints (token-authed, not session-guarded)
	mux.HandleFunc("POST /agent/register", s.handleAgentRegister)
	mux.HandleFunc("GET /agent/poll", s.handleAgentPoll)
	mux.HandleFunc("POST /agent/result", s.handleAgentResult)

	s.reconcileSelfUpdate() // settle (and record) any self-update that was in flight when we died
	go s.sweepRollbacks()   // discard snapshots whose retention window has passed
	go s.autoCheckLoop()    // periodic version checks (cache + notify), per settings
	go s.selfUpdateWatch()  // cache whether a newer belay image is available

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

// groupAllowed applies the optional "only this Authentik group may use Belay" gate.
//
// Forward-auth answers *who* the caller is; it says nothing about whether they should be here, so
// without this any account the identity provider authenticates gets in. The gate reads the groups
// header the proxy attaches and requires the configured one.
//
// It fails CLOSED: with a group required and no groups header present, access is denied. A missing
// header is indistinguishable from a proxy that was never configured to send one, and treating
// "I don't know your groups" as "you may pass" would make the gate ornamental. Password sessions
// are exempt — that login is the local admin, and locking it out would leave no way back in if the
// proxy is misconfigured.
func (s *Server) groupAllowed(r *http.Request) bool {
	want := strings.TrimSpace(s.set.Get().RequireGroup)
	if want == "" || s.cfg.ForwardHeader == "" {
		return true
	}
	if r.Header.Get(s.cfg.ForwardHeader) == "" {
		return true // not a forward-auth identity (password session)
	}
	header := s.cfg.ForwardGroupsHeader
	if header == "" {
		header = "X-authentik-groups"
	}
	for _, g := range strings.FieldsFunc(r.Header.Get(header), func(c rune) bool { return c == '|' || c == ',' }) {
		if strings.EqualFold(strings.TrimSpace(g), want) {
			return true
		}
	}
	return false
}

func (s *Server) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.authEnabled() && s.user(r) == "" {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		if !s.groupAllowed(r) {
			http.Error(w, "forbidden: your account is not in the group required to use Belay",
				http.StatusForbidden)
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
	m["Theme"] = s.set.Get().Theme // "" = follow the OS
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
	rows := newRecViews(s.store.Failed())
	data := s.base(r, "failed")
	data["Records"] = rows
	data["Rev"] = listRev(rows)
	s.render(w, "failed", data)
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	recs := s.store.Succeeded()
	now := time.Now()
	seen := map[string]bool{} // project+service already shown a newer row
	rows := make([]recView, 0, len(recs))
	for _, rec := range recs {
		row := newRecView(rec)
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
	data["Rev"] = listRev(rows)
	s.render(w, "history", data)
}

// handleRecord returns one stored attempt's bulky parts as JSON, for the details popout. Keeping
// logs out of the polled lists is what lets those refresh cheaply and without churn.
func (s *Server) handleRecord(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.URL.Query().Get("id"))
	if err != nil {
		http.Error(w, "bad record id", http.StatusBadRequest)
		return
	}
	rec, ok := s.store.ByID(id)
	if !ok {
		http.Error(w, "no such record", http.StatusNotFound)
		return
	}
	v := newRecView(rec)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"service": rec.Service,
		"project": rec.Project,
		"from":    v.FromRef,
		"to":      v.ToRef,
		"repo":    v.Repo,
		"outcome": rec.Outcome,
		"when":    rec.Time.Format("Jan 2, 15:04:05"),
		"error":   rec.Err,
		"logs":    rec.Logs,
	})
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

	if why := s.protectionFor(host == "", host, file, service, image); why != "" {
		s.render(w, "status", map[string]any{"Protected": why})
		return
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
		if cm, ok := majorOf(ref.Tag); ok {
			if lm, ok := majorOf(latest); ok && lm != cm {
				view["Major"] = true // major bump — excluded from "update all", needs a manual click
			}
		}
		if src, e := s.reg.SourceRepo(r.Context(), ref); e == nil && src != "" {
			view["Changelog"] = registry.ChangelogURL(src, latest)
		}
	default:
		// No newer tag — but the tag itself may now point at a different build. This is the only
		// signal for "latest"/"stable" and for rolling tags like "15", which never change name.
		// Reported as up-to-date for remote services, whose digests the server cannot inspect.
		if host == "" && !strings.Contains(image, "@") {
			if up := s.rebaseFor(r.Context(), image); up {
				view["Comparable"] = true
				view["Rebase"] = true
				view["Latest"] = ref.Tag + " (new build)"
				view["Count"] = 1
				view["Target"] = image
			}
		}
	}
	s.render(w, "status", view)
}

// rebaseFor reports whether the registry's digest for an image's tag differs from the digest running
// locally. Any uncertainty (registry unreachable, image built locally and never pulled) answers
// "no" — a check that guesses would produce updates that do nothing.
func (s *Server) rebaseFor(ctx context.Context, image string) bool {
	remote, err := s.reg.Digest(ctx, registry.ParseRef(image))
	if err != nil || remote == "" {
		return false
	}
	have, err := agent.ImageDigest(ctx, image)
	return err == nil && have != "" && have != remote
}

func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	p, ok := s.project(r.FormValue("p"))
	if !ok {
		http.Error(w, "unknown project", http.StatusBadRequest)
		return
	}
	service, target := r.FormValue("s"), r.FormValue("image")
	if why := s.protectedReason(service, target); why != "" {
		s.render(w, "result", map[string]any{"Service": service, "Err": why})
		return
	}
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
	Rebase                 bool // same tag, new build behind it
	Steps                  []verStep
}

// pendingUpdates scans every non-pinned service across the local host and all agents and returns the
// ones with a newer stable version, including the changelog for each intermediate version.
// filterMajor drops candidate tags whose major version differs from current when major bumps aren't
// allowed. Returns the kept list and the highest dropped (major) version if any.
func filterMajor(currentTag string, newer []string, allowMajor bool) (kept []string, majorAvail string) {
	if allowMajor {
		return newer, ""
	}
	cm, ok := majorOf(currentTag)
	if !ok {
		return newer, "" // can't determine major → don't filter
	}
	for _, v := range newer {
		if m, ok := majorOf(v); ok && m != cm {
			majorAvail = v // newer is ascending, so this ends up the highest major
			continue
		}
		kept = append(kept, v)
	}
	return kept, majorAvail
}

// majorOf returns the leading integer (major version) of a tag like "15" or "v1.2.3".
func majorOf(tag string) (int, bool) {
	t := strings.TrimPrefix(tag, "v")
	i := 0
	for i < len(t) && t[i] >= '0' && t[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, false
	}
	n, _ := strconv.Atoi(t[:i])
	return n, true
}

func (s *Server) pendingUpdates(ctx context.Context) []pendingUpd {
	var out []pendingUpd
	add := func(local bool, host, projName, file string, pid int, svcName, image string) {
		if image == "" || !strings.Contains(image, ":") || s.set.Pinned(file, svcName) {
			return
		}
		// Belay's own container and its Docker transport must not be recreated by the Belay doing
		// the recreating — locally that is this process, remotely it is the agent, which reports
		// its own protected services (a remote host's same-named service is not necessarily either).
		if s.protectionFor(local, host, file, svcName, image) != "" {
			return
		}
		up := s.checkService(ctx, image, local)
		if up.Err != nil || up.Target == "" {
			return
		}
		ref := registry.ParseRef(image)
		u := pendingUpd{
			Host: host, Project: projName, Service: svcName, Local: local, File: file, PID: pid,
			From: image, To: up.Target, Rebase: up.Rebase,
		}
		if up.Rebase {
			// Same tag, new build: there is no version to name, and no release to link to.
			u.Latest = ref.Tag + " (new build)"
			out = append(out, u)
			return
		}
		u.Latest = up.Newer[len(up.Newer)-1]
		src, _ := s.reg.SourceRepo(ctx, ref)
		for _, v := range up.Newer {
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
	var majorAvail string
	if err == nil && comparable {
		newer, majorAvail = filterMajor(ref.Tag, newer, s.set.Get().AllowMajor)
	}
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
	case majorAvail != "":
		view["MajorOnly"] = majorAvail // a major bump exists but is excluded from update-all
	}
	s.render(w, "reviewstatus", view)
}

// protectedReason explains why a service must not be updated through the ordinary path, or "" when
// it is a normal service.
//
// Two services in a Belay deployment cannot be recreated by the very Belay doing the recreating:
//
//   - Belay's own container. `docker compose up -d belay` kills the process mid-update, so the update
//     never finishes and nothing rolls it back. Self-update exists precisely for this, handing off to
//     a throwaway helper container that outlives Belay's death.
//   - Whatever DOCKER_HOST points at. Reaching Docker to recreate the socket-proxy requires the
//     socket-proxy, so the moment it stops, the operation loses the connection it was travelling
//     over and cannot finish or revert. Belay saws off the branch it is sitting on.
//
// This was latent until digest tracking made `latest` updatable: a socket-proxy on `:latest` had
// never been a candidate before, so the hazard had never been reachable.
func (s *Server) protectedReason(service, image string) string {
	return s.su.Protects(service, image)
}

// remoteProtected returns the reason an AGENT gave for refusing to update one of its own services.
//
// The server cannot work this out itself: which container is the agent, and what its Docker
// transport is, are facts about a machine the server only reaches over the wire. So the agent
// answers for its own host and reports the verdict with its registration, and the server treats
// that as authoritative. An agent too old to send it reports nothing, and the agent's own refusal
// is then the only guard — which is why the Hosts tab flags version skew.
func (s *Server) remoteProtected(host, file, service string) string {
	s.agentsMu.Lock()
	defer s.agentsMu.Unlock()
	c := s.agents[host]
	if c == nil {
		return ""
	}
	for _, p := range c.projects {
		if p.File != file {
			continue
		}
		for _, sv := range p.Services {
			if sv.Name == service {
				return sv.Protected
			}
		}
	}
	return ""
}

// protectionFor is the single question "may this service be updated through the ordinary path?",
// answered for local and remote services alike.
func (s *Server) protectionFor(local bool, host, file, service, image string) string {
	if local {
		return s.protectedReason(service, image)
	}
	return s.remoteProtected(host, file, service)
}

// svcUpdate is what a single service has waiting for it: either a newer version tag, or a rebase —
// the same tag now resolving to a different build.
type svcUpdate struct {
	Target string   // image ref to deploy; "" means nothing to do
	Rebase bool     // Target equals the current ref; apply by pulling, not by rewriting the tag
	Newer  []string // version steps, oldest first (empty for a rebase)
	Err    error
}

// checkService decides what update a service has, tracking BOTH ways a container image can move.
//
// Version tags are compared by name: "1.27.1" -> "1.27.2". Tags that are re-pointed in place cannot
// be, because the name never changes — "15" silently becomes 15.4.0, and "latest" carries no version
// at all. Those are caught by comparing the registry's digest for the tag against the digest that was
// actually pulled locally, which is the only signal that distinguishes them. Version bumps take
// precedence: if a genuinely newer tag exists, that is the more useful thing to report.
//
// Digest tracking needs to inspect the local image, so it applies to services on this host only;
// remote agents report their images but not yet their digests, so rebases there go unnoticed.
func (s *Server) checkService(ctx context.Context, image string, local bool) svcUpdate {
	// A digest-pinned ref is deliberately frozen — that is what pinning by digest means.
	if strings.Contains(image, "@") {
		return svcUpdate{}
	}
	ref := registry.ParseRef(image)
	newer, comparable, err := s.reg.Newer(ctx, ref)
	if err != nil {
		return svcUpdate{Err: err}
	}
	if comparable && len(newer) > 0 {
		newer, _ = filterMajor(ref.Tag, newer, s.set.Get().AllowMajor)
		if len(newer) > 0 {
			latest := newer[len(newer)-1]
			return svcUpdate{Target: strings.TrimSuffix(image, ":"+ref.Tag) + ":" + latest, Newer: newer}
		}
	}
	if local && s.rebaseFor(ctx, image) {
		return svcUpdate{Target: image, Rebase: true}
	}
	return svcUpdate{}
}

// batch is a set of pending updates that must be applied together, in order, and abandoned as soon
// as one of them fails. Every update belongs to exactly one batch.
type batch struct {
	key      string // display name used when reporting a skip
	lockstep bool   // true => already-applied members are rolled back if a later one fails
	ups      []pendingUpd
}

// batches partitions pending updates into units that are applied sequentially with a failure guard.
//
// An explicitly configured ServiceGroup forms a lockstep batch: its members are meant to run the
// same version, so a partial application is worse than none and the successes are reverted. Anything
// else is grouped by compose file, which only stops the run — services sharing a stack usually share
// a database or a config contract, so continuing to upgrade siblings after one has just failed tends
// to compound the damage rather than make progress.
func (s *Server) batches(ups []pendingUpd) []*batch {
	set := s.set.Get()
	var order []*batch
	byKey := map[string]*batch{}
	for _, u := range ups {
		key, lockstep := "file:"+u.Host+"\x00"+u.File, false
		if g, ok := set.GroupFor(u.File, u.Service); ok {
			key, lockstep = "group:"+g, true
		}
		b := byKey[key]
		if b == nil {
			name := u.Project
			if lockstep {
				name = strings.TrimPrefix(key, "group:")
			}
			b = &batch{key: name, lockstep: lockstep}
			byKey[key] = b
			order = append(order, b)
		}
		b.ups = append(b.ups, u)
	}
	return order
}

// handleUpdateAll applies every pending update (local runs the engine; remote is enqueued to its
// agent) in the background, then returns to the dashboard where the Activity tray shows progress.
// Batches run concurrently up to the configured limit; the updates inside one run in sequence.
func (s *Server) handleUpdateAll(w http.ResponseWriter, r *http.Request) {
	go func() {
		ctx := context.Background()
		conc := s.set.Get().Concurrency
		if conc < 1 {
			conc = 1
		}
		sem := make(chan struct{}, conc)
		var wg sync.WaitGroup
		for _, b := range s.batches(s.pendingUpdates(ctx)) {
			wg.Add(1)
			sem <- struct{}{}
			go func(b *batch) {
				defer wg.Done()
				defer func() { <-sem }()
				s.runBatch(ctx, b)
			}(b)
		}
		wg.Wait()
	}()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// runBatch applies one batch in order, stopping at the first failure. Everything after the failure is
// recorded as skipped so the History tab explains why those services did not move, rather than
// leaving them silently untouched.
func (s *Server) runBatch(ctx context.Context, b *batch) {
	var applied []pendingUpd // succeeded so far, for lockstep unwinding
	for i, u := range b.ups {
		if !u.Local {
			// Remote work is handed to the agent, which runs it sequentially but reports back
			// asynchronously — the server cannot yet await a remote result mid-batch, so a remote
			// failure does not stop the rest of its batch.
			s.enqueue(u.Host, u.File, u.Service, u.To)
			continue
		}
		p, ok := s.project(strconv.Itoa(u.PID))
		if !ok {
			continue
		}
		res, _ := s.applyUpdate(ctx, p, u.Service, u.To)
		if res.Outcome == engine.OutcomeUpdated {
			applied = append(applied, u)
			continue
		}
		if res.Outcome == engine.OutcomeSkipped {
			continue
		}

		// Failed: abandon the rest of the batch.
		for _, rest := range b.ups[i+1:] {
			s.recordSkipped(rest, fmt.Sprintf("skipped: %s failed earlier in %q", u.Service, b.key))
		}
		if b.lockstep {
			s.unwindBatch(ctx, applied, u.Service, b.key)
		}
		return
	}
}

// recordSkipped notes an update that was never attempted because an earlier one in its batch failed.
func (s *Server) recordSkipped(u pendingUpd, reason string) {
	s.store.Add(store.Record{
		Project: u.Project, Service: u.Service, From: u.From, To: u.To,
		Outcome: string(engine.OutcomeSkipped), Err: reason,
	})
}

// unwindBatch reverts the members of a lockstep group that already succeeded, so the group is left
// wholly on the old version rather than split across two. A revert that itself fails is recorded as
// an error: that is a state only the user can untangle.
func (s *Server) unwindBatch(ctx context.Context, applied []pendingUpd, failedSvc, group string) {
	for i := len(applied) - 1; i >= 0; i-- { // reverse order, mirroring how they were applied
		u := applied[i]
		// Retention may be off, in which case the snapshot was discarded the moment the update
		// succeeded and there is nothing to revert to — say so rather than fail silently.
		pt, found := s.store.TakeRollback(u.Project, u.Service)
		if !found {
			s.recordSkipped(u, fmt.Sprintf(
				"lockstep group %q broke on %s, but no rollback point remained for %s — it is STILL on %s",
				group, failedSvc, u.Service, u.To))
			continue
		}
		req := engine.Request{Project: pt.File, Service: u.Service, FromImage: pt.FromImage, ToImage: pt.ToImage}
		res := s.eng.ManualRollback(ctx, req, pt.Snapshot)

		outcome, note := "reverted", fmt.Sprintf("lockstep group %q broke on %s", group, failedSvc)
		if res.Outcome == engine.OutcomeError {
			outcome = string(engine.OutcomeError) // needs a human — let it light the Failed badge
			if res.Err != nil {
				note += ": " + res.Err.Error()
			}
		} else {
			_ = compose.CommitIfRepo(pt.File, fmt.Sprintf("belay: lockstep revert %s %s -> %s", u.Service, pt.ToImage, pt.FromImage))
		}
		s.store.Add(store.Record{
			Project: u.Project, Service: u.Service, From: pt.ToImage, To: pt.FromImage,
			Outcome: outcome, Duration: res.Duration.Round(time.Millisecond).String(),
			Err: note, Logs: strings.TrimSpace(res.Logs),
		})
	}
}

// applyUpdate runs the safe-update engine for one service, commits on success, records the attempt,
// and surfaces the whole thing live in the Activity tray (with streamed logs).
func (s *Server) applyUpdate(ctx context.Context, p Project, service, target string) (engine.Result, string) {
	current, err := compose.FindImage(p.File, service)
	if err != nil {
		return engine.Result{Outcome: engine.OutcomeError, Err: err}, ""
	}
	// Deploying the ref that is already in the compose file means the tag itself moved: a rebase.
	rebase := target == current

	// A rebase keeps the tag, so "revert to the previous image" cannot be expressed as a tag — the tag
	// now points at the build we are trying to escape. Capture the digest we are currently running and
	// roll back to that instead, which also stops the bad build being pulled straight back in.
	from := current
	if rebase {
		if dig, err := agent.ImageDigest(ctx, current); err == nil && dig != "" {
			ref := registry.ParseRef(current)
			from = strings.TrimSuffix(current, ":"+ref.Tag) + "@" + dig
		}
	}

	job := s.jobs.start(p.Name, service, current, target)
	logCtx, stopLogs := context.WithCancel(ctx)
	go s.streamLogs(logCtx, p.File, service, job) // live progress in the Activity tray

	res := s.eng.SafeUpdate(ctx, engine.Request{
		Project: p.File, Service: service, FromImage: from, ToImage: target, Rebase: rebase,
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
	if res.Outcome == engine.OutcomeRolledBack || res.Outcome == engine.OutcomeError {
		if caveat := rollbackCaveat(p.File, service); caveat != "" {
			errStr = strings.TrimSpace(errStr + "\n" + caveat)
		}
	}
	s.jobs.finish(job, string(res.Outcome), strings.TrimSpace(res.Logs))
	s.store.Add(store.Record{
		Project: p.Name, Service: service, From: current, To: target,
		Outcome: string(res.Outcome), Err: errStr, Logs: strings.TrimSpace(res.Logs),
		Duration: res.Duration.Round(time.Millisecond).String(),
	})
	switch res.Outcome {
	case engine.OutcomeRolledBack, engine.OutcomeError:
		s.notify.Failure(notify.Event{
			Project: p.Name, Service: service, From: current, To: target,
			Outcome: string(res.Outcome), Error: errStr, Logs: strings.TrimSpace(res.Logs),
		})
	case engine.OutcomeUpdated:
		s.notify.Success(p.Name, service, current, target)
	}
	return res, current
}

// statefulImages are image names that indicate a service holds persistent state a rollback cannot
// reach. Matched as a substring of the image reference, so "postgres:16-alpine" and
// "docker.io/library/mariadb" both hit.
var statefulImages = []string{
	"postgres", "postgis", "mysql", "mariadb", "mongo", "redis", "valkey",
	"clickhouse", "cockroach", "elasticsearch", "opensearch", "influxdb", "timescale",
}

// rollbackCaveat warns when a rolled-back service shares its compose file with a datastore.
//
// Belay's rollback is image-tag plus volume snapshot, scoped to the service being updated. When the
// new version writes to a database owned by a *sibling* service before failing its health gate —
// schema migrations are the classic case — reverting the tag restores the code but not the data, and
// nothing in the snapshot machinery covers a volume belay never took. The rollback genuinely
// succeeded, so saying nothing would imply a clean recovery that did not happen.
func rollbackCaveat(file, service string) string {
	svcs, err := compose.Services(file)
	if err != nil {
		return ""
	}
	var found []string
	for _, sv := range svcs {
		if sv.Name == service {
			continue
		}
		img := strings.ToLower(sv.Image)
		for _, db := range statefulImages {
			if strings.Contains(img, db) {
				found = append(found, sv.Name)
				break
			}
		}
	}
	if len(found) == 0 {
		return ""
	}
	return fmt.Sprintf("NOTE: the image tag was reverted, but this stack also runs %s. "+
		"Belay's rollback does not cover state owned by another service, so anything the failed "+
		"version wrote there (schema migrations especially) is still in place and may need "+
		"attention before retrying.", strings.Join(found, ", "))
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
