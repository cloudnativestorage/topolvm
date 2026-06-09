//go:build kms_gcp

package keyprovider_test

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"github.com/topolvm/topolvm/internal/keyprovider"
	"github.com/topolvm/topolvm/internal/keyprovider/providertest"
)

const testGCPKey = "projects/test/locations/us/keyRings/r/cryptoKeys/k"

// fakeGCPKMS implements the narrow gcpKMSAPI surface using local AES-GCM.
type fakeGCPKMS struct {
	mu      sync.Mutex
	primary string // current primary version resource name
	kek     []byte
}

func newFakeGCPKMS(t *testing.T) *fakeGCPKMS {
	t.Helper()
	kek := sha256.Sum256([]byte("gcpkms-kek-v1"))
	return &fakeGCPKMS{primary: testGCPKey + "/cryptoKeyVersions/1", kek: kek[:]}
}

func (f *fakeGCPKMS) gcm() cipher.AEAD {
	b, _ := aes.NewCipher(f.kek)
	g, _ := cipher.NewGCM(b)
	return g
}

func (f *fakeGCPKMS) Encrypt(_ context.Context, req *kmspb.EncryptRequest) (*kmspb.EncryptResponse, error) {
	gcm := f.gcm()
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	ct := gcm.Seal(nil, nonce, req.Plaintext, req.AdditionalAuthenticatedData)
	blob := append(nonce, ct...)
	return &kmspb.EncryptResponse{Ciphertext: blob, Name: f.primary}, nil
}

func (f *fakeGCPKMS) Decrypt(_ context.Context, req *kmspb.DecryptRequest) (*kmspb.DecryptResponse, error) {
	gcm := f.gcm()
	if len(req.Ciphertext) < gcm.NonceSize() {
		return nil, errors.New("truncated blob")
	}
	nonce := req.Ciphertext[:gcm.NonceSize()]
	ct := req.Ciphertext[gcm.NonceSize():]
	pt, err := gcm.Open(nil, nonce, ct, req.AdditionalAuthenticatedData)
	if err != nil {
		return nil, errors.New("fakegcp: decrypt failed (aad mismatch)")
	}
	return &kmspb.DecryptResponse{Plaintext: pt}, nil
}

func (f *fakeGCPKMS) GetCryptoKey(_ context.Context, req *kmspb.GetCryptoKeyRequest) (*kmspb.CryptoKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &kmspb.CryptoKey{
		Name:    req.Name,
		Primary: &kmspb.CryptoKeyVersion{Name: f.primary},
	}, nil
}

func TestGCPKMS_Conformance_MetadataOnly(t *testing.T) {
	fk := newFakeGCPKMS(t)
	p := keyprovider.NewGCPKMSProviderWithClient(fk, testGCPKey, "")
	providertest.Run(t, p, testGCPKey)
}

func TestGCPKMS_Conformance_Reencrypt(t *testing.T) {
	fk := newFakeGCPKMS(t)
	p := keyprovider.NewGCPKMSProviderWithClient(fk, testGCPKey, "reencrypt")
	providertest.Run(t, p, testGCPKey)
}

func TestGCPKMS_AADMismatchFails(t *testing.T) {
	fk := newFakeGCPKMS(t)
	p := keyprovider.NewGCPKMSProviderWithClient(fk, testGCPKey, "")
	ctx := context.Background()
	plain, w, err := p.GenerateDEK(ctx, keyprovider.KeyOpts{VolumeID: "vol-a", KeyRef: testGCPKey})
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}
	original := append([]byte(nil), plain.Bytes()...)
	plain.Destroy()
	if len(original) != 32 {
		t.Fatalf("DEK length: %d", len(original))
	}
	if bytes.Equal(w.Ciphertext, original) {
		t.Fatal("ciphertext equals plaintext")
	}
	if _, err := p.Unwrap(ctx, w, "vol-b"); err == nil {
		t.Fatal("expected AAD mismatch error")
	}
}

func TestGCPKMS_MetadataOnlyDoesNotExposePlaintext(t *testing.T) {
	// Wrap a tracker that fails if Decrypt is called during Rewrap.
	fk := newFakeGCPKMS(t)
	tracker := &noDecryptDuringRewrap{inner: fk}
	p := keyprovider.NewGCPKMSProviderWithClient(tracker, testGCPKey, "metadata-only")
	ctx := context.Background()
	plain, w, err := p.GenerateDEK(ctx, keyprovider.KeyOpts{VolumeID: "vol", KeyRef: testGCPKey})
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}
	plain.Destroy()
	tracker.armRewrap()
	w2, err := p.Rewrap(ctx, w, "vol")
	if err != nil {
		t.Fatalf("Rewrap: %v", err)
	}
	if !bytes.Equal(w.Ciphertext, w2.Ciphertext) {
		t.Fatal("metadata-only rewrap must preserve ciphertext")
	}
}

// noDecryptDuringRewrap fails the test if Decrypt or Encrypt is invoked
// after armRewrap() is called.
type noDecryptDuringRewrap struct {
	inner *fakeGCPKMS
	armed bool
}

func (n *noDecryptDuringRewrap) armRewrap() { n.armed = true }
func (n *noDecryptDuringRewrap) Encrypt(ctx context.Context, req *kmspb.EncryptRequest) (*kmspb.EncryptResponse, error) {
	if n.armed {
		return nil, errors.New("Encrypt called during metadata-only rewrap")
	}
	return n.inner.Encrypt(ctx, req)
}
func (n *noDecryptDuringRewrap) Decrypt(ctx context.Context, req *kmspb.DecryptRequest) (*kmspb.DecryptResponse, error) {
	if n.armed {
		return nil, errors.New("Decrypt called during metadata-only rewrap")
	}
	return n.inner.Decrypt(ctx, req)
}
func (n *noDecryptDuringRewrap) GetCryptoKey(ctx context.Context, req *kmspb.GetCryptoKeyRequest) (*kmspb.CryptoKey, error) {
	return n.inner.GetCryptoKey(ctx, req)
}
