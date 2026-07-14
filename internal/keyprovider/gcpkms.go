//go:build kms_gcp

// Package keyprovider GCP Cloud KMS implementation. Build with -tags=kms_gcp.
//
// Operation mapping (see design/tde/TDE-Provider-GCP-KMS.md):
//   GenerateDEK -> crypto/rand DEK + Encrypt with AAD=volumeID
//   Unwrap      -> Decrypt with the identical AAD
//   Rewrap      -> rewrapMode=reencrypt (Decrypt + Encrypt under primary)
//                  or metadata-only (refresh KEKVersion without
//                  re-encrypting, relying on transparent version selection)
//   KEKVersion  -> GetCryptoKey().Primary.Name (CryptoKeyVersion resource)
package keyprovider

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"github.com/topolvm/topolvm/internal/crypt"
	"sigs.k8s.io/yaml"
)

// GCPKMSProviderName is the registered provider name for GCP Cloud KMS.
const GCPKMSProviderName = "gcp-kms"

func init() {
	Register(GCPKMSProviderName, func(cfgPath string) (KeyProvider, error) {
		return NewGCPKMSProviderFromConfig(context.Background(), cfgPath)
	})
}

// GCPKMSConfig is the non-secret config consumed by the provider. Auth uses
// the default credentials chain (Workload Identity on GKE, ADC env, etc).
type GCPKMSConfig struct {
	KeyRef string `json:"keyRef" yaml:"keyRef"`
	// RewrapMode picks "metadata-only" (do not re-encrypt; just refresh
	// KEKVersion) or "reencrypt" (Decrypt + Encrypt under current primary,
	// brief in-process plaintext held in SecretBuf). Defaults to
	// metadata-only because Cloud KMS auto-selects the version on
	// Decrypt; metadata-only keeps the controller process out of any
	// plaintext path during rotation.
	RewrapMode string `json:"rewrapMode,omitempty" yaml:"rewrapMode,omitempty"`
}

// gcpKMSAPI is the narrow surface we exercise.
type gcpKMSAPI interface {
	Encrypt(ctx context.Context, req *kmspb.EncryptRequest) (*kmspb.EncryptResponse, error)
	Decrypt(ctx context.Context, req *kmspb.DecryptRequest) (*kmspb.DecryptResponse, error)
	GetCryptoKey(ctx context.Context, req *kmspb.GetCryptoKeyRequest) (*kmspb.CryptoKey, error)
}

// GCPKMSProvider implements KeyProvider against GCP Cloud KMS.
type GCPKMSProvider struct {
	client     gcpKMSAPI
	defaultKey string
	rewrapMode string
	closeFn    func() error
}

const (
	rewrapModeMetadataOnly = "metadata-only"
	rewrapModeReencrypt    = "reencrypt"
)

// NewGCPKMSProviderFromConfig builds a provider using ADC.
func NewGCPKMSProviderFromConfig(ctx context.Context, cfgPath string) (*GCPKMSProvider, error) {
	if cfgPath == "" {
		return nil, errors.New("keyprovider/gcp-kms: --key-provider-config is required")
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("keyprovider/gcp-kms: read config %s: %w", cfgPath, err)
	}
	var c GCPKMSConfig
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("keyprovider/gcp-kms: parse config: %w", err)
	}
	if c.KeyRef == "" {
		return nil, errors.New("keyprovider/gcp-kms: keyRef is required")
	}
	if c.RewrapMode == "" {
		c.RewrapMode = rewrapModeMetadataOnly
	}
	cli, err := kms.NewKeyManagementClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("keyprovider/gcp-kms: new client: %w", err)
	}
	return &GCPKMSProvider{
		client:     gcpKMSAPIShim{c: cli},
		defaultKey: c.KeyRef,
		rewrapMode: c.RewrapMode,
		closeFn:    cli.Close,
	}, nil
}

// NewGCPKMSProviderWithClient is for tests; pass any gcpKMSAPI implementation.
func NewGCPKMSProviderWithClient(client gcpKMSAPI, defaultKey, rewrapMode string) *GCPKMSProvider {
	if rewrapMode == "" {
		rewrapMode = rewrapModeMetadataOnly
	}
	return &GCPKMSProvider{client: client, defaultKey: defaultKey, rewrapMode: rewrapMode}
}

// Close releases the underlying gRPC client when the provider was built from
// config. Safe to call multiple times.
func (p *GCPKMSProvider) Close() error {
	if p.closeFn != nil {
		err := p.closeFn()
		p.closeFn = nil
		return err
	}
	return nil
}

// Name reports the registered provider name.
func (p *GCPKMSProvider) Name() string { return GCPKMSProviderName }

// BindsContext reports that Cloud KMS Encrypt/Decrypt enforce AAD binding.
func (p *GCPKMSProvider) BindsContext() bool { return true }

