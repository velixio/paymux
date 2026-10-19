package auth

import (
	"sync/atomic"
	"testing"
)

// cost is the argon2id work factor of new hashes. A hash records the cost it was
// made with, and verifying uses that, so changing this never locks anyone out.
type cost struct {
	time    uint32
	memory  uint32 // KiB
	threads uint8
}

// productionCost is what the hub hashes passwords with: 64 MiB, 3 passes.
var productionCost = cost{time: argonTime, memory: argonMemory, threads: argonThreads}

// testCost is for test binaries only: every test that signs a user in would
// otherwise spend 64 MiB and tens of milliseconds (several times that under the
// race detector) on a password nobody wants to protect.
var testCost = cost{time: 1, memory: 64, threads: 1}

// costFor is the cost used by a program that is (or is not) a test binary.
func costFor(inTestBinary bool) cost {
	if inTestBinary {
		return testCost
	}
	return productionCost
}

var current atomic.Pointer[cost]

func init() {
	// testing.Testing is true only in a binary built by "go test"; the hub, the
	// CLI and a custom build cannot reach the cheap cost.
	c := costFor(testing.Testing())
	current.Store(&c)
}

func hashCost() cost { return *current.Load() }

// UseProductionCost makes hashing use the production cost for the rest of the
// test (the default in a test binary is cheap). It panics outside a test binary.
func UseProductionCost(tb testing.TB) {
	tb.Helper()
	if !testing.Testing() {
		panic("auth: UseProductionCost is for tests")
	}
	prev := current.Load()
	c := productionCost
	current.Store(&c)
	tb.Cleanup(func() { current.Store(prev) })
}
