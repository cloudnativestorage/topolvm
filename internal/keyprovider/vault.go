package keyprovider

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/topolvm/topolvm/internal/crypt"
	"sigs.k8s.io/yaml"
)

// VaultProviderName is the registered name of the Vault Transit provider.
const VaultProviderName = "vault"

func init() {
	Register(VaultProviderName, func(cfgPath string) (KeyProvider, error) {
		return NewVaultProviderFromConfig(cfgPath)
	})
}

// VaultConfig holds non-secret configuration for the Vault provider. Tokens
// are acquired via Kubernetes auth using the pod's service account.
type VaultConfig struct {
	// Address is the Vault HTTPS endpoint, e.g. "https://vault.svc:8200".
	Address string `json:"address" yaml:"address"`
	// AuthMethod is currently "kubernetes" (default) or "token" (only for
	// tests/dev). The token method reads VAULT_TOKEN from the env.
	AuthMethod string `json:"authMethod,omitempty" yaml:"authMethod,omitempty"`
	// Role is the Kubernetes-auth role name (required for authMethod=kubernetes).
	Role string `json:"role,omitempty" yaml:"role,omitempty"`
	// AuthPath is the mount path of the Kubernetes auth method
	// (defaults to "kubernetes").
	AuthPath string `json:"authPath,omitempty" yaml:"authPath,omitempty"`
	// TransitPath is the mount path of the transit secrets engine
	// (defaults to "transit").
	TransitPath string `json:"transitPath,omitempty" yaml:"transitPath,omitempty"`
	// ServiceAccountTokenPath points at the projected SA token file. The
	// default is the in-cluster path.
	ServiceAccountTokenPath string `json:"serviceAccountTokenPath,omitempty" yaml:"serviceAccountTokenPath,omitempty"`
	// CACertFile is an optional PEM file with Vault's CA bundle.
	CACertFile string `json:"caCertFile,omitempty" yaml:"caCertFile,omitempty"`
	// TLSInsecure disables TLS verification (dev only).
	TLSInsecure bool `json:"tlsInsecure,omitempty" yaml:"tlsInsecure,omitempty"`
	// HTTPTimeout caps every HTTP call (default 30s).
	HTTPTimeout time.Duration `json:"httpTimeout,omitempty" yaml:"httpTimeout,omitempty"`
}

func (c *VaultConfig) applyDefaults() {
	if c.AuthMethod == "" {
		c.AuthMethod = "kubernetes"
	}
	if c.AuthPath == "" {
		c.AuthPath = "kubernetes"
	}
	if c.TransitPath == "" {
		c.TransitPath = "transit"
	}
	if c.ServiceAccountTokenPath == "" {
		c.ServiceAccountTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	}
	if c.HTTPTimeout == 0 {
		c.HTTPTimeout = 30 * time.Second
	}
}

// VaultProvider implements KeyProvider over a minimal HTTPS client to Vault.
// We only need three endpoints: transit/datakey, transit/decrypt, transit/rewrap.
type VaultProvider struct {
	cfg    VaultConfig
	client *http.Client

	mu          sync.Mutex
	cachedToken string
	tokenExpiry time.Time
}

// NewVaultProviderFromConfig reads the YAML/JSON config at cfgPath and builds
// a provider. The config file must NOT contain any token or other secret.
func NewVaultProviderFromConfig(cfgPath string) (*VaultProvider, error) {
	if cfgPath == "" {
		return nil, errors.New("keyprovider/vault: --key-provider-config is required")
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("keyprovider/vault: read config %s: %w", cfgPath, err)
	}
	var cfg VaultConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("keyprovider/vault: parse config: %w", err)
	}
	return NewVaultProvider(cfg)
}

