package keyprovider_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/topolvm/topolvm/internal/keyprovider"
	"github.com/topolvm/topolvm/internal/keyprovider/providertest"
)

func TestFake_Conformance(t *testing.T) {
	providertest.Run(t, keyprovider.NewFakeProvider(), "transit/keys/topolvm")
}

func TestFake_RoundTrip(t *testing.T) {
	ctx := context.Background()
	p := keyprovider.NewFakeProvider()
	plain, wrapped, err := p.GenerateDEK(ctx, keyprovider.KeyOpts{VolumeID: "vol-1", KeyRef: "transit/keys/topolvm"})
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}
	original := append([]byte(nil), plain.Bytes()...)
	plain.Destroy()

	if wrapped.Provider != keyprovider.FakeProviderName {
		t.Fatalf("provider = %q", wrapped.Provider)
	}
	if wrapped.KeyRef != "transit/keys/topolvm" {
		t.Fatalf("keyRef = %q", wrapped.KeyRef)
	}
	if wrapped.KEKVersion == "" {
		t.Fatal("missing KEKVersion")
	}

	// Plaintext blob must never equal wrapped blob (sanity check the cipher actually wraps).
	if bytes.Equal(original, wrapped.Ciphertext) {
		t.Fatal("wrapped ciphertext equals plaintext")
	}

	unwrapped, err := p.Unwrap(ctx, wrapped, "vol-1")
	if err != nil {
		t.Fatalf("Unwrap: %v", err)
	}
	defer unwrapped.Destroy()
	if !bytes.Equal(original, unwrapped.Bytes()) {
		t.Fatalf("round trip mismatch:\n got %x\nwant %x", unwrapped.Bytes(), original)
	}
}

func TestFake_ContextBindingMismatch(t *testing.T) {
	ctx := context.Background()
	p := keyprovider.NewFakeProvider()
	plain, wrapped, err := p.GenerateDEK(ctx, keyprovider.KeyOpts{VolumeID: "vol-1", KeyRef: "k"})
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}
	plain.Destroy()

	_, err = p.Unwrap(ctx, wrapped, "wrong-volume")
	if !errors.Is(err, keyprovider.ErrContextMismatch) {
		t.Fatalf("expected ErrContextMismatch, got %v", err)
	}
}

func TestFake_Rewrap(t *testing.T) {
	ctx := context.Background()
	p := keyprovider.NewFakeProvider()
	plain, wrapped1, err := p.GenerateDEK(ctx, keyprovider.KeyOpts{VolumeID: "vol-1", KeyRef: "k"})
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}
	original := append([]byte(nil), plain.Bytes()...)
	plain.Destroy()

	// Rotate the KEK; an unwrap of the v1 blob still succeeds because v1 is retained.
	v2 := p.RotateKEK("k")
	if v2 == wrapped1.KEKVersion {
		t.Fatalf("rotation did not bump version: %s", v2)
	}

	// Rewrap to v2, then unwrap and confirm plaintext is preserved.
	wrapped2, err := p.Rewrap(ctx, wrapped1, "vol-1")
	if err != nil {
		t.Fatalf("Rewrap: %v", err)
	}
	if wrapped2.KEKVersion != v2 {
		t.Fatalf("rewrap did not move to current version: got %s, want %s", wrapped2.KEKVersion, v2)
	}
	unwrapped, err := p.Unwrap(ctx, wrapped2, "vol-1")
	if err != nil {
		t.Fatalf("Unwrap after rewrap: %v", err)
	}
	defer unwrapped.Destroy()
	if !bytes.Equal(original, unwrapped.Bytes()) {
		t.Fatal("rewrap altered plaintext")
	}
}

func TestFake_KEKVersionInitializesOnDemand(t *testing.T) {
	ctx := context.Background()
	p := keyprovider.NewFakeProvider()
	v, err := p.KEKVersion(ctx, "k-new")
	if err != nil {
		t.Fatalf("KEKVersion: %v", err)
	}
	if v != "v1" {
		t.Fatalf("first version = %s, want v1", v)
	}
}

func TestRegistry_FakeAvailable(t *testing.T) {
	p, err := keyprovider.New(keyprovider.FakeProviderName, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.Name() != keyprovider.FakeProviderName {
		t.Fatalf("Name() = %q", p.Name())
	}
}
