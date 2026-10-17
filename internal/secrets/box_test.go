package secrets

import (
	"bytes"
	"errors"
	"testing"
)

func key(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func newBox(t *testing.T, active []byte, older ...[]byte) *Box {
	t.Helper()
	b, err := NewBox(active, older...)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRoundTrip(t *testing.T) {
	b := newBox(t, key(1))
	ct, err := b.Seal([]byte("hunter2"), []byte("providers.config.row1"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ct, []byte("hunter2")) {
		t.Fatal("plaintext leaked into ciphertext")
	}
	pt, err := b.Open(ct, []byte("providers.config.row1"))
	if err != nil || string(pt) != "hunter2" {
		t.Fatalf("got %q,%v", pt, err)
	}
	ct, _ = b.Seal(nil, nil)
	if pt, err := b.Open(ct, nil); err != nil || len(pt) != 0 {
		t.Fatalf("empty: %q,%v", pt, err)
	}
}

func TestFormat(t *testing.T) {
	k := key(1)
	ct, _ := newBox(t, k).Seal([]byte("x"), nil)
	id := KeyID(k)
	if len(id) != 8 {
		t.Fatalf("key id %q", id)
	}
	if ct[0] != 0x01 || int(ct[1]) != 8 || string(ct[2:10]) != id {
		t.Fatalf("bad header % x", ct[:10])
	}
	if len(ct) != 2+8+12+1+16 {
		t.Fatalf("len %d", len(ct))
	}
}

func TestTamperedCiphertextFails(t *testing.T) {
	b := newBox(t, key(1))
	ct, _ := b.Seal([]byte("secret"), []byte("a"))
	ct[len(ct)-1] ^= 1
	if _, err := b.Open(ct, []byte("a")); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("got %v", err)
	}
}

func TestWrongAADFails(t *testing.T) {
	b := newBox(t, key(1))
	ct, _ := b.Seal([]byte("secret"), []byte("a"))
	if _, err := b.Open(ct, []byte("b")); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("got %v", err)
	}
}

func TestUnknownKeyID(t *testing.T) {
	ct, _ := newBox(t, key(1)).Seal([]byte("secret"), nil)
	if _, err := newBox(t, key(2)).Open(ct, nil); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("got %v", err)
	}
}

func TestMalformedInput(t *testing.T) {
	b := newBox(t, key(1))
	for _, in := range [][]byte{nil, {0x01}, {0x02, 0, 0}, {0x01, 200, 1}} {
		if _, err := b.Open(in, nil); err == nil {
			t.Errorf("% x: expected error", in)
		}
	}
}

func TestRotationOpensOld(t *testing.T) {
	a, bk := key(1), key(2)
	old, _ := newBox(t, a).Seal([]byte("v"), nil)
	rot := newBox(t, bk, a)
	if pt, err := rot.Open(old, nil); err != nil || string(pt) != "v" {
		t.Fatalf("got %q,%v", pt, err)
	}
	fresh, _ := rot.Seal([]byte("v"), nil)
	if string(fresh[2:10]) != KeyID(bk) {
		t.Fatalf("new seal not under active key")
	}
}

func TestNonceUnique(t *testing.T) {
	b := newBox(t, key(1))
	seen := map[string]bool{}
	for i := 0; i < 10000; i++ {
		ct, _ := b.Seal([]byte("x"), nil)
		n := string(ct[10:22])
		if seen[n] {
			t.Fatal("nonce reused")
		}
		seen[n] = true
	}
}

func TestKeyLength(t *testing.T) {
	if _, err := NewBox(make([]byte, 31)); err == nil {
		t.Fatal("31-byte active key accepted")
	}
	if _, err := NewBox(key(1), make([]byte, 16)); err == nil {
		t.Fatal("16-byte older key accepted")
	}
}
