// Package crypto provides envelope encryption for sensitive fields such as
// TOTP secrets, PAN and bank details (LLD 15, TRS NFR-SEC-05). Each value gets
// its own random data key, which is itself encrypted with the master key, so
// the master key can later be replaced by a managed key without re-design.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

const version byte = 1

// Box encrypts and decrypts values with AES-256-GCM.
type Box struct{ master cipher.AEAD }

// NewBox creates a box from a base64 32-byte master key.
func NewBox(masterKeyB64 string) (*Box, error) {
	key, err := base64.StdEncoding.DecodeString(masterKeyB64)
	if err != nil || len(key) != 32 {
		return nil, errors.New("crypto: master key must be 32 bytes, base64-encoded")
	}
	aead, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	return &Box{master: aead}, nil
}

// Seal encrypts plaintext. aad binds the ciphertext to its context (for
// example the account ID), so it cannot be copied to another record.
//
// Layout: version | nonce(master) | sealed data key | nonce(data) | sealed plaintext
func (b *Box) Seal(plaintext, aad []byte) ([]byte, error) {
	dataKey := make([]byte, 32)
	if _, err := rand.Read(dataKey); err != nil {
		return nil, err
	}
	dataAEAD, err := newGCM(dataKey)
	if err != nil {
		return nil, err
	}
	n1 := nonce(b.master)
	sealedKey := b.master.Seal(nil, n1, dataKey, aad)
	n2 := nonce(dataAEAD)
	sealed := dataAEAD.Seal(nil, n2, plaintext, aad)

	out := make([]byte, 0, 1+len(n1)+len(sealedKey)+len(n2)+len(sealed))
	out = append(out, version)
	out = append(out, n1...)
	out = append(out, sealedKey...)
	out = append(out, n2...)
	return append(out, sealed...), nil
}

// Open decrypts a value produced by Seal with the same aad.
func (b *Box) Open(ciphertext, aad []byte) ([]byte, error) {
	ns := b.master.NonceSize()
	keyLen := 32 + b.master.Overhead()
	if len(ciphertext) < 1+ns+keyLen+ns || ciphertext[0] != version {
		return nil, errors.New("crypto: malformed ciphertext")
	}
	p := ciphertext[1:]
	n1, p := p[:ns], p[ns:]
	sealedKey, p := p[:keyLen], p[keyLen:]
	dataKey, err := b.master.Open(nil, n1, sealedKey, aad)
	if err != nil {
		return nil, fmt.Errorf("crypto: open data key: %w", err)
	}
	dataAEAD, err := newGCM(dataKey)
	if err != nil {
		return nil, err
	}
	n2, sealed := p[:ns], p[ns:]
	plain, err := dataAEAD.Open(nil, n2, sealed, aad)
	if err != nil {
		return nil, fmt.Errorf("crypto: open value: %w", err)
	}
	return plain, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func nonce(a cipher.AEAD) []byte {
	n := make([]byte, a.NonceSize())
	_, _ = rand.Read(n)
	return n
}
