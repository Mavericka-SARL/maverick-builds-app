package notification

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeSMTP speaks just enough SMTP (no TLS, no AUTH) to record one
// message, or to accept the connection and never answer.
type fakeSMTP struct {
	ln    net.Listener
	got   chan string // the DATA payload, envelope prepended
	stall bool
}

func newFakeSMTP(t *testing.T, stall bool) *fakeSMTP {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln, got: make(chan string, 1), stall: stall}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close() //nolint:errcheck
		if f.stall {
			time.Sleep(5 * time.Second)
			return
		}
		r, w := bufio.NewReader(conn), bufio.NewWriter(conn)
		say := func(s string) { _, _ = w.WriteString(s + "\r\n"); _ = w.Flush() }
		say("220 fake ESMTP")
		var env, data strings.Builder
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			switch {
			case strings.HasPrefix(line, "EHLO"):
				say("250-fake")
				say("250 8BITMIME")
			case strings.HasPrefix(line, "MAIL FROM:"), strings.HasPrefix(line, "RCPT TO:"):
				env.WriteString(line + "\n")
				say("250 OK")
			case line == "DATA":
				say("354 go ahead")
				for {
					l, err := r.ReadString('\n')
					if err != nil || l == ".\r\n" {
						break
					}
					data.WriteString(l)
				}
				say("250 queued")
				f.got <- env.String() + data.String()
			case line == "QUIT":
				say("221 bye")
				return
			default:
				say("250 OK")
			}
		}
	}()
	return f
}

func (f *fakeSMTP) hostPort() (string, int) {
	addr := f.ln.Addr().(*net.TCPAddr)
	return addr.IP.String(), addr.Port
}

// SMTPMailer against a real (if minimal) SMTP conversation: the envelope
// carries the configured sender and the bare recipient, the headers the
// display names, and a stalled relay is cut off by the caller's deadline
// rather than hanging the dispatcher.
func TestSMTPMailerWire(t *testing.T) {
	t.Run("delivers with envelope and headers", func(t *testing.T) {
		srv := newFakeSMTP(t, false)
		host, port := srv.hostPort()
		m := NewMailer(SMTPConfig{Host: host, Port: port, From: "no-reply@example.test"}).(*SMTPMailer)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := m.SendAs(ctx, "Acme Planning", "ann@example.test", "Ann Lee", "Budget due", "Please review."); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-srv.got:
			for _, want := range []string{
				"MAIL FROM:<no-reply@example.test>",
				"RCPT TO:<ann@example.test>",
				"From: \"Acme Planning\" <no-reply@example.test>",
				"To: Ann Lee <ann@example.test>",
				"Subject: Budget due",
				"Content-Type: text/plain; charset=UTF-8",
				"\r\n\r\nPlease review.",
			} {
				if !strings.Contains(got, want) {
					t.Fatalf("message lacks %q:\n%s", want, got)
				}
			}
		case <-time.After(5 * time.Second):
			t.Fatal("relay recorded nothing")
		}
	})

	t.Run("an unbranded message is from the platform by name", func(t *testing.T) {
		srv := newFakeSMTP(t, false)
		host, port := srv.hostPort()
		m := NewMailer(SMTPConfig{Host: host, Port: port, From: "no-reply@example.test"})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := m.Send(ctx, "ann@example.test", "", "Budget due", "Please review."); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-srv.got:
			if want := "From: \"" + PlatformName + "\" <no-reply@example.test>"; !strings.Contains(got, want) {
				t.Fatalf("message lacks %q:\n%s", want, got)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("relay recorded nothing")
		}
	})

	t.Run("a stalled relay is bounded by the caller's deadline", func(t *testing.T) {
		srv := newFakeSMTP(t, true)
		host, port := srv.hostPort()
		m := NewMailer(SMTPConfig{Host: host, Port: port})
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		start := time.Now()
		err := m.Send(ctx, "ann@example.test", "", "x", "y")
		if err == nil || !strings.Contains(err.Error(), "deadline") || time.Since(start) > 2*time.Second {
			t.Fatalf("err=%v after %s", err, time.Since(start))
		}
	})

	t.Run("no host means no mailer", func(t *testing.T) {
		if NewMailer(SMTPConfig{}) != nil {
			t.Fatal("expected nil")
		}
	})
}
