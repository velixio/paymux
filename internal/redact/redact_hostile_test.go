package redact

import (
	"strings"
	"testing"
	"time"
)

// Hostile input terminates. This runs everywhere, including under -race and
// on slow CI runners, so it asserts only that the work finishes, with a
// ceiling far above any honest run; the scaling check is
// TestHostileInputIsLinear, which runs without the race detector.
func TestHostileInputTerminates(t *testing.T) {
	r := New()
	r.SetKnown("a", "KNOWNSECRETVALUE")
	for name, in := range map[string]string{
		"colons": strings.Repeat("a:", 16<<10), "form": strings.Repeat("a=b&", 8<<10), "digits+": strings.Repeat("1 ", 16<<10),
		"json": strings.Repeat(`{"a":`, 5000), "digits": strings.Repeat("4", 32<<10), "pem": strings.Repeat("-----BEGIN X-----", 2000),
	} {
		done := make(chan struct{})
		go func() { r.Body("", []byte(in)); r.Text(in); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Minute):
			t.Fatalf("%s: redacting %d bytes does not finish", name, len(in))
		}
	}
}
