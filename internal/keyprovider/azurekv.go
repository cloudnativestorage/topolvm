//go:build kms_azure

// Package keyprovider Azure Key Vault / Managed HSM implementation.
// Build with -tags=kms_azure.
//
// Operation mapping (see design/tde/TDE-Provider-Azure-KeyVault.md):
//   GenerateDEK -> crypto/rand DEK + WrapKey (RSA-OAEP-256 or A256KW)
//   Unwrap      -> UnwrapKey
//   Rewrap      -> UnwrapKey then WrapKey under current key version
//   KEKVersion  -> key id version segment
//
// Key Vault wrap/unwrap does not carry AAD; if bindVolume is true (default)
// the provider prepends an HMAC-SHA256(dek, volumeID) header to the stored
// ciphertext to give tamper-evident binding. Otherwise BindsContext()=false
// and the conformance ContextMismatchFails test self-skips.
package keyprovider

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azkeys"
	"github.com/topolvm/topolvm/internal/crypt"
	"sigs.k8s.io/yaml"
)

// AzureKVProviderName is the registered provider name for Azure Key Vault.
const AzureKVProviderName = "azure-kv"

func init() {
	Register(AzureKVProviderName, func(cfgPath string) (KeyProvider, error) {
		return NewAzureKVProviderFromConfig(cfgPath)
	})
}

// AzureKVConfig is the non-secret config consumed by the provider.
type AzureKVConfig struct {
	// KeyRef is the Key Vault key identifier:
	// https://VAULT.vault.azure.net/keys/NAME[/VERSION]
	KeyRef string `json:"keyRef" yaml:"keyRef"`
	// WrapAlg is "RSA-OAEP-256" (RSA keys) or "A256KW" (Managed HSM AES).
	WrapAlg string `json:"wrapAlg,omitempty" yaml:"wrapAlg,omitempty"`
	// BindVolume turns on the HMAC(volumeID) header so Unwrap fails on a
	// mismatched volume id. Defaults to true.
	BindVolume *bool `json:"bindVolume,omitempty" yaml:"bindVolume,omitempty"`
}

// azureKVAPI is the narrow Key Vault surface we exercise.
type azureKVAPI interface {
	WrapKey(ctx context.Context, name, version string, params azkeys.KeyOperationParameters, opts *azkeys.WrapKeyOptions) (azkeys.WrapKeyResponse, error)
	UnwrapKey(ctx context.Context, name, version string, params azkeys.KeyOperationParameters, opts *azkeys.UnwrapKeyOptions) (azkeys.UnwrapKeyResponse, error)
	GetKey(ctx context.Context, name, version string, opts *azkeys.GetKeyOptions) (azkeys.GetKeyResponse, error)
}

// AzureKVProvider implements KeyProvider against Azure Key Vault / Managed HSM.
type AzureKVProvider struct {
	api      azureKVAPI
	keyName  string
	keyVer   string
	wrapAlg  string
	bindVol  bool
	rawKeyID string
}

// NewAzureKVProviderFromConfig builds a provider using DefaultAzureCredential
// (Workload Identity on AKS, env, managed identity, cli creds).
func NewAzureKVProviderFromConfig(cfgPath string) (*AzureKVProvider, error) {
	if cfgPath == "" {
		return nil, errors.New("keyprovider/azure-kv: --key-provider-config is required")
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("keyprovider/azure-kv: read config %s: %w", cfgPath, err)
	}
	var c AzureKVConfig
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("keyprovider/azure-kv: parse config: %w", err)
	}
	if c.KeyRef == "" {
		return nil, errors.New("keyprovider/azure-kv: keyRef is required")
	}
	if c.WrapAlg == "" {
		c.WrapAlg = string(azkeys.EncryptionAlgorithmRSAOAEP256)
	}
	bind := true
	if c.BindVolume != nil {
		bind = *c.BindVolume
	}
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("keyprovider/azure-kv: credential: %w", err)
	}
	vaultURL, keyName, keyVer, err := parseAzureKeyID(c.KeyRef)
	if err != nil {
		return nil, err
	}
	cli, err := azkeys.NewClient(vaultURL, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("keyprovider/azure-kv: new client: %w", err)
	}
	return &AzureKVProvider{
		api:      azureKVAPIShim{cli: cli},
		keyName:  keyName,
		keyVer:   keyVer,
		wrapAlg:  c.WrapAlg,
		bindVol:  bind,
		rawKeyID: c.KeyRef,
	}, nil
}

// NewAzureKVProviderWithAPI is for tests; pass a fake azureKVAPI.
func NewAzureKVProviderWithAPI(api azureKVAPI, keyID, wrapAlg string, bindVolume bool) (*AzureKVProvider, error) {
	_, keyName, keyVer, err := parseAzureKeyID(keyID)
	if err != nil {
		return nil, err
	}
	if wrapAlg == "" {
		wrapAlg = string(azkeys.EncryptionAlgorithmRSAOAEP256)
	}
	return &AzureKVProvider{
		api:      api,
		keyName:  keyName,
		keyVer:   keyVer,
		wrapAlg:  wrapAlg,
		bindVol:  bindVolume,
		rawKeyID: keyID,
	}, nil
}

