package notifications

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime/multipart"
	"net"
	"net/smtp"
	"net/textproto"
	"net/url"
	"strings"
	"time"
)

type Message struct{ To, Subject, Text, HTML string }
type Sender interface {
	Send(context.Context, Message) (accepted bool, err error)
}
type SenderFunc func(context.Context, Message) (bool, error)

func (f SenderFunc) Send(ctx context.Context, message Message) (bool, error) { return f(ctx, message) }

type SMTPSender struct {
	address, host, username, password, from string
	tls                                     bool
	timeout                                 time.Duration
}

func NewSMTPSender(rawURL string) (*SMTPSender, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "smtp" && u.Scheme != "smtps") || u.Hostname() == "" {
		return nil, errors.New("valid smtp URL is required")
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "smtps" {
			port = "465"
		} else {
			port = "25"
		}
	}
	username, password := "", ""
	if u.User != nil {
		username = u.User.Username()
		password, _ = u.User.Password()
	}
	return &SMTPSender{address: net.JoinHostPort(u.Hostname(), port), host: u.Hostname(), username: username, password: password, from: "next-game-crush@localhost", tls: u.Scheme == "smtps", timeout: 10 * time.Second}, nil
}

func (s *SMTPSender) Send(ctx context.Context, message Message) (bool, error) {
	if strings.TrimSpace(message.To) == "" {
		return false, errors.New("recipient is required")
	}
	dialer := net.Dialer{Timeout: s.timeout}
	var conn net.Conn
	var err error
	if s.tls {
		conn, err = tls.DialWithDialer(&dialer, "tcp", s.address, &tls.Config{ServerName: s.host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", s.address)
	}
	if err != nil {
		return false, fmt.Errorf("connect SMTP: %w", err)
	}
	defer conn.Close()
	deadline := time.Now().Add(s.timeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return false, fmt.Errorf("set SMTP deadline: %w", err)
	}
	client, err := smtp.NewClient(conn, s.host)
	if err != nil {
		return false, fmt.Errorf("start SMTP: %w", err)
	}
	defer client.Close()
	if s.username != "" {
		if err = client.Auth(smtp.PlainAuth("", s.username, s.password, s.host)); err != nil {
			return false, fmt.Errorf("authenticate SMTP: %w", err)
		}
	}
	if err = client.Mail(s.from); err != nil {
		return false, fmt.Errorf("set SMTP sender: %w", err)
	}
	if err = client.Rcpt(message.To); err != nil {
		return false, fmt.Errorf("set SMTP recipient: %w", err)
	}
	w, err := client.Data()
	if err != nil {
		return false, fmt.Errorf("open SMTP data: %w", err)
	}
	raw, err := mimeMessage(s.from, message)
	if err != nil {
		return false, err
	}
	if _, err = w.Write(raw); err != nil {
		_ = w.Close()
		return false, fmt.Errorf("write SMTP data: %w", err)
	}
	if err = w.Close(); err != nil {
		var rejection *textproto.Error
		if errors.As(err, &rejection) {
			return false, fmt.Errorf("SMTP rejected message: %w", err)
		}
		return true, fmt.Errorf("SMTP acceptance uncertain: %w", err)
	}
	return true, nil
}

func mimeMessage(from string, message Message) ([]byte, error) {
	var buffer bytes.Buffer
	boundary := "next-game-crush-alternative"
	fmt.Fprintf(&buffer, "From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: multipart/alternative; boundary=%q\r\n\r\n", from, message.To, strings.ReplaceAll(strings.ReplaceAll(message.Subject, "\r", ""), "\n", ""), boundary)
	w := multipart.NewWriter(&buffer)
	if err := w.SetBoundary(boundary); err != nil {
		return nil, err
	}
	for _, part := range []struct{ kind, value string }{{"text/plain; charset=utf-8", message.Text}, {"text/html; charset=utf-8", message.HTML}} {
		h := textproto.MIMEHeader{}
		h.Set("Content-Type", part.kind)
		h.Set("Content-Transfer-Encoding", "8bit")
		p, err := w.CreatePart(h)
		if err != nil {
			return nil, err
		}
		if _, err = p.Write([]byte(part.value)); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}
