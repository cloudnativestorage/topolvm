# TDE Provider: AWS KMS

**Implements:** `keyprovider.KeyProvider` (main spec Section 5.1).
**Acceptance:** passes `providertest.Run` (see `TDE-Provider-Conformance-Suite.md`) against a real KMS CMK (or LocalStack) in CI, plus the secret-leak grep.
**File:** `internal/keyprovider/awskms.go`, tests `awskms_test.go` (build tag `kms_aws`).

---

## 1. SDK and dependency

`github.com/aws/aws-sdk-go-v2/config`, `github.com/aws/aws-sdk-go-v2/service/kms`.

## 2. Operation mapping

| Interface method | KMS call | Notes |
|---|---|---|
| `GenerateDEK` | `GenerateDataKey` | `KeySpec: AES_256`, `KeyId: keyRef`, `EncryptionContext`. Returns `Plaintext` + `CiphertextBlob`. |
| `Unwrap` | `Decrypt` | `CiphertextBlob` + same `EncryptionContext`. |
| `Rewrap` | `ReEncrypt` | `SourceEncryptionContext` + `DestinationEncryptionContext` (both = the volume context); `DestinationKeyId` = current keyRef. |
| `KEKVersion` | `GetKeyRotationStatus` + key ARN | See rotation note. |

`EncryptionContext` (AAD) for every call:

```go
ec := map[string]string{"volumeID": o.VolumeID, "csi": "topolvm.io"}
```

KMS enforces that `Decrypt`/`ReEncrypt` use the identical context, giving tamper-evident binding. The conformance `ContextMismatchFails` test relies on this; `BindsContext() bool { return true }`.

## 3. Rotation semantics (important)

AWS KMS automatic key rotation keeps the same key ARN and rotates the backing key yearly; `Decrypt` transparently selects the right backing key, so old ciphertext stays valid with no rewrap. Therefore:

- `KEKVersion` returns the CMK ARN plus the rotation status / current rotation date (for example `arn...#rotated=2026-01-12`). It changes only when the operator migrates to a different CMK.
- `Rewrap` is a true operation only when the desired `keyRef` differs from the blob's `KeyRef` (CMK migration). When they match, `Rewrap` may return the blob unchanged. The encryptionkey reconciler (main spec 9.1) treats "version unchanged" as a no-op, which is correct here.

Document this so the reconciler does not loop trying to rewrap an already-current AWS blob.

## 4. Auth

IRSA: the controller and node service accounts are annotated with an IAM role; the SDK default credential chain consumes the projected web-identity token automatically. No keys in config. The IAM policy grants `kms:GenerateDataKey`, `kms:Decrypt`, `kms:ReEncrypt*`, `kms:DescribeKey`, `kms:GetKeyRotationStatus` on the specific CMK ARN, with a condition on the `volumeID`/`csi` encryption-context keys if desired. Node role omits any management actions.

## 5. Config file (`--key-provider-config`)

```yaml
provider: aws-kms
region: us-east-1
keyRef: arn:aws:kms:us-east-1:1234:key/abcd-...   # default CMK; StorageClass key-ref overrides
# no credentials here; IRSA only
```

## 6. Skeleton

```go
type awsProvider struct{ c *kms.Client; defaultKey string }

func newAWSProvider(ctx context.Context, cfgPath string) (*awsProvider, error) {
	conf := loadCfg(cfgPath)
	awsCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(conf.Region))
	if err != nil { return nil, err }
	return &awsProvider{c: kms.NewFromConfig(awsCfg), defaultKey: conf.KeyRef}, nil
}

func (p *awsProvider) GenerateDEK(ctx context.Context, o keyprovider.KeyOpts) (keyprovider.SecretBuf, keyprovider.WrappedKey, error) {
	key := orDefault(o.KeyRef, p.defaultKey)
	out, err := p.c.GenerateDataKey(ctx, &kms.GenerateDataKeyInput{
		KeyId: &key, KeySpec: types.DataKeySpecAes256, EncryptionContext: ec(o.VolumeID),
	})
	if err != nil { return nil, keyprovider.WrappedKey{}, err }
	buf := crypt.NewSecretBuf(len(out.Plaintext)); copy(buf.Bytes(), out.Plaintext); zero(out.Plaintext)
	return buf, keyprovider.WrappedKey{Ciphertext: out.CiphertextBlob, KeyRef: key, KEKVersion: key, Provider: "aws-kms"}, nil
}
// Unwrap -> Decrypt(CiphertextBlob, ec(volumeID)); copy Plaintext into SecretBuf, zero the response.
// Rewrap -> ReEncrypt(CiphertextBlob, DestinationKeyId=KeyRef, src+dst ec); return new blob.
// KEKVersion -> DescribeKey/GetKeyRotationStatus, return ARN + rotation marker.
func (p *awsProvider) Name() string { return "aws-kms" }
func (p *awsProvider) BindsContext() bool { return true }
```

`zero(out.Plaintext)` immediately after copy; never log `out`.

## 7. Tests

`awskms_test.go` (tag `kms_aws`): `providertest.Run(t, p, os.Getenv("KMS_KEY_ARN"))`. CI uses LocalStack (`kms` service) or a dedicated test CMK. Add a unit test (untagged) asserting `ec()` shape and that `GenerateDEK` zeroes the SDK plaintext (use a wrapper around the SDK in tests).

## 8. Acceptance checklist

- [ ] `providertest.Run` green against real/LocalStack KMS.
- [ ] Context mismatch fails Decrypt.
- [ ] Reconciler does not loop on already-current blobs (rotation note honored).
- [ ] Secret-leak grep clean; SDK plaintext zeroized.
