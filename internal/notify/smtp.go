package notify

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"github.com/velixio/paymux/internal/config"
)

const (
	dialTimeout = 10 * time.Second
	sendTimeout = 30 * time.Second
)

// SMTPConfig configures an SMTPMailer.
type SMTPConfig struct {
	Host     string
	Port     int
	Username string // empty means no authentication
	Password string
	From     string // "Name <addr>" or a bare address
	// TLSConfig optionally customises STARTTLS; ServerName defaults to Host.
	TLSConfig *tls.Config
	rootCAs   *x509.CertPool // tests only
}

// SMTPMailer sends mail over SMTP with STARTTLS.
type SMTPMailer struct{ cfg SMTPConfig }

// NewSMTPMailer builds an SMTPMailer.
func NewSMTPMailer(c SMTPConfig) *SMTPMailer { return &SMTPMailer{cfg: c} }

// New returns the mailer for cfg: an SMTP mailer when PAYMUX_SMTP_HOST is
// set, otherwise a no-op mailer.
func New(cfg config.Config) Mailer {
	if cfg.SMTPHost == "" {
		return NoopMailer{}
	}
	return NewSMTPMailer(SMTPConfig{Host: cfg.SMTPHost, Port: cfg.SMTPPort, Username: cfg.SMTPUsername,
		Password: cfg.SMTPPassword.Reveal(), From: cfg.SMTPFrom})
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (m *SMTPMailer) tlsConfig() *tls.Config {
	var c *tls.Config
	if m.cfg.TLSConfig != nil {
		c = m.cfg.TLSConfig.Clone()
	} else {
		c = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	if c.ServerName == "" {
		c.ServerName = m.cfg.Host
	}
	if m.cfg.rootCAs != nil {
		c.RootCAs = m.cfg.rootCAs
	}
	return c
}

// Send delivers msg. Mail and credentials only go over TLS (STARTTLS); the
// one exception is a credential-less connection to a loopback host, for local
// development servers.
func (m *SMTPMailer) Send(ctx context.Context, msg Message) error {
	from, err := mail.ParseAddress(m.cfg.From)
	if err != nil {
		return fmt.Errorf("smtp: invalid from address: %w", err)
	}
	var idRand [12]byte
	_, _ = rand.Read(idRand[:])
	domain := "paymux.invalid"
	if i := strings.LastIndex(from.Address, "@"); i >= 0 {
		domain = from.Address[i+1:]
	}
	raw, err := buildMIME(m.cfg.From, msg, time.Now(), "<"+hex.EncodeToString(idRand[:])+"@"+domain+">")
	if err != nil {
		return fmt.Errorf("smtp: %w", err)
	}

	d := net.Dialer{Timeout: dialTimeout}
	addr := net.JoinHostPort(m.cfg.Host, strconv.Itoa(m.cfg.Port))
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("smtp: connect %s: %w", addr, err)
	}
	deadline := time.Now().Add(sendTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	c, err := smtp.NewClient(conn, m.cfg.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("smtp: greeting from %s: %w", addr, err)
	}
	defer c.Close()

	if ok, _ := c.Extension("STARTTLS"); ok {
		if err := c.StartTLS(m.tlsConfig()); err != nil {
			return fmt.Errorf("smtp: STARTTLS: %w", err)
		}
	} else if m.cfg.Username != "" || !isLoopback(m.cfg.Host) {
		return errors.New("smtp: server does not offer STARTTLS; refusing to send mail or credentials in clear text")
	}
	if m.cfg.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", m.cfg.Username, m.cfg.Password, m.cfg.Host)); err != nil {
			return fmt.Errorf("smtp: authentication failed: %w", err)
		}
	}
	if err := c.Mail(from.Address); err != nil {
		return fmt.Errorf("smtp: MAIL FROM: %w", err)
	}
	for _, r := range msg.To {
		a, _ := mail.ParseAddress(r) // validated by buildMIME
		if err := c.Rcpt(a.Address); err != nil {
			return fmt.Errorf("smtp: RCPT TO %s: %w", a.Address, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp: DATA: %w", err)
	}
	if _, err := w.Write(raw); err != nil {
		return fmt.Errorf("smtp: write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp: send: %w", err)
	}
	_ = c.Quit()
	return nil
}
