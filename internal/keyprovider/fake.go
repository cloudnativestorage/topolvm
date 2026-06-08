package keyprovider

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"

	"github.com/topolvm/topolvm/internal/crypt"
)

// FakeProviderName is the registered name for the in-memory fake provider.
const FakeProviderName = "fake"

func init() {
	Register(FakeProviderName, func(cfgPath string) (KeyProvider, error) {
		return NewFakeProvider(), nil
	})
}

// FakeProvider is a deterministic, in-memory KeyProvider used by unit and
// envtest. It implements envelope wrap with AES-GCM under per-keyRef KEKs
// it generates on demand. The "encryption context" is the volumeID; mismatch
// fails Unwrap with ErrContextMismatch.
type FakeProvider struct {
	mu      sync.Mutex
	keys    map[string]*fakeKEK // keyRef -> versioned KEKs
	dekSize int
}

type fakeKEK struct {
	versions [][]byte // versions[0] is v1; versions[len-1] is current
}

// ErrContextMismatch indicates an Unwrap was attempted against a wrong volumeID.
var ErrContextMismatch = errors.New("keyprovider: encryption-context mismatch")

// NewFakeProvider returns a freshly initialized fake provider. dekSize is the
// generated DEK length in bytes (32 = 256 bit by default, suitable for use as
// a high-entropy LUKS passphrase).
func NewFakeProvider() *FakeProvider {
	return &FakeProvider{
		keys:    map[string]*fakeKEK{},
		dekSize: 32,
	}
}

// Name reports the provider name.
func (f *FakeProvider) Name() string { return FakeProviderName }

// RotateKEK appends a new KEK version for keyRef and returns its version
// string. Used by tests that exercise KEK rotation flows.
func (f *FakeProvider) RotateKEK(keyRef string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := f.ensureKEK(keyRef)
	newKEK := make([]byte, 32)
	if _, err := rand.Read(newKEK); err != nil {
		panic(err)
	}
	k.versions = append(k.versions, newKEK)
	return f.currentVersionLocked(keyRef)
}

// CurrentVersion exposes the current KEK version for tests.
func (f *FakeProvider) CurrentVersion(keyRef string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureKEK(keyRef)
	return f.currentVersionLocked(keyRef)
}

func (f *FakeProvider) ensureKEK(keyRef string) *fakeKEK {
	k, ok := f.keys[keyRef]
	if !ok {
		seed := make([]byte, 32)
		if _, err := rand.Read(seed); err != nil {
			panic(err)
		}
		k = &fakeKEK{versions: [][]byte{seed}}
		f.keys[keyRef] = k
	}
	return k
}

func (f *FakeProvider) currentVersionLocked(keyRef string) string {
	k := f.keys[keyRef]
	return fmt.Sprintf("v%d", len(k.versions))
}

func (f *FakeProvider) versionBytesLocked(keyRef, version string) ([]byte, error) {
	k, ok := f.keys[keyRef]
	if !ok {
		return nil, fmt.Errorf("keyprovider/fake: unknown keyRef %q", keyRef)
	}
	var idx int
	if _, err := fmt.Sscanf(version, "v%d", &idx); err != nil || idx < 1 || idx > len(k.versions) {
		return nil, fmt.Errorf("keyprovider/fake: unknown version %q", version)
	}
	return k.versions[idx-1], nil
}

// GenerateDEK creates a fresh DEK and wraps it under the current KEK version
// for keyRef. The plaintext is returned in a SecretBuf the caller owns.
func (f *FakeProvider) GenerateDEK(ctx context.Context, opts KeyOpts) (crypt.SecretBuf, WrappedKey, error) {
	plain, err := crypt.RandomSecretBuf(f.dekSize)
	if err != nil {
		return nil, WrappedKey{}, err
	}
	wrapped, err := f.wrap(opts.KeyRef, opts.VolumeID, plain.Bytes())
	if err != nil {
		plain.Destroy()
		return nil, WrappedKey{}, err
	}
	return plain, wrapped, nil
}

