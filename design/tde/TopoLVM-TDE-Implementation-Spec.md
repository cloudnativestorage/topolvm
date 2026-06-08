# TopoLVM TDE: Implementation Spec for Claude Code

**Companion to:** `TopoLVM-TDE-Design.md` (read it first for rationale; this doc is the build plan).
**Target repo:** `cloudnativestorage/topolvm`, branch `online-snapshot`.
**Audience:** an implementing agent (Claude Code). This doc is written to be executed top to bottom.

---

## 0. How to use this doc

Work phase by phase. Each phase is one or more PRs with explicit acceptance criteria and verification commands. Do not start a phase before the previous phase's acceptance criteria pass. After every code change run the codegen + build + unit gate in Section 11.

Hard rules for this work (also added to `CLAUDE.md` in Section 14):

1. A plaintext passphrase or master key must never be written to argv, env vars, temp files, logs, or CR status. Pass secrets to `cryptsetup` only over stdin. Hold them in `mlock`ed, zeroized buffers.
2. All crypto operations happen on the **ciphertext LV** (`/dev/topolvm/<uuid>`). The filesystem and `mkfs` happen on the **mapper device** (`/dev/mapper/<dm-name>`). Never snapshot or reencrypt the mapper device.
3. Additive, backward compatible. A volume with `encryption.enabled=false` must follow the exact existing code path with zero behavior change.
4. No em-dashes in any code, comment, doc, or commit message. Use commas, colons, or parentheses.

---

## 1. Step 0: repository reconnaissance (do this first, commit nothing)

The exact file layout must be confirmed against the branch before writing code. Run these and record the real paths in a scratch file `docs/tde/RECON.md` (gitignored or PR-excluded):

```bash
git clone -b online-snapshot https://github.com/cloudnativestorage/topolvm
cd topolvm

# CRD API types and group/version
fd -e go logicalvolume_types.go
rg -n "kubebuilder:object:root" api/

# CSI servers: confirm whether STAGE_UNSTAGE is advertised
rg -n "NodeStageVolume|NodePublishVolume|NodeUnpublishVolume|STAGE_UNSTAGE" internal/ pkg/
rg -n "func .*NodeServer" internal/ pkg/

# Controller-side CreateVolume / CreateSnapshot and LogicalVolume reconcilers
rg -n "func .*CreateVolume|func .*CreateSnapshot|func .*ControllerExpandVolume" internal/ pkg/
rg -n "LogicalVolume" internal/controller/ internal/driver/

# lvmd command wrappers (lvcreate/lvremove/lvextend/thin snapshot)
rg -n "lvcreate|lvremove|lvextend|lvcreate.*--snapshot|thin" internal/lvmd/

# mkfs / mount helpers
rg -n "mkfs|Mount|Format" internal/filesystem/ internal/ pkg/

# online snapshot additions on this fork (fsfreeze)
rg -n "FIFREEZE|fsfreeze|freeze|Freeze|snapshot" internal/ pkg/

# build, codegen, test entrypoints
rg -n "generate|manifests|envtest|csi-sanity|kind" Makefile
```

Expected layout (TopoLVM convention, confirm and correct in RECON.md):

| Concern | Expected path |
|---|---|
| CRD types | `api/v1/logicalvolume_types.go`, group `topolvm.io` |
| CSI node server | `internal/driver/node.go` (or `pkg/driver/`) |
| CSI controller server | `internal/driver/controller.go` |
| LV reconciler (node side) | `internal/controller/logicalvolume_controller.go` |
| lvmd command wrappers | `internal/lvmd/command/*.go` |
| filesystem/mount | `internal/filesystem/*.go` |
| main entrypoints | `cmd/topolvm-node/`, `cmd/topolvm-controller/`, `cmd/lvmd/` (or `cmd/hypertopolvm`) |
| Makefile targets | `make generate manifests test e2e` |

**Critical branch:** confirm whether the node server advertises `STAGE_UNSTAGE_VOLUME`.
- If **yes**: format/open in `NodeStageVolume`, close in `NodeUnstageVolume`.
- If **no** (TopoLVM historically does mkfs+mount in `NodePublishVolume`): format/open in `NodePublishVolume`, close in `NodeUnpublishVolume`.

