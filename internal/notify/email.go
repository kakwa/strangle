package notify

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/smtp"
	"strings"

	"github.com/kakwa/strangle/internal/config"
)

// emailSender sends a plain-text email through an SMTP submission server.
// STARTTLS is used when the server advertises it; AUTH PLAIN is used when a
// username (or password) is configured.
type emailSender struct{}

func (emailSender) Send(ctx context.Context, cfg config.NotificationConfig, event Event) error {
	message := strings.Join([]string{
		"From: " + cfg.From,
		"To: " + strings.Join(cfg.To, ", "),
		"Subject: " + subject(event),
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=UTF-8",
		"",
		emailBody(event),
	}, "\r\n")

	host, port, err := net.SplitHostPort(cfg.SMTP)
	if err != nil || host == "" {
		return fmt.Errorf("invalid smtp address %q", cfg.SMTP)
	}
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		return fmt.Errorf("dial smtp: %w", err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	// Closing the connection unblocks SMTP commands if the context expires
	// after the TCP connection was established.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	client, err := smtp.NewClient(conn, host)
	if err != nil {
		return fmt.Errorf("smtp client: %w", err)
	}
	defer client.Close()

	if ok, _ := client.Extension("STARTTLS"); ok {
		if err := client.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
			return fmt.Errorf("smtp STARTTLS: %w", err)
		}
	}
	if cfg.Username != "" || cfg.Password != "" {
		if ok, _ := client.Extension("AUTH"); !ok {
			return fmt.Errorf("smtp server does not advertise AUTH")
		}
		if err := client.Auth(smtp.PlainAuth("", cfg.Username, cfg.Password, host)); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}
	if err := client.Mail(cfg.From); err != nil {
		return fmt.Errorf("smtp MAIL FROM: %w", err)
	}
	for _, to := range cfg.To {
		if err := client.Rcpt(to); err != nil {
			return fmt.Errorf("smtp RCPT TO %s: %w", to, err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("smtp DATA: %w", err)
	}
	// smtp.Data's writer dot-stuffs lines and Close writes the terminating
	// "." line, so the message is written verbatim here.
	if _, err := io.WriteString(w, message); err != nil {
		return fmt.Errorf("smtp write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp close data: %w", err)
	}
	// The message is already accepted; some servers close immediately after
	// DATA, so a failed QUIT is not a delivery failure.
	_ = client.Quit()
	return nil
}

func subject(event Event) string {
	return fmt.Sprintf("[strangle] run failed: %s/%s", event.Collection, event.Playbook)
}

func emailBody(event Event) string {
	var b strings.Builder
	fmt.Fprintln(&b, summary(event))
	fmt.Fprintf(&b, "Org: %s\n", event.Org)
	fmt.Fprintf(&b, "Started: %s\n", event.StartedAt.Format("2006-01-02 15:04:05 MST"))
	fmt.Fprintf(&b, "Finished: %s\n", event.FinishedAt.Format("2006-01-02 15:04:05 MST"))
	fmt.Fprintf(&b, "Exit code: %d\n", event.ExitCode)
	fmt.Fprintf(&b, "Hosts: %d (failed: %d)\n", event.Hosts, event.FailedHosts)
	if event.Output != "" {
		fmt.Fprintln(&b, "\n--- output tail ---")
		fmt.Fprintln(&b, event.Output)
	}
	return strings.ReplaceAll(b.String(), "\n", "\r\n")
}
