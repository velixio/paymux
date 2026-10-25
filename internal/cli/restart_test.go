package cli

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
)

// After the hub stopped because an update was installed, the process starts the
// new binary: the same path, arguments and environment. A normal stop does not.
func TestServeRestartsOntoTheNewBinary(t *testing.T) {
	var gotExe string
	var gotArgv, gotEnv []string
	old := reexec
	reexec = func(exe string, argv, env []string) error { gotExe, gotArgv, gotEnv = exe, argv, env; return nil }
	t.Cleanup(func() { reexec = old })

	var errb bytes.Buffer
	if code := finishServe(false, "/opt/paymux/paymux", &errb); code != 0 || gotExe != "" {
		t.Errorf("a normal stop: code=%d exe=%q", code, gotExe)
	}
	if code := finishServe(true, "/opt/paymux/paymux", &errb); code != 0 {
		t.Errorf("a restart: code=%d err=%q", code, errb.String())
	}
	if gotExe != "/opt/paymux/paymux" || strings.Join(gotArgv, " ") != strings.Join(os.Args, " ") || len(gotEnv) != len(os.Environ()) {
		t.Errorf("re-exec of %q %v (%d env)", gotExe, gotArgv, len(gotEnv))
	}

	reexec = func(string, []string, []string) error { return errors.New("exec format error") }
	errb.Reset()
	if code := finishServe(true, "/opt/paymux/paymux", &errb); code != 1 || !strings.Contains(errb.String(), "exec format error") || !strings.Contains(errb.String(), "start it again") {
		t.Errorf("a failed re-exec: code=%d err=%q", code, errb.String())
	}
}