All node-side insertion points below are written as `NodeStage/NodeUnstage` but map them to whichever pair the fork actually uses. Record the decision in RECON.md.

---

## 2. Module layout (new and modified)

```
api/v1/
  logicalvolume_types.go         # MODIFY: add Encryption to Spec and Status
  encryptionkey_types.go         # NEW CRD: EncryptionKey
  reencryptrequest_types.go      # NEW CRD: ReencryptRequest
  zz_generated.deepcopy.go       # regenerated

internal/crypt/
  crypt.go                       # NEW: cryptsetup wrapper (Manager interface + exec impl)
  crypt_test.go                  # NEW: unit tests (fake exec)
  secretbuf.go                   # NEW: mlock'd zeroizing secret buffer

internal/keyprovider/
  provider.go                    # NEW: KeyProvider interface, WrappedKey, registry
  vault.go                       # NEW: Vault Transit provider (Phase 1)
  awskms.go gcpkms.go azurekv.go # NEW: Phase 2
  pkcs11.go                      # NEW: Phase 2
  fake.go                        # NEW: in-memory provider for tests

internal/controller/
  encryptionkey_controller.go    # NEW: KEK rewrap reconciliation (Phase 3)
  reencrypt_controller.go        # NEW: master-key reencrypt orchestration (Phase 4)
  logicalvolume_controller.go    # MODIFY: freeze mapper, snapshot ciphertext LV

internal/driver/
  node.go                        # MODIFY: open/format/close, expand resize, block publish
  controller.go                  # MODIFY: key gen on CreateVolume, key pin on CreateSnapshot

cmd/topolvm-node/ cmd/topolvm-controller/
  *.go                           # MODIFY: flags + wiring (provider, crypt manager)

charts/  config/crd/             # MODIFY: ship new CRDs, RBAC, StorageClass examples
docs/tde/                        # NEW: RECON.md, runbook
```

---

## 3. Config and flags

Add to both node and controller entrypoints (cobra flags or config file, match the fork's existing pattern; TopoLVM uses a YAML config for lvmd and flags for others):

```
--encryption-enabled            (bool, default false)   gate the whole feature
--key-provider                  (string)  vault|aws-kms|gcp-kms|azure-kv|pkcs11
--key-provider-config           (path)    provider-specific config file
--cryptsetup-path               (string, default "cryptsetup")
--luks-cipher                   (string, default "aes-xts-plain64")
--luks-key-size                 (int, default 512)
--luks-pbkdf                    (string, default "argon2id")
--reencrypt-max-concurrent-per-node (int, default 1)   # node flag
--reencrypt-throughput-limit    (string, default "")    # e.g. 50MiB/s
```

Provider auth comes from the pod's service account (Vault k8s auth, IRSA, Workload Identity), not from flags. The provider config file holds only non-secret references (Vault address + role + transit key, KMS key id, PKCS#11 token label + slot).

---

## 4. CRD types (pasteable)

### 4.1 Modify `api/v1/logicalvolume_types.go`

```go
// EncryptionSpec is set by the controller at provisioning time.
type EncryptionSpec struct {
	// +optional
	Enabled  bool   `json:"enabled,omitempty"`
	Provider string `json:"provider,omitempty"` // vault|aws-kms|gcp-kms|azure-kv|pkcs11
	KeyRef   string `json:"keyRef,omitempty"`   // provider KEK identifier
	Cipher   string `json:"cipher,omitempty"`   // default aes-xts-plain64
	KeySize  int32  `json:"keySize,omitempty"`  // default 512
}

type EncryptionState string

const (
	EncryptionPending      EncryptionState = "Pending"
	EncryptionFormatted    EncryptionState = "Formatted"
	EncryptionOpened       EncryptionState = "Opened"
	EncryptionReencrypting EncryptionState = "Reencrypting"
	EncryptionError        EncryptionState = "Error"
)

type EncryptionStatus struct {
	// +optional
	State          EncryptionState `json:"state,omitempty"`
	HeaderUUID     string          `json:"headerUUID,omitempty"`
	ActiveKeyID    string          `json:"activeKeyID,omitempty"`   // EncryptionKey object name
	Keyslot        int32           `json:"keyslot,omitempty"`
	MasterKeyEpoch int32           `json:"masterKeyEpoch,omitempty"` // bumped only by a completed reencrypt
}
```

Add `Encryption *EncryptionSpec` to `LogicalVolumeSpec` and `Encryption *EncryptionStatus` to `LogicalVolumeStatus`. Keep them pointers so absence == legacy path.

### 4.2 New `api/v1/encryptionkey_types.go`

```go
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=enckey
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.spec.provider`
// +kubebuilder:printcolumn:name="KEKVersion",type=string,JSONPath=`.status.kekVersion`
type EncryptionKey struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec   EncryptionKeySpec   `json:"spec,omitempty"`
	Status EncryptionKeyStatus `json:"status,omitempty"`
}

