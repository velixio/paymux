// Package notify sends email (customer documents, ops alerts) and raises ops
// alerts to email and a Slack-compatible webhook.
package notify

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strings"
	"time"
)

// Attachment is a file attached to a Message.
type Attachment struct {
	Filename    string
	ContentType string // default application/octet-stream
	Data        []byte
}

// Message is an email. Text is required; HTML is an optional alternative.
type Message struct {
	To          []string
	Subject     string
	Text        string
	HTML        string
	Attachments []Attachment
}

// Mailer sends messages.
type Mailer interface {
	Send(ctx context.Context, m Message) error
}

// NoopMailer discards every message. It is used when SMTP is not configured.
type NoopMailer struct{}

// Send does nothing.
func (NoopMailer) Send(context.Context, Message) error { return nil }

func hasCRLF(s string) bool { return strings.ContainsAny(s, "\r\n") }

func newBoundary() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "paymux-" + hex.EncodeToString(b)
}

// buildMIME renders m as an RFC 5322 message with CRLF line endings.
func buildMIME(from string, m Message, now time.Time, msgID string) ([]byte, error) {
	fa, err := mail.ParseAddress(from)
	if err != nil || hasCRLF(from) {
		return nil, fmt.Errorf("invalid From address %q", from)
	}
	if len(m.To) == 0 {
		return nil, errors.New("message has no recipients")
	}
	var to []string
	for _, r := range m.To {
		a, err := mail.ParseAddress(r)
		if err != nil || hasCRLF(r) {
			return nil, fmt.Errorf("invalid recipient %q", r)
		}
		to = append(to, a.String())
	}
	if hasCRLF(m.Subject) || hasCRLF(msgID) {
		return nil, errors.New("header values must not contain line breaks")
	}
	for _, a := range m.Attachments {
		if a.Filename == "" || hasCRLF(a.Filename) || hasCRLF(a.ContentType) {
			return nil, fmt.Errorf("invalid attachment name %q", a.Filename)
		}
	}

	var buf bytes.Buffer
	hdr := func(k, v string) { buf.WriteString(k + ": " + v + "\r\n") }
	hdr("From", fa.String())
	hdr("To", strings.Join(to, ", "))
	hdr("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	hdr("Date", now.Format(time.RFC1123Z))
	hdr("Message-ID", msgID)
	hdr("MIME-Version", "1.0")

	switch {
	case len(m.Attachments) > 0:
		w := multipart.NewWriter(&buf)
		_ = w.SetBoundary(newBoundary())
		hdr("Content-Type", "multipart/mixed; boundary="+w.Boundary())
		buf.WriteString("\r\n")
		if err := writeBody(w, m); err != nil {
			return nil, err
		}
		for _, a := range m.Attachments {
			ct := a.ContentType
			if ct == "" {
				ct = "application/octet-stream"
			}
			h := textproto.MIMEHeader{}
			h.Set("Content-Type", ct)
			h.Set("Content-Transfer-Encoding", "base64")
			h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": a.Filename}))
			pw, err := w.CreatePart(h)
			if err != nil {
				return nil, err
			}
			writeBase64(pw, a.Data)
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
	case m.HTML != "":
		w := multipart.NewWriter(&buf)
		_ = w.SetBoundary(newBoundary())
		hdr("Content-Type", "multipart/alternative; boundary="+w.Boundary())
		buf.WriteString("\r\n")
		if err := writeAlternatives(w, m); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
	default:
		hdr("Content-Type", `text/plain; charset="utf-8"`)
		hdr("Content-Transfer-Encoding", "quoted-printable")
		buf.WriteString("\r\n")
		writeQP(&buf, m.Text)
	}
	return buf.Bytes(), nil
}

func writeQP(w interface{ Write([]byte) (int, error) }, s string) {
	q := quotedprintable.NewWriter(w)
	_, _ = q.Write([]byte(s))
	_ = q.Close()
}

func writeAlternatives(w *multipart.Writer, m Message) error {
	for _, p := range []struct{ ct, body string }{{"text/plain", m.Text}, {"text/html", m.HTML}} {
		if p.body == "" {
			continue
		}
		h := textproto.MIMEHeader{}
		h.Set("Content-Type", p.ct+`; charset="utf-8"`)
		h.Set("Content-Transfer-Encoding", "quoted-printable")
		pw, err := w.CreatePart(h)
		if err != nil {
			return err
		}
		writeQP(pw, p.body)
	}
	return nil
}

// writeBody writes the text (and optional HTML) part(s) into a mixed message.
func writeBody(w *multipart.Writer, m Message) error {
	if m.HTML == "" {
		return writeAlternatives(w, Message{Text: m.Text})
	}
	boundary := newBoundary()
	h := textproto.MIMEHeader{}
	h.Set("Content-Type", "multipart/alternative; boundary="+boundary)
	pw, err := w.CreatePart(h)
	if err != nil {
		return err
	}
	iw := multipart.NewWriter(pw)
	_ = iw.SetBoundary(boundary)
	if err := writeAlternatives(iw, m); err != nil {
		return err
	}
	return iw.Close()
}

func writeBase64(w interface{ Write([]byte) (int, error) }, data []byte) {
	enc := base64.StdEncoding.EncodeToString(data)
	for len(enc) > 76 {
		_, _ = w.Write([]byte(enc[:76] + "\r\n"))
		enc = enc[76:]
	}
	_, _ = w.Write([]byte(enc + "\r\n"))
}
