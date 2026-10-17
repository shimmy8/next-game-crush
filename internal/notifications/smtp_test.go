package notifications

import (
	"bufio"
	"bytes"
	"context"
	"mime"
	"mime/multipart"
	"net"
	"net/mail"
	"os"
	"strings"
	"testing"
)

func TestSMTPSenderTreatsLostFinalReplyAsAmbiguous(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback listener unavailable: %v", err)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		writer := bufio.NewWriter(conn)
		write := func(line string) { _, _ = writer.WriteString(line + "\r\n"); _ = writer.Flush() }
		write("220 test SMTP")
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			switch {
			case strings.HasPrefix(line, "EHLO "):
				write("250-test")
				write("250 8BITMIME")
			case strings.HasPrefix(line, "MAIL FROM:"):
				write("250 ok")
			case strings.HasPrefix(line, "RCPT TO:"):
				write("250 ok")
			case strings.TrimSpace(line) == "DATA":
				write("354 continue")
				for {
					dataLine, readErr := reader.ReadString('\n')
					if readErr != nil {
						return
					}
					if dataLine == ".\r\n" {
						return
					}
				}
			default:
				write("250 ok")
			}
		}
	}()
	sender, err := NewSMTPSender("smtp://" + listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	accepted, sendErr := sender.Send(context.Background(), Message{To: "test@example.test", Subject: "uncertain", Text: "plain", HTML: "<p>plain</p>"})
	<-done
	if !accepted || sendErr == nil {
		t.Fatalf("accepted=%v err=%v", accepted, sendErr)
	}
}

func TestSMTPSenderTreatsExplicitDATARejectionAsRetryable(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback listener unavailable: %v", err)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		writer := bufio.NewWriter(conn)
		write := func(line string) { _, _ = writer.WriteString(line + "\r\n"); _ = writer.Flush() }
		write("220 test SMTP")
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			switch {
			case strings.HasPrefix(line, "EHLO "):
				write("250 test")
			case strings.HasPrefix(line, "MAIL FROM:"), strings.HasPrefix(line, "RCPT TO:"):
				write("250 ok")
			case strings.TrimSpace(line) == "DATA":
				write("354 continue")
				for {
					dataLine, readErr := reader.ReadString('\n')
					if readErr != nil {
						return
					}
					if dataLine == ".\r\n" {
						write("451 temporary rejection")
						return
					}
				}
			default:
				write("250 ok")
			}
		}
	}()
	sender, err := NewSMTPSender("smtp://" + listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	accepted, sendErr := sender.Send(context.Background(), Message{To: "test@example.test", Subject: "rejected", Text: "plain", HTML: "<p>plain</p>"})
	<-done
	if accepted || sendErr == nil {
		t.Fatalf("accepted=%v err=%v", accepted, sendErr)
	}
}

func TestSMTPSenderDeliversToMailpit(t *testing.T) {
	rawURL := os.Getenv("SMTP_TEST_URL")
	if rawURL == "" {
		t.Skip("SMTP_TEST_URL is required")
	}
	sender, err := NewSMTPSender(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := sender.Send(context.Background(), Message{To: "test@next-game-crush.local", Subject: "Task 10 Mailpit verification", Text: "plain digest", HTML: "<p>HTML digest</p>"})
	if err != nil || !accepted {
		t.Fatalf("accepted=%v err=%v", accepted, err)
	}
}

func TestMIMEMessageHasPlainAndHTMLParts(t *testing.T) {
	raw, err := mimeMessage("sender@example.test", Message{To: "reader@example.test", Subject: "Weekly picks\r\nBcc: attacker@example.test", Text: "plain picks", HTML: "<p>html picks</p>"})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Header.Get("Bcc") != "" || strings.ContainsAny(parsed.Header.Get("Subject"), "\r\n") {
		t.Fatal("subject permitted header injection")
	}
	mediaType, params, err := mime.ParseMediaType(parsed.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/alternative" {
		t.Fatalf("type=%q err=%v", mediaType, err)
	}
	reader := multipart.NewReader(parsed.Body, params["boundary"])
	seen := map[string]string{}
	for {
		part, err := reader.NextPart()
		if err != nil {
			break
		}
		buffer := new(bytes.Buffer)
		_, _ = buffer.ReadFrom(part)
		seen[part.Header.Get("Content-Type")] = buffer.String()
	}
	if seen["text/plain; charset=utf-8"] != "plain picks" || seen["text/html; charset=utf-8"] != "<p>html picks</p>" {
		t.Fatalf("parts=%#v", seen)
	}
}
