// Package keyprovider abstracts the external key custody backend that wraps
// per-volume DEKs. The driver only ever sees ciphertext (WrappedKey) or
// plaintext held in a SecretBuf; the KEK never leaves the provider.
package keyprovider

import (
	"context"
	"fmt"

	"github.com/topolvm/topolvm/internal/crypt"
)

// WrappedKey is the ciphertext blob stored in EncryptionKey.status.
type WrappedKey struct {
	// Ciphertext is the provider-wrapped DEK. Safe to store in etcd.
	Ciphertext []byte
	// KeyRef is the provider-specific KEK identifier (Vault transit key,
	// KMS key ARN, PKCS#11 label).
	KeyRef string
	// KEKVersion is the provider key version used at wrap time; drives
	// rewrap drift detection.
	KEKVersion string
	// Provider is the registered provider name.
	Provider string
}

// KeyOpts parameterizes a wrap/unwrap operation. VolumeID becomes the
// provider's encryption context, so a blob cannot be replayed against a
// different volume.
type KeyOpts struct {
	VolumeID string
	KeyRef   string
}

// KeyProvider performs envelope operations against the external key custody
// backend. Implementations must:
//   - never log plaintext key material;
//   - hand plaintext to the caller only via a SecretBuf;
//   - reject Unwrap with the wrong encryption context where the backend
//     supports binding (see ContextBinder).
type KeyProvider interface {
	// Name returns the registered provider name.
	Name() string
	// GenerateDEK returns a fresh per-volume passphrase, wrapped under the
	// provider's current KEK. The plaintext is delivered via a SecretBuf
	// that the caller is responsible for destroying.
	GenerateDEK(ctx context.Context, opts KeyOpts) (crypt.SecretBuf, WrappedKey, error)
	// Unwrap recovers the plaintext passphrase from a wrapped blob. When
	// the provider supports context binding (BindsContext()==true), a
	// volumeID mismatch must return an error.
	Unwrap(ctx context.Context, wrapped WrappedKey, volumeID string) (crypt.SecretBuf, error)
	// Rewrap re-wraps a blob under the provider's current KEK version
	// without exposing plaintext (where the backend supports server-side
	// rewrap) or with brief in-process plaintext held in a SecretBuf.
	Rewrap(ctx context.Context, wrapped WrappedKey, volumeID string) (WrappedKey, error)
	// KEKVersion returns the provider's current KEK version.
	KEKVersion(ctx context.Context, keyRef string) (string, error)
}

// ContextBinder is an optional capability advertised by providers whose
// backend natively binds the wrapped blob to an encryption context (AAD,
// AWS KMS EncryptionContext, Vault Transit context). Providers without
// native binding return false; the conformance suite's context-mismatch
// case self-skips for those providers.
type ContextBinder interface {
	BindsContext() bool
}

// BindsContext returns whether p natively binds the wrapped DEK to the
// volume's encryption context. Defaults to false when p does not implement
// ContextBinder.
func BindsContext(p KeyProvider) bool {
	cb, ok := p.(ContextBinder)
	if !ok {
		return false
	}
	return cb.BindsContext()
}

// Factory builds a KeyProvider from a config path (provider-specific YAML/JSON
// referencing non-secret material only).
type Factory func(cfgPath string) (KeyProvider, error)

var registry = map[string]Factory{}

// Register adds a provider factory under name. Safe to call from init().
func Register(name string, f Factory) {
	registry[name] = f
}

// New constructs a KeyProvider by name. cfgPath is provider-specific.
func New(name, cfgPath string) (KeyProvider, error) {
	f, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("keyprovider: unknown provider %q", name)
	}
	return f(cfgPath)
}

// RegisteredNames returns the providers currently registered. Order is not
// guaranteed; useful for logging at startup.
func RegisteredNames() []string {
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	return out
}
