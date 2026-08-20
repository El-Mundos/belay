package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/El-Mundos/belay/internal/agent"
	"github.com/El-Mundos/belay/internal/cluster"
	"github.com/El-Mundos/belay/internal/compose"
	"github.com/El-Mundos/belay/internal/discover"
	"github.com/El-Mundos/belay/internal/engine"
	"github.com/El-Mundos/belay/internal/health"
	"github.com/El-Mundos/belay/internal/registry"
	"github.com/El-Mundos/belay/internal/version"
)

// runAgent runs a headless Belay agent: it dials OUT to a server, reports its local compose stacks,
// long-polls for update commands, runs them with the full safe-update engine, and posts results back.
func runAgent(args []string) {
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	server := fs.String("server", os.Getenv("BELAY_SERVER"), "belay server base URL, e.g. http://10.10.0.2:8080")
	token := fs.String("token", os.Getenv("BELAY_AGENT_TOKEN"), "shared token (must match the server's --agent-token)")
	name := fs.String("name", "", "host name to report (default: OS hostname)")
	var projects stringList
	fs.Var(&projects, "project", "compose project path; repeatable (default: auto-discover running stacks)")
	timeout := fs.Duration("timeout", 90*time.Second, "health-gate timeout")
	minUptime := fs.Duration("min-uptime", 10*time.Second, "stayed-running window when the image has no healthcheck")
	snapshot := fs.Bool("snapshot", true, "snapshot volumes before updating and restore data on rollback")
	fs.Parse(args)

	if *server == "" || *token == "" {
		fatal(fmt.Errorf("agent needs --server and --token (or BELAY_SERVER / BELAY_AGENT_TOKEN)"))
	}
	host := *name
	if host == "" {
		host, _ = os.Hostname()
	}
	eng := &engine.Engine{Deployer: agent.Local{}, Health: health.Gate{Timeout: *timeout, MinUptime: *minUptime}}
	if *snapshot {
		eng.Snapshot = agent.Snapshotter{}
	}
	ac := &agentClient{base: strings.TrimRight(*server, "/"), token: *token, host: host,
		hc: &http.Client{Timeout: 15 * time.Second}, poller: &http.Client{Timeout: 35 * time.Second}}

	log.Printf("belay agent %q → %s", host, ac.base)

	// register once up front (retrying) so the first poll won't 404, then heartbeat every 60s
	for {
		if err := ac.register(collectProjects(projects)); err == nil {
			break
		} else {
			log.Printf("register: %v (retrying in 5s)", err)
			time.Sleep(5 * time.Second)
		}
	}
	go func() {
		for {
			time.Sleep(60 * time.Second)
			if err := ac.register(collectProjects(projects)); err != nil {
				log.Printf("register: %v", err)
			}
		}
	}()

	// poll loop: block for a command, run it, post the result
	for {
		cmd, ok, err := ac.poll()
		if err != nil {
			log.Printf("poll: %v (retrying in 5s)", err)
			time.Sleep(5 * time.Second)
			continue
		}
		if !ok {
			continue // 204 timeout, just poll again
		}
		log.Printf("command %s: update %s in %s → %s", cmd.ID, cmd.Service, cmd.Project, cmd.Image)
		res := runCommand(eng, cmd, host)
		if err := ac.postResult(res); err != nil {
			log.Printf("post result: %v", err)
		}
	}
}

// collectProjects returns the stacks to report: the explicit --project list, else auto-discovery.
func collectProjects(explicit stringList) []cluster.Project {
	var files []string
	if len(explicit) > 0 {
		for _, p := range explicit {
			if f, err := compose.FileFor(p); err == nil {
				files = append(files, f)
			}
		}
	} else if found, err := discover.RunningProjects(context.Background()); err == nil {
		for _, d := range found {
			files = append(files, d.File)
		}
	}
	var out []cluster.Project
	for _, f := range files {
		services, err := compose.Services(f)
		if err != nil {
			continue
		}
		p := cluster.Project{Name: projName(f), File: f}
		for _, sv := range services {
			p.Services = append(p.Services, cluster.Service{Name: sv.Name, Image: sv.Image})
		}
		out = append(out, p)
	}
	return out
}

