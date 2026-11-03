// Package mail sends transactional email (login codes, notifications).
package mail

import (
	"context"
	"fmt"
	"log/slog"
	"net/smtp"
	"strings"
)

// Mailer sends a plain-text message.
type Mailer interface {
	Send(ctx context.Context, to, subject, body string) error
}

// LogMailer writes messages to the log (development only).
type LogMailer struct{ Log *slog.Logger }

func (m LogMailer) Send(_ context.Context, to, subject, body string) error {
	m.Log.Info("mail (dev)", "to", to, "subject", subject, "body", body)
	return nil
}

// SMTP sends via an SMTP relay with STARTTLS/PLAIN auth.
type SMTP struct {
	Addr, From, User, Password string
}

func (m SMTP) Send(_ context.Context, to, subject, body string) error {
	if strings.ContainsAny(to+subject, "\r\n") {
		return fmt.Errorf("mail: header injection rejected")
	}
	host := m.Addr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	var auth smtp.Auth
	if m.User != "" {
		auth = smtp.PlainAuth("", m.User, m.Password, host)
	}
	msg := "From: " + m.From + "\r\nTo: " + to + "\r\nSubject: " + subject +
		"\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + body
	return smtp.SendMail(m.Addr, auth, m.From, []string{to}, []byte(msg))
}