type EncryptionKeySpec struct {
	Provider string `json:"provider"`
	KeyRef   string `json:"keyRef"`
}

type EncryptionKeyStatus struct {
	WrappedDEK    string   `json:"wrappedDEK,omitempty"`    // base64 ciphertext, never plaintext
	KEKVersion    string   `json:"kekVersion,omitempty"`
	BoundVolumeID string   `json:"boundVolumeID,omitempty"` // encryption-context binding
	Keyslot       int32    `json:"keyslot,omitempty"`
	Consumers     []string `json:"consumers,omitempty"`     // volumes/snapshots depending on this key
	RetiredAt     *metav1.Time `json:"retiredAt,omitempty"`
}
```

Finalizer constant: `topolvm.io/encryptionkey-protection`. A key with non-empty `Consumers` must not be deleted.

### 4.3 New `api/v1/reencryptrequest_types.go`

```go
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
type ReencryptRequest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec   ReencryptRequestSpec   `json:"spec,omitempty"`
	Status ReencryptRequestStatus `json:"status,omitempty"`
}

type ReencryptRequestSpec struct {
	Selector             *metav1.LabelSelector `json:"selector,omitempty"`
	Reason               string `json:"reason,omitempty"`
	NewCipher            string `json:"newCipher,omitempty"` // optional crypto-agility
	MaxConcurrentPerNode int32  `json:"maxConcurrentPerNode,omitempty"`
	ThroughputLimit      string `json:"throughputLimit,omitempty"`
}

type ReencryptRequestStatus struct {
	Phase     string `json:"phase,omitempty"` // Pending|Running|Completed|Failed
	Total     int32  `json:"total,omitempty"`
	Completed int32  `json:"completed,omitempty"`
	Failed    int32  `json:"failed,omitempty"`
}
```

After writing types: `make generate manifests` (Section 11).

---

## 5. KeyProvider interface and Vault implementation

### 5.1 `internal/keyprovider/provider.go`

```go
package keyprovider

import "context"

type WrappedKey struct {
	Ciphertext []byte
	KeyRef     string
	KEKVersion string
	Provider   string
}

// SecretBuf wraps an mlock'd byte slice; Bytes() for stdin, Destroy() zeroizes.
type SecretBuf interface {
	Bytes() []byte
	Destroy()
}

type KeyOpts struct {
	VolumeID string // becomes encryption context / Vault context
	KeyRef   string
}

type KeyProvider interface {
	GenerateDEK(ctx context.Context, o KeyOpts) (SecretBuf, WrappedKey, error)
	Unwrap(ctx context.Context, w WrappedKey, volumeID string) (SecretBuf, error)
	Rewrap(ctx context.Context, w WrappedKey) (WrappedKey, error)
	KEKVersion(ctx context.Context, keyRef string) (string, error)
	Name() string
}

