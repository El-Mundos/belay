package server

import (
	"context"
	"fmt"
	"html"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/El-Mundos/belay/internal/cluster"
	"github.com/El-Mundos/belay/internal/compose"
	"github.com/El-Mundos/belay/internal/config"
	"github.com/El-Mundos/belay/internal/notify"
	"github.com/El-Mundos/belay/internal/registry"
	"github.com/El-Mundos/belay/internal/selfupdate"
	"github.com/El-Mundos/belay/internal/store"
	"github.com/El-Mundos/belay/internal/version"
)

// regRow is one private-registry credential row on the settings page.
type regRow struct {
	Idx      int
	Host     string
	Username string
	Token    string
}

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
	Group   string // lockstep group this service belongs to ("" = none)
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
			grp, _ := cur.GroupFor(p.File, sv.Name)
			rows = append(rows, probeRow{
				Idx: i, PID: p.ID, Project: p.Name, Service: sv.Name, Image: sv.Image,
				Pinned: cur.Pins[config.Key(p.File, sv.Name)],
				Type:   pr.Type, Target: pr.Target, Expect: pr.Expect,
				Group: grp,
			})
			i++
		}
	}
	// registry rows: existing creds + two blank rows so new ones can be added without JS.
	var regs []regRow
	for j, rg := range cur.Registries {
		regs = append(regs, regRow{Idx: j, Host: rg.Host, Username: rg.Username, Token: rg.Token})
	}
	for b := 0; b < 2; b++ {
		regs = append(regs, regRow{Idx: len(regs)})
	}

	data := s.base(r, "settings")
	data["S"] = cur
	data["Services"] = rows
	data["N"] = i
	data["Registries"] = regs
	data["RN"] = len(regs)
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
			// The form posts every service, so groups are rebuilt wholesale: clearing a service's
			// group box is how a member is removed, and a group left with one member is dropped
			// (a lockstep group of one is just a normal update).
			members := map[string][]string{}
			var groupOrder []string
			for i := 0; i < n; i++ {
				pid, _ := strconv.Atoi(r.FormValue(fmt.Sprintf("pid_%d", i)))
				svc := r.FormValue(fmt.Sprintf("svc_%d", i))
				file := s.fileByID(pid)
				name := strings.TrimSpace(r.FormValue(fmt.Sprintf("pgroup_%d", i)))
				if file == "" || svc == "" || name == "" {
					continue
				}
				if _, seen := members[name]; !seen {
					groupOrder = append(groupOrder, name)
				}
				members[name] = append(members[name], config.Key(file, svc))
			}
			st.Groups = st.Groups[:0]
			for _, name := range groupOrder {
				if len(members[name]) < 2 {
					continue
				}
				st.Groups = append(st.Groups, config.ServiceGroup{Name: name, Members: members[name]})
			}

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
			st.Notify.OnSuccess = r.FormValue("on_success") == "on"
			st.AutoCheckHours = atoiDefault(r.FormValue("auto_check_hours"), 0)
			st.RollbackWindowHours = atoiDefault(r.FormValue("rollback_window_hours"), 24)
			st.AllowMajor = r.FormValue("allow_major") == "on"
			st.QuietHours = r.FormValue("quiet_hours") == "on"
			st.QuietStart = atoiDefault(r.FormValue("quiet_start"), 0)
			st.QuietEnd = atoiDefault(r.FormValue("quiet_end"), 0)
			st.Concurrency = atoiDefault(r.FormValue("concurrency"), 1)
			st.MetricsToken = strings.TrimSpace(r.FormValue("metrics_token"))
			st.RequireGroup = strings.TrimSpace(r.FormValue("require_group"))
			if t := r.FormValue("theme"); t == "" || t == "light" || t == "dark" {
				st.Theme = t // anything else is not a theme we have; keep the current one
			}
			rn, _ := strconv.Atoi(r.FormValue("rn"))
			st.Registries = st.Registries[:0]
			for i := 0; i < rn; i++ {
				host := strings.TrimSpace(r.FormValue(fmt.Sprintf("reg_host_%d", i)))
				user := strings.TrimSpace(r.FormValue(fmt.Sprintf("reg_user_%d", i)))
				token := strings.TrimSpace(r.FormValue(fmt.Sprintf("reg_token_%d", i)))
				if host == "" || user == "" {
					continue // a blank row = removed
				}
				st.Registries = append(st.Registries, config.Registry{Host: host, Username: user, Token: token})
			}
		})
		s.syncDockerAuth() // rewrite docker config.json for pulls
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
			if avail {
				s.suVersion = s.su.TargetVersion(context.Background())
			}
			s.mu.Unlock()
		}
		time.Sleep(30 * time.Minute)
	}
}

