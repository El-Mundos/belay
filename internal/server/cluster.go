package server

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"html"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/El-Mundos/belay/internal/cluster"
	"github.com/El-Mundos/belay/internal/compose"
	"github.com/El-Mundos/belay/internal/notify"
	"github.com/El-Mundos/belay/internal/registry"
	"github.com/El-Mundos/belay/internal/store"
	"github.com/El-Mundos/belay/internal/version"
)

// agentConn is a connected remote agent: the stacks it reported and a queue of commands for it.
type agentConn struct {
	host     string
	version  string // agent's build version ("" = predates the field)
	projects []cluster.Project
	lastSeen time.Time
	queue    chan cluster.Command
}

func (s *Server) agentsEnabled() bool { return s.cfg.AgentToken != "" }

// agentAuth checks the shared bearer token on every /agent/* request.
func (s *Server) agentAuth(r *http.Request) bool {
	if !s.agentsEnabled() {
		return false
	}
	h := r.Header.Get("Authorization")
	return strings.HasPrefix(h, "Bearer ") && strings.TrimPrefix(h, "Bearer ") == s.cfg.AgentToken
}

func (s *Server) handleAgentRegister(w http.ResponseWriter, r *http.Request) {
	if !s.agentAuth(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var reg cluster.Registration
	if json.NewDecoder(r.Body).Decode(&reg) != nil || reg.Host == "" {
		http.Error(w, "bad registration", http.StatusBadRequest)
		return
	}
	s.agentsMu.Lock()
	c := s.agents[reg.Host]
	if c == nil {
		c = &agentConn{host: reg.Host, queue: make(chan cluster.Command, 32)}
		s.agents[reg.Host] = c
	}
	c.projects = reg.Projects
	c.version = reg.Version
	c.lastSeen = time.Now()
	s.agentsMu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// handleAgentPoll long-polls: it blocks up to 25s for a queued command, else 204 (agent re-polls).
func (s *Server) handleAgentPoll(w http.ResponseWriter, r *http.Request) {
	if !s.agentAuth(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	host := r.URL.Query().Get("host")
	s.agentsMu.Lock()
	c := s.agents[host]
	if c != nil {
		c.lastSeen = time.Now()
	}
	s.agentsMu.Unlock()
	if c == nil {
		http.Error(w, "register first", http.StatusNotFound)
		return
	}
	select {
	case cmd := <-c.queue:
		json.NewEncoder(w).Encode(cmd)
	case <-time.After(25 * time.Second):
		w.WriteHeader(http.StatusNoContent)
	case <-r.Context().Done():
	}
}

// handleAgentResult records a remote update outcome into the shared store (host-labelled) + notifies.
func (s *Server) handleAgentResult(w http.ResponseWriter, r *http.Request) {
	if !s.agentAuth(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var res cluster.Result
	if json.NewDecoder(r.Body).Decode(&res) != nil {
		http.Error(w, "bad result", http.StatusBadRequest)
		return
	}
	label := res.Host + "/" + res.Project
	s.store.Add(store.Record{
		Project: label, Service: res.Service, From: res.From, To: res.To,
		Outcome: res.Outcome, Err: res.Err, Logs: res.Logs, Duration: res.Duration,
	})
	switch res.Outcome {
	case "rolled_back", "error":
		s.notify.Failure(notify.Event{
			Project: label, Service: res.Service, From: res.From, To: res.To,
			Outcome: res.Outcome, Error: res.Err, Logs: res.Logs,
		})
	case "updated":
		s.notify.Success(label, res.Service, res.From, res.To)
	}
	s.jobs.finishCmd(res.CommandID, res.Outcome, res.From, res.To, res.Logs) // complete the Activity job
	w.WriteHeader(http.StatusNoContent)
}

// ---- UI: Hosts tab ----

type hostView struct {
	Host     string
	Online   bool
	Ago      string
	Projects []cluster.Project
	Version  string // build version ("" = an agent older than the field)
	Stale    bool   // differs from this server's version
	Local    bool   // this Belay itself, not an agent
	Idx      int    // stable per-render id, so each card can target its own status line
}

func (s *Server) handleHosts(w http.ResponseWriter, r *http.Request) {
	s.agentsMu.Lock()
	hosts := make([]hostView, 0, len(s.agents)+1)
	for _, c := range s.agents {
		online := time.Since(c.lastSeen) < 90*time.Second
		hosts = append(hosts, hostView{
			Host: c.host, Online: online,
			Ago:      time.Since(c.lastSeen).Round(time.Second).String(),
			Projects: c.projects,
			Version:  c.version,
			Stale:    c.version != version.Version,
		})
	}
	s.agentsMu.Unlock()
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].Host < hosts[j].Host })
	// The server's own host is a host like any other — it runs stacks, it has a version, it can be
	// out of date. Leaving it out made "Hosts" mean "everywhere except here", so a single-host
	// install saw an empty page describing a machine it was looking at.
	hosts = append([]hostView{s.localHost()}, hosts...)
	for i := range hosts {
		hosts[i].Idx = i
	}
	stale := 0
	for _, h := range hosts {
		if !h.Local && h.Stale {
			stale++
		}
	}
	data := s.base(r, "hosts")
	data["Hosts"] = hosts
	data["Rev"] = hostsRev(hosts)
	data["StaleAgents"] = stale
	data["AgentsEnabled"] = s.agentsEnabled()
	s.render(w, "hosts", data)
}

// hostsRev fingerprints the cards so an unchanged poll skips the DOM swap, exactly as the record
// lists do. Everything the card renders goes in; nothing else does.
func hostsRev(hosts []hostView) string {
	h := fnv.New64a()
	for _, v := range hosts {
		fmt.Fprintf(h, "%s|%s|%t|%t|%d|", v.Host, v.Version, v.Online, v.Stale, len(v.Projects))
	}
	return strconv.FormatUint(h.Sum64(), 36)
}

// localHost describes the machine this server runs on, in the same shape as an agent.
func (s *Server) localHost() hostView {
	s.mu.Lock()
	avail := s.suAvail
	s.mu.Unlock()
	h := hostView{
		Host: s.hostName, Online: true, Ago: "now", Local: true,
		Version: version.Version, Stale: avail,
	}
	for _, p := range s.cfg.Projects {
		cp := cluster.Project{Name: p.Name, File: p.File}
		if services, err := compose.Services(p.File); err == nil {
			for _, sv := range services {
				cp.Services = append(cp.Services, cluster.Service{
					Name: sv.Name, Image: sv.Image, Protected: s.protectedReason(sv.Name, sv.Image),
				})
			}
		}
		h.Projects = append(h.Projects, cp)
	}
	return h
}

// handleRemoteUpdate resolves the newest stable tag for a remote service (the server has registry
// access) and enqueues an update command for that agent host.
func (s *Server) handleRemoteUpdate(w http.ResponseWriter, r *http.Request) {
	host := r.FormValue("host")
	s.agentsMu.Lock()
	c := s.agents[host]
	s.agentsMu.Unlock()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if c == nil {
		fmt.Fprint(w, `<span class="err">host offline</span>`)
		return
	}
	current := r.FormValue("image")
	if why := s.remoteProtected(host, r.FormValue("file"), r.FormValue("s")); why != "" {
		fmt.Fprintf(w, `<span class="err">🔒 %s</span>`, html.EscapeString(why))
		return
	}
	ref := registry.ParseRef(current)
	newer, comparable, err := s.reg.Newer(r.Context(), ref)
	switch {
	case err != nil:
		fmt.Fprintf(w, `<span class="err">%s</span>`, err.Error())
		return
	case !comparable || len(newer) == 0:
		fmt.Fprint(w, `<span class="muted">up to date</span>`)
		return
	}
	target := strings.TrimSuffix(current, ":"+ref.Tag) + ":" + newer[len(newer)-1]
	cmd := cluster.Command{
		ID: newToken()[:12], Kind: "update",
		Project: r.FormValue("file"), Service: r.FormValue("s"), Image: target,
		Auth: s.authForImage(target),
	}
	select {
	case c.queue <- cmd:
		fmt.Fprintf(w, `<span class="ok">queued → %s — see History</span>`, newer[len(newer)-1])
	default:
		fmt.Fprint(w, `<span class="err">agent queue full</span>`)
	}
}

// enqueue queues an update command for a host's agent; returns false if the host is unknown/full.
func (s *Server) enqueue(host, file, service, image string) bool {
	s.agentsMu.Lock()
	c := s.agents[host]
	s.agentsMu.Unlock()
	if c == nil {
		return false
	}
	if s.remoteProtected(host, file, service) != "" {
		return false // the agent's own container / Docker transport; it would kill the executor
	}
	cmd := cluster.Command{ID: newToken()[:12], Kind: "update", Project: file, Service: service, Image: image, Auth: s.authForImage(image)}
	select {
	case c.queue <- cmd:
		s.jobs.startRemote(host, filepath.Base(filepath.Dir(file)), service, image, cmd.ID) // show in Activity tray
		return true
	default:
		return false
	}
}

// authForImage returns the scoped pull credential for an image's registry, or nil if none is
// configured — the agent then pulls anonymously or with whatever login its host already holds.
func (s *Server) authForImage(image string) *cluster.RegistryAuth {
	host := registry.ParseRef(image).Registry
	if user, token, ok := s.set.Get().RegistryCred(host); ok {
		return &cluster.RegistryAuth{Host: host, Username: user, Token: token}
	}
	return nil
}

// agentCount is the number of currently-online agents (for the nav badge).
func (s *Server) agentCount() int {
	s.agentsMu.Lock()
	defer s.agentsMu.Unlock()
	n := 0
	for _, c := range s.agents {
		if time.Since(c.lastSeen) < 90*time.Second {
			n++
		}
	}
	return n
}
