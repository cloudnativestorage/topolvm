# TDE KeyProvider Conformance Suite (shared)

**Companion to:** `TopoLVM-TDE-Implementation-Spec.md` (PR 3 defines the round-trip suite; this formalizes it as a reusable package).
**Purpose:** one acceptance gate that every provider (Vault, AWS KMS, GCP KMS, Azure Key Vault, PKCS#11) must pass. Each provider spec references this file as its definition of done.

---

## 1. Package: `internal/keyprovider/providertest`

A table-driven suite that takes any `keyprovider.KeyProvider` and exercises the full contract. Real backends run behind build tags; the `fake` provider runs in normal unit tests.

```go
package providertest

import (
	"context"
	"testing"
	"bytes"
	"github.com/cloudnativestorage/topolvm/internal/keyprovider"
)

// Run executes the full conformance suite against p.
// keyRef is a provider-usable KEK id; backends that bind context use volumeID.
func Run(t *testing.T, p keyprovider.KeyProvider, keyRef string) {
	ctx := context.Background()
	const vol = "pvc-conformance-0001"

	t.Run("GenerateThenUnwrap", func(t *testing.T) {
		buf, wrapped, err := p.GenerateDEK(ctx, keyprovider.KeyOpts{VolumeID: vol, KeyRef: keyRef})
		must(t, err)
		plain := append([]byte(nil), buf.Bytes()...) // copy before Destroy
		buf.Destroy()

		got, err := p.Unwrap(ctx, wrapped, vol)
		must(t, err)
		defer got.Destroy()
		if !bytes.Equal(plain, got.Bytes()) {
			t.Fatal("unwrap did not return the original DEK")
		}
		if len(wrapped.Ciphertext) == 0 || string(wrapped.Ciphertext) == string(plain) {
			t.Fatal("wrapped blob is empty or equals plaintext")
		}
	})

	t.Run("RewrapPreservesPlaintext", func(t *testing.T) {
		buf, w0, err := p.GenerateDEK(ctx, keyprovider.KeyOpts{VolumeID: vol, KeyRef: keyRef})
		must(t, err)
		plain := append([]byte(nil), buf.Bytes()...); buf.Destroy()

		w1, err := p.Rewrap(ctx, w0)
		must(t, err)
		if bytes.Equal(w0.Ciphertext, w1.Ciphertext) {
			t.Log("warning: rewrap returned identical ciphertext (acceptable only if backend rotates transparently)")
		}
		got, err := p.Unwrap(ctx, w1, vol); must(t, err); defer got.Destroy()
		if !bytes.Equal(plain, got.Bytes()) {
			t.Fatal("rewrapped blob did not unwrap to the original DEK")
		}
	})

	t.Run("ContextMismatchFails", func(t *testing.T) {
		// Providers that support AAD/encryption-context binding MUST fail here.
		// Providers without native binding (Azure) skip via Capabilities().
		if !p.(keyprovider.ContextBinder).BindsContext() {
			t.Skip("provider does not bind context natively")
		}
		_, wrapped, err := p.GenerateDEK(ctx, keyprovider.KeyOpts{VolumeID: vol, KeyRef: keyRef}); must(t, err)
		if _, err := p.Unwrap(ctx, wrapped, "pvc-DIFFERENT"); err == nil {
			t.Fatal("unwrap with wrong volumeID context must fail")
		}
	})

	t.Run("KEKVersionNonEmpty", func(t *testing.T) {
		v, err := p.KEKVersion(ctx, keyRef); must(t, err)
		if v == "" { t.Fatal("KEKVersion must be non-empty") }
	})

	t.Run("NoPlaintextLeakInWrapped", func(t *testing.T) {
		buf, wrapped, err := p.GenerateDEK(ctx, keyprovider.KeyOpts{VolumeID: vol, KeyRef: keyRef}); must(t, err)
		plain := append([]byte(nil), buf.Bytes()...); buf.Destroy()
		if bytes.Contains(wrapped.Ciphertext, plain) {
			t.Fatal("wrapped blob contains the plaintext DEK")
		}
	})
}
```

Add a small optional capability interface so the context test can self-skip:

```go
// in internal/keyprovider/provider.go
type ContextBinder interface{ BindsContext() bool }
```

## 2. Mandatory contract (all providers)

1. `GenerateDEK` returns a high-entropy DEK (at least 32 bytes) in an mlock'd `SecretBuf`, plus a `WrappedKey` whose `Ciphertext` is not the plaintext.
2. `Unwrap(GenerateDEK(...))` returns the exact same bytes.
3. `Rewrap` output still unwraps to the same plaintext (whether it changes the ciphertext or relies on transparent backend rotation).
4. `KEKVersion` returns a stable, comparable, non-empty string.
5. The DEK plaintext appears only inside `SecretBuf`. Never in `WrappedKey`, logs, errors, or argv.
6. Providers that support binding fail `Unwrap` when the volumeID context differs from generation.

## 3. How each provider wires its tests

```go
//go:build kms_aws
func TestAWSKMSConformance(t *testing.T) {
	p := newAWSProviderFromEnv(t)             // reads KMS_KEY_ARN, region from env
	providertest.Run(t, p, os.Getenv("KMS_KEY_ARN"))
}
```

CI matrix runs each tagged suite against a real or emulated backend (LocalStack for AWS, the GCP KMS emulator or a test project, Azurite/Key Vault test instance, SoftHSM for PKCS#11). The `fake` provider runs untagged on every PR.

## 4. Acceptance

A provider is accepted when `providertest.Run` passes against its real backend in CI and the secret-leak grep from the main spec finds nothing in the provider package.
