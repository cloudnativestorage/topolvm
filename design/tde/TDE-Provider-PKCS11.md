# TDE Provider: PKCS#11 / HSM

**Implements:** `keyprovider.KeyProvider`.
**Acceptance:** passes `providertest.Run` against SoftHSM2 in CI (and a real HSM where available), plus the secret-leak grep.
**File:** `internal/keyprovider/pkcs11.go`, tests `pkcs11_test.go` (build tag `kms_pkcs11`).

---

## 1. Library

`github.com/ThalesGroup/crypto11` (higher-level, session pooling) over `github.com/miekg/pkcs11`. crypto11 gives a `cipher.AEAD` / symmetric key handle backed by the token.

## 2. Model

The KEK is a non-exportable AES-256 key resident in the HSM, identified by label (and CKA_ID). The DEK (LUKS passphrase) is data that we encrypt under the KEK using AES-GCM (`CKM_AES_GCM`). The KEK never leaves the token.

| Interface method | PKCS#11 op | Notes |
|---|---|---|
| `GenerateDEK` | `crypto/rand` DEK, then AES-GCM `C_Encrypt` under KEK | store `nonce || ciphertext || tag`. |
| `Unwrap` | AES-GCM `C_Decrypt` under KEK | AAD = volumeID (GCM AAD binds the volume). |
| `Rewrap` | `C_Decrypt` under old label, `C_Encrypt` under current label | label-based versioning, Section 4. |
| `KEKVersion` | the KEK label suffix | for example `topolvm-kek-v3`. |

Using GCM AAD = volumeID gives the same tamper-evident binding as AWS/GCP, so `BindsContext() bool { return true }`.

## 3. Sessions, PIN, concurrency

- Module path (`.so`), token label, slot, and KEK label come from config. The user PIN comes from a mounted secret file (never a flag), read once into an mlock'd buffer.
- crypto11 manages a session pool; size it to the node's max concurrent mount/unmount. Log in once per process.
- All crypto ops run inside the HSM; only the DEK plaintext (briefly) and the GCM blob are in driver memory. Hold the DEK in `SecretBuf`, zeroize after `luksFormat`/`luksOpen`.

## 4. Versioning and rotation

PKCS#11 has no native key versioning. Convention: KEK objects are labeled `topolvm-kek-vN`. `KEKVersion` returns the label of the configured current KEK. `Rewrap` decrypts the blob under the label embedded in `WrappedKey.KEKVersion` (or `KeyRef`) and re-encrypts under the current label. Rotation procedure: create `topolvm-kek-v(N+1)` in the HSM, update config to point at it, let the encryptionkey reconciler rewrap all blobs, then retire the old KEK once no blob references it.

`WrappedKey.KeyRef` stores the KEK label used; `KEKVersion` stores the same label so the reconciler can detect drift against the configured current label.

## 5. Config

```yaml
provider: pkcs11
modulePath: /usr/lib/softhsm/libsofthsm2.so   # or vendor PKCS#11 .so
tokenLabel: topolvm
slot: 0
kekLabel: topolvm-kek-v1
pinFile: /etc/topolvm/hsm/pin                  # mounted secret, mode 0400
```

The node container must have the vendor PKCS#11 module available (mount from host or bake into the image) and the PIN secret mounted. Privileged is already required for cryptsetup.

## 6. Skeleton

```go
type p11Provider struct{ ctx *crypto11.Context; kek *crypto11.SecretKey; kekLabel string }

func newP11Provider(cfgPath string) (*p11Provider, error) {
	c := loadCfg(cfgPath)
	pin := readMlock(c.PinFile); defer pin.Destroy()
	ctx, err := crypto11.Configure(&crypto11.Config{
		Path: c.ModulePath, TokenLabel: c.TokenLabel, Pin: string(pin.Bytes()),
	})
	if err != nil { return nil, err }
	kek, err := ctx.FindKey(nil, []byte(c.KekLabel))   // non-exportable AES key
	if err != nil { return nil, err }
	return &p11Provider{ctx: ctx, kek: kek, kekLabel: c.KekLabel}, nil
}

func (p *p11Provider) GenerateDEK(ctx context.Context, o keyprovider.KeyOpts) (keyprovider.SecretBuf, keyprovider.WrappedKey, error) {
	dek := crypt.NewSecretBuf(32)
	if _, err := rand.Read(dek.Bytes()); err != nil { return nil, keyprovider.WrappedKey{}, err }
	aead, err := p.kek.NewGCM()            // AES-GCM via the HSM key
	if err != nil { dek.Destroy(); return nil, keyprovider.WrappedKey{}, err }
	nonce := make([]byte, aead.NonceSize()); rand.Read(nonce)
	ct := aead.Seal(nil, nonce, dek.Bytes(), []byte(o.VolumeID))  // AAD = volumeID
	blob := append(nonce, ct...)
	return dek, keyprovider.WrappedKey{Ciphertext: blob, KeyRef: p.kekLabel, KEKVersion: p.kekLabel, Provider: "pkcs11"}, nil
}
// Unwrap -> split nonce, aead.Open(ct, AAD=volumeID) -> SecretBuf.
// Rewrap -> Open under the blob's KEK label, Seal under current kekLabel.
// KEKVersion -> p.kekLabel.
func (p *p11Provider) BindsContext() bool { return true }
```

(If the HSM does not expose GCM via crypto11, fall back to `CKM_AES_KEY_WRAP_PAD` for the DEK and store the volumeID HMAC separately as in the Azure spec. Note which path the target HSM supports in `docs/tde/RECON.md`.)

## 7. Tests

`pkcs11_test.go` (tag `kms_pkcs11`): initialize SoftHSM2 in CI (`softhsm2-util --init-token`), create the KEK, run `providertest.Run`. Untagged: skip if no module. Add a test that `Rewrap` across two KEK labels (v1 -> v2) preserves plaintext.

## 8. Acceptance checklist

- [ ] `providertest.Run` green against SoftHSM2.
- [ ] GCM AAD mismatch (wrong volumeID) fails Unwrap.
- [ ] Label-based rewrap v1 -> v2 preserves plaintext; old KEK retired only after no blob references it.
- [ ] PIN read from a mounted secret into mlock'd memory, never a flag or log.
- [ ] Secret-leak grep clean.
