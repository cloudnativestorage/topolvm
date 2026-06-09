//go:build kms_aws

// Package keyprovider AWS KMS implementation. Build with -tags=kms_aws to
// include this provider in topolvm-controller / topolvm-node.
//
// Operation mapping (see design/tde/TDE-Provider-AWS-KMS.md):
//   GenerateDEK -> GenerateDataKey (AES_256) + EncryptionContext
//   Unwrap      -> Decrypt with the identical EncryptionContext
//   Rewrap      -> ReEncrypt (no-op when CMK is unchanged, since KMS
//                  rotates the backing key transparently)
//   KEKVersion  -> DescribeKey + GetKeyRotationStatus combined into a
//                  stable comparable string (CMK arn + rotation marker)
package keyprovider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	kmstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"
	"github.com/topolvm/topolvm/internal/crypt"
	"sigs.k8s.io/yaml"
)

// AWSKMSProviderName is the registered provider name for AWS KMS.
const AWSKMSProviderName = "aws-kms"

func init() {
	Register(AWSKMSProviderName, func(cfgPath string) (KeyProvider, error) {
		return NewAWSKMSProviderFromConfig(cfgPath)
	})
}

// AWSKMSConfig is the non-secret config consumed by the provider. Auth is
// done by IRSA / the default SDK credential chain; no keys live here.
type AWSKMSConfig struct {
	Region string `json:"region" yaml:"region"`
	KeyRef string `json:"keyRef,omitempty" yaml:"keyRef,omitempty"`
}

// awsKMSAPI is the narrow surface of kms.Client we exercise; tests inject a fake.
type awsKMSAPI interface {
	GenerateDataKey(ctx context.Context, in *kms.GenerateDataKeyInput, opts ...func(*kms.Options)) (*kms.GenerateDataKeyOutput, error)
	Decrypt(ctx context.Context, in *kms.DecryptInput, opts ...func(*kms.Options)) (*kms.DecryptOutput, error)
	ReEncrypt(ctx context.Context, in *kms.ReEncryptInput, opts ...func(*kms.Options)) (*kms.ReEncryptOutput, error)
	DescribeKey(ctx context.Context, in *kms.DescribeKeyInput, opts ...func(*kms.Options)) (*kms.DescribeKeyOutput, error)
	GetKeyRotationStatus(ctx context.Context, in *kms.GetKeyRotationStatusInput, opts ...func(*kms.Options)) (*kms.GetKeyRotationStatusOutput, error)
}

// AWSKMSProvider implements KeyProvider against AWS KMS.
type AWSKMSProvider struct {
	client     awsKMSAPI
	defaultKey string
}

// NewAWSKMSProviderFromConfig reads a YAML config file (region + default
// keyRef) and builds a provider using the default SDK credential chain
// (IRSA, instance profile, env, shared config).
func NewAWSKMSProviderFromConfig(cfgPath string) (*AWSKMSProvider, error) {
	if cfgPath == "" {
		return nil, errors.New("keyprovider/aws-kms: --key-provider-config is required")
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("keyprovider/aws-kms: read config %s: %w", cfgPath, err)
	}
	var c AWSKMSConfig
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("keyprovider/aws-kms: parse config: %w", err)
	}
	if c.Region == "" {
		return nil, errors.New("keyprovider/aws-kms: region is required")
	}
	awsConf, err := awscfg.LoadDefaultConfig(context.Background(), awscfg.WithRegion(c.Region))
	if err != nil {
		return nil, fmt.Errorf("keyprovider/aws-kms: load aws config: %w", err)
	}
	return &AWSKMSProvider{client: kms.NewFromConfig(awsConf), defaultKey: c.KeyRef}, nil
}

// NewAWSKMSProviderWithClient is for tests; pass a fake KMS client.
func NewAWSKMSProviderWithClient(client awsKMSAPI, defaultKey string) *AWSKMSProvider {
	return &AWSKMSProvider{client: client, defaultKey: defaultKey}
}

// Name reports the registered provider name.
func (p *AWSKMSProvider) Name() string { return AWSKMSProviderName }

// BindsContext reports that AWS KMS EncryptionContext is server-enforced.
func (p *AWSKMSProvider) BindsContext() bool { return true }

func awsEncryptionContext(volumeID string) map[string]string {
	return map[string]string{
		"volumeID": volumeID,
		"csi":      "topolvm.io",
	}
}

