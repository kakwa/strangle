package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/kakwa/strangle/internal/config"
)

// webhookSender POSTs the event as JSON. It works with any generic HTTP
// endpoint (including ntfy, Gotify or a custom receiver).
type webhookSender struct{}

func (webhookSender) Send(ctx context.Context, cfg config.NotificationConfig, event Event) error {
	return postJSON(ctx, cfg.URL, cfg.Headers, event)
}

// slackSender POSTs a Slack incoming-webhook message. Slack uses the same
// URL/header configuration as a webhook, with a message-shaped payload.
type slackSender struct{}

func (slackSender) Send(ctx context.Context, cfg config.NotificationConfig, event Event) error {
	payload := struct {
		Text        string            `json:"text"`
		Attachments []slackAttachment `json:"attachments,omitempty"`
	}{
		Text: summary(event),
		Attachments: []slackAttachment{{
			Color: "danger",
			Title: fmt.Sprintf("%s/%s", event.Collection, event.Playbook),
			Text:  tail(event.Output, 1500),
			Fields: []slackField{
				{Title: "Started", Value: event.StartedAt.Format("2006-01-02 15:04:05"), Short: true},
				{Title: "Duration", Value: fmt.Sprintf("%dms", event.DurationMS), Short: true},
				{Title: "Exit code", Value: fmt.Sprintf("%d", event.ExitCode), Short: true},
				{Title: "Failed hosts", Value: fmt.Sprintf("%d/%d", event.FailedHosts, event.Hosts), Short: true},
			},
		}},
	}
	return postJSON(ctx, cfg.URL, cfg.Headers, payload)
}

type slackAttachment struct {
	Color  string       `json:"color"`
	Title  string       `json:"title"`
	Text   string       `json:"text"`
	Fields []slackField `json:"fields,omitempty"`
}

type slackField struct {
	Title string `json:"title"`
	Value string `json:"value"`
	Short bool   `json:"short"`
}

func postJSON(ctx context.Context, url string, headers map[string]string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", url, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("POST %s: unexpected status %s", url, resp.Status)
	}
	return nil
}
