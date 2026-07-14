// Package crypt provides a thin, testable wrapper around cryptsetup plus
// mlock'd, zeroizing secret buffers. Plaintext key material lives only in
// SecretBuf and is written to cryptsetup over stdin (--key-file=-), never
// argv, env, or named temp files.
package crypt

import (
	"crypto/rand"
	"fmt"
)

// SecretBuf is a memory-locked buffer for secret material. Callers must
// Destroy() it when done. The zero value is unusable; use NewSecretBuf or
// SecretBufFrom to construct.
type SecretBuf interface {
	// Bytes returns the underlying buffer. The slice aliases the locked
	// memory: callers must not retain the slice beyond Destroy().
	Bytes() []byte
	// Len returns the current logical length.
	Len() int
	// Destroy zeroizes the buffer and releases the mlock.
	Destroy()
}

// NewSecretBuf returns an empty buffer of capacity n bytes, mlock'd if the
// platform supports it. Use Append or write to Bytes()[:0:n] to fill.
func NewSecretBuf(n int) (SecretBuf, error) {
	if n <= 0 {
		return nil, fmt.Errorf("crypt: invalid secret buffer size %d", n)
	}
	return newLockedBuf(n)
}

// SecretBufFrom copies src into a new mlock'd buffer and zeroizes src in place.
// Use this when receiving plaintext from a provider so the caller-allocated
// buffer does not leave plaintext lying around.
func SecretBufFrom(src []byte) (SecretBuf, error) {
	b, err := NewSecretBuf(len(src))
	if err != nil {
		return nil, err
	}
	dst := b.Bytes()
	copy(dst[:len(src)], src)
	if lb, ok := b.(*lockedBuf); ok {
		lb.n = len(src)
	}
	// zeroize source
	for i := range src {
		src[i] = 0
	}
	return b, nil
}

// RandomSecretBuf fills a fresh mlock'd buffer with n bytes from crypto/rand.
// Useful for the in-memory fake KeyProvider and for tests.
func RandomSecretBuf(n int) (SecretBuf, error) {
	b, err := NewSecretBuf(n)
	if err != nil {
		return nil, err
	}
	if _, err := rand.Read(b.Bytes()[:n]); err != nil {
		b.Destroy()
		return nil, err
	}
	if lb, ok := b.(*lockedBuf); ok {
		lb.n = n
	}
	return b, nil
}