// reconcileSelfUpdate runs once at startup, before serving. A self-update replaces the process that
// ordered it, so its outcome cannot be observed by the process that started it — this is where the
// answer finally lands, in the same History every other update is recorded in.
//
// The interesting case is the silent one: a helper that died, or a new image that failed its gate
// and got rolled back. Without this, that shows up only as "Belay is somehow still on the old
// version", with nothing to explain it.
func (s *Server) reconcileSelfUpdate() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out := s.su.Reconcile(ctx, s.cfg.DataDir)
	if out.Kind != "" {
		from, to := out.From, out.To
		s.store.Add(store.Record{
			Project: SelfUpdateProject, Service: SelfUpdateService, From: from, To: to,
			Outcome: out.Kind, Err: outcomeErr(out), Duration: "-",
		})
		log.Printf("self-update: %s — %s", out.Kind, out.Detail)
		if out.Kind != "updated" {
			s.notify.Failure(notify.Event{
				Project: SelfUpdateProject, Service: SelfUpdateService, From: from, To: to,
				Outcome: out.Kind, Error: out.Detail,
			})
		}
	}
	// The previous process asked for the fleet to follow. Only now — with this Belay booted on the
	// new image — is that request worth acting on, and only after the gate window, so an image that
	// cannot stay up never gets to tell three other hosts to install it.
	if out.Kind == "updated" && out.FollowUp == followUpAgents {
		log.Printf("self-update: fleet rollout queued, starting in %s", selfupdate.GateWindow)
		go func() {
			time.Sleep(selfupdate.GateWindow)
			s.fleetRollout()
		}()
	}

	// The predecessor container is the rollback target; it may only be discarded once the helper's
	// gate window has passed, or we would destroy the way back while a rollback is still possible.
	if out.ReapAfter != "" {
		go func(c string) {
			time.Sleep(selfupdate.GateWindow + 5*time.Second)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			s.su.Reap(ctx, s.cfg.DataDir, c)
		}(out.ReapAfter)
	}
}

// outcomeErr is the explanation stored with a non-successful self-update ("" when it worked).
func outcomeErr(o selfupdate.Outcome) string {
	if o.Kind == "updated" {
		return ""
	}
	return o.Detail
}

// shortID trims a "sha256:…" image ID to something readable in a History row.
func shortID(id string) string {
	if algo, hex, ok := strings.Cut(id, ":"); ok && len(hex) > 12 {
		return algo + ":" + hex[:12]
	}
	return id
}

// handleAgentSelfUpdate asks an agent to replace its own container. The server cannot do this to a
// remote host itself — only the agent can hand off to a helper on its own machine.
func (s *Server) handleAgentSelfUpdate(w http.ResponseWriter, r *http.Request) {
	host := r.FormValue("host")
	s.agentsMu.Lock()
	c := s.agents[host]
	s.agentsMu.Unlock()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if c == nil {
		fmt.Fprint(w, `<span class="err">host offline</span>`)
		return
	}
	// Nothing to do is not a silent no-op: pressing update on an agent that already matches the
	// server would queue a command whose helper correctly decides there is nothing to swap, and the
	// button would appear to do nothing at all.
	if c.version == version.Version {
		fmt.Fprintf(w, `<span class="muted">already on v%s</span>`, html.EscapeString(version.Version))
		return
	}
	cmd := cluster.Command{ID: newToken()[:12], Kind: cluster.KindSelf, Image: s.su.Image()}
	select {
	case c.queue <- cmd:
		// Same treatment as any other remote update, so it is visible while it happens.
		s.jobs.startRemote(host, "belay-agent", "agent", s.su.Image(), cmd.ID)
		fmt.Fprint(w, `<span class="ok">queued — see Activity; the agent re-registers on the new image</span>`)
	default:
		fmt.Fprint(w, `<span class="err">agent queue full</span>`)
	}
}

