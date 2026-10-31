package ids

import (
	"math/rand"
	"regexp"
	"testing"
	"time"
)

func steppingClock() func() time.Time {
	t := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	return func() time.Time {
		t = t.Add(time.Millisecond)
		return t
	}
}

func testGen(clock func() time.Time) *Generator {
	return NewGenerator(clock, rand.New(rand.NewSource(1)))
}

func TestNewFormat(t *testing.T) {
	re := regexp.MustCompile(`^pay_[0-9a-hjkmnp-tv-z]{26}$`)
	for _, g := range []*Generator{testGen(steppingClock()), NewGenerator(time.Now, nil)} {
		if id := g.New("pay"); !re.MatchString(id) {
			t.Fatalf("bad id %q", id)
		}
	}
	if id := New("cus"); !regexp.MustCompile(`^cus_[0-9a-hjkmnp-tv-z]{26}$`).MatchString(id) {
		t.Fatalf("bad default id %q", id)
	}
}

func TestPaymentFormat(t *testing.T) {
	re := regexp.MustCompile(`^[0-9A-Za-z]{14}$`)
	for _, g := range []*Generator{testGen(steppingClock()), NewGenerator(time.Now, nil)} {
		if id := g.Payment(); !re.MatchString(id) {
			t.Fatalf("bad payment id %q", id)
		}
	}
	if id := Payment(); !re.MatchString(id) {
		t.Fatalf("bad default payment id %q", id)
	}
}

func TestNewPanicsOnBadPrefix(t *testing.T) {
	g := testGen(steppingClock())
	for _, p := range []string{"", "Pay", "pa_y", "a", "toolong"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("prefix %q did not panic", p)
				}
			}()
			g.New(p)
		}()
	}
}

func TestProviderRefFormat(t *testing.T) {
	re := regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{20}$`)
	if r := testGen(steppingClock()).ProviderRef(); !re.MatchString(r) {
		t.Fatalf("bad ref %q", r)
	}
	if r := ProviderRef(); !re.MatchString(r) {
		t.Fatalf("bad default ref %q", r)
	}
}

func decodeBits(s string) []int {
	var all []int
	for _, c := range s {
		v := -1
		for i := 0; i < len(lowerAlphabet); i++ {
			if lowerAlphabet[i] == byte(c) {
				v = i
			}
		}
		for i := 4; i >= 0; i-- {
			all = append(all, (v>>i)&1)
		}
	}
	return all[2:] // drop the 2 padding bits of the 130-bit encoding
}

func TestUUIDv7Bits(t *testing.T) {
	clock := steppingClock()
	id := testGen(clock).New("pay")[4:]
	if id[0] > '7' {
		t.Fatalf("leading char %q exceeds 3 bits", id[0])
	}
	bits := decodeBits(id)
	if len(bits) != 128 {
		t.Fatalf("len %d", len(bits))
	}
	if bits[48] != 0 || bits[49] != 1 || bits[50] != 1 || bits[51] != 1 {
		t.Fatalf("version nibble not 0111: %v", bits[48:52])
	}
	if bits[64] != 1 || bits[65] != 0 {
		t.Fatalf("variant not 10: %v", bits[64:66])
	}
	var ms int64
	for _, b := range bits[:48] {
		ms = ms<<1 | int64(b)
	}
	want := time.Date(2026, 10, 17, 0, 0, 0, int(time.Millisecond), time.UTC).UnixMilli()
	if ms != want {
		t.Fatalf("timestamp %d, want %d", ms, want)
	}
}

func TestSortableByTime(t *testing.T) {
	gens := map[string]*Generator{
		"seeded": testGen(steppingClock()),
		"crypto": NewGenerator(steppingClock(), nil),
	}
	for name, g := range gens {
		prevID, prevRef := "", ""
		for i := 0; i < 1000; i++ {
			id, ref := g.New("pay"), g.ProviderRef()
			if id <= prevID || ref <= prevRef {
				t.Fatalf("%s: not strictly increasing at %d", name, i)
			}
			prevID, prevRef = id, ref
		}
	}
}

func TestUniqueSameMillisecond(t *testing.T) {
	frozen := func() time.Time { return time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC) }
	g := NewGenerator(frozen, nil)
	seen := make(map[string]struct{}, 100000)
	for i := 0; i < 100000; i++ {
		r := g.ProviderRef()
		if _, dup := seen[r]; dup {
			t.Fatalf("duplicate %q at %d", r, i)
		}
		seen[r] = struct{}{}
	}
}

func TestPaymentIDsUnique(t *testing.T) {
	g := NewGenerator(time.Now, nil)
	seen := make(map[string]struct{}, 100000)
	for i := 0; i < 100000; i++ {
		id := g.Payment()
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate %q at %d", id, i)
		}
		seen[id] = struct{}{}
	}
}
