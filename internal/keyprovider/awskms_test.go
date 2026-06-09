//go:build kms_aws

package keyprovider_test

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"sort"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	kmstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"
	"github.com/topolvm/topolvm/internal/keyprovider"
	"github.com/topolvm/topolvm/internal/keyprovider/providertest"
)

// fakeKMS is an in-memory stand-in for the AWS KMS service. It implements
// the awsKMSAPI surface and gives correct semantics for context binding so
// the conformance suite passes against it.
type fakeKMS struct {
	kek         []byte // per-test KEK
	rotation    bool
	contextFail bool
}

func newFakeKMS(t *testing.T) *fakeKMS {
	t.Helper()
	kek := sha256.Sum256([]byte("fakekms-kek"))
	return &fakeKMS{kek: kek[:], rotation: true}
}

func (f *fakeKMS) gcm() cipher.AEAD {
	block, err := aes.NewCipher(f.kek)
	if err != nil {
		panic(err)
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	return g
}

func (f *fakeKMS) encContext(m map[string]string) []byte {
	// Canonical AAD: sort keys deterministically.
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var buf bytes.Buffer
	for _, k := range keys {
		buf.WriteString(k)
		buf.WriteByte('=')
		buf.WriteString(m[k])
		buf.WriteByte(';')
	}
	return buf.Bytes()
}

func (f *fakeKMS) GenerateDataKey(_ context.Context, in *kms.GenerateDataKeyInput, _ ...func(*kms.Options)) (*kms.GenerateDataKeyOutput, error) {
	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		return nil, err
	}
	gcm := f.gcm()
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	ct := gcm.Seal(nil, nonce, dek, f.encContext(in.EncryptionContext))
	blob := append(nonce, ct...)
	return &kms.GenerateDataKeyOutput{
		Plaintext:      dek,
		CiphertextBlob: blob,
		KeyId:          in.KeyId,
	}, nil
}

func (f *fakeKMS) Decrypt(_ context.Context, in *kms.DecryptInput, _ ...func(*kms.Options)) (*kms.DecryptOutput, error) {
	if f.contextFail {
		return nil, errors.New("fakekms: context mismatch")
	}
	gcm := f.gcm()
	if len(in.CiphertextBlob) < gcm.NonceSize() {
		return nil, errors.New("fakekms: truncated blob")
	}
	nonce := in.CiphertextBlob[:gcm.NonceSize()]
	ct := in.CiphertextBlob[gcm.NonceSize():]
	dek, err := gcm.Open(nil, nonce, ct, f.encContext(in.EncryptionContext))
	if err != nil {
		return nil, errors.New("fakekms: decrypt failed (context mismatch)")
	}
	return &kms.DecryptOutput{Plaintext: dek, KeyId: in.KeyId}, nil
}

func (f *fakeKMS) ReEncrypt(_ context.Context, in *kms.ReEncryptInput, _ ...func(*kms.Options)) (*kms.ReEncryptOutput, error) {
	gcm := f.gcm()
	if len(in.CiphertextBlob) < gcm.NonceSize() {
		return nil, errors.New("fakekms: truncated blob")
	}
	nonce := in.CiphertextBlob[:gcm.NonceSize()]
	ct := in.CiphertextBlob[gcm.NonceSize():]
	dek, err := gcm.Open(nil, nonce, ct, f.encContext(in.SourceEncryptionContext))
	if err != nil {
		return nil, err
	}
	// Re-encrypt with a fresh nonce, simulating AWS rotating the backing key.
	newNonce := make([]byte, gcm.NonceSize())
	_, _ = rand.Read(newNonce)
	newCT := gcm.Seal(nil, newNonce, dek, f.encContext(in.DestinationEncryptionContext))
	blob := append(newNonce, newCT...)
	return &kms.ReEncryptOutput{CiphertextBlob: blob, KeyId: in.DestinationKeyId}, nil
}

