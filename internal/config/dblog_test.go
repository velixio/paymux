package config

import (
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestDBLogLevel(t *testing.T) {
	c, err := Load(base())
	if err != nil || c.DBLogLevel != slog.LevelWarn {
		t.Fatalf("default is warn: %v %v", c.DBLogLevel, err)
	}
	for in, want := range map[string]slog.Level{"warn": slog.LevelWarn, "error": slog.LevelError, "ERROR": slog.LevelError, "info": slog.LevelInfo, "debug": slog.LevelDebug} {
		c, err := Load(base("PAYMUX_DB_LOG_LEVEL", in))
		if err != nil || c.DBLogLevel != want {
			t.Errorf("%s: %v %v", in, c.DBLogLevel, err)
		}
	}
	_, err = Load(base("PAYMUX_DB_LOG_LEVEL", "loud"))
	wantErr(t, err, "PAYMUX_DB_LOG_LEVEL")
}

func TestOTLPLogsConfig(t *testing.T) {
	c, err := Load(base())
	if err != nil || c.OTLPLogsEndpoint != "" || len(c.OTLPLogsHeaders) != 0 {
		t.Fatalf("off by default: %+v %v", c, err)
	}
	c, err = Load(base("PAYMUX_OTLP_LOGS_ENDPOINT", "http://127.0.0.1:4318/v1/logs", "PAYMUX_OTLP_LOGS_HEADERS", "Authorization=Bearer abc, X-Tenant=t1"))
	if err != nil || c.OTLPLogsEndpoint != "http://127.0.0.1:4318/v1/logs" || c.OTLPLogsHeaders["Authorization"].Reveal() != "Bearer abc" || c.OTLPLogsHeaders["X-Tenant"].Reveal() != "t1" {
		t.Fatalf("%+v %v", c, err)
	}
	if strings.Contains(fmt.Sprintf("%v", c.OTLPLogsHeaders), "abc") {
		t.Error("header values must not print")
	}
	for _, bad := range []string{"ftp://x/y", "not a url", "/v1/logs"} {
		_, err = Load(base("PAYMUX_OTLP_LOGS_ENDPOINT", bad))
		wantErr(t, err, "PAYMUX_OTLP_LOGS_ENDPOINT")
	}
	_, err = Load(base("PAYMUX_OTLP_LOGS_ENDPOINT", "https://otel.example.com/v1/logs", "PAYMUX_OTLP_LOGS_HEADERS", "novalue"))
	wantErr(t, err, "PAYMUX_OTLP_LOGS_HEADERS")
	_, err = Load(base("PAYMUX_OTLP_LOGS_HEADERS", "A=b"))
	wantErr(t, err, "PAYMUX_OTLP_LOGS_ENDPOINT")
}
