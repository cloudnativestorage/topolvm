// Package providertest is the conformance suite that every KeyProvider must
// pass. The fake provider runs the suite as an ordinary unit test; real
// backends (AWS, GCP, Azure, PKCS#11) run the suite behind a build tag.
//
// See design/tde/TDE-Provider-Conformance-Suite.md.
package providertest

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/topolvm/topolvm/internal/keyprovider"
)

// Run executes the full conformance suite against p. keyRef is a
// provider-usable KEK id (Vault transit path, KMS key arn, Key Vault key id,
// PKCS#11 KEK label). Tests that depend on native context binding self-skip
// when the provider does not implement ContextBinder or returns false.
func Run(t *testing.T, p keyprovider.KeyProvider, keyRef string) {
	t.Helper()
	ctx := context.Background()
	const vol = "pvc-conformance-0001"

	t.Run("GenerateThenUnwrap", func(t *testing.T) {
		buf, wrapped, err := p.GenerateDEK(ctx, keyprovider.KeyOpts{VolumeID: vol, KeyRef: keyRef})
		mustNoErr(t, err, "GenerateDEK")
		if buf.Len() < 32 {
			t.Fatalf("DEK length %d, want at least 32", buf.Len())
		}
		plain := snapshot(buf)
		buf.Destroy()
		assertNonEmptyCiphertext(t, wrapped, plain)

		got, err := p.Unwrap(ctx, wrapped, vol)
		mustNoErr(t, err, "Unwrap")
		defer got.Destroy()
		if !bytes.Equal(plain, got.Bytes()) {
			t.Fatal("unwrap did not return the original DEK")
		}
	})

	t.Run("RewrapPreservesPlaintext", func(t *testing.T) {
		buf, w0, err := p.GenerateDEK(ctx, keyprovider.KeyOpts{VolumeID: vol, KeyRef: keyRef})
		mustNoErr(t, err, "GenerateDEK")
		plain := snapshot(buf)
		buf.Destroy()

		w1, err := p.Rewrap(ctx, w0, vol)
		mustNoErr(t, err, "Rewrap")
		// Identical ciphertext is acceptable for backends that rotate
		// transparently (AWS KMS without CMK migration).
		if bytes.Equal(w0.Ciphertext, w1.Ciphertext) {
			t.Logf("rewrap returned identical ciphertext (provider %q); acceptable when backend rotates transparently", p.Name())
		}

		got, err := p.Unwrap(ctx, w1, vol)
		mustNoErr(t, err, "Unwrap after Rewrap")
		defer got.Destroy()
		if !bytes.Equal(plain, got.Bytes()) {
			t.Fatal("rewrapped blob did not unwrap to the original DEK")
		}
	})

	t.Run("ContextMismatchFails", func(t *testing.T) {
		if !keyprovider.BindsContext(p) {
			t.Skipf("provider %q does not bind context natively", p.Name())
		}
		_, wrapped, err := p.GenerateDEK(ctx, keyprovider.KeyOpts{VolumeID: vol, KeyRef: keyRef})
		mustNoErr(t, err, "GenerateDEK")
		if _, err := p.Unwrap(ctx, wrapped, "pvc-DIFFERENT"); err == nil {
			t.Fatal("unwrap with wrong volumeID context must fail")
		}
	})

	t.Run("KEKVersionNonEmpty", func(t *testing.T) {
		v, err := p.KEKVersion(ctx, keyRef)
		mustNoErr(t, err, "KEKVersion")
		if strings.TrimSpace(v) == "" {
			t.Fatal("KEKVersion must be non-empty")
		}
	})

	t.Run("NoPlaintextLeakInWrapped", func(t *testing.T) {
		buf, wrapped, err := p.GenerateDEK(ctx, keyprovider.KeyOpts{VolumeID: vol, KeyRef: keyRef})
		mustNoErr(t, err, "GenerateDEK")
		plain := snapshot(buf)
		buf.Destroy()
		if len(plain) > 0 && bytes.Contains(wrapped.Ciphertext, plain) {
			t.Fatal("wrapped blob contains the plaintext DEK")
		}
	})

	t.Run("ProviderName", func(t *testing.T) {
		if strings.TrimSpace(p.Name()) == "" {
			t.Fatal("Name() must be non-empty")
		}
	})
}

func mustNoErr(t *testing.T, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// snapshot copies a SecretBuf's bytes into a heap slice for later comparison.
// The conformance suite is the only place we materialize plaintext outside a
// SecretBuf, and we never log or return it.
func snapshot(b interface{ Bytes() []byte }) []byte {
	src := b.Bytes()
	out := make([]byte, len(src))
	copy(out, src)
	return out
}

func assertNonEmptyCiphertext(t *testing.T, w keyprovider.WrappedKey, plain []byte) {
	t.Helper()
	if len(w.Ciphertext) == 0 {
		t.Fatal("wrapped ciphertext is empty")
	}
	if bytes.Equal(w.Ciphertext, plain) {
		t.Fatal("wrapped ciphertext equals plaintext")
	}
}