// New returns a provider by name using cfgPath.
func New(name, cfgPath string) (KeyProvider, error) { /* registry switch */ }
```

### 5.2 `internal/keyprovider/vault.go` (Phase 1, recommended default)

Use `github.com/hashicorp/vault/api`. Map operations to the Transit engine:

```
GenerateDEK -> POST transit/datakey/plaintext/<key>   (returns plaintext + ciphertext)
Unwrap      -> POST transit/decrypt/<key>  with ciphertext + context=base64(volumeID)
Rewrap      -> POST transit/rewrap/<key>   with ciphertext (re-wraps to current key version)
KEKVersion  -> GET  transit/keys/<key>     read latest_version
```

- Auth: Kubernetes auth method, login with the pod SA token at `/var/run/secrets/kubernetes.io/serviceaccount/token`, cache the client token, renew before TTL.
- Always pass `context=base64(volumeID)` so a wrapped blob is cryptographically bound to its volume (Transit ciphertext context binding); decrypt fails if the blob is replayed against a different volume.
- Plaintext returned by `datakey` and `decrypt` goes straight into a `SecretBuf`; never log the response body.

### 5.3 `internal/keyprovider/fake.go`

Deterministic in-memory provider (XOR or AES with a fixed test KEK) for unit and envtest. No network. Required for CI.

---

## 6. cryptsetup wrapper

### 6.1 `internal/crypt/secretbuf.go`

```go
// lockedBuf implements keyprovider.SecretBuf with unix.Mlock and explicit zeroization.
type lockedBuf struct{ b []byte }
func NewSecretBuf(n int) *lockedBuf            // mlock'd
func (s *lockedBuf) Bytes() []byte             // for stdin only
func (s *lockedBuf) Destroy()                  // zero + munlock
```

### 6.2 `internal/crypt/crypt.go`

```go
package crypt

type FormatOpts struct {
	Cipher  string // aes-xts-plain64
	KeySize int    // 512
	PBKDF   string // argon2id
}

type Manager interface {
	IsLuks(ctx context.Context, device string) (bool, error)
	Format(ctx context.Context, device string, pass SecretBuf, o FormatOpts) error
	Open(ctx context.Context, device, name string, pass SecretBuf) (mapper string, err error)
	Close(ctx context.Context, name string) error
	Resize(ctx context.Context, name string, pass SecretBuf) error
	AddKey(ctx context.Context, device string, old, new SecretBuf) (slot int, err error)
	KillSlot(ctx context.Context, device string, slot int) error
	Reencrypt(ctx context.Context, device string, pass SecretBuf, o ReencryptOpts) error
	HeaderUUID(ctx context.Context, device string) (string, error)
}
```

Exact commands (passphrase always on stdin via `--key-file=-`, run in host namespace with the fork's existing nsenter/exec helper):

```
IsLuks:    cryptsetup isLuks <device>                         # exit 0 == luks
Format:    cryptsetup luksFormat --type luks2 --cipher <c> --key-size <n> --pbkdf <p> --batch-mode <device> --key-file=-
Open:      cryptsetup open <device> <name> --key-file=-       # -> /dev/mapper/<name>
Close:     cryptsetup close <name>
Resize:    cryptsetup resize <name> --key-file=-
AddKey:    cryptsetup luksAddKey <device> --key-file=- --new-keyfile=-   # old via stdin, new via fd; see note
KillSlot:  cryptsetup luksKillSlot <device> <slot> --key-file=-
Reencrypt: cryptsetup reencrypt <device> --resilience checksum --batch-mode --key-file=-  [--cipher <new>]
HeaderUUID:cryptsetup luksUUID <device>
```

Note for AddKey: cryptsetup takes the existing key on stdin and the new key from a separate file descriptor. Implement by writing the new key to an `O_TMPFILE` memfd (never a named path) or use `--new-keyfile=/proc/self/fd/N`. Document the chosen mechanism inline.

Mapper name convention: `topolvm-<short(volumeID)>` to stay within dm name limits and match the existing volume naming.

### 6.3 Tests `internal/crypt/crypt_test.go`

Inject a fake `exec` runner; assert exact argv (and that no passphrase appears in argv), assert stdin carries the key, assert `IsLuks` branch logic. A separate `//go:build integration` test exercises real cryptsetup on a loop device in CI.

---

## 7. Node server integration

File: `internal/driver/node.go` (map to the real STAGE vs PUBLISH pair from Section 1).

### 7.1 Dependencies