// runCommand executes one update command locally with the safe-update engine.
func runCommand(eng *engine.Engine, cmd cluster.Command, host string) cluster.Result {
	res := cluster.Result{CommandID: cmd.ID, Host: host, Project: projName(cmd.Project), Service: cmd.Service, To: cmd.Image}
	current, err := compose.FindImage(cmd.Project, cmd.Service)
	if err != nil {
		res.Outcome, res.Err = "error", err.Error()
		return res
	}
	res.From = current
	// a server-pushed scoped credential lets the pull authenticate to a private registry; merge it
	// into the host's docker config so it composes with any manual `docker login` already present.
	if cmd.Auth != nil {
		a := registry.Auth{Host: cmd.Auth.Host, Username: cmd.Auth.Username, Token: cmd.Auth.Token}
		if err := registry.MergeDockerConfig(dockerConfigDir(), a); err != nil {
			log.Printf("command %s: registry auth: %v", cmd.ID, err)
		}
	}
	r := engine.Request{Project: cmd.Project, Service: cmd.Service, FromImage: current, ToImage: cmd.Image}
	out := eng.SafeUpdate(context.Background(), r)
	// the agent has no rollback-retention UI, so discard the success snapshot immediately
	if out.Outcome == engine.OutcomeUpdated && eng.Snapshot != nil && out.Snapshot != "" {
		eng.Snapshot.Discard(context.Background(), r, out.Snapshot)
		_ = compose.CommitIfRepo(cmd.Project, fmt.Sprintf("belay: update %s %s -> %s", cmd.Service, current, cmd.Image))
	}
	res.Outcome = string(out.Outcome)
	res.Logs = strings.TrimSpace(out.Logs)
	res.Duration = out.Duration.Round(time.Millisecond).String()
	if out.Err != nil {
		res.Err = out.Err.Error()
	}
	return res
}

func projName(file string) string { return filepath.Base(filepath.Dir(file)) }

// dockerConfigDir is where `docker` reads/writes config.json — DOCKER_CONFIG if the operator set one,
// else the default ~/.docker. Merging the pushed credential here keeps the host's own logins intact.
func dockerConfigDir() string {
	if d := os.Getenv("DOCKER_CONFIG"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".docker")
}

// ---- HTTP client for the dial-out protocol ----

type agentClient struct {
	base, token, host string
	hc, poller        *http.Client
}

func (a *agentClient) register(projects []cluster.Project) error {
	body, _ := json.Marshal(cluster.Registration{Host: a.host, Version: version.Version, Projects: projects})
	return a.do(a.hc, http.MethodPost, "/agent/register", body, nil)
}

func (a *agentClient) postResult(res cluster.Result) error {
	body, _ := json.Marshal(res)
	return a.do(a.hc, http.MethodPost, "/agent/result", body, nil)
}

// poll returns (command, true, nil) when work is available, (_, false, nil) on a 204 timeout.
func (a *agentClient) poll() (cluster.Command, bool, error) {
	req, err := http.NewRequest(http.MethodGet, a.base+"/agent/poll?host="+a.host, nil)
	if err != nil {
		return cluster.Command{}, false, err
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	resp, err := a.poller.Do(req)
	if err != nil {
		return cluster.Command{}, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return cluster.Command{}, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return cluster.Command{}, false, fmt.Errorf("poll status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var cmd cluster.Command
	if err := json.NewDecoder(resp.Body).Decode(&cmd); err != nil {
		return cluster.Command{}, false, err
	}
	return cmd, true, nil
}

func (a *agentClient) do(hc *http.Client, method, path string, body []byte, out any) error {
	req, err := http.NewRequest(method, a.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
