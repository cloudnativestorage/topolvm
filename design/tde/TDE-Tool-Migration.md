# TDE Tool: Plaintext-to-Encrypted Migration

**Companion to:** `TopoLVM-TDE-Implementation-Spec.md`.
**Goal:** migrate existing unencrypted TopoLVM volumes to encrypted (and optionally between providers or ciphers), safely and resumably.
**Deliverables:** a CLI `topolvm-tde-migrate` (`cmd/topolvm-tde-migrate/`), an optional `MigrationRequest` CRD + controller, and a node-pinned privileged Job manifest generator.

---

## 1. Two strategies

### Strategy A: clone-and-copy (default, safe)

Provision a new encrypted volume, copy data from the source while it is quiesced, verify, then cut the workload over. No in-place risk; the source is untouched until the operator confirms.

Because PV/PVC bindings are immutable, the realistic Kubernetes flow is:

1. Scale down or quiesce the workload owning the source PVC (the tool refuses to copy a mounted-rw source unless `--online` with an fsfreeze is given).
2. Create a new PVC `name-enc` on an encrypted StorageClass, size >= source.
3. Run a migration Job pinned (nodeAffinity) to the node holding both LVs; it mounts source (ro) and target (rw) and copies: `rsync -aHAX --numeric-ids` for filesystem volumes, `dd` (or `cp` of the block device) for block volumes.
4. Verify: checksum compare (Section 3).
5. Cut over: for a StatefulSet, update the `volumeClaimTemplate` / repoint the app to `name-enc`; for a Deployment, swap the PVC reference. The tool prints exact kubectl steps and never deletes the source automatically.
6. After operator confirmation, optionally delete the source PVC.

### Strategy B: in-place encrypt (experimental, opt-in)

Use LUKS2 in-place encryption to add a header to the existing plaintext LV:

```
cryptsetup reencrypt --encrypt --reduce-device-size 32M --resilience checksum <device> --key-file=-
```

`--reduce-device-size` makes room for the LUKS2 header at the end of the device (the filesystem must have free space or be shrunk first), or use `--header` for a detached header to avoid shrinking. Constraints:

- Source must be **unmounted** (workload down) for the duration.
- A backup or snapshot must exist first; the tool requires `--i-have-a-backup` and ideally takes a TopoLVM snapshot of the source before starting.
- Resumable via `--resilience checksum`, but a failure mid-operation on a non-backed-up volume risks data loss. This is why it is opt-in and gated.
- After success, register the new key as an `EncryptionKey` and set `lv.Spec/Status.Encryption` so the node opens it on next mount.

Default is Strategy A. Strategy B requires `--strategy in-place --i-have-a-backup`.

## 2. CLI shape

```
topolvm-tde-migrate plan   --selector app=db --provider vault --key-ref transit/keys/topolvm
topolvm-tde-migrate run    --pvc mydata --namespace prod --strategy clone   [--online]
topolvm-tde-migrate run    --pvc mydata --namespace prod --strategy in-place --i-have-a-backup
topolvm-tde-migrate verify --pvc mydata --namespace prod
topolvm-tde-migrate status
```

- `plan` discovers unencrypted TopoLVM PVs (PV `storageClassName` provisioner == topolvm and `lv.Spec.Encryption == nil`), prints a per-volume plan (size, node, strategy, estimated time), and writes a `MigrationRequest` if the CRD mode is enabled. Dry-run by default.
- `run` executes one volume (or a selector with `--max-concurrent`, default 1 per node, small global cap).
- `verify` recomputes and compares checksums.
- `status` reads `MigrationRequest`/Job status.

## 3. Verification (mandatory before any source deletion)

- Filesystem: compare a deterministic digest, for example `find . -type f -print0 | sort -z | xargs -0 sha256sum` over both mounts, or `rsync -ni --checksum` returning no differences.
- Block: compare `sha256sum` of the whole decrypted target device against the source device (both quiesced).
- The tool stores the source and target digests in the `MigrationRequest.status` (or a local report) and refuses cutover/cleanup if they differ.

## 4. Optional MigrationRequest CRD

```yaml
apiVersion: topolvm.io/v1
kind: MigrationRequest
spec:
  selector: {matchLabels: {app: db}}
  strategy: clone            # clone | in-place
  provider: vault
  keyRef: transit/keys/topolvm
  maxConcurrentPerNode: 1
  online: false
status:
  phase: Running             # Pending|Running|AwaitingCutover|Completed|Failed
  perVolume:
    - pvc: mydata
      node: node-1
      sourceDigest: "sha256:..."
      targetDigest: "sha256:..."
      state: AwaitingCutover  # never auto-deletes source
```

The controller schedules node-pinned Jobs honoring concurrency, records digests, and parks each volume in `AwaitingCutover` until the operator confirms. It never deletes a source PVC; cleanup is a separate explicit `kubectl` step the tool prints.

## 5. Safety rules (hard)

1. Never delete or modify the source until target verification passes and the operator confirms.
2. Refuse to copy a mounted-rw source unless `--online` is set; `--online` must fsfreeze the source filesystem during the copy and thaw after.
3. In-place (Strategy B) requires `--i-have-a-backup`; the tool takes a source snapshot first when snapshots are available.
4. Per-node concurrency default 1, global cap configurable; migration is I/O heavy and must not starve running workloads.
5. Resumable: a killed Job can be re-run; clone restarts the copy (rsync resumes), in-place resumes via LUKS2 checksum resilience.
6. All key material via the provider into `SecretBuf`; the migration Job follows the same stdin/mlock/zeroize rules. No em-dashes in output.

## 6. Tests

- e2e clone (kind): provision a plaintext TopoLVM PVC, write known data, run clone migration, verify digests match, mount the encrypted target and read the data back, confirm source untouched.
- e2e in-place (kind, gated): small plaintext volume, take snapshot, `reencrypt --encrypt`, kill the Job mid-way, re-run, confirm resume and data integrity, confirm an `EncryptionKey` was registered and the volume opens on next mount.
- Verification negative test: corrupt one byte on the target before `verify` and assert cutover is refused.
- Concurrency test: two volumes on one node respect `maxConcurrentPerNode: 1`.

## 7. Acceptance checklist

- [ ] `plan` is dry-run by default and lists only unencrypted TopoLVM volumes.
- [ ] Clone migration copies, verifies by checksum, and parks at `AwaitingCutover` without touching the source.
- [ ] In-place gated behind `--strategy in-place --i-have-a-backup`, takes a pre-snapshot, resumes after interruption.
- [ ] Source never deleted automatically; cutover/cleanup steps printed, not executed.
- [ ] Online copy fsfreezes the source; offline copy refuses a mounted-rw source.
- [ ] Secret-leak grep clean across `cmd/topolvm-tde-migrate` and any controller.
