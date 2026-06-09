package keyprovider_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/topolvm/topolvm/internal/keyprovider"
	"github.com/topolvm/topolvm/internal/keyprovider/providertest"
)

func TestVault_Conformance(t *testing.T) {
	p, _, closeSrv := newTestVault(t, true)
	defer closeSrv()
	providertest.Run(t, p, "k")
}

// fakeVault is a minimal stand-in for transit + kubernetes auth. It does not
// implement any real crypto; instead it records calls and returns deterministic
// blobs so we can assert request shape and round-trip semantics.
type fakeVault struct {
	t            *testing.T
	keyVersion   int
	requireToken bool
}

func (f *fakeVault) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/auth/kubernetes/login", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Role, Jwt string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Role != "topolvm" {
			http.Error(w, "wrong role", http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"auth": map[string]any{
				"client_token":   "s.test-token",
				"lease_duration": 600,
			},
		})
	})
	mux.HandleFunc("/v1/transit/datakey/plaintext/k", func(w http.ResponseWriter, r *http.Request) {
		f.requireTokenHdr(w, r)
		var body struct{ Context string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Context == "" {
			http.Error(w, "missing context", http.StatusBadRequest)
			return
		}
		// Generate a deterministic 32-byte DEK derived from context so
		// that decrypt can reproduce it. (Tests do not care about
		// cryptographic strength; only that lengths and round-trip
		// behavior match the conformance suite.)
		f.keyVersion++
		dek := make([]byte, 32)
		for i := range dek {
			dek[i] = byte((int(body.Context[i%len(body.Context)]) + i) & 0xff)
		}
		plain := base64.StdEncoding.EncodeToString(dek)
		ct := "vault:v" + itoa(f.keyVersion) + ":" + body.Context
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"plaintext":   plain,
				"ciphertext":  ct,
				"key_version": f.keyVersion,
			},
		})
	})
	mux.HandleFunc("/v1/transit/decrypt/k", func(w http.ResponseWriter, r *http.Request) {
		f.requireTokenHdr(w, r)
		var body struct{ Ciphertext, Context string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		// The fake echo expects the context embedded in the ciphertext.
		parts := strings.SplitN(body.Ciphertext, ":", 3)
		if len(parts) != 3 || parts[0] != "vault" {
			http.Error(w, "bad ct", http.StatusBadRequest)
			return
		}
		if parts[2] != body.Context {
			http.Error(w, "context mismatch", http.StatusForbidden)
			return
		}
		dek := make([]byte, 32)
		for i := range dek {
			dek[i] = byte((int(body.Context[i%len(body.Context)]) + i) & 0xff)
		}
		plain := base64.StdEncoding.EncodeToString(dek)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"plaintext": plain},
		})
	})
	mux.HandleFunc("/v1/transit/rewrap/k", func(w http.ResponseWriter, r *http.Request) {
		f.requireTokenHdr(w, r)
		var body struct{ Ciphertext, Context string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		// Bump the version on rewrap.
		f.keyVersion++
		ct := "vault:v" + itoa(f.keyVersion) + ":" + body.Context
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"ciphertext": ct, "key_version": f.keyVersion},
		})
	})
	mux.HandleFunc("/v1/transit/keys/k", func(w http.ResponseWriter, r *http.Request) {
		f.requireTokenHdr(w, r)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"latest_version": f.keyVersion},
		})
	})
	return mux
}

func (f *fakeVault) requireTokenHdr(w http.ResponseWriter, r *http.Request) {
	if !f.requireToken {
		return
	}
	if r.Header.Get("X-Vault-Token") == "" {
		http.Error(w, "missing token", http.StatusForbidden)
	}
}

func itoa(i int) string {
	// avoid strconv import just to keep the test file lean
	if i == 0 {
		return "0"
	}
	var buf [16]byte
	pos := len(buf)
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

func newTestVault(t *testing.T, requireToken bool) (*keyprovider.VaultProvider, *fakeVault, func()) {
	fv := &fakeVault{t: t, keyVersion: 0, requireToken: requireToken}
	srv := httptest.NewServer(fv.handler())
	// Use token auth method backed by env to keep the test free of files.
	t.Setenv("VAULT_TOKEN", "s.test-token")
	cfg := keyprovider.VaultConfig{
		Address:     srv.URL,
		AuthMethod:  "token",
		TransitPath: "transit",
		HTTPTimeout: 5 * time.Second,
	}
	p, err := keyprovider.NewVaultProvider(cfg)
	if err != nil {
		srv.Close()
		t.Fatalf("NewVaultProvider: %v", err)
	}
	return p, fv, srv.Close
}

func TestVault_DatakeyDecryptRoundTrip(t *testing.T) {
	p, _, close := newTestVault(t, true)
	defer close()

	ctx := context.Background()
	plain, wrapped, err := p.GenerateDEK(ctx, keyprovider.KeyOpts{VolumeID: "vol-1", KeyRef: "k"})
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}
	original := append([]byte(nil), plain.Bytes()...)
	plain.Destroy()
	if len(original) != 32 {
		t.Fatalf("DEK length: %d", len(original))
	}
	if wrapped.KEKVersion != "v1" {
		t.Fatalf("kek version: %q", wrapped.KEKVersion)
	}
	if !strings.HasPrefix(string(wrapped.Ciphertext), "vault:v1:") {
		t.Fatalf("ciphertext: %q", wrapped.Ciphertext)
	}

	unwrapped, err := p.Unwrap(ctx, wrapped, "vol-1")
	if err != nil {
		t.Fatalf("Unwrap: %v", err)
	}
	defer unwrapped.Destroy()
	if string(unwrapped.Bytes()) != string(original) {
		t.Fatalf("unwrap mismatch")
	}
}

func TestVault_ContextMismatchFails(t *testing.T) {
	p, _, close := newTestVault(t, true)
	defer close()

	ctx := context.Background()
	plain, wrapped, err := p.GenerateDEK(ctx, keyprovider.KeyOpts{VolumeID: "vol-1", KeyRef: "k"})
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}
	plain.Destroy()

	// Use the wrong volume id; fake replies 403 context mismatch.
	if _, err := p.Unwrap(ctx, wrapped, "vol-2"); err == nil {
		t.Fatal("expected error on context mismatch")
	}
}

func TestVault_Rewrap(t *testing.T) {
	p, _, close := newTestVault(t, true)
	defer close()

	ctx := context.Background()
	plain, w1, err := p.GenerateDEK(ctx, keyprovider.KeyOpts{VolumeID: "vol-1", KeyRef: "k"})
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}
	plain.Destroy()

	w2, err := p.Rewrap(ctx, w1, "vol-1")
	if err != nil {
		t.Fatalf("Rewrap: %v", err)
	}
	if w2.KEKVersion == w1.KEKVersion {
		t.Fatalf("rewrap did not bump version: %s", w2.KEKVersion)
	}
}

func TestVault_KEKVersion(t *testing.T) {
	p, fv, close := newTestVault(t, true)
	defer close()
	fv.keyVersion = 5
	v, err := p.KEKVersion(context.Background(), "k")
	if err != nil {
		t.Fatalf("KEKVersion: %v", err)
	}
	if v != "v5" {
		t.Fatalf("version: %s", v)
	}
}

