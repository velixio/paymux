package auth

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

// Password hashing needs 64 MiB each. An unauthenticated caller can ask for as
// many hashes as it can open connections, so the number running at once is
// bounded and the surplus is refused instead of queued.
func TestHashingIsBoundedAndRefusesWhenFull(t *testing.T) {
	SetConcurrency(2)
	t.Cleanup(func() { SetConcurrency(defaultSlots()); observe = nil })
	var inFlight, peak atomic.Int32
	observe = func(delta int) {
		n := inFlight.Add(int32(delta))
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
	}
	hash, err := HashPassword("a long enough password")
	if err != nil {
		t.Fatal(err)
	}
	peak.Store(0)
	var wg sync.WaitGroup
	var busy, done atomic.Int32
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := VerifyPassword(hash, "a long enough password")
			switch {
			case errors.Is(err, ErrBusy):
				busy.Add(1)
			case err == nil && ok:
				done.Add(1)
			default:
				t.Errorf("verify: %v %v", ok, err)
			}
		}()
	}
	wg.Wait()
	if peak.Load() > 2 {
		t.Errorf("%d hashes ran at once, limit 2", peak.Load())
	}
	if busy.Load() == 0 || done.Load() == 0 || busy.Load()+done.Load() != 50 {
		t.Errorf("busy=%d done=%d", busy.Load(), done.Load())
	}
	// Hashing a new password is bounded the same way.
	SetConcurrency(1)
	release, ok := acquire()
	if !ok {
		t.Fatal("slot")
	}
	if _, err := HashPassword("another long password"); !errors.Is(err, ErrBusy) {
		t.Errorf("hash while full: %v", err)
	}
	release()
	// A malformed hash or a weak password costs no slot.
	if _, err := VerifyPassword("garbage", "x"); !errors.Is(err, ErrMalformedHash) {
		t.Errorf("malformed: %v", err)
	}
}
