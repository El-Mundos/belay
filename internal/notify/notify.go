// Package notify sends notifications about update events (failures and, from auto-check, newly
// available versions) so you find out without watching the dashboard. It targets ntfy, Discord,
// Slack, Gotify, or any generic JSON webhook, selected by the user's settings.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/El-Mundos/belay/internal/config"
)

// Event describes a failed update (used by Failure).
type Event struct {
	Project string `json:"project"`
	Service string `json:"service"`
	From    string `json:"from"`
	To      string `json:"to"`
	Outcome string `json:"outcome"` // rolled_back | error
	Error   string `json:"error"`
	Logs    string `json:"logs"`
}

// Notifier reads the live Notify config on each send, so changes in the settings page take effect
// immediately. A nil Notifier or a disabled/empty config is a no-op.
type Notifier struct {
	cfg   func() config.Notify
	quiet func() bool // suppress non-critical notifications during quiet hours
	http  *http.Client
}

func New(cfg func() config.Notify, quiet func() bool) *Notifier {
	return &Notifier{cfg: cfg, quiet: quiet, http: &http.Client{Timeout: 10 * time.Second}}
}

func (n *Notifier) quieted() bool { return n.quiet != nil && n.quiet() }

// Failure fires a notification about a failed/rolled-back update (async; never blocks the update).
// Failures ignore quiet hours — they're the ones you want any time.
func (n *Notifier) Failure(e Event) {
	if n == nil {
		return
	}
	c := n.cfg()
	if !c.Enabled || !c.OnFailure || c.URL == "" {
		return
	}
	title := fmt.Sprintf("Belay: %s %s", e.Service, e.Outcome)
	msg := fmt.Sprintf("%s → %s failed on %s", e.From, e.To, e.Project)
	if e.Error != "" {
		msg += "\n" + e.Error
	}
	go n.post(c, title, msg, "warning")
}

// Success fires a notification about a successful update (respects quiet hours).
func (n *Notifier) Success(project, service, from, to string) {
	if n == nil {
		return
	}
	c := n.cfg()
	if !c.Enabled || !c.OnSuccess || c.URL == "" || n.quieted() {
		return
	}
	title := fmt.Sprintf("Belay: %s updated", service)
	msg := fmt.Sprintf("%s: %s → %s (on %s)", service, from, to, project)
	go n.post(c, title, msg, "white_check_mark")
}

// NewVersion fires a notification that a newer version is available (respects quiet hours).
func (n *Notifier) NewVersion(project, service, from, to string) {
	if n == nil {
		return
	}
	c := n.cfg()
	if !c.Enabled || !c.OnNewVersion || c.URL == "" || n.quieted() {
		return
	}
	title := fmt.Sprintf("Belay: update available for %s", service)
	msg := fmt.Sprintf("%s: %s → %s (on %s)", service, from, to, project)
	go n.post(c, title, msg, "arrow_up")
}

// Test sends a one-off notification so the user can verify their settings from the config page.
// It blocks and returns any delivery error.
func (n *Notifier) Test(c config.Notify) error {
	if c.URL == "" {
		return fmt.Errorf("no webhook URL configured")
	}
	return n.deliver(c, "Belay: test notification", "If you can read this, notifications are wired up. ⚓", "white_check_mark")
}

func (n *Notifier) post(c config.Notify, title, msg, tag string) {
	if err := n.deliver(c, title, msg, tag); err != nil {
		log.Printf("notify: %v", err)
	}
}

func (n *Notifier) deliver(c config.Notify, title, msg, tag string) error {
	ctype, body, headers := build(c, title, msg, tag)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", ctype)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := n.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned %s", resp.Status)
	}
	return nil
}

// build renders the request body + headers for the configured provider.
func build(c config.Notify, title, msg, tag string) (ctype string, body []byte, headers map[string]string) {
	kind := c.Kind
	if kind == "" || kind == "auto" {
		switch {
		case strings.Contains(c.URL, "discord.com") || strings.Contains(c.URL, "discordapp.com"):
			kind = "discord"
		case strings.Contains(c.URL, "hooks.slack.com"):
			kind = "slack"
		default:
			kind = "ntfy"
		}
	}
	switch kind {
	case "discord":
		b, _ := json.Marshal(map[string]string{"content": title + "\n" + msg})
		return "application/json", b, nil
	case "slack":
		b, _ := json.Marshal(map[string]string{"text": title + "\n" + msg})
		return "application/json", b, nil
	case "gotify":
		b, _ := json.Marshal(map[string]any{"title": title, "message": msg, "priority": 5})
		return "application/json", b, nil
	case "generic":
		b, _ := json.Marshal(map[string]string{"title": title, "message": msg})
		return "application/json", b, nil
	case "alertmanager": // POST the URL should be <alertmanager>/api/v2/alerts — shows up in Karma
		b, _ := json.Marshal([]map[string]any{{
			"labels":      map[string]string{"alertname": "Belay", "severity": "warning", "source": "belay"},
			"annotations": map[string]string{"summary": title, "description": msg},
		}})
		return "application/json", b, nil
	default: // ntfy: plain-text body + headers
		return "text/plain", []byte(msg), map[string]string{"Title": title, "Tags": tag}
	}
}