// GenerateDEK creates a 32 byte DEK with crypto/rand and wraps it via the
// primary CryptoKeyVersion, binding the volumeID as AAD.
func (p *GCPKMSProvider) GenerateDEK(ctx context.Context, o KeyOpts) (crypt.SecretBuf, WrappedKey, error) {
	key := o.KeyRef
	if key == "" {
		key = p.defaultKey
	}
	if key == "" {
		return nil, WrappedKey{}, errors.New("keyprovider/gcp-kms: no key resource provided")
	}
	dek, err := crypt.RandomSecretBuf(32)
	if err != nil {
		return nil, WrappedKey{}, err
	}
	resp, err := p.client.Encrypt(ctx, &kmspb.EncryptRequest{
		Name:                        key,
		Plaintext:                   dek.Bytes(),
		AdditionalAuthenticatedData: []byte(o.VolumeID),
	})
	if err != nil {
		dek.Destroy()
		return nil, WrappedKey{}, fmt.Errorf("keyprovider/gcp-kms: Encrypt: %w", err)
	}
	version, _ := p.primaryVersion(ctx, key)
	if version == "" {
		// fall back to the response's name (which is the
		// CryptoKeyVersion that performed the encrypt).
		version = resp.Name
	}
	return dek, WrappedKey{
		Ciphertext: resp.Ciphertext,
		KeyRef:     key,
		KEKVersion: version,
		Provider:   GCPKMSProviderName,
	}, nil
}

// Unwrap calls Cloud KMS Decrypt with AAD=volumeID. AAD mismatch fails.
func (p *GCPKMSProvider) Unwrap(ctx context.Context, w WrappedKey, volumeID string) (crypt.SecretBuf, error) {
	resp, err := p.client.Decrypt(ctx, &kmspb.DecryptRequest{
		Name:                        w.KeyRef,
		Ciphertext:                  w.Ciphertext,
		AdditionalAuthenticatedData: []byte(volumeID),
	})
	if err != nil {
		return nil, fmt.Errorf("keyprovider/gcp-kms: Decrypt: %w", err)
	}
	return crypt.SecretBufFrom(resp.Plaintext)
}

// Rewrap behavior is driven by config: metadata-only just refreshes
// KEKVersion (transparent version selection at decrypt time keeps the blob
// valid); reencrypt performs Decrypt+Encrypt with a SecretBuf in between.
func (p *GCPKMSProvider) Rewrap(ctx context.Context, w WrappedKey, volumeID string) (WrappedKey, error) {
	switch p.rewrapMode {
	case rewrapModeMetadataOnly:
		version, err := p.primaryVersion(ctx, w.KeyRef)
		if err != nil {
			return WrappedKey{}, err
		}
		out := w
		out.KEKVersion = version
		return out, nil
	case rewrapModeReencrypt:
		dek, err := p.Unwrap(ctx, w, volumeID)
		if err != nil {
			return WrappedKey{}, err
		}
		defer dek.Destroy()
		resp, err := p.client.Encrypt(ctx, &kmspb.EncryptRequest{
			Name:                        w.KeyRef,
			Plaintext:                   dek.Bytes(),
			AdditionalAuthenticatedData: []byte(volumeID),
		})
		if err != nil {
			return WrappedKey{}, fmt.Errorf("keyprovider/gcp-kms: Encrypt(rewrap): %w", err)
		}
		version, _ := p.primaryVersion(ctx, w.KeyRef)
		if version == "" {
			version = resp.Name
		}
		return WrappedKey{
			Ciphertext: resp.Ciphertext,
			KeyRef:     w.KeyRef,
			KEKVersion: version,
			Provider:   GCPKMSProviderName,
		}, nil
	default:
		return WrappedKey{}, fmt.Errorf("keyprovider/gcp-kms: unknown rewrapMode %q", p.rewrapMode)
	}
}

// KEKVersion returns the primary CryptoKeyVersion resource name. Empty key
// falls back to the configured default.
func (p *GCPKMSProvider) KEKVersion(ctx context.Context, keyRef string) (string, error) {
	if keyRef == "" {
		keyRef = p.defaultKey
	}
	return p.primaryVersion(ctx, keyRef)
}

func (p *GCPKMSProvider) primaryVersion(ctx context.Context, keyRef string) (string, error) {
	ck, err := p.client.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: keyRef})
	if err != nil {
		return "", fmt.Errorf("keyprovider/gcp-kms: GetCryptoKey: %w", err)
	}
	if ck.Primary == nil {
		return "", errors.New("keyprovider/gcp-kms: CryptoKey has no primary version")
	}
	return ck.Primary.Name, nil
}

// _ = rand keeps the random source mentioned to avoid an unused import when
// the SDK is added later via a transition.
var _ = rand.Reader

// gcpKMSAPIShim adapts the concrete client to the narrow gcpKMSAPI surface.
type gcpKMSAPIShim struct{ c *kms.KeyManagementClient }

func (s gcpKMSAPIShim) Encrypt(ctx context.Context, req *kmspb.EncryptRequest) (*kmspb.EncryptResponse, error) {
	return s.c.Encrypt(ctx, req)
}
func (s gcpKMSAPIShim) Decrypt(ctx context.Context, req *kmspb.DecryptRequest) (*kmspb.DecryptResponse, error) {
	return s.c.Decrypt(ctx, req)
}
func (s gcpKMSAPIShim) GetCryptoKey(ctx context.Context, req *kmspb.GetCryptoKeyRequest) (*kmspb.CryptoKey, error) {
	return s.c.GetCryptoKey(ctx, req)
}
