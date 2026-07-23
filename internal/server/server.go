// Package server is Belay's web UI + controller: a dashboard that shows available updates and
// applies them through the safe-update engine (health-gated, auto-rollback).
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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/belay-sh/belay/internal/agent"
	"github.com/belay-sh/belay/internal/compose"
	"github.com/belay-sh/belay/internal/engine"
	"github.com/belay-sh/belay/internal/health"
	"github.com/belay-sh/belay/internal/registry"
	"github.com/belay-sh/belay/web"
)

// Project is a compose project the server manages.
type Project struct {
	ID   int
	Name string
	File string
}

// Config configures the server.
type Config struct {
	Addr          string
	Projects      []Project
	Password      string        // built-in login; empty = no built-in auth
	ForwardHeader string        // trusted reverse-proxy user header (e.g. X-authentik-username)
	Timeout       time.Duration // health-gate timeout
	MinUptime     time.Duration // stayed-running window
}

type Server struct {
	cfg  Config
	reg  *registry.Client
	eng  *engine.Engine
	tpl  map[string]*template.Template
	mu   sync.Mutex
	sess map[string]struct{}
}

func New(cfg Config) *Server {
	if cfg.Timeout == 0 {
		cfg.Timeout = 90 * time.Second
	}
	if cfg.MinUptime == 0 {
		cfg.MinUptime = 10 * time.Second
	}
	funcs := template.FuncMap{"sub": func(a, b int) int { return a - b }}
	page := func(files ...string) *template.Template {
		return template.Must(template.New("").Funcs(funcs).ParseFS(web.FS, files...))
	}
	return &Server{
		cfg: cfg,
		reg: registry.New(),
		eng: &engine.Engine{Deployer: agent.Local{}, Health: health.Gate{Timeout: cfg.Timeout, MinUptime: cfg.MinUptime}},
		tpl: map[string]*template.Template{
			"dashboard": page("templates/layout.html", "templates/dashboard.html"),
			"login":     page("templates/layout.html", "templates/login.html"),
			"status":    page("templates/status.html"),
			"result":    page("templates/result.html"),
		},
		sess: map[string]struct{}{},
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
	mux.HandleFunc("GET /check", s.guard(s.handleCheck))
	mux.HandleFunc("POST /update", s.guard(s.handleUpdate))

	if !s.authEnabled() {
		log.Printf("WARNING: no auth configured (set --password or --forward-header). Bind to localhost only.")
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
	if r.FormValue("password") != s.cfg.Password || s.cfg.Password == "" {
		s.render(w, "login", map[string]any{"Error": "Incorrect password."})
		return
	}
	tok := token()
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

// ---- pages ----

type svcView struct {
	Name  string
	Image string
}
type projView struct {
	ID       int
	Name     string
	File     string
	Services []svcView
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	var projects []projView
	for _, p := range s.cfg.Projects {
		services, err := compose.Services(p.File)
		if err != nil {
			log.Printf("dashboard: %s: %v", p.File, err)
			continue
		}
		pv := projView{ID: p.ID, Name: p.Name, File: p.File}
		for _, sv := range services {
			pv.Services = append(pv.Services, svcView{Name: sv.Name, Image: sv.Image})
		}
		projects = append(projects, pv)
	}
	s.render(w, "dashboard", map[string]any{"User": s.user(r), "Projects": projects})
}

func (s *Server) handleCheck(w http.ResponseWriter, r *http.Request) {
	p, ok := s.project(r.URL.Query().Get("p"))
	if !ok {
		http.Error(w, "unknown project", http.StatusBadRequest)
		return
	}
	service := r.URL.Query().Get("s")
	image, err := compose.FindImage(p.File, service)
	if err != nil {
		s.render(w, "status", map[string]any{"Err": err.Error()})
		return
	}
	ref := registry.ParseRef(image)
	newer, comparable, err := s.reg.Newer(r.Context(), ref)
	view := map[string]any{"PID": p.ID, "Service": service, "Comparable": comparable}
	switch {
	case err != nil:
		view["Err"] = err.Error()
	case comparable && len(newer) > 0:
		latest := newer[len(newer)-1]
		view["Latest"] = latest
		view["Count"] = len(newer)
		view["Target"] = strings.TrimSuffix(image, ":"+ref.Tag) + ":" + latest
	}
	s.render(w, "status", view)
}

func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	p, ok := s.project(r.FormValue("p"))
	if !ok {
		http.Error(w, "unknown project", http.StatusBadRequest)
		return
	}
	service := r.FormValue("s")
	target := r.FormValue("image")
	current, err := compose.FindImage(p.File, service)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Timeout+30*time.Second)
	defer cancel()
	res := s.eng.SafeUpdate(ctx, engine.Request{Project: p.File, Service: service, FromImage: current, ToImage: target})

	shownImage := current
	newTag := ""
	if res.Outcome == engine.OutcomeUpdated {
		shownImage = target
		if i := strings.LastIndex(target, ":"); i >= 0 {
			newTag = target[i+1:]
		}
		_ = compose.CommitIfRepo(p.File, fmt.Sprintf("belay: update %s %s -> %s", service, current, target))
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

// ---- helpers ----

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
	t := s.tpl[name]
	root := "layout.html"
	if name == "status" || name == "result" {
		root = name
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, root, data); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

func token() string {
	b := make([]byte, 24)
	rand.Read(b)
	return hex.EncodeToString(b)
}
