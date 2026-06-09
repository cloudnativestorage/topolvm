//go:build kms_azure

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

	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azkeys"
	"github.com/topolvm/topolvm/internal/keyprovider"
	"github.com/topolvm/topolvm/internal/keyprovider/providertest"
)

const testAzureKeyID = "https://vault.vault.azure.net/keys/topolvm/abc123"

// fakeAzureKV is an in-memory Key Vault. It uses AES-GCM under a fixed KEK
// to simulate wrap/unwrap. Note that Key Vault's WrapKey takes plaintext;
// our fake does not enforce AAD because real Key Vault doesn't either.
type fakeAzureKV struct {
	mu  sync.Mutex
	kek []byte
}

func newFakeAzureKV(t *testing.T) *fakeAzureKV {
	t.Helper()
	kek := sha256.Sum256([]byte("azurekv-kek"))
	return &fakeAzureKV{kek: kek[:]}
}

func (f *fakeAzureKV) gcm() cipher.AEAD {
	b, _ := aes.NewCipher(f.kek)
	g, _ := cipher.NewGCM(b)
	return g
}

func (f *fakeAzureKV) WrapKey(_ context.Context, _ string, _ string, params azkeys.KeyOperationParameters, _ *azkeys.WrapKeyOptions) (azkeys.WrapKeyResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	gcm := f.gcm()
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return azkeys.WrapKeyResponse{}, err
	}
	ct := gcm.Seal(nil, nonce, params.Value, nil)
	blob := append(nonce, ct...)
	return azkeys.WrapKeyResponse{
		KeyOperationResult: azkeys.KeyOperationResult{Result: blob},
	}, nil
}

func (f *fakeAzureKV) UnwrapKey(_ context.Context, _ string, _ string, params azkeys.KeyOperationParameters, _ *azkeys.UnwrapKeyOptions) (azkeys.UnwrapKeyResponse, error) {
	gcm := f.gcm()
	if len(params.Value) < gcm.NonceSize() {
		return azkeys.UnwrapKeyResponse{}, errors.New("truncated")
	}
	nonce := params.Value[:gcm.NonceSize()]
	ct := params.Value[gcm.NonceSize():]
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return azkeys.UnwrapKeyResponse{}, err
	}
	return azkeys.UnwrapKeyResponse{
		KeyOperationResult: azkeys.KeyOperationResult{Result: pt},
	}, nil
}

func (f *fakeAzureKV) GetKey(_ context.Context, _ string, _ string, _ *azkeys.GetKeyOptions) (azkeys.GetKeyResponse, error) {
	kid := azkeys.ID(testAzureKeyID)
	return azkeys.GetKeyResponse{
		KeyBundle: azkeys.KeyBundle{Key: &azkeys.JSONWebKey{KID: &kid}},
	}, nil
}

func TestAzureKV_Conformance_BindVolume(t *testing.T) {
	fk := newFakeAzureKV(t)
	p, err := keyprovider.NewAzureKVProviderWithAPI(fk, testAzureKeyID, "RSA-OAEP-256", true)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	providertest.Run(t, p, testAzureKeyID)
}

func TestAzureKV_Conformance_NoBind(t *testing.T) {
	fk := newFakeAzureKV(t)
	p, err := keyprovider.NewAzureKVProviderWithAPI(fk, testAzureKeyID, "RSA-OAEP-256", false)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	providertest.Run(t, p, testAzureKeyID)
}

func TestAzureKV_HMACBindingFails(t *testing.T) {
	fk := newFakeAzureKV(t)
	p, err := keyprovider.NewAzureKVProviderWithAPI(fk, testAzureKeyID, "RSA-OAEP-256", true)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	ctx := context.Background()
	plain, w, err := p.GenerateDEK(ctx, keyprovider.KeyOpts{VolumeID: "vol-a", KeyRef: testAzureKeyID})
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}
	if plain.Len() != 32 {
		t.Fatalf("DEK length: %d", plain.Len())
	}
	plain.Destroy()
	if _, err := p.Unwrap(ctx, w, "vol-b"); err == nil {
		t.Fatal("expected binding error on wrong volumeID")
	}
}

func TestAzureKV_NoBindMode_SkipsHMACCheck(t *testing.T) {
	// In no-bind mode, Unwrap on a different volumeID must succeed
	// because the HMAC header is not validated.
	fk := newFakeAzureKV(t)
	p, err := keyprovider.NewAzureKVProviderWithAPI(fk, testAzureKeyID, "RSA-OAEP-256", false)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	ctx := context.Background()
	plain, w, err := p.GenerateDEK(ctx, keyprovider.KeyOpts{VolumeID: "vol-a", KeyRef: testAzureKeyID})
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}
	original := append([]byte(nil), plain.Bytes()...)
	plain.Destroy()
	got, err := p.Unwrap(ctx, w, "vol-b")
	if err != nil {
		t.Fatalf("Unwrap with bind=false should ignore volume id: %v", err)
	}
	defer got.Destroy()
	if !bytes.Equal(original, got.Bytes()) {
		t.Fatal("unwrap mismatch in no-bind mode")
	}
}
