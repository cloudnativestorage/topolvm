//go:build kms_pkcs11

// Package keyprovider PKCS#11 / HSM implementation. Build with -tags=kms_pkcs11.
//
// The KEK is a non-exportable AES-256 key resident in the HSM, identified by
// label. We use AES-GCM (CKM_AES_GCM) with AAD=volumeID to wrap a 32-byte DEK,
// giving the same tamper-evident binding as AWS/GCP. The HSM key never leaves
// the token.
//
// Operation mapping (see design/tde/TDE-Provider-PKCS11.md):
//   GenerateDEK -> crypto/rand DEK, AES-GCM seal under current KEK with
//                  AAD=volumeID; blob = nonce || ciphertext.
//   Unwrap      -> AES-GCM open with AAD=volumeID; mismatch fails the tag.
//   Rewrap      -> Open under blob's KEK label, Seal under current label.
//                  Label-based versioning, see design doc.
//   KEKVersion  -> current KEK label.
package keyprovider

import (
	"context"
	"crypto/cipher"
	"errors"
	"fmt"
	"os"

	"github.com/topolvm/topolvm/internal/crypt"
	"sigs.k8s.io/yaml"
)

// PKCS11ProviderName is the registered provider name.
const PKCS11ProviderName = "pkcs11"

func init() {
	Register(PKCS11ProviderName, func(cfgPath string) (KeyProvider, error) {
		return NewPKCS11ProviderFromConfig(cfgPath)
	})
}

// PKCS11Config is the non-secret config consumed by the provider. The PIN
// lives in a separate file (mounted Secret), read once into an mlock'd buffer
// inside NewPKCS11Provider.
type PKCS11Config struct {
	ModulePath string `json:"modulePath" yaml:"modulePath"`
	TokenLabel string `json:"tokenLabel" yaml:"tokenLabel"`
	Slot       int    `json:"slot,omitempty" yaml:"slot,omitempty"`
	KekLabel   string `json:"kekLabel" yaml:"kekLabel"`
	PinFile    string `json:"pinFile" yaml:"pinFile"`
}

// KEKResolver looks up an AES-GCM cipher backed by a KEK label in the HSM.
// Tests inject an in-memory implementation; the real backend wraps crypto11.
type KEKResolver interface {
	// Resolve returns the AES-GCM AEAD backed by the HSM key labeled
	// kekLabel. It must not expose the KEK material in the returned value.
	Resolve(kekLabel string) (cipher.AEAD, error)
	// Close releases any HSM session pool.
	Close() error
}

// PKCS11Provider implements KeyProvider against a PKCS#11 token.
type PKCS11Provider struct {
	resolver    KEKResolver
	currentKEK  string
	noncePrefix int // for tests; production uses gcm.NonceSize()
}

// NewPKCS11ProviderFromConfig builds the real provider using crypto11. Real
// HSM operations happen behind the kms_pkcs11_real build tag (see
// pkcs11_crypto11.go) so unit tests on machines without CGo / SoftHSM2 still
// compile.
func NewPKCS11ProviderFromConfig(cfgPath string) (*PKCS11Provider, error) {
	if cfgPath == "" {
		return nil, errors.New("keyprovider/pkcs11: --key-provider-config is required")
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("keyprovider/pkcs11: read config %s: %w", cfgPath, err)
	}
	var c PKCS11Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("keyprovider/pkcs11: parse config: %w", err)
	}
	if c.ModulePath == "" || c.TokenLabel == "" || c.KekLabel == "" || c.PinFile == "" {
		return nil, errors.New("keyprovider/pkcs11: modulePath/tokenLabel/kekLabel/pinFile are required")
	}
	resolver, err := newCrypto11Resolver(c)
	if err != nil {
		return nil, err
	}
	return &PKCS11Provider{resolver: resolver, currentKEK: c.KekLabel}, nil
}

// NewPKCS11ProviderWithResolver builds a provider for tests using an injected
// resolver (e.g., a map from label to local AES-GCM).
func NewPKCS11ProviderWithResolver(r KEKResolver, currentKEK string) *PKCS11Provider {
	return &PKCS11Provider{resolver: r, currentKEK: currentKEK}
}

