package hub

import (
	"testing"
	"time"
)

// The dashboard's update asks for a restart: Start must stop gracefully, like
// on SIGTERM, return nil, and say that a restart was asked for.
func TestRestartRequestStopsStartGracefully(t *testing.T) {
	dir := t.TempDir()
	r := startApp(t, testConfig(t, "sqlite://"+dir+"/paymux.db", dir))
	waitReady(t, r)
	if r.app.RestartRequested() {
		t.Fatal("a restart is requested before anyone asked")
	}
	r.app.requestRestart()
	select {
	case <-r.done:
	case <-time.After(30 * time.Second):
		t.Fatal("Start did not return after the restart request")
	}
	if r.err != nil {
		t.Errorf("Start returned %v", r.err)
	}
	if !r.app.RestartRequested() {
		t.Error("RestartRequested is false after a restart request")
	}
}

// A plain shutdown is not a restart.
func TestShutdownIsNotARestart(t *testing.T) {
	dir := t.TempDir()
	r := startApp(t, testConfig(t, "sqlite://"+dir+"/paymux.db", dir))
	waitReady(t, r)
	r.stop()
	<-r.done
	if r.app.RestartRequested() {
		t.Error("a signal shutdown counts as a restart")
	}
}