func (f *fakeKMS) DescribeKey(_ context.Context, in *kms.DescribeKeyInput, _ ...func(*kms.Options)) (*kms.DescribeKeyOutput, error) {
	return &kms.DescribeKeyOutput{KeyMetadata: &kmstypes.KeyMetadata{Arn: in.KeyId}}, nil
}

func (f *fakeKMS) GetKeyRotationStatus(_ context.Context, _ *kms.GetKeyRotationStatusInput, _ ...func(*kms.Options)) (*kms.GetKeyRotationStatusOutput, error) {
	return &kms.GetKeyRotationStatusOutput{KeyRotationEnabled: f.rotation}, nil
}

func TestAWSKMS_Conformance(t *testing.T) {
	fk := newFakeKMS(t)
	p := keyprovider.NewAWSKMSProviderWithClient(fk, "arn:aws:kms:us-east-1:0:key/test")
	providertest.Run(t, p, "arn:aws:kms:us-east-1:0:key/test")
}

func TestAWSKMS_EncryptionContextShape(t *testing.T) {
	// Capture the EncryptionContext keys to assert volumeID + csi marker.
	captured := map[string]string{}
	fk := &awsCaptureKMS{inner: newFakeKMS(t), captured: captured}
	p := keyprovider.NewAWSKMSProviderWithClient(fk, "arn:aws:kms:us-east-1:0:key/test")
	if _, _, err := p.GenerateDEK(context.Background(), keyprovider.KeyOpts{VolumeID: "pvc-abc", KeyRef: "arn:aws:kms:us-east-1:0:key/test"}); err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}
	if captured["volumeID"] != "pvc-abc" {
		t.Fatalf("volumeID = %q", captured["volumeID"])
	}
	if captured["csi"] != "topolvm.io" {
		t.Fatalf("csi marker = %q", captured["csi"])
	}
}

type awsCaptureKMS struct {
	inner    *fakeKMS
	captured map[string]string
}

func (a *awsCaptureKMS) GenerateDataKey(ctx context.Context, in *kms.GenerateDataKeyInput, opts ...func(*kms.Options)) (*kms.GenerateDataKeyOutput, error) {
	for k, v := range in.EncryptionContext {
		a.captured[k] = v
	}
	return a.inner.GenerateDataKey(ctx, in, opts...)
}
func (a *awsCaptureKMS) Decrypt(ctx context.Context, in *kms.DecryptInput, opts ...func(*kms.Options)) (*kms.DecryptOutput, error) {
	return a.inner.Decrypt(ctx, in, opts...)
}
func (a *awsCaptureKMS) ReEncrypt(ctx context.Context, in *kms.ReEncryptInput, opts ...func(*kms.Options)) (*kms.ReEncryptOutput, error) {
	return a.inner.ReEncrypt(ctx, in, opts...)
}
func (a *awsCaptureKMS) DescribeKey(ctx context.Context, in *kms.DescribeKeyInput, opts ...func(*kms.Options)) (*kms.DescribeKeyOutput, error) {
	return a.inner.DescribeKey(ctx, in, opts...)
}
func (a *awsCaptureKMS) GetKeyRotationStatus(ctx context.Context, in *kms.GetKeyRotationStatusInput, opts ...func(*kms.Options)) (*kms.GetKeyRotationStatusOutput, error) {
	return a.inner.GetKeyRotationStatus(ctx, in, opts...)
}

func TestAWSKMS_ContextMismatchDecryptFails(t *testing.T) {
	fk := newFakeKMS(t)
	p := keyprovider.NewAWSKMSProviderWithClient(fk, "arn:aws:kms:us-east-1:0:key/test")
	ctx := context.Background()
	plain, w, err := p.GenerateDEK(ctx, keyprovider.KeyOpts{VolumeID: "vol-a", KeyRef: "arn:aws:kms:us-east-1:0:key/test"})
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}
	plain.Destroy()
	if _, err := p.Unwrap(ctx, w, "vol-b"); err == nil {
		t.Fatal("expected context-mismatch error on Decrypt")
	}
}

var _ = aws.String // keep aws import used in non-tagged builds via test