// GenerateDEK calls KMS GenerateDataKey to mint a fresh AES-256 DEK bound to
// the volume's encryption context. The plaintext is copied into a SecretBuf
// and the SDK's plaintext slice is zeroized.
func (p *AWSKMSProvider) GenerateDEK(ctx context.Context, o KeyOpts) (crypt.SecretBuf, WrappedKey, error) {
	key := o.KeyRef
	if key == "" {
		key = p.defaultKey
	}
	if key == "" {
		return nil, WrappedKey{}, errors.New("keyprovider/aws-kms: no key arn provided")
	}
	out, err := p.client.GenerateDataKey(ctx, &kms.GenerateDataKeyInput{
		KeyId:             aws.String(key),
		KeySpec:           kmstypes.DataKeySpecAes256,
		EncryptionContext: awsEncryptionContext(o.VolumeID),
	})
	if err != nil {
		return nil, WrappedKey{}, fmt.Errorf("keyprovider/aws-kms: GenerateDataKey: %w", err)
	}
	buf, err := crypt.SecretBufFrom(out.Plaintext) // zeroizes out.Plaintext
	if err != nil {
		return nil, WrappedKey{}, err
	}
	version, err := p.kekVersion(ctx, key)
	if err != nil {
		// KEKVersion is best-effort; default to the key arn so the
		// reconciler still has a stable comparable string.
		version = key
	}
	return buf, WrappedKey{
		Ciphertext: out.CiphertextBlob,
		KeyRef:     key,
		KEKVersion: version,
		Provider:   AWSKMSProviderName,
	}, nil
}

// Unwrap calls KMS Decrypt with the same EncryptionContext. Context mismatch
// is enforced by KMS.
func (p *AWSKMSProvider) Unwrap(ctx context.Context, w WrappedKey, volumeID string) (crypt.SecretBuf, error) {
	out, err := p.client.Decrypt(ctx, &kms.DecryptInput{
		CiphertextBlob:    w.Ciphertext,
		KeyId:             aws.String(w.KeyRef),
		EncryptionContext: awsEncryptionContext(volumeID),
	})
	if err != nil {
		return nil, fmt.Errorf("keyprovider/aws-kms: Decrypt: %w", err)
	}
	return crypt.SecretBufFrom(out.Plaintext)
}

// Rewrap calls KMS ReEncrypt. AWS KMS rotates the backing key transparently,
// so when DestinationKeyId equals the source key id, ReEncrypt simply
// re-wraps under the current backing key; the resulting blob may be byte
// identical to the input. The encryptionkey reconciler treats unchanged
// versions as a no-op so this never loops.
func (p *AWSKMSProvider) Rewrap(ctx context.Context, w WrappedKey, volumeID string) (WrappedKey, error) {
	out, err := p.client.ReEncrypt(ctx, &kms.ReEncryptInput{
		CiphertextBlob:               w.Ciphertext,
		SourceEncryptionContext:      awsEncryptionContext(volumeID),
		DestinationEncryptionContext: awsEncryptionContext(volumeID),
		DestinationKeyId:             aws.String(w.KeyRef),
	})
	if err != nil {
		return WrappedKey{}, fmt.Errorf("keyprovider/aws-kms: ReEncrypt: %w", err)
	}
	version, vErr := p.kekVersion(ctx, w.KeyRef)
	if vErr != nil {
		version = w.KEKVersion
	}
	return WrappedKey{
		Ciphertext: out.CiphertextBlob,
		KeyRef:     w.KeyRef,
		KEKVersion: version,
		Provider:   AWSKMSProviderName,
	}, nil
}

// KEKVersion returns a stable comparable version string. AWS KMS automatic
// rotation keeps the same CMK arn and rotates the backing key yearly, so we
// combine the arn with a rotation marker so the reconciler can detect a CMK
// migration (the only event that requires rewrap on AWS).
func (p *AWSKMSProvider) KEKVersion(ctx context.Context, keyRef string) (string, error) {
	if keyRef == "" {
		keyRef = p.defaultKey
	}
	return p.kekVersion(ctx, keyRef)
}

func (p *AWSKMSProvider) kekVersion(ctx context.Context, keyRef string) (string, error) {
	if keyRef == "" {
		return "", errors.New("keyprovider/aws-kms: empty keyRef")
	}
	desc, err := p.client.DescribeKey(ctx, &kms.DescribeKeyInput{KeyId: aws.String(keyRef)})
	if err != nil {
		return "", fmt.Errorf("keyprovider/aws-kms: DescribeKey: %w", err)
	}
	rotation := "norot"
	rstat, rerr := p.client.GetKeyRotationStatus(ctx, &kms.GetKeyRotationStatusInput{KeyId: aws.String(keyRef)})
	if rerr == nil {
		if rstat.KeyRotationEnabled {
			rotation = "rot"
		}
	}
	arn := ""
	if desc.KeyMetadata != nil {
		arn = aws.ToString(desc.KeyMetadata.Arn)
	}
	if arn == "" {
		arn = keyRef
	}
	return strings.Join([]string{arn, rotation}, "#"), nil
}