// Name reports the registered provider name.
func (p *AzureKVProvider) Name() string { return AzureKVProviderName }

// BindsContext reports whether the HMAC(volumeID) header is enabled.
func (p *AzureKVProvider) BindsContext() bool { return p.bindVol }

// GenerateDEK mints a fresh 32 byte DEK, wraps it under the Key Vault key,
// and (when bindVolume) prepends an HMAC header binding the volume.
func (p *AzureKVProvider) GenerateDEK(ctx context.Context, o KeyOpts) (crypt.SecretBuf, WrappedKey, error) {
	dek, err := crypt.RandomSecretBuf(32)
	if err != nil {
		return nil, WrappedKey{}, err
	}
	wrapped, err := p.api.WrapKey(ctx, p.keyName, p.keyVer, azkeys.KeyOperationParameters{
		Algorithm: to.Ptr(azkeys.EncryptionAlgorithm(p.wrapAlg)),
		Value:     dek.Bytes(),
	}, nil)
	if err != nil {
		dek.Destroy()
		return nil, WrappedKey{}, fmt.Errorf("keyprovider/azure-kv: WrapKey: %w", err)
	}
	blob := p.encode(o.VolumeID, dek.Bytes(), wrapped.Result)
	version := p.keyVer
	if version == "" {
		version = "primary"
	}
	return dek, WrappedKey{
		Ciphertext: blob,
		KeyRef:     p.rawKeyID,
		KEKVersion: version,
		Provider:   AzureKVProviderName,
	}, nil
}

