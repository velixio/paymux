package auth

import (
	"fmt"
	"strings"
	"testing"
)

// Production hashes are 64 MiB, three passes, two lanes, and a program that is
// not a test binary gets exactly that.
func TestProductionHashingCostIsUnchanged(t *testing.T) {
	if c := costFor(false); c != (cost{time: 3, memory: 64 * 1024, threads: 2}) {
		t.Fatalf("production cost changed: %+v", c)
	}
	if costFor(true) == costFor(false) {
		t.Fatal("test binaries are meant to hash cheaply")
	}
	UseProductionCost(t)
	h, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("$m=%d,t=%d,p=%d$", 64*1024, 3, 2); !strings.Contains(h, want) {
		t.Errorf("hash %s lacks %s", h, want)
	}
	if ok, err := VerifyPassword(h, "correct horse battery staple"); err != nil || !ok {
		t.Errorf("verify: %v %v", ok, err)
	}
}

// A cheap hash and a production hash both verify, whichever cost is current.
func TestHashesVerifyWhateverCostMadeThem(t *testing.T) {
	cheap, _ := HashPassword("correct horse battery staple")
	UseProductionCost(t)
	prod, _ := HashPassword("correct horse battery staple")
	for _, h := range []string{cheap, prod} {
		if ok, err := VerifyPassword(h, "correct horse battery staple"); err != nil || !ok {
			t.Errorf("%s: %v %v", h[:30], ok, err)
		}
		if ok, _ := VerifyPassword(h, "wrong horse battery staple"); ok {
			t.Error("a wrong password verified")
		}
	}
}
