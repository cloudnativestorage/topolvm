//go:build kms_pkcs11 && kms_pkcs11_real

// Real PKCS#11 backend using ThalesGroup/crypto11.
//
// This file is gated by an additional build tag so that local tests and CI
// jobs without CGo / SoftHSM2 can still verify the provider against the
// fake resolver. To enable, build with -tags "kms_pkcs11 kms_pkcs11_real"
// and ensure cgo + libsofthsm2 (or vendor module) are on the build host.

package keyprovider

import (
	"crypto/cipher"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/ThalesGroup/crypto11"
	"github.com/topolvm/topolvm/internal/crypt"
)

// crypto11Resolver wraps a crypto11.Context and resolves KEK labels to an
// HSM-backed AES-GCM AEAD.
type crypto11Resolver struct {
	mu  sync.Mutex
	ctx *crypto11.Context
}

func newCrypto11Resolver(c PKCS11Config) (KEKResolver, error) {
	pin, err := readPINMlock(c.PinFile)
	if err != nil {
		return nil, err
	}
	defer pin.Destroy()
	cfg := &crypto11.Config{
		Path:       c.ModulePath,
		TokenLabel: c.TokenLabel,
		Pin:        string(pin.Bytes()),
	}
	if c.Slot != 0 {
		s := c.Slot
		cfg.SlotNumber = &s
	}
	ctx, err := crypto11.Configure(cfg)
	if err != nil {
		return nil, fmt.Errorf("keyprovider/pkcs11: crypto11.Configure: %w", err)
	}
	return &crypto11Resolver{ctx: ctx}, nil
}

func (r *crypto11Resolver) Resolve(label string) (cipher.AEAD, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key, err := r.ctx.FindKey(nil, []byte(label))
	if err != nil {
		return nil, fmt.Errorf("keyprovider/pkcs11: find key %q: %w", label, err)
	}
	if key == nil {
		return nil, fmt.Errorf("keyprovider/pkcs11: KEK label %q not found", label)
	}
	aead, err := key.NewGCM()
	if err != nil {
		return nil, fmt.Errorf("keyprovider/pkcs11: NewGCM: %w", err)
	}
	return aead, nil
}

func (r *crypto11Resolver) Close() error {
	if r.ctx == nil {
		return nil
	}
	err := r.ctx.Close()
	r.ctx = nil
	return err
}

// readPINMlock reads the PIN file into an mlock'd SecretBuf. World/group
// readable PIN files are rejected.
func readPINMlock(path string) (crypt.SecretBuf, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("keyprovider/pkcs11: stat pin file: %w", err)
	}
	if st.Mode()&0o077 != 0 {
		return nil, fmt.Errorf("keyprovider/pkcs11: pin file %s is world / group readable (mode %v); chmod 0400", path, st.Mode())
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("keyprovider/pkcs11: read pin: %w", err)
	}
	body = []byte(strings.TrimRight(string(body), "\r\n"))
	if len(body) == 0 {
		return nil, fmt.Errorf("keyprovider/pkcs11: pin file %s is empty", path)
	}
	return crypt.SecretBufFrom(body)
}