// Unwrap decodes the binding header (if any), unwraps the DEK via Key Vault,
// and verifies the HMAC over volumeID before returning the SecretBuf.
func (p *AzureKVProvider) Unwrap(ctx context.Context, w WrappedKey, volumeID string) (crypt.SecretBuf, error) {
	storedVolume, expectedTag, wrappedDEK, err := p.decode(w.Ciphertext)
	if err != nil {
		return nil, err
	}
	if p.bindVol && storedVolume != volumeID {
		return nil, errors.New("keyprovider/azure-kv: stored volumeID does not match requested volumeID")
	}
	resp, err := p.api.UnwrapKey(ctx, p.keyName, p.keyVer, azkeys.KeyOperationParameters{
		Algorithm: to.Ptr(azkeys.EncryptionAlgorithm(p.wrapAlg)),
		Value:     wrappedDEK,
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("keyprovider/azure-kv: UnwrapKey: %w", err)
	}
	if p.bindVol {
		if !hmac.Equal(expectedTag, hmacSHA256(resp.Result, []byte(volumeID))) {
			return nil, errors.New("keyprovider/azure-kv: HMAC volume binding failed")
		}
	}
	return crypt.SecretBufFrom(resp.Result)
}

// Rewrap unwraps the DEK and re-wraps under the current key version. The DEK
// transits in process memory briefly in a SecretBuf and is zeroized after.
func (p *AzureKVProvider) Rewrap(ctx context.Context, w WrappedKey, volumeID string) (WrappedKey, error) {
	dek, err := p.Unwrap(ctx, w, volumeID)
	if err != nil {
		return WrappedKey{}, err
	}
	defer dek.Destroy()
	resp, err := p.api.WrapKey(ctx, p.keyName, p.keyVer, azkeys.KeyOperationParameters{
		Algorithm: to.Ptr(azkeys.EncryptionAlgorithm(p.wrapAlg)),
		Value:     dek.Bytes(),
	}, nil)
	if err != nil {
		return WrappedKey{}, fmt.Errorf("keyprovider/azure-kv: WrapKey(rewrap): %w", err)
	}
	blob := p.encode(volumeID, dek.Bytes(), resp.Result)
	version := p.keyVer
	if version == "" {
		version = "primary"
	}
	return WrappedKey{
		Ciphertext: blob,
		KeyRef:     w.KeyRef,
		KEKVersion: version,
		Provider:   AzureKVProviderName,
	}, nil
}

// KEKVersion returns the configured key version, refreshing from GetKey when
// the configured KeyRef is unversioned.
func (p *AzureKVProvider) KEKVersion(ctx context.Context, keyRef string) (string, error) {
	if p.keyVer != "" {
		return p.keyVer, nil
	}
	resp, err := p.api.GetKey(ctx, p.keyName, "", nil)
	if err != nil {
		return "", fmt.Errorf("keyprovider/azure-kv: GetKey: %w", err)
	}
	if resp.Key == nil || resp.Key.KID == nil {
		return "", errors.New("keyprovider/azure-kv: empty kid")
	}
	_, _, ver, err := parseAzureKeyID(string(*resp.Key.KID))
	if err != nil {
		return "", err
	}
	if ver == "" {
		return "primary", nil
	}
	return ver, nil
}

// encode serializes:
//
//	1 byte version (=1)
//	uint16 volumeID length, volumeID bytes
//	HMAC-SHA256(dek, volumeID) (32 bytes) when bindVolume; else 32 zero bytes
//	uint32 wrappedDEK length, wrappedDEK bytes
func (p *AzureKVProvider) encode(volumeID string, dek, wrappedDEK []byte) []byte {
	var buf bytes.Buffer
	buf.WriteByte(1)
	writeUint16(&buf, uint16(len(volumeID)))
	buf.WriteString(volumeID)
	var tag [32]byte
	if p.bindVol {
		t := hmacSHA256(dek, []byte(volumeID))
		copy(tag[:], t)
	}
	buf.Write(tag[:])
	writeUint32(&buf, uint32(len(wrappedDEK)))
	buf.Write(wrappedDEK)
	return buf.Bytes()
}

func (p *AzureKVProvider) decode(blob []byte) (string, []byte, []byte, error) {
	if len(blob) < 1+2+0+32+4 {
		return "", nil, nil, errors.New("keyprovider/azure-kv: truncated blob")
	}
	if blob[0] != 1 {
		return "", nil, nil, fmt.Errorf("keyprovider/azure-kv: unknown blob version %d", blob[0])
	}
	off := 1
	if off+2 > len(blob) {
		return "", nil, nil, errors.New("keyprovider/azure-kv: truncated blob (volId len)")
	}
	volLen := int(binary.BigEndian.Uint16(blob[off : off+2]))
	off += 2
	if off+volLen > len(blob) {
		return "", nil, nil, errors.New("keyprovider/azure-kv: truncated blob (volId)")
	}
	vol := string(blob[off : off+volLen])
	off += volLen
	if off+32 > len(blob) {
		return "", nil, nil, errors.New("keyprovider/azure-kv: truncated blob (tag)")
	}
	tag := append([]byte(nil), blob[off:off+32]...)
	off += 32
	if off+4 > len(blob) {
		return "", nil, nil, errors.New("keyprovider/azure-kv: truncated blob (wrapped len)")
	}
	wrappedLen := int(binary.BigEndian.Uint32(blob[off : off+4]))
	off += 4
	if off+wrappedLen != len(blob) {
		return "", nil, nil, errors.New("keyprovider/azure-kv: truncated blob (wrapped body)")
	}
	wrapped := append([]byte(nil), blob[off:off+wrappedLen]...)
	return vol, tag, wrapped, nil
}

func hmacSHA256(key, msg []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return m.Sum(nil)
}

func writeUint16(b *bytes.Buffer, v uint16) { var x [2]byte; binary.BigEndian.PutUint16(x[:], v); b.Write(x[:]) }
func writeUint32(b *bytes.Buffer, v uint32) { var x [4]byte; binary.BigEndian.PutUint32(x[:], v); b.Write(x[:]) }

// parseAzureKeyID splits "https://VAULT.vault.azure.net/keys/NAME[/VERSION]"
// into (vaultURL, name, version).
func parseAzureKeyID(keyID string) (vault, name, version string, err error) {
	u, err := url.Parse(keyID)
	if err != nil {
		return "", "", "", fmt.Errorf("keyprovider/azure-kv: parse keyRef: %w", err)
	}
	if u.Scheme != "https" {
		return "", "", "", fmt.Errorf("keyprovider/azure-kv: keyRef must be https")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || parts[0] != "keys" {
		return "", "", "", fmt.Errorf("keyprovider/azure-kv: keyRef path must be /keys/NAME[/VERSION]")
	}
	vault = u.Scheme + "://" + u.Host
	name = parts[1]
	if len(parts) >= 3 {
		version = parts[2]
	}
	return vault, name, version, nil
}

// azureKVAPIShim adapts the concrete azkeys.Client to azureKVAPI.
type azureKVAPIShim struct{ cli *azkeys.Client }

func (s azureKVAPIShim) WrapKey(ctx context.Context, name, version string, params azkeys.KeyOperationParameters, opts *azkeys.WrapKeyOptions) (azkeys.WrapKeyResponse, error) {
	return s.cli.WrapKey(ctx, name, version, params, opts)
}
func (s azureKVAPIShim) UnwrapKey(ctx context.Context, name, version string, params azkeys.KeyOperationParameters, opts *azkeys.UnwrapKeyOptions) (azkeys.UnwrapKeyResponse, error) {
	return s.cli.UnwrapKey(ctx, name, version, params, opts)
}
func (s azureKVAPIShim) GetKey(ctx context.Context, name, version string, opts *azkeys.GetKeyOptions) (azkeys.GetKeyResponse, error) {
	return s.cli.GetKey(ctx, name, version, opts)
}
