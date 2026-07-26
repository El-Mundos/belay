package server

import (
	"context"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Job is one update attempt as shown in the live Activity tray (browser-downloads style): it starts
// "running", streams the service's container logs while the engine works, and ends on an outcome.
type Job struct {
	ID               int
	Project, Service string
	From, To         string
	State            string // running | updated | rolled_back | error | reverted | skipped
	Logs             string
	Started, Ended   time.Time
}

func (j Job) Running() bool { return j.State == "running" }
func (j Job) Elapsed() string {
	end := j.Ended
	if end.IsZero() {
		end = time.Now()
	}
	return end.Sub(j.Started).Round(time.Second).String()
}

// jobManager holds the recent jobs for the Activity tray. All mutation goes through it under a lock.
type jobManager struct {
	mu   sync.Mutex
	seq  int
	jobs []*Job
	max  int
}

func newJobManager() *jobManager { return &jobManager{max: 50} }

func (m *jobManager) start(project, service, from, to string) *Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	j := &Job{ID: m.seq, Project: project, Service: service, From: from, To: to, State: "running", Started: time.Now()}
	m.jobs = append(m.jobs, j)
	if len(m.jobs) > m.max {
		m.jobs = m.jobs[len(m.jobs)-m.max:]
	}
	return j
}

func (m *jobManager) setLogs(j *Job, logs string) {
	m.mu.Lock()
	j.Logs = logs
	m.mu.Unlock()
}

func (m *jobManager) finish(j *Job, state, logs string) {
	m.mu.Lock()
	j.State = state
	if strings.TrimSpace(logs) != "" {
		j.Logs = logs
	}
	j.Ended = time.Now()
	m.mu.Unlock()
}

// snapshot returns a copy of the jobs, newest first, plus the count still running.
func (m *jobManager) snapshot() ([]Job, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Job, 0, len(m.jobs))
	active := 0
	for i := len(m.jobs) - 1; i >= 0; i-- {
		out = append(out, *m.jobs[i])
		if m.jobs[i].State == "running" {
			active++
		}
	}
	return out, active
}

func (m *jobManager) active() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, j := range m.jobs {
		if j.State == "running" {
			n++
		}
	}
	return n
}

// streamLogs polls the service's container logs into the job while it runs, so the Activity tray
// shows progress live. It stops when ctx is cancelled (i.e. the update finished).
func (s *Server) streamLogs(ctx context.Context, file, service string, j *Job) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		out, err := exec.CommandContext(ctx, "docker", "compose", "-f", file, "logs",
			"--no-color", "--no-log-prefix", "--tail", "40", service).CombinedOutput()
		if err == nil {
			s.setLogsIfChanged(j, strings.TrimRight(string(out), "\n"))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) setLogsIfChanged(j *Job, logs string) {
	s.jobs.mu.Lock()
	changed := j.Logs != logs && logs != ""
	if changed {
		j.Logs = logs
	}
	s.jobs.mu.Unlock()
}

// handleActivity renders the Activity tray fragment; it polls itself via htmx while jobs run.
func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	jobs, active := s.jobs.snapshot()
	s.render(w, "activity", map[string]any{"Jobs": jobs, "Active": active})
}
