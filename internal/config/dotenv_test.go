package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDotEnvParsing(t *testing.T) {
	content := "# a comment\r\n\r\nA=value\r\nB=\"quoted value\"\nC='single'\nexport D=v\nE=plain # trailing\nF=\nG=\"has # hash\"\n  H = spaced  \n"
	got, err := LoadDotEnv(writeTemp(t, content))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"A": "value", "B": "quoted value", "C": "single", "D": "v",
		"E": "plain", "F": "", "G": "has # hash", "H": "spaced",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestDotEnvMissingFile(t *testing.T) {
	got, err := LoadDotEnv(filepath.Join(t.TempDir(), "nope"))
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestDotEnvMalformedLine(t *testing.T) {
	_, err := LoadDotEnv(writeTemp(t, "A=1\n\nNOEQUALS\n"))
	if err == nil || !strings.Contains(err.Error(), "line 3") {
		t.Fatalf("err = %v", err)
	}
}

func TestRealEnvWinsOverDotEnv(t *testing.T) {
	real := func(k string) (string, bool) {
		if k == "A" {
			return "real", true
		}
		return "", false
	}
	l := WithDotEnv(real, map[string]string{"A": "file", "B": "file"})
	if v, _ := l("A"); v != "real" {
		t.Errorf("A = %q", v)
	}
	if v, ok := l("B"); !ok || v != "file" {
		t.Errorf("B = %q %v", v, ok)
	}
	if _, ok := l("C"); ok {
		t.Error("C should be unset")
	}
}