// rollbackWindow is how long a manual rollback stays on offer, shared with per-service retention:
// one setting, because it answers the same question for both.
func (s *Server) rollbackWindow() time.Duration {
	h := s.set.Get().RollbackWindowHours
	if h <= 0 {
		return 0
	}
	return time.Duration(h) * time.Hour
}

// selfRollbackPoint returns the live manual-rollback offer for a completed self-update, if any.
func (s *Server) selfRollbackPoint() *selfupdate.RollbackPoint {
	rb := selfupdate.LoadState(s.cfg.DataDir).Rollback
	if rb == nil || time.Now().After(rb.Until) {
		return nil
	}
	return rb
}

// handleSelfRollback puts Belay back on the image it ran before its last self-update.
func (s *Server) handleSelfRollback(w http.ResponseWriter, r *http.Request) {
	if s.su == nil || !s.su.Enabled() {
		http.Error(w, "self-update not available (belay is not running in a detectable container)", http.StatusBadRequest)
		return
	}
	if err := s.su.Rollback(r.Context(), s.cfg.DataDir); err != nil {
		http.Error(w, "rollback failed to launch: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.writeReconnectPage(w, "Belay is rolling back…", "Restoring the previous image. This page will reconnect in ~15s.")
}

// checkBelayImage refreshes "is a newer Belay published?" from the registry. Server and agents run
// the SAME image, so one registry round trip answers for every host — an agent card does not need
// to reach the agent to know whether a newer build exists.
func (s *Server) checkBelayImage(ctx context.Context) (bool, error) {
	if s.su == nil || !s.su.Enabled() {
		return false, fmt.Errorf("not running in a container")
	}
	avail, err := s.su.Available(ctx)
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	s.suAvail = avail
	if avail {
		s.suVersion = s.su.TargetVersion(ctx)
	}
	s.mu.Unlock()
	return avail, nil
}

// checkView is one card's answer. Refresh carries the out-of-band half: the host's primary button
// and the banner, so a check that finds an update changes the page rather than describing what a
// reload would show.
type checkView struct {
	Msg               string
	Class             string // "ok" | "muted" | "err"
	Refresh           bool
	Host              hostView
	SelfUpdate        bool
	SelfUpdateImage   string
	SelfUpdateVersion string
	Version           string
}

// handleHostCheck answers "is this host behind?" for one card — the local host or an agent.
func (s *Server) handleHostCheck(w http.ResponseWriter, r *http.Request) {
	host := r.FormValue("host")
	idx, _ := strconv.Atoi(r.FormValue("idx"))
	// Best effort: only the LOCAL card's answer depends on the registry. An agent is judged by the
	// version it reported against the version we run, which needs neither Docker nor a network --
	// so a failed registry check must not turn every agent card's button into an error.
	avail, err := s.checkBelayImage(r.Context())

	v := checkView{Class: "muted", Host: hostView{Idx: idx}}
	if host == "" { // the local host: compare the registry against what we are running
		switch {
		case err != nil:
			v.Class, v.Msg = "err", "check failed: "+err.Error()
		case avail:
			// Flip this card's button and raise the banner in the same response.
			v.Class = "ok"
			v.Msg = "a newer image is available"
			v.Refresh, v.SelfUpdate, v.SelfUpdateImage = true, true, s.su.Image()
			s.mu.Lock()
			v.SelfUpdateVersion, v.Version = s.suVersion, version.Version
			s.mu.Unlock()
			v.Host = s.localHost()
			v.Host.Idx = idx
		default:
			v.Msg = "up to date (v" + version.Version + ", just checked)"
		}
		s.render(w, "hostcheck", v)
		return
	}

	s.agentsMu.Lock()
	c := s.agents[host]
	var av string
	if c != nil {
		av = c.version
	}
	s.agentsMu.Unlock()
	switch {
	case c == nil:
		v.Class, v.Msg = "err", "host offline"
	case av == "":
		v.Class, v.Msg = "err", "agent too old to report its version — update it"
	case av != version.Version:
		v.Class, v.Msg = "ok", "v"+av+" → v"+version.Version+" available"
	case err == nil && avail:
		// The agent matches us, but we are both behind the registry — say so, rather than
		// "up to date", which would be true only relative to a server that is itself stale.
		v.Msg = "matches this server (v" + av + ") — but a newer image exists; update the main host first"
	default:
		v.Msg = "up to date (v" + av + ", just checked)"
	}
	s.render(w, "hostcheck", v)
}

// handleHostsCheckAll refreshes the registry once and re-renders every card, so a fleet answers in
// one click instead of one click per host.
func (s *Server) handleHostsCheckAll(w http.ResponseWriter, r *http.Request) {
	_, _ = s.checkBelayImage(r.Context())
	s.handleHosts(w, r) // htmx picks #host-cards out of the re-rendered page
}

// followUpAgents is the FollowUp token meaning "roll the agents once this update is confirmed".
const followUpAgents = "agents"

// agentWaitTimeout is how long one agent gets to come back on the new version before the rollout
// gives up. An agent that has not re-registered by then either failed or rolled itself back; either
// way the release is not proven, and the rest of the fleet should be left alone.
const agentWaitTimeout = 3 * time.Minute

// handleUpdateEverything updates Belay first and the agents afterwards.
//
// The order is the whole point. Belay is the canary: it runs on the machine you can actually reach,
// its update is health-gated and reversible, and if the new image is bad it rolls itself back and
// the agents are never told to do anything. Updating everything at once would instead take down the
// only observer at the moment N remote hosts start replacing themselves -- and an agent posts its
// result BEFORE it hands off, so a server mid-restart loses exactly the reports it would need.
func (s *Server) handleUpdateEverything(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	serverStale := s.suAvail
	s.mu.Unlock()

	if serverStale && s.su != nil && s.su.Enabled() {
		// Write the intention down before going down: this process will not be here to continue.
		err := s.su.Apply(r.Context(), selfupdate.Opts{
			Dir: s.cfg.DataDir, FromVersion: version.Version,
			Window: s.rollbackWindow(), FollowUp: followUpAgents,
		})
		if err != nil {
			http.Error(w, "self-update failed to launch: "+err.Error(), http.StatusInternalServerError)
			return
		}
		s.writeReconnectPage(w, "Belay is updating itself…",
			"The agents follow once this Belay comes back and proves healthy. This page reconnects in ~15s.")
		return
	}
	// Belay is already current, so the canary step is already satisfied.
	go s.fleetRollout()
	s.handleHosts(w, r)
}

// fleetRollout updates stale agents ONE AT A TIME, stopping at the first that does not come back.
//
// Sequential is not politeness, it is the safety property: a release that breaks agents costs one
// agent, on one host, instead of the entire fleet at once -- and the surviving agents are the
// evidence for what went wrong.
func (s *Server) fleetRollout() {
	if !s.rolloutStart() {
		return // one at a time; a second rollout would fight the first over the same agents
	}
	defer s.rolloutDone()

	for {
		c := s.nextStaleAgent()
		if c == nil {
			return
		}
		cmd := cluster.Command{ID: newToken()[:12], Kind: cluster.KindSelf, Image: s.su.Image()}
		select {
		case c.queue <- cmd:
			s.jobs.startRemote(c.host, "belay-agent", "agent", s.su.Image(), cmd.ID)
		default:
			log.Printf("fleet: %s queue full, stopping rollout", c.host)
			return
		}
		if s.waitForAgent(c.host, version.Version, agentWaitTimeout) {
			log.Printf("fleet: %s is now on %s", c.host, version.Version)
			continue
		}
		// It never came back on the new version. Record it where a failure belongs and STOP.
		errStr := "agent did not re-register on v" + version.Version + " within " + agentWaitTimeout.String() +
			" — fleet rollout stopped so the rest keep the version they have"
		log.Printf("fleet: %s: %s", c.host, errStr)
		s.store.Add(store.Record{
			Project: c.host + "/belay-agent", Service: "agent", From: c.version, To: s.su.Image(),
			Outcome: "error", Err: errStr, Duration: agentWaitTimeout.String(),
		})
		s.notify.Failure(notify.Event{
			Project: c.host + "/belay-agent", Service: "agent", From: c.version,
			To: s.su.Image(), Outcome: "error", Error: errStr,
		})
		return
	}
}

// nextStaleAgent returns an online agent behind the server, or nil when the fleet is level.
func (s *Server) nextStaleAgent() *agentConn {
	s.agentsMu.Lock()
	defer s.agentsMu.Unlock()
	for _, c := range s.agents {
		if c.version != version.Version && time.Since(c.lastSeen) < 90*time.Second {
			return c
		}
	}
	return nil
}

// waitForAgent blocks until a host re-registers reporting want, or the timeout passes. The agent's
// posted result is not the signal — it reports "helper launched" and then dies — so the only honest
// evidence that an agent survived its own update is that it came back and said what it is now.
func (s *Server) waitForAgent(host, want string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(5 * time.Second)
		s.agentsMu.Lock()
		c := s.agents[host]
		got := ""
		if c != nil {
			got = c.version
		}
		s.agentsMu.Unlock()
		if got == want {
			return true
		}
	}
	return false
}

func (s *Server) rolloutStart() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rollout {
		return false
	}
	s.rollout = true
	return true
}