// NewVaultProvider builds a provider directly from a config struct.
func NewVaultProvider(cfg VaultConfig) (*VaultProvider, error) {
	cfg.applyDefaults()
	if cfg.Address == "" {
		return nil, errors.New("keyprovider/vault: address is required")
	}
	tr := &http.Transport{}
	tlsCfg := &tls.Config{InsecureSkipVerify: cfg.TLSInsecure} //nolint:gosec // gated by TLSInsecure dev flag
	if cfg.CACertFile != "" {
		pem, err := os.ReadFile(cfg.CACertFile)
		if err != nil {
			return nil, fmt.Errorf("keyprovider/vault: read CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("keyprovider/vault: CA bundle has no usable certs")
		}
		tlsCfg.RootCAs = pool
	}
	tr.TLSClientConfig = tlsCfg
	return &VaultProvider{
		cfg:    cfg,
		client: &http.Client{Transport: tr, Timeout: cfg.HTTPTimeout},
	}, nil
}

// Name reports the provider name.
func (v *VaultProvider) Name() string { return VaultProviderName }

// BindsContext reports that Vault Transit binds the wrapped blob to the
// `context` parameter (base64(volumeID)), so Decrypt with a mismatched
// volume id fails on the server side.
func (v *VaultProvider) BindsContext() bool { return true }

// GenerateDEK uses transit/datakey/plaintext which returns the data key in
// plaintext + ciphertext form, bound to the encryption context.
func (v *VaultProvider) GenerateDEK(ctx context.Context, opts KeyOpts) (crypt.SecretBuf, WrappedKey, error) {
	body := map[string]any{
		"context": base64.StdEncoding.EncodeToString([]byte(opts.VolumeID)),
	}
	var resp struct {
		Data struct {
			Plaintext  string `json:"plaintext"`
			Ciphertext string `json:"ciphertext"`
			KeyVersion int    `json:"key_version"`
		} `json:"data"`
	}
	url := v.urlf("/v1/%s/datakey/plaintext/%s", v.cfg.TransitPath, opts.KeyRef)
	if err := v.do(ctx, http.MethodPost, url, body, &resp); err != nil {
		return nil, WrappedKey{}, fmt.Errorf("keyprovider/vault: datakey: %w", err)
	}
	pt, err := base64.StdEncoding.DecodeString(resp.Data.Plaintext)
	if err != nil {
		return nil, WrappedKey{}, fmt.Errorf("keyprovider/vault: decode plaintext: %w", err)
	}
	buf, err := crypt.SecretBufFrom(pt)
	if err != nil {
		return nil, WrappedKey{}, err
	}
	version := resp.Data.Ciphertext // e.g. "vault:v3:..."; we keep the prefix as the canonical version
	return buf, WrappedKey{
		Ciphertext: []byte(resp.Data.Ciphertext),
		KeyRef:     opts.KeyRef,
		KEKVersion: extractVaultVersion(version),
		Provider:   VaultProviderName,
	}, nil
}

// Unwrap calls transit/decrypt with the encryption context derived from volumeID.
func (v *VaultProvider) Unwrap(ctx context.Context, w WrappedKey, volumeID string) (crypt.SecretBuf, error) {
	body := map[string]any{
		"ciphertext": string(w.Ciphertext),
		"context":    base64.StdEncoding.EncodeToString([]byte(volumeID)),
	}
	var resp struct {
		Data struct {
			Plaintext string `json:"plaintext"`
		} `json:"data"`
	}
	url := v.urlf("/v1/%s/decrypt/%s", v.cfg.TransitPath, w.KeyRef)
	if err := v.do(ctx, http.MethodPost, url, body, &resp); err != nil {
		return nil, fmt.Errorf("keyprovider/vault: decrypt: %w", err)
	}
	pt, err := base64.StdEncoding.DecodeString(resp.Data.Plaintext)
	if err != nil {
		return nil, fmt.Errorf("keyprovider/vault: decode plaintext: %w", err)
	}
	return crypt.SecretBufFrom(pt)
}

// Rewrap calls transit/rewrap which re-encrypts the ciphertext under the
// current KEK version without exposing plaintext.
func (v *VaultProvider) Rewrap(ctx context.Context, w WrappedKey, volumeID string) (WrappedKey, error) {
	body := map[string]any{
		"ciphertext": string(w.Ciphertext),
		"context":    base64.StdEncoding.EncodeToString([]byte(volumeID)),
	}
	var resp struct {
		Data struct {
			Ciphertext string `json:"ciphertext"`
			KeyVersion int    `json:"key_version"`
		} `json:"data"`
	}
	url := v.urlf("/v1/%s/rewrap/%s", v.cfg.TransitPath, w.KeyRef)
	if err := v.do(ctx, http.MethodPost, url, body, &resp); err != nil {
		return WrappedKey{}, fmt.Errorf("keyprovider/vault: rewrap: %w", err)
	}
	return WrappedKey{
		Ciphertext: []byte(resp.Data.Ciphertext),
		KeyRef:     w.KeyRef,
		KEKVersion: extractVaultVersion(resp.Data.Ciphertext),
		Provider:   VaultProviderName,
	}, nil
}

// KEKVersion reads transit/keys/<keyRef> and returns latest_version.
func (v *VaultProvider) KEKVersion(ctx context.Context, keyRef string) (string, error) {
	var resp struct {
		Data struct {
			LatestVersion int `json:"latest_version"`
		} `json:"data"`
	}
	url := v.urlf("/v1/%s/keys/%s", v.cfg.TransitPath, keyRef)
	if err := v.do(ctx, http.MethodGet, url, nil, &resp); err != nil {
		return "", fmt.Errorf("keyprovider/vault: read key: %w", err)
	}
	return fmt.Sprintf("v%d", resp.Data.LatestVersion), nil
}

// extractVaultVersion parses "vault:v3:..." into "v3".
func extractVaultVersion(blob string) string {
	parts := strings.SplitN(blob, ":", 3)
	if len(parts) < 2 {
		return ""
	}
	return parts[1]
}

func (v *VaultProvider) urlf(format string, args ...any) string {
	rel := fmt.Sprintf(format, args...)
	return strings.TrimRight(v.cfg.Address, "/") + path.Clean(rel)
}

func (v *VaultProvider) do(ctx context.Context, method, url string, body any, out any) error {
	tok, err := v.token(ctx)
	if err != nil {
		return err
	}
	var br io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		br = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, br)
	if err != nil {
		return err
	}
	req.Header.Set("X-Vault-Token", tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("vault %s %s: status %d: %s", method, url, resp.StatusCode, redact(string(data)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// token returns a cached Vault client token, refreshing if expired.
func (v *VaultProvider) token(ctx context.Context) (string, error) {
	v.mu.Lock()
	if v.cachedToken != "" && time.Now().Before(v.tokenExpiry.Add(-30*time.Second)) {
		t := v.cachedToken
		v.mu.Unlock()
		return t, nil
	}
	v.mu.Unlock()

	switch strings.ToLower(v.cfg.AuthMethod) {
	case "token":
		t := os.Getenv("VAULT_TOKEN")
		if t == "" {
			return "", errors.New("keyprovider/vault: VAULT_TOKEN env var is empty under authMethod=token")
		}
		v.mu.Lock()
		v.cachedToken = t
		v.tokenExpiry = time.Now().Add(1 * time.Hour)
		v.mu.Unlock()
		return t, nil
	case "kubernetes":
		return v.loginKubernetes(ctx)
	default:
		return "", fmt.Errorf("keyprovider/vault: unsupported authMethod %q", v.cfg.AuthMethod)
	}
}

func (v *VaultProvider) loginKubernetes(ctx context.Context) (string, error) {
	if v.cfg.Role == "" {
		return "", errors.New("keyprovider/vault: kubernetes auth requires role")
	}
	jwt, err := os.ReadFile(v.cfg.ServiceAccountTokenPath)
	if err != nil {
		return "", fmt.Errorf("keyprovider/vault: read SA token: %w", err)
	}
	body := map[string]string{"role": v.cfg.Role, "jwt": strings.TrimSpace(string(jwt))}
	var resp struct {
		Auth struct {
			ClientToken   string `json:"client_token"`
			LeaseDuration int    `json:"lease_duration"`
		} `json:"auth"`
	}
	url := v.urlf("/v1/auth/%s/login", v.cfg.AuthPath)
	// We must not call v.do() here (recursion); inline a tokenless POST.
	buf, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	httpResp, err := v.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = httpResp.Body.Close() }()
	if httpResp.StatusCode/100 != 2 {
		data, _ := io.ReadAll(io.LimitReader(httpResp.Body, 4096))
		return "", fmt.Errorf("vault kubernetes login: status %d: %s", httpResp.StatusCode, redact(string(data)))
	}
	if err := json.NewDecoder(httpResp.Body).Decode(&resp); err != nil {
		return "", err
	}
	if resp.Auth.ClientToken == "" {
		return "", errors.New("keyprovider/vault: empty client_token from kubernetes login")
	}
	lease := time.Duration(resp.Auth.LeaseDuration) * time.Second
	if lease <= 0 {
		lease = 30 * time.Minute
	}
	v.mu.Lock()
	v.cachedToken = resp.Auth.ClientToken
	v.tokenExpiry = time.Now().Add(lease)
	v.mu.Unlock()
	return resp.Auth.ClientToken, nil
}

// redact strips obvious key-looking fields out of error bodies. Defensive
// only: Vault's standard 4xx/5xx bodies don't include key material, but if a
// custom audit handler echoed input we do not want to log it.
func redact(s string) string {
	for _, k := range []string{"plaintext", "ciphertext", "key", "data_key"} {
		s = strings.ReplaceAll(s, k, "[redacted]")
	}
	return s
}