// Unwrap recovers the plaintext DEK, binding to volumeID. Mismatch returns
// ErrContextMismatch.
func (f *FakeProvider) Unwrap(ctx context.Context, w WrappedKey, volumeID string) (crypt.SecretBuf, error) {
	plain, err := f.unwrap(w, volumeID)
	if err != nil {
		return nil, err
	}
	buf, err := crypt.SecretBufFrom(plain)
	if err != nil {
		return nil, err
	}
	return buf, nil
}

// Rewrap re-encrypts the wrapped DEK under the provider's current KEK version.
func (f *FakeProvider) Rewrap(ctx context.Context, w WrappedKey, volumeID string) (WrappedKey, error) {
	plain, err := f.unwrap(w, volumeID)
	if err != nil {
		return WrappedKey{}, err
	}
	defer zeroize(plain)
	return f.wrap(w.KeyRef, volumeID, plain)
}

// KEKVersion returns the current KEK version for keyRef, creating it on first use.
func (f *FakeProvider) KEKVersion(ctx context.Context, keyRef string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureKEK(keyRef)
	return f.currentVersionLocked(keyRef), nil
}

func (f *FakeProvider) wrap(keyRef, volumeID string, plain []byte) (WrappedKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureKEK(keyRef)
	version := f.currentVersionLocked(keyRef)
	kek := f.keys[keyRef].versions[len(f.keys[keyRef].versions)-1]

	gcm, err := newGCM(kek)
	if err != nil {
		return WrappedKey{}, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return WrappedKey{}, err
	}
	aad := contextAAD(volumeID, version)
	ct := gcm.Seal(nil, nonce, plain, aad)

	// Pack: 1 byte nonce-length, nonce, ciphertext-with-tag.
	out := make([]byte, 0, 1+len(nonce)+len(ct))
	out = append(out, byte(len(nonce)))
	out = append(out, nonce...)
	out = append(out, ct...)

	return WrappedKey{
		Ciphertext: out,
		KeyRef:     keyRef,
		KEKVersion: version,
		Provider:   FakeProviderName,
	}, nil
}

func (f *FakeProvider) unwrap(w WrappedKey, volumeID string) ([]byte, error) {
	if len(w.Ciphertext) < 1 {
		return nil, errors.New("keyprovider/fake: empty ciphertext")
	}
	nonceLen := int(w.Ciphertext[0])
	if 1+nonceLen >= len(w.Ciphertext) {
		return nil, errors.New("keyprovider/fake: truncated ciphertext")
	}
	nonce := w.Ciphertext[1 : 1+nonceLen]
	ct := w.Ciphertext[1+nonceLen:]

	f.mu.Lock()
	kek, err := f.versionBytesLocked(w.KeyRef, w.KEKVersion)
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	gcm, err := newGCM(kek)
	if err != nil {
		return nil, err
	}
	aad := contextAAD(volumeID, w.KEKVersion)
	plain, err := gcm.Open(nil, nonce, ct, aad)
	if err != nil {
		// Most likely an AAD mismatch (wrong volumeID) but the
		// underlying error is opaque; surface a typed sentinel.
		return nil, ErrContextMismatch
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

// contextAAD binds (volumeID, KEKVersion) into the authenticated header so a
// blob cannot be replayed against another volume or under a different KEK.
func contextAAD(volumeID, version string) []byte {
	out := make([]byte, 0, 4+len(volumeID)+4+len(version))
	out = appendLenPrefixed(out, []byte(volumeID))
	out = appendLenPrefixed(out, []byte(version))
	return out
}

func appendLenPrefixed(b, x []byte) []byte {
	var lp [4]byte
	binary.BigEndian.PutUint32(lp[:], uint32(len(x)))
	b = append(b, lp[:]...)
	b = append(b, x...)
	return b
}

func zeroize(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