func (s *Server) rolloutDone() {
	s.mu.Lock()
	s.rollout = false
	s.mu.Unlock()
}

// handleAgentsUpdateAll updates every agent that is behind.
//
// The SERVER is deliberately excluded. Its update tears down the page issuing the request, and a
// fleet action that also logs you out mid-click is a surprise -- the local card keeps its own
// explicit button.
func (s *Server) handleAgentsUpdateAll(w http.ResponseWriter, r *http.Request) {
	s.agentsMu.Lock()
	var behind []*agentConn
	for _, c := range s.agents {
		if c.version != version.Version && time.Since(c.lastSeen) < 90*time.Second {
			behind = append(behind, c)
		}
	}
	s.agentsMu.Unlock()

	queued := 0
	for _, c := range behind {
		cmd := cluster.Command{ID: newToken()[:12], Kind: cluster.KindSelf, Image: s.su.Image()}
		select {
		case c.queue <- cmd:
			s.jobs.startRemote(c.host, "belay-agent", "agent", s.su.Image(), cmd.ID)
			queued++
		default:
		}
	}
	log.Printf("agents: queued %d self-update(s)", queued)
	s.handleHosts(w, r)
}

// handleSelfUpdate recreates the belay container from the current image tag. Belay is torn down and
// replaced by a detached helper moments after this responds, so we render a "reconnecting" page first.
func (s *Server) handleSelfUpdate(w http.ResponseWriter, r *http.Request) {
	if s.su == nil || !s.su.Enabled() {
		http.Error(w, "self-update not available (belay is not running in a detectable container)", http.StatusBadRequest)
		return
	}
	if err := s.su.Apply(r.Context(), selfupdate.Opts{Dir: s.cfg.DataDir, FromVersion: version.Version,
		Window: s.rollbackWindow(), FollowUp: r.FormValue("then")}); err != nil {
		http.Error(w, "self-update failed to launch: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.writeReconnectPage(w, "Belay is updating itself…",
		"The container is being recreated on the new image. This page will reconnect in ~15s.")
}

// writeReconnectPage is shown while Belay is being replaced: the helper is about to kill this
// process, so the response has to be sent before anything happens and stand on its own afterwards.
func (s *Server) writeReconnectPage(w http.ResponseWriter, title, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><meta charset=utf-8><title>Belay</title>
<meta http-equiv="refresh" content="15;url=/">
<style>body{font:15px/1.6 -apple-system,sans-serif;background:#0e1014;color:#e6e8ee;display:grid;place-items:center;height:100vh;margin:0}</style>
<div style="text-align:center"><h2>⚓ %s</h2><p>%s</p></div>`,
		html.EscapeString(title), html.EscapeString(detail))
}

// ---- metrics ----

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if tok := s.set.Get().MetricsToken; tok != "" {
		got := r.URL.Query().Get("token")
		if got == "" {
			got = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		if got != tok {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	}
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
