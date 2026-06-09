//go:build kms_pkcs11

package keyprovider_test

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"

	"github.com/topolvm/topolvm/internal/keyprovider"
	"github.com/topolvm/topolvm/internal/keyprovider/providertest"
)

// fakeKEKResolver maps KEK labels to local AES-GCM AEADs derived from the
// label. This simulates the HSM keystore for the conformance suite without
// requiring CGo or a real PKCS#11 module.
type fakeKEKResolver struct {
	mu     sync.Mutex
	labels map[string][]byte // label -> KEK bytes
}

func newFakeKEKResolver(labels ...string) *fakeKEKResolver {
	r := &fakeKEKResolver{labels: map[string][]byte{}}
	for _, l := range labels {
		k := sha256.Sum256([]byte("pkcs11-kek-" + l))
		r.labels[l] = k[:]
	}
	return r
}

func (r *fakeKEKResolver) Resolve(label string) (cipher.AEAD, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k, ok := r.labels[label]
	if !ok {
		return nil, errors.New("fakeresolver: unknown label " + label)
	}
	b, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

func (*fakeKEKResolver) Close() error { return nil }

func TestPKCS11_Conformance(t *testing.T) {
	r := newFakeKEKResolver("topolvm-kek-v1")
	p := keyprovider.NewPKCS11ProviderWithResolver(r, "topolvm-kek-v1")
	providertest.Run(t, p, "topolvm-kek-v1")
}

func TestPKCS11_AADMismatchFails(t *testing.T) {
	r := newFakeKEKResolver("topolvm-kek-v1")
	p := keyprovider.NewPKCS11ProviderWithResolver(r, "topolvm-kek-v1")
	ctx := context.Background()
	plain, w, err := p.GenerateDEK(ctx, keyprovider.KeyOpts{VolumeID: "vol-a", KeyRef: "topolvm-kek-v1"})
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}
	plain.Destroy()
	if _, err := p.Unwrap(ctx, w, "vol-b"); err == nil {
		t.Fatal("expected AAD mismatch error")
	}
}

func TestPKCS11_LabelBasedRewrap(t *testing.T) {
	// Two labels v1 and v2 represent KEK rotation; rewrap migrates a v1
	// blob to v2 while preserving plaintext.
	r := newFakeKEKResolver("topolvm-kek-v1", "topolvm-kek-v2")
	p := keyprovider.NewPKCS11ProviderWithResolver(r, "topolvm-kek-v1")
	ctx := context.Background()
	plain, w1, err := p.GenerateDEK(ctx, keyprovider.KeyOpts{VolumeID: "vol", KeyRef: "topolvm-kek-v1"})
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}
	original := append([]byte(nil), plain.Bytes()...)
	plain.Destroy()

	// Pretend the operator updated config to make v2 the current label.
	p2 := keyprovider.NewPKCS11ProviderWithResolver(r, "topolvm-kek-v2")
	w2, err := p2.Rewrap(ctx, w1, "vol")
	if err != nil {
		t.Fatalf("Rewrap v1->v2: %v", err)
	}
	if w2.KeyRef != "topolvm-kek-v2" || w2.KEKVersion != "topolvm-kek-v2" {
		t.Fatalf("rewrap did not migrate to v2: %+v", w2)
	}
	got, err := p2.Unwrap(ctx, w2, "vol")
	if err != nil {
		t.Fatalf("Unwrap v2: %v", err)
	}
	defer got.Destroy()
	if !bytes.Equal(original, got.Bytes()) {
		t.Fatal("rewrap altered plaintext")
	}
	// The original v1 blob also still unwraps via p (current=v1); proves
	// old labels remain usable until retired.
	got1, err := p.Unwrap(ctx, w1, "vol")
	if err != nil {
		t.Fatalf("v1 still must unwrap: %v", err)
	}
	got1.Destroy()
}
