package notify

import (
	"context"
	"strings"
	"testing"

	"github.com/paymux/paymux/internal/config"
)

func smtpFor(s *fakeSMTP, user, pass string) *SMTPMailer {
	host, port := s.addr()
	return NewSMTPMailer(SMTPConfig{Host: host, Port: port, Username: user, Password: pass,
		From: "Paymux <billing@example.com>", TLSConfig: nil, rootCAs: s.roots})
}

func TestSMTPSendsOverSTARTTLSWithAuth(t *testing.T) {
	s := newFakeSMTP(t, true)
	m := smtpFor(s, "mailer", "s3cret")
	err := m.Send(context.Background(), Message{To: []string{"cust@example.com", "cc@example.com"}, Subject: "Invoice", Text: "attached",
		Attachments: []Attachment{{Filename: "inv.pdf", ContentType: "application/pdf", Data: []byte("%PDF-bytes")}}})
	if err != nil {
		t.Fatal(err)
	}
	msgs := s.messages()
	if len(msgs) != 1 {
		t.Fatalf("messages=%d", len(msgs))
	}
	got := msgs[0]
	if got.From != "billing@example.com" || strings.Join(got.To, ",") != "cust@example.com,cc@example.com" {
		t.Errorf("envelope %+v", got)
	}
	if !s.sawTLS {
		t.Error("STARTTLS not used")
	}
	if s.sawPlain {
		t.Error("credentials or data sent in clear")
	}
	if s.sawAuth != "|mailer|s3cret" {
		t.Errorf("auth %q", s.sawAuth)
	}
	_, files, _ := parseMsg(t, got.Data)
	if files["inv.pdf"] != "%PDF-bytes" {
		t.Errorf("attachment %q", files)
	}
}

func TestSMTPRefusesCredentialsWithoutSTARTTLS(t *testing.T) {
	s := newFakeSMTP(t, false)
	err := smtpFor(s, "mailer", "s3cret").Send(context.Background(), Message{To: []string{"a@example.com"}, Subject: "s", Text: "t"})
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("err=%v", err)
	}
	if s.sawAuth != "" || len(s.messages()) != 0 {
		t.Fatal("sent despite missing STARTTLS")
	}
}

func TestSMTPPlaintextAllowedOnlyForCredentiallessLoopback(t *testing.T) {
	s := newFakeSMTP(t, false)
	if err := smtpFor(s, "", "").Send(context.Background(), Message{To: []string{"a@example.com"}, Subject: "s", Text: "t"}); err != nil {
		t.Fatalf("loopback dev server: %v", err)
	}
	if len(s.messages()) != 1 {
		t.Fatal("not delivered")
	}
}

func TestSMTPConnectionFailureAndCanceledContext(t *testing.T) {
	s := newFakeSMTP(t, true)
	m := smtpFor(s, "", "")
	s.ln.Close()
	if err := m.Send(context.Background(), Message{To: []string{"a@example.com"}, Subject: "s", Text: "t"}); err == nil {
		t.Fatal("expected a connection error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.Send(ctx, Message{To: []string{"a@example.com"}, Subject: "s", Text: "t"}); err == nil {
		t.Fatal("canceled context ignored")
	}
}

func TestSMTPErrorsNeverContainPassword(t *testing.T) {
	s := newFakeSMTP(t, false)
	err := smtpFor(s, "mailer", "s3cret-pass").Send(context.Background(), Message{To: []string{"a@example.com"}, Subject: "s", Text: "t"})
	if err == nil || strings.Contains(err.Error(), "s3cret-pass") {
		t.Fatalf("err=%v", err)
	}
}

func TestNewFromConfig(t *testing.T) {
	if _, ok := New(config.Config{}).(NoopMailer); !ok {
		t.Fatal("unset host must give the no-op mailer")
	}
	m := New(config.Config{SMTPHost: "smtp.example.com", SMTPPort: 587, SMTPFrom: "b@example.com", SMTPPassword: "x"})
	if _, ok := m.(*SMTPMailer); !ok {
		t.Fatalf("got %T", m)
	}
}
