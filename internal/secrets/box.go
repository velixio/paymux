// Package secrets provides AES-256-GCM sealing with key ids for rotation.
//
// Ciphertext format: 0x01 | len(keyID) | keyID | 12-byte nonce | GCM ciphertext+tag.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

const (
	formatV1 = 0x01
	keyLen   = 32
)

var (
	ErrUnknownKey = errors.New("secrets: unknown key id")
	ErrDecrypt    = errors.New("secrets: decryption failed")
)

// Box seals with the active key and opens with the active or any older key.
type Box struct {
	activeID string
	aeads    map[string]cipher.AEAD
}

// KeyID returns the first 8 hex chars of sha256(key).
func KeyID(key []byte) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:4])
}

// NewBox builds a Box. Every key must be exactly 32 bytes.
func NewBox(active []byte, older ...[]byte) (*Box, error) {
	b := &Box{activeID: KeyID(active), aeads: map[string]cipher.AEAD{}}
	for i, k := range append([][]byte{active}, older...) {
		if len(k) != keyLen {
			if i == 0 {
				return nil, fmt.Errorf("secrets: active key must be %d bytes, got %d", keyLen, len(k))
			}
			return nil, fmt.Errorf("secrets: older key %d must be %d bytes, got %d", i, keyLen, len(k))
		}
		blk, err := aes.NewCipher(k)
		if err != nil {
			return nil, err
		}
		g, err := cipher.NewGCM(blk)
		if err != nil {
			return nil, err
		}
		b.aeads[KeyID(k)] = g
	}
	return b, nil
}

// Seal encrypts plaintext under the active key, binding aad.
func (b *Box) Seal(plaintext, aad []byte) ([]byte, error) {
	g := b.aeads[b.activeID]
	out := make([]byte, 0, 2+len(b.activeID)+g.NonceSize()+len(plaintext)+g.Overhead())
	out = append(out, formatV1, byte(len(b.activeID)))
	out = append(out, b.activeID...)
	nonce := make([]byte, g.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out = append(out, nonce...)
	return g.Seal(out, nonce, plaintext, aad), nil
}

// Open decrypts a Seal output, checking aad.
func (b *Box) Open(ciphertext, aad []byte) ([]byte, error) {
	if len(ciphertext) < 2 || ciphertext[0] != formatV1 {
		return nil, fmt.Errorf("%w: bad format", ErrDecrypt)
	}
	idLen := int(ciphertext[1])
	rest := ciphertext[2:]
	if len(rest) < idLen {
		return nil, fmt.Errorf("%w: truncated", ErrDecrypt)
	}
	g, ok := b.aeads[string(rest[:idLen])]
	if !ok {
		return nil, ErrUnknownKey
	}
	rest = rest[idLen:]
	if len(rest) < g.NonceSize()+g.Overhead() {
		return nil, fmt.Errorf("%w: truncated", ErrDecrypt)
	}
	pt, err := g.Open(nil, rest[:g.NonceSize()], rest[g.NonceSize():], aad)
	if err != nil {
		return nil, ErrDecrypt
	}
	return pt, nil
}