Add to `NodeServer` struct: `crypt crypt.Manager`, `keys keyprovider.KeyProvider`, `k8s client.Client` (to read `LogicalVolume` and `EncryptionKey`), `encEnabled bool`. Wire in `cmd/topolvm-node`.

### 7.2 NodeStageVolume (or NodePublishVolume) pseudocode

```
dev := "/dev/topolvm/<uuid>"                 // existing resolution from volumeContext/LV
lv := getLogicalVolume(volumeID)
if lv.Spec.Encryption == nil || !lv.Spec.Encryption.Enabled {
    // EXISTING PATH, unchanged
    return legacyStageOrPublish(...)
}

ek := getEncryptionKey(lv.Status.Encryption.ActiveKeyID)
pass := keys.Unwrap(ctx, ek.toWrappedKey(), volumeID); defer pass.Destroy()

isLuks := crypt.IsLuks(ctx, dev)
if !isLuks {
    crypt.Format(ctx, dev, pass, formatOptsFrom(lv.Spec.Encryption))
    setStatus(lv, Formatted, headerUUID=crypt.HeaderUUID(dev), keyslot=0)
}
mapper := crypt.Open(ctx, dev, dmName(volumeID), pass)  // /dev/mapper/topolvm-<id>
setStatus(lv, Opened)

if fsMode {
    if firstTime { mkfs(mapper, fstype) }     // reuse existing filesystem helper, target=mapper
    mount(mapper, stagingPath)                // reuse existing mount helper
} else { // block mode
    publishBlock(mapper, targetPath)          // bind the mapper device, not dev
}
```

### 7.3 NodeUnstageVolume (or NodeUnpublishVolume)

```
unmount(stagingPath)              // existing
if encrypted { crypt.Close(ctx, dmName(volumeID)) }   // drops master key from kernel
```

### 7.4 NodeExpandVolume

```
// after lvmd lvextend has grown the ciphertext LV (existing controller/resizer path)
if encrypted {
    pass := keys.Unwrap(...); defer pass.Destroy()
    crypt.Resize(ctx, dmName(volumeID), pass)   // grow dm-crypt mapping
}
growFilesystem(mapper)                          // existing resize2fs/xfs_growfs, target=mapper
```

Guard every encrypted branch behind `encEnabled` and the per-LV flag so the legacy path is untouched.

---

## 8. Controller server integration

File: `internal/driver/controller.go`.

### 8.1 CreateVolume

After building the `LogicalVolume` for an encrypted StorageClass:

```
if scParams["topolvm.io/encryption"] == "true" {
    buf, wrapped := keys.GenerateDEK(ctx, KeyOpts{VolumeID: volumeID, KeyRef: scParams["topolvm.io/key-ref"]})
    buf.Destroy()                               // controller never needs plaintext
    ek := newEncryptionKey(name="vk-"+short(volumeID), wrapped, boundVolumeID=volumeID)
    create(ek)
    lv.Spec.Encryption = encryptionSpecFrom(scParams)
    lv.Status.Encryption = &EncryptionStatus{State: Pending, ActiveKeyID: ek.Name}
}
create(lv)
```

The controller persists only the wrapped DEK (in the `EncryptionKey` status). The node does the actual `luksFormat` on first stage (Section 7.2), which is where the on-disk header and keyslot get created.

### 8.2 CreateSnapshot (online-snapshot path)

Pin the key (Design Section 7.2):

```
origin := getLogicalVolume(sourceVolumeID)
if origin.Spec.Encryption.Enabled {
    srcKey := getEncryptionKey(origin.Status.Encryption.ActiveKeyID)
    snapKey := copyEncryptionKey(srcKey, name="vk-"+short(snapshotID))  // same wrappedDEK + keyslot
    addConsumer(snapKey, snapshotID)
    addFinalizer(srcKey)                        // protect against rotation/deletion stranding the snapshot
}
// existing thin-snapshot creation on the CIPHERTEXT LV proceeds unchanged
```

### 8.3 Freeze targeting (online snapshot)

