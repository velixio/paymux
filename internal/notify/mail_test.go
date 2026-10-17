package notify

import (
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"
	"time"
)

func parseMsg(t *testing.T, raw string) (*mail.Message, map[string]string, []string) {
	t.Helper()
	m, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("unparseable message: %v\n%s", err, raw)
	}
	mt, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mt, "multipart/") {
		t.Fatalf("content-type %q: %v", m.Header.Get("Content-Type"), err)
	}
	mr := multipart.NewReader(m.Body, params["boundary"])
	files := map[string]string{} // attachment filename -> content
	var texts []string
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		var body []byte
		switch p.Header.Get("Content-Transfer-Encoding") {
		case "base64":
			body, err = io.ReadAll(base64Reader(p))
		case "quoted-printable":
			body, err = io.ReadAll(qpReader(p))
		default:
			body, err = io.ReadAll(p)
		}
		if err != nil {
			t.Fatal(err)
		}
		if fn := p.FileName(); fn != "" {
			files[fn] = string(body)
		} else {
			texts = append(texts, string(body))
		}
	}
	return m, files, texts
}

func TestBuildMIMEWithAttachment(t *testing.T) {
	pdf := []byte("%PDF-1.4\n\x00\x01\x02 binary \xff\xfe bytes\n%%EOF")
	raw, err := buildMIME("Paymux <billing@example.com>", Message{
		To: []string{"a@example.com", "b@example.com"}, Subject: "Your invoice — VS/2026-27/000042 ₹1,180.00",
		Text:        "Hi,\nYour invoice is attached.\n.leading dot line\nदन्यवाद",
		Attachments: []Attachment{{Filename: "VS-2026-27-000042.pdf", ContentType: "application/pdf", Data: pdf}},
	}, time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC), "<id1@paymux.example>")
	if err != nil {
		t.Fatal(err)
	}
	m, files, texts := parseMsg(t, string(raw))
	if got := m.Header.Get("To"); !strings.Contains(got, "a@example.com") || !strings.Contains(got, "b@example.com") {
		t.Errorf("To=%q", got)
	}
	subj, _ := new(mime.WordDecoder).DecodeHeader(m.Header.Get("Subject"))
	if subj != "Your invoice — VS/2026-27/000042 ₹1,180.00" {
		t.Errorf("subject %q", subj)
	}
	if m.Header.Get("MIME-Version") != "1.0" || m.Header.Get("Message-Id") != "<id1@paymux.example>" || m.Header.Get("Date") == "" {
		t.Errorf("headers %v", m.Header)
	}
	if files["VS-2026-27-000042.pdf"] != string(pdf) {
		t.Errorf("attachment corrupted: %q", files["VS-2026-27-000042.pdf"])
	}
	if len(texts) != 1 || !strings.Contains(texts[0], "Your invoice is attached.") || !strings.Contains(texts[0], "दन्यवाद") {
		t.Errorf("text %q", texts)
	}
	for _, line := range strings.Split(string(raw), "\r\n") {
		if len(line) > 998 {
			t.Fatalf("line too long (%d)", len(line))
		}
	}
	if strings.Contains(strings.ReplaceAll(string(raw), "\r\n", ""), "\n") {
		t.Error("bare LF in message")
	}
}

func TestBuildMIMEHTMLAlternativeAndNoAttachment(t *testing.T) {
	raw, err := buildMIME("billing@example.com", Message{To: []string{"a@example.com"}, Subject: "s", Text: "plain", HTML: "<p>rich</p>"},
		time.Now(), "<x@y>")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "text/html") || !strings.Contains(string(raw), "text/plain") {
		t.Fatalf("missing alternative parts:\n%s", raw)
	}
}

func TestBuildMIMEHTMLWithAttachment(t *testing.T) {
	raw, err := buildMIME("billing@example.com", Message{To: []string{"a@example.com"}, Subject: "s", Text: "plain body", HTML: "<p>rich body</p>",
		Attachments: []Attachment{{Filename: "a.txt", Data: []byte("hello")}}}, time.Now(), "<x@y>")
	if err != nil {
		t.Fatal(err)
	}
	_, files, _ := parseMsg(t, string(raw))
	if files["a.txt"] != "hello" || !strings.Contains(string(raw), "multipart/alternative") ||
		!strings.Contains(string(raw), "<p>rich body</p>") || !strings.Contains(string(raw), "plain body") {
		t.Fatalf("bad message:\n%s", raw)
	}
}

func TestBuildMIMERejectsHeaderInjectionAndBadInput(t *testing.T) {
	ok := Message{To: []string{"a@example.com"}, Subject: "s", Text: "t"}
	bad := map[string]Message{
		"subject newline": {To: ok.To, Subject: "hi\r\nBcc: evil@example.com", Text: "t"},
		"to newline":      {To: []string{"a@example.com\r\nBcc: evil@example.com"}, Subject: "s", Text: "t"},
		"no recipients":   {Subject: "s", Text: "t"},
		"bad address":     {To: []string{"not an address"}, Subject: "s", Text: "t"},
		"filename newline": {To: ok.To, Subject: "s", Text: "t",
			Attachments: []Attachment{{Filename: "a.pdf\r\nX: y", ContentType: "application/pdf", Data: []byte("x")}}},
		"attachment no name": {To: ok.To, Subject: "s", Text: "t", Attachments: []Attachment{{Data: []byte("x")}}},
	}
	for name, m := range bad {
		if _, err := buildMIME("billing@example.com", m, time.Now(), "<x@y>"); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := buildMIME("billing@example.com", ok, time.Now(), "<x@y>"); err != nil {
		t.Fatal(err)
	}
}

func TestNoopMailer(t *testing.T) {
	var m Mailer = NoopMailer{}
	if err := m.Send(context.Background(), Message{To: []string{"a@example.com"}, Subject: "s"}); err != nil {
		t.Fatal(err)
	}
}
