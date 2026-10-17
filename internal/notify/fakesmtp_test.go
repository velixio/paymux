package notify

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSMTP is a tiny SMTP server for tests. It records each accepted message.
type fakeSMTP struct {
	ln       net.Listener
	tlsConf  *tls.Config
	roots    *x509.CertPool
	starttls bool // advertise STARTTLS

	mu       sync.Mutex
	msgs     []sentMail
	sawTLS   bool
	sawAuth  string // decoded AUTH PLAIN payload
	sawPlain bool   // AUTH or DATA seen on an unencrypted connection
}

type sentMail struct {
	From string
	To   []string
	Data string
}

func newFakeSMTP(t *testing.T, starttls bool) *fakeSMTP {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fake smtp"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"}, IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeSMTP{ln: ln, roots: pool, starttls: starttls,
		tlsConf: &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	return s
}

func (s *fakeSMTP) addr() (string, int) {
	a := s.ln.Addr().(*net.TCPAddr)
	return a.IP.String(), a.Port
}

func (s *fakeSMTP) messages() []sentMail {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sentMail(nil), s.msgs...)
}

func (s *fakeSMTP) serve(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	conn := c
	r := bufio.NewReader(conn)
	say := func(f string) { conn.Write([]byte(f + "\r\n")) }
	say("220 fake ESMTP")
	secure := false
	var cur sentMail
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		up := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(up, "EHLO"), strings.HasPrefix(up, "HELO"):
			conn.Write([]byte("250-fake\r\n"))
			if s.starttls && !secure {
				conn.Write([]byte("250-STARTTLS\r\n"))
			}
			say("250 AUTH PLAIN")
		case up == "STARTTLS":
			say("220 go ahead")
			tc := tls.Server(conn, s.tlsConf)
			if err := tc.Handshake(); err != nil {
				return
			}
			conn, r, secure = tc, bufio.NewReader(tc), true
			s.mu.Lock()
			s.sawTLS = true
			s.mu.Unlock()
		case strings.HasPrefix(up, "AUTH PLAIN"):
			payload := strings.TrimSpace(line[len("AUTH PLAIN"):])
			dec, _ := base64.StdEncoding.DecodeString(payload)
			s.mu.Lock()
			s.sawAuth = strings.ReplaceAll(string(dec), "\x00", "|")
			if !secure {
				s.sawPlain = true
			}
			s.mu.Unlock()
			say("235 ok")
		case strings.HasPrefix(up, "MAIL FROM:"):
			cur = sentMail{From: strings.Trim(line[len("MAIL FROM:"):], "<> ")}
			say("250 ok")
		case strings.HasPrefix(up, "RCPT TO:"):
			cur.To = append(cur.To, strings.Trim(line[len("RCPT TO:"):], "<> "))
			say("250 ok")
		case up == "DATA":
			say("354 go")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(strings.TrimPrefix(l, "."))
			}
			cur.Data = b.String()
			s.mu.Lock()
			s.msgs = append(s.msgs, cur)
			if !secure {
				s.sawPlain = true
			}
			s.mu.Unlock()
			say("250 queued")
		case up == "QUIT":
			say("221 bye")
			return
		default:
			say("250 ok")
		}
	}
}