In `internal/controller/logicalvolume_controller.go` (or wherever the fork issues FIFREEZE): the freeze must be issued against the **mounted filesystem on the mapper device**, then the LVM thin snapshot taken on the **underlying ciphertext LV**, then thaw. If the current code freezes by mount path this is already correct; confirm the snapshot target is the ciphertext LV and not the mapper. Add a regression test.

### 8.4 DeleteVolume / DeleteSnapshot

On delete, remove the consumer from the `EncryptionKey`; delete the key only when `Consumers` is empty and it is not pinned by a snapshot (finalizer enforces this).

---

## 9. Reconcilers (Phases 3 and 4)

### 9.1 `encryptionkey_controller.go` (Phase 3: KEK rotation)

Reconcile loop: for each `EncryptionKey`, compare `status.kekVersion` to `provider.KEKVersion(keyRef)`. If stale, call `provider.Rewrap`, write the new ciphertext + version. No node interaction, no I/O. Requeue with a jittered period (for example 6h). Emit a metric `topolvm_encryptionkey_rewrap_total`.

### 9.2 `reencrypt_controller.go` (Phase 4: master-key rotation)

```
expand selector -> volume list
maintain a work queue honoring MaxConcurrentPerNode and a global cap
for each volume:
   set lv.Status.Encryption.State = Reencrypting   // node watches this
   node runs crypt.Reencrypt(dev, pass, {resilience: checksum, cipher: NewCipher, throughput})
   on success: bump MasterKeyEpoch, optionally rotate passphrase, mark progress
update ReencryptRequest.Status counters; resumable because LUKS2 stores reencrypt progress in the header
```

The node side watches `LogicalVolume` for `State=Reencrypting` and runs the long operation in a bounded worker, reporting completion via status. A crash on either side resumes: re-running `cryptsetup reencrypt` continues from the header's recorded offset.

---

## 10. Manifests, RBAC, packaging

- `config/crd/`: generated CRDs for `EncryptionKey`, `ReencryptRequest` (via `make manifests`).
- RBAC: controller SA gets full CRUD on `encryptionkeys`/`reencryptrequests` and `logicalvolumes`; node SA gets get/list/watch on `logicalvolumes`/`encryptionkeys` and update on `logicalvolumes/status` only. Node SA must not be able to delete `EncryptionKey`.
- Helm chart (`charts/`): add `encryption.*` values, provider config mounting, and example encrypted StorageClass.
- Node DaemonSet: ensure `cryptsetup` is present (add to the node image) and the container is privileged with `/dev` host access (TopoLVM node already is). Add `mountPropagation: Bidirectional` if not already set for the staging dir.
- Suppress core dumps on the node binary (`prctl(PR_SET_DUMPABLE, 0)` or `RLIMIT_CORE=0`) so secret buffers cannot leak via a crash dump.

---

## 11. Build, codegen, and test gate (run after every change)

```bash
make generate            # deepcopy
make manifests           # CRDs + RBAC
go build ./...
go vet ./...
make test                # unit + envtest; must include crypt/ and keyprovider/fake
golangci-lint run        # if configured

# integration (loop device), gated build tag:
go test -tags integration ./internal/crypt/...

# e2e on kind with a loopback VG (extend the fork's existing e2e):
make e2e                 # add: provision encrypted PVC, write, remount, expand, snapshot, restore, delete

# CSI conformance:
csi-sanity --csi.endpoint <socket>   # against the encrypted StorageClass

# secret-leak regression (must find nothing):
rg -n "passphrase|--key=|MASTER_KEY" --glob '!**/*_test.go' internal/ | rg -v "key-file=-"
```

Definition of done for a phase = all of the above green plus the phase's acceptance criteria.

---

## 12. PR plan with acceptance criteria

**PR 1: API types + codegen (no behavior).**
Add the three CRD changes, run generate/manifests. Accept: builds, CRDs install on kind, existing e2e unchanged.

**PR 2: crypt wrapper + secretbuf.**
`internal/crypt` with unit tests (fake exec) and an integration test on a loop device. Accept: unit + integration green, no passphrase in argv asserted by tests.