// Name reports the registered provider name.
func (p *PKCS11Provider) Name() string { return PKCS11ProviderName }

// BindsContext reports that AES-GCM AAD=volumeID binds the wrapped blob to
// the volume.
func (p *PKCS11Provider) BindsContext() bool { return true }

// Close releases the resolver (and the HSM session pool when applicable).
func (p *PKCS11Provider) Close() error { return p.resolver.Close() }

// GenerateDEK mints a 32-byte DEK and AES-GCM seals it under the current KEK
// with AAD=volumeID. Blob layout: nonce || ciphertext.
func (p *PKCS11Provider) GenerateDEK(ctx context.Context, o KeyOpts) (crypt.SecretBuf, WrappedKey, error) {
	kek := o.KeyRef
	if kek == "" {
		kek = p.currentKEK
	}
	if kek == "" {
		return nil, WrappedKey{}, errors.New("keyprovider/pkcs11: empty KEK label")
	}
	aead, err := p.resolver.Resolve(kek)
	if err != nil {
		return nil, WrappedKey{}, fmt.Errorf("keyprovider/pkcs11: resolve %s: %w", kek, err)
	}
	dek, err := crypt.RandomSecretBuf(32)
	if err != nil {
		return nil, WrappedKey{}, err
	}
	nonce, err := randomBytes(aead.NonceSize())
	if err != nil {
		dek.Destroy()
		return nil, WrappedKey{}, err
	}
	ct := aead.Seal(nil, nonce, dek.Bytes(), []byte(o.VolumeID))
	blob := append(append([]byte{}, nonce...), ct...)
	return dek, WrappedKey{
		Ciphertext: blob,
		KeyRef:     kek,
		KEKVersion: kek,
		Provider:   PKCS11ProviderName,
	}, nil
}

// Unwrap AES-GCM opens with AAD=volumeID. The tag check enforces binding.
func (p *PKCS11Provider) Unwrap(ctx context.Context, w WrappedKey, volumeID string) (crypt.SecretBuf, error) {
	aead, err := p.resolver.Resolve(w.KeyRef)
	if err != nil {
		return nil, fmt.Errorf("keyprovider/pkcs11: resolve %s: %w", w.KeyRef, err)
	}
	if len(w.Ciphertext) < aead.NonceSize() {
		return nil, errors.New("keyprovider/pkcs11: truncated blob")
	}
	nonce := w.Ciphertext[:aead.NonceSize()]
	ct := w.Ciphertext[aead.NonceSize():]
	pt, err := aead.Open(nil, nonce, ct, []byte(volumeID))
	if err != nil {
		return nil, errors.New("keyprovider/pkcs11: AES-GCM open failed (wrong AAD or tampered blob)")
	}
	return crypt.SecretBufFrom(pt)
}

// Rewrap opens under the blob's original KEK label and re-seals under the
// current label. Old labels can be retired only after no blob references them.
func (p *PKCS11Provider) Rewrap(ctx context.Context, w WrappedKey, volumeID string) (WrappedKey, error) {
	dek, err := p.Unwrap(ctx, w, volumeID)
	if err != nil {
		return WrappedKey{}, err
	}
	defer dek.Destroy()
	aead, err := p.resolver.Resolve(p.currentKEK)
	if err != nil {
		return WrappedKey{}, fmt.Errorf("keyprovider/pkcs11: resolve current %s: %w", p.currentKEK, err)
	}
	nonce, err := randomBytes(aead.NonceSize())
	if err != nil {
		return WrappedKey{}, err
	}
	ct := aead.Seal(nil, nonce, dek.Bytes(), []byte(volumeID))
	blob := append(append([]byte{}, nonce...), ct...)
	return WrappedKey{
		Ciphertext: blob,
		KeyRef:     p.currentKEK,
		KEKVersion: p.currentKEK,
		Provider:   PKCS11ProviderName,
	}, nil
}

// KEKVersion returns the current KEK label so the reconciler detects drift.
func (p *PKCS11Provider) KEKVersion(_ context.Context, _ string) (string, error) {
	return p.currentKEK, nil
}
