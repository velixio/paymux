// Package ids generates public prefixed IDs (UUIDv7, Crockford base32) and
// short provider references.
package ids

import (
	"crypto/rand"
	"fmt"
	"io"
	"regexp"
	"sync"
	"time"
)

const (
	lowerAlphabet = "0123456789abcdefghjkmnpqrstvwxyz"
	upperAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
)

var prefixRE = regexp.MustCompile(`^[a-z]{2,5}$`)

// Generator produces IDs from an injected clock and entropy source.
// It is safe for concurrent use.
type Generator struct {
	clock func() time.Time
	mu    sync.Mutex
	rand  io.Reader
}

// NewGenerator returns a Generator. A nil clock means time.Now and a nil
// rand means crypto/rand.
func NewGenerator(clock func() time.Time, r io.Reader) *Generator {
	if clock == nil {
		clock = time.Now
	}
	if r == nil {
		r = rand.Reader
	}
	return &Generator{clock: clock, rand: r}
}

var defaultGen = NewGenerator(time.Now, rand.Reader)

// New returns a public ID using the default generator.
func New(prefix string) string { return defaultGen.New(prefix) }

// ProviderRef returns a 20-char reference using the default generator.
func ProviderRef() string { return defaultGen.ProviderRef() }

func (g *Generator) read(b []byte) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, err := io.ReadFull(g.rand, b); err != nil {
		panic(fmt.Sprintf("ids: entropy source failed: %v", err))
	}
}

// New returns "<prefix>_<26 lowercase Crockford base32 chars of a UUIDv7>".
// It panics if prefix does not match ^[a-z]{2,5}$.
func (g *Generator) New(prefix string) string {
	if !prefixRE.MatchString(prefix) {
		panic(fmt.Sprintf("ids: invalid prefix %q", prefix))
	}
	var u [16]byte
	g.read(u[6:]) // 74 random bits remain after version/variant are set
	ms := uint64(g.clock().UnixMilli())
	u[0], u[1], u[2] = byte(ms>>40), byte(ms>>32), byte(ms>>24)
	u[3], u[4], u[5] = byte(ms>>16), byte(ms>>8), byte(ms)
	u[6] = 0x70 | u[6]&0x0f // version 7
	u[8] = 0x80 | u[8]&0x3f // variant 10 (RFC 9562)

	// 128 bits -> 26 groups of 5 bits (2 leading zero bits of padding).
	out := make([]byte, 0, len(prefix)+1+26)
	out = append(out, prefix...)
	out = append(out, '_')
	var acc uint16
	nbits := 2 // pad: two zero bits ahead of the first byte
	for _, b := range u {
		acc = acc<<8 | uint16(b)
		nbits += 8
		for nbits >= 5 {
			out = append(out, lowerAlphabet[(acc>>(nbits-5))&31])
			nbits -= 5
		}
		acc &= 1<<nbits - 1
	}
	return string(out)
}

// ProviderRef returns 20 uppercase Crockford chars: a 48-bit millisecond
// timestamp (10 chars) followed by 50 random bits (10 chars).
func (g *Generator) ProviderRef() string {
	var rnd [7]byte
	g.read(rnd[:])
	ms := uint64(g.clock().UnixMilli()) & (1<<48 - 1)
	rv := (uint64(rnd[0])<<48 | uint64(rnd[1])<<40 | uint64(rnd[2])<<32 |
		uint64(rnd[3])<<24 | uint64(rnd[4])<<16 | uint64(rnd[5])<<8 | uint64(rnd[6])) & (1<<50 - 1)
	var out [20]byte
	for i := 9; i >= 0; i-- {
		out[i] = upperAlphabet[ms&31]
		ms >>= 5
		out[10+i] = upperAlphabet[rv&31]
		rv >>= 5
	}
	return string(out[:])
}