**PR 3: keyprovider interface + fake + Vault.**
`internal/keyprovider` with `fake` and `vault`. Accept: fake provider round-trips in unit tests; Vault provider tested against a dev Vault in CI (or skipped behind a tag) doing datakey/decrypt/rewrap with context binding.

**PR 4: controller CreateVolume key gen.**
Wire provider into `controller.go`, create `EncryptionKey`, set `lv.Spec/Status.Encryption`. Accept: creating an encrypted PVC yields an `EncryptionKey` with only ciphertext, and a `LogicalVolume` with `Encryption.Enabled=true`.

**PR 5: node format/open/close + mount on mapper.**
The Section 7 changes. Accept (e2e): encrypted PVC mounts, app writes data, pod deleted and rescheduled, data survives (proves open works), `cryptsetup status` shows the device; unencrypted PVCs behave identically to before.

**PR 6: expansion.**
`NodeExpandVolume` resize. Accept: patching PVC size grows LV, crypt mapping, and fs; data intact.

**PR 7: snapshot key pinning + freeze target.**
Section 8.2 and 8.3. Accept: snapshot under writes produces a consistent, restorable volume; snapshot has its own pinned `EncryptionKey`; restore opens with the pinned key.

**PR 8: KEK rotation reconciler (Phase 3).**
Accept: rotating the Vault transit key causes all `EncryptionKey` blobs to reconcile to the new version with no volume downtime.

**PR 9: keyslot rotation (Phase 3).**
Online `luksAddKey`/`luksKillSlot`. Accept: rotate a live volume's passphrase, volume stays mounted, old keyslot no longer opens it, a pre-rotation snapshot still opens with its pinned key.

**PR 10: reencrypt orchestration (Phase 4).**
`ReencryptRequest` + reconciler + node worker. Accept: a request reencrypts matched volumes online, `MasterKeyEpoch` bumps, killing the node mid-reencrypt resumes cleanly, retained snapshots still readable with the old master key.

**PR 11: providers (Phase 2) and options (Phase 5).**
AWS/GCP/Azure KMS, PKCS#11; dm-integrity option; migration tool. Accept: each provider passes the PR 3 round-trip suite; integrity option gated and documented.

---

## 13. Guardrails for the implementing agent

1. Do not change the unencrypted code path. Diff it: an unencrypted PVC must produce byte-identical lvmd calls and mounts as before.
2. If `STAGE_UNSTAGE` is not advertised, do not add it just for encryption; put open/close in publish/unpublish to match the fork.
3. Never `mkfs`, `mount`, `fsfreeze`, `resize2fs`, or snapshot against `/dev/topolvm/<uuid>`. Filesystem ops target `/dev/mapper/...`; LVM and crypt-header ops target the LV.
4. If a provider call fails at mount time, fail the CSI call with a retryable error and a message that names the provider and volume but contains no key material. Do not fall back to any unencrypted path.
5. Keep every secret in a `SecretBuf`; `defer buf.Destroy()` at the point of acquisition.
6. Run the secret-leak regression grep (Section 11) before every commit.
7. No em-dashes anywhere.

---

## 14. CLAUDE.md to add to the repo

```md
# TopoLVM TDE: agent conventions

- Encryption is additive. An unencrypted volume must follow the exact pre-existing code path.
- Crypto-header and LVM operations act on the ciphertext LV (/dev/topolvm/<uuid>).
  Filesystem operations act on the mapper device (/dev/mapper/...). Never cross these.
- Secrets (LUKS passphrase, master key) go to cryptsetup only via stdin (--key-file=-),
  live only in mlock'd SecretBuf, and are zeroized with defer buf.Destroy().
  Never argv, env, temp files, logs, or CR status.
- EncryptionKey objects store only ciphertext (wrapped DEK). Nodes cannot delete them.
- After any change: make generate manifests, go build ./..., make test, and the
  secret-leak grep in docs/tde. All must pass.
- No em-dashes in code, comments, docs, or commit messages.
```

---

## 15. First task for the agent

Execute Section 1 (recon), write `docs/tde/RECON.md` with the confirmed paths and the STAGE-vs-PUBLISH decision, then open PR 1 (API types + codegen). Stop and report the recon findings before writing node-side code, since every later insertion point depends on them.
