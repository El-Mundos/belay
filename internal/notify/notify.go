// Package notify sends a notification when an update fails (rolls back or errors), so you find out
// without watching the dashboard. It POSTs JSON to a webhook — compatible with ntfy, Discord,
// Slack, Gotify, or any generic receiver.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
)

// Event describes a failed update.
type Event struct {
	Project string `json:"project"`
	Service string `json:"service"`
	From    string `json:"from"`
	To      string `json:"to"`
	Outcome string `json:"outcome"` // rolled_back | error
	Error   string `json:"error"`
	Logs    string `json:"logs"`
}

// Notifier POSTs failure events to a webhook. A nil or empty-URL Notifier is a no-op.
type Notifier struct {
	URL  string
	HTTP *http.Client
}

func New(webhookURL string) *Notifier {
	return &Notifier{URL: webhookURL, HTTP: &http.Client{Timeout: 10 * time.Second}}
}

// Failure fires a notification (asynchronously; never blocks or fails the update).
func (n *Notifier) Failure(e Event) {
	if n == nil || n.URL == "" {
		return
	}
	go func() {
		summary := fmt.Sprintf("Belay: %s update %s (%s → %s)", e.Service, e.Outcome, e.From, e.To)
		body, _ := json.Marshal(e)
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, n.URL, bytes.NewReader(body))
		if err != nil {
			log.Printf("notify: %v", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Title", summary)   // ntfy: notification title
		req.Header.Set("X-Title", summary) // gotify/others
		req.Header.Set("Tags", "warning")  // ntfy: emoji tag
		resp, err := n.HTTP.Do(req)
		if err != nil {
			log.Printf("notify %s: %v", e.Service, err)
			return
		}
		resp.Body.Close()
	}()
}
