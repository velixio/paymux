package logging

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
)

func logOnce(t *testing.T, fn func(l *slog.Logger)) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	fn(New(&buf, slog.LevelInfo))
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("not JSON: %q: %v", buf.String(), err)
	}
	return m
}

func maskStr(t *testing.T, in string) string {
	t.Helper()
	m := logOnce(t, func(l *slog.Logger) { l.Info("m", "v", in) })
	return m["v"].(string)
}

func TestRedactsSecretKeys(t *testing.T) {
	m := logOnce(t, func(l *slog.Logger) {
		l.Info("m",
			"client_secret", "a", "Password", "b", "access_token", "c", "Authorization", "d",
			"set-cookie", "e", "x_signature", "f", "api_key", "g", "master_key", "h",
			"request_id", "req_1", "idempotency_key", "idem_1",
			slog.Group("req", slog.String("password", "p"), slog.Group("inner", slog.String("token", "t"), slog.String("ok", "fine"))),
		)
	})
	for _, k := range []string{"client_secret", "Password", "access_token", "Authorization", "set-cookie", "x_signature", "api_key", "master_key"} {
		if m[k] != "[REDACTED]" {
			t.Errorf("%s = %v", k, m[k])
		}
	}
	if m["request_id"] != "req_1" || m["idempotency_key"] != "idem_1" {
		t.Errorf("ids changed: %v", m)
	}
	req := m["req"].(map[string]any)
	inner := req["inner"].(map[string]any)
	if req["password"] != "[REDACTED]" || inner["token"] != "[REDACTED]" || inner["ok"] != "fine" {
		t.Errorf("nested: %v", req)
	}
}

func TestRedactsViaWithGroup(t *testing.T) {
	m := logOnce(t, func(l *slog.Logger) { l.WithGroup("g").Info("m", "secret", "x") })
	if m["g"].(map[string]any)["secret"] != "[REDACTED]" {
		t.Fatal(m)
	}
}

func TestMasksEmail(t *testing.T) {
	if got := maskStr(t, "riya.sharma@example.com"); got != "r***@example.com" {
		t.Fatal(got)
	}
	if got := maskStr(t, "mail riya.sharma@example.com now"); got != "mail r***@example.com now" {
		t.Fatal(got)
	}
}

func TestMasksCardLikeDigits(t *testing.T) {
	for _, in := range []string{"4111 1111 1111 1111", "4111111111111111", "4111-1111-1111-1111"} {
		if got := maskStr(t, in); got != "[PAN]" {
			t.Errorf("%q -> %q", in, got)
		}
	}
}

func TestMasksPhone(t *testing.T) {
	for _, in := range []string{"+91 98765 43210", "9876543210"} {
		if got := maskStr(t, in); got != "******3210" {
			t.Errorf("%q -> %q", in, got)
		}
	}
}

func TestMasksGSTIN(t *testing.T) {
	if got := maskStr(t, "27AAPFU0939F1ZV"); got != "27**********1ZV" {
		t.Fatal(got)
	}
}

func TestMasksURLPassword(t *testing.T) {
	if got := maskStr(t, "dial postgres://u:s3cret@h/db failed"); got != "dial postgres://u:xxxxx@h/db failed" {
		t.Fatal(got)
	}
}

func TestMasksErrorValues(t *testing.T) {
	m := logOnce(t, func(l *slog.Logger) { l.Error("m", "err", errors.New("open postgres://u:s3cret@h/db")) })
	if m["err"] != "open postgres://u:xxxxx@h/db" {
		t.Fatal(m["err"])
	}
}

func TestMasksMessage(t *testing.T) {
	m := logOnce(t, func(l *slog.Logger) { l.Info("sent to riya.sharma@example.com") })
	if m["msg"] != "sent to r***@example.com" {
		t.Fatal(m["msg"])
	}
}

func TestLeavesIDsAlone(t *testing.T) {
	for _, in := range []string{"pay_01jbx4k9z3m7q2w8e5r6t1y0ab", "0123456789ABCDEFGHJK", "2026-10-17T10:00:00Z"} {
		if got := maskStr(t, in); got != in {
			t.Errorf("%q -> %q", in, got)
		}
	}
}

func TestNonStringValuesUntouched(t *testing.T) {
	m := logOnce(t, func(l *slog.Logger) { l.Info("m", "amount", int64(1234567890123), "ok", true) })
	if m["amount"] != float64(1234567890123) || m["ok"] != true {
		t.Fatal(m)
	}
}
