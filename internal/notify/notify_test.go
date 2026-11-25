package notify

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kakwa/strangle/internal/config"
	"github.com/kakwa/strangle/internal/store"
	"go.uber.org/zap"
)

func failedRun() store.RunRecord {
	return store.RunRecord{
		Playbook:    "site",
		StartedAt:   time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC),
		FinishedAt:  time.Date(2026, 11, 25, 10, 1, 0, 0, time.UTC),
		DurationMS:  60000,
		ExitCode:    2,
		Hosts:       3,
		FailedHosts: 1,
		Output:      strings.Repeat("x", 5000) + "fatal: boom",
		Success:     false,
	}
}

func TestWebhookNotification(t *testing.T) {
	var mu sync.Mutex
	var got Event
	var header string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		mu.Lock()
		_ = json.NewDecoder(r.Body).Decode(&got)
		header = r.Header.Get("X-Strangle-Secret")
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	target := config.TargetConfig{
		Name: "prod",
		Org:  "lab",
		Notifications: []config.NotificationConfig{{
			Type:    config.NotificationWebhook,
			URL:     srv.URL,
			Headers: map[string]string{"X-Strangle-Secret": "s3cr3t"},
			Timeout: time.Second,
		}},
	}
	if err := New(zap.NewNop()).NotifyFailure(context.Background(), target, failedRun()); err != nil {
		t.Fatalf("NotifyFailure() error: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got.Event != "run_failed" || got.Org != "lab" || got.Collection != "prod" ||
		got.Playbook != "site" || got.ExitCode != 2 || got.FailedHosts != 1 || got.Hosts != 3 {
		t.Errorf("event = %+v", got)
	}
	if len(got.Output) != maxOutput+len("…") || !strings.HasSuffix(got.Output, "fatal: boom") {
		t.Errorf("output length = %d, want bounded tail ending in the failure", len(got.Output))
	}
	if header != "s3cr3t" {
		t.Errorf("X-Strangle-Secret = %q", header)
	}
}

func TestSlackNotification(t *testing.T) {
	var payload struct {
		Text        string `json:"text"`
		Attachments []struct {
			Color string `json:"color"`
			Title string `json:"title"`
		} `json:"attachments"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	target := config.TargetConfig{
		Name: "prod",
		Notifications: []config.NotificationConfig{{
			Type:    config.NotificationSlack,
			URL:     srv.URL,
			Timeout: time.Second,
		}},
	}
	if err := New(zap.NewNop()).NotifyFailure(context.Background(), target, failedRun()); err != nil {
		t.Fatalf("NotifyFailure() error: %v", err)
	}
	if !strings.Contains(payload.Text, "prod/site failed") {
		t.Errorf("slack text = %q", payload.Text)
	}
	if len(payload.Attachments) != 1 || payload.Attachments[0].Color != "danger" ||
		payload.Attachments[0].Title != "prod/site" {
		t.Errorf("slack attachments = %+v", payload.Attachments)
	}
}

func TestEmailNotification(t *testing.T) {
	messages := make(chan string, 1)
	addr := fakeSMTP(t, messages)
	target := config.TargetConfig{
		Name: "prod",
		Notifications: []config.NotificationConfig{{
			Type:    config.NotificationEmail,
			From:    "strangle@example.com",
			To:      []string{"ops@example.com"},
			SMTP:    addr,
			Timeout: 2 * time.Second,
		}},
	}
	if err := New(zap.NewNop()).NotifyFailure(context.Background(), target, failedRun()); err != nil {
		t.Fatalf("NotifyFailure() error: %v", err)
	}
	select {
	case msg := <-messages:
		for _, want := range []string{
			"From: strangle@example.com", "To: ops@example.com",
			"Subject: [strangle] run failed: prod/site", "exit code 2", "fatal: boom",
		} {
			if !strings.Contains(msg, want) {
				t.Errorf("message does not contain %q:\n%s", want, msg)
			}
		}
	default:
		t.Fatal("no email was delivered")
	}
}

func TestNotifyFailureErrorsAndEmptyTarget(t *testing.T) {
	d := New(zap.NewNop())
	if err := d.NotifyFailure(context.Background(), config.TargetConfig{}, failedRun()); err != nil {
		t.Fatalf("NotifyFailure() with no notifiers = %v", err)
	}
	target := config.TargetConfig{
		Name: "prod",
		Notifications: []config.NotificationConfig{{
			Type:    "pager",
			Timeout: time.Second,
		}},
	}
	if err := d.NotifyFailure(context.Background(), target, failedRun()); err == nil ||
		!strings.Contains(err.Error(), "unknown notification type") {
		t.Fatalf("NotifyFailure() error = %v, want unknown type error", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	target.Notifications = []config.NotificationConfig{{
		Type:    config.NotificationWebhook,
		URL:     srv.URL,
		Timeout: time.Second,
	}}
	if err := d.NotifyFailure(context.Background(), target, failedRun()); err == nil ||
		!strings.Contains(err.Error(), "500") {
		t.Fatalf("NotifyFailure() error = %v, want HTTP 500 error", err)
	}
}

// fakeSMTP is just enough of an SMTP server to verify that the email notifier
// speaks the protocol and sends the expected message. It does not advertise
// STARTTLS or AUTH, matching the simple local-relay case.
func fakeSMTP(t *testing.T, messages chan<- string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		writer := bufio.NewWriter(conn)
		send := func(line string) {
			_, _ = writer.WriteString(line + "\r\n")
			_ = writer.Flush()
		}
		send("220 localhost ESMTP test")
		var data strings.Builder
		inData := false
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if inData {
				if line == "." {
					inData = false
					messages <- data.String()
					send("250 queued")
					continue
				}
				data.WriteString(line + "\n")
				continue
			}
			upper := strings.ToUpper(line)
			switch {
			case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
				send("250-localhost")
				send("250 SIZE 1000000")
			case upper == "DATA":
				send("354 go ahead")
				inData = true
			case upper == "QUIT":
				send("221 bye")
				return
			default:
				send("250 ok")
			}
		}
	}()
	return ln.Addr().String()
}
