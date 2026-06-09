# TDE Option: dm-integrity (authenticated encryption)

This page documents the LUKS2 `--integrity hmac-sha256` option exposed by
TopoLVM TDE. It is **off by default**: only StorageClasses that set
`topolvm.io/integrity: hmac-sha256` get authenticated encryption. The trade
offs are real; read this before enabling.

## What it adds

Plain LUKS2 with AES-XTS provides confidentiality only: an attacker who can
flip ciphertext bits causes undetectable plaintext changes. dm-integrity
adds an HMAC-SHA256 tag per sector. Tampered sectors fail authentication
and reads of corrupted sectors fail; the volume cannot be silently
modified.

## What it costs

1. **Throughput.** Every write is journaled to the integrity area then
   applied. Expect roughly half the throughput of integrity-free LUKS on
   the same hardware, with additional latency on small random writes.
   Always benchmark before enabling.
2. **Capacity.** Integrity tags take a few percent of raw device capacity.
   `PersistentVolume.spec.capacity` and `.status.capacity` reflect the
   post-integrity usable size, not the raw LV size.
3. **Format cost.** The integrity area must be initialized. A full wipe is
   slow on large devices. `topolvm.io/integrity-no-wipe: "true"` skips
   that, but reads of never-written sectors fail authentication until they
   have been written (mkfs initializes most of the device).
4. **Resize.** Growing an integrity device requires reinitializing the new
   integrity region. The node code calls `cryptsetup resize` after
   `lvextend`; on cryptsetup versions where integrity resize is
   unsupported, the node returns a clear error rather than corrupting the
   device.
5. **Reencrypt.** LUKS2 online reencrypt on integrity-enabled devices is
   version-dependent. The ReencryptRequest worker refuses to operate on a
   volume whose `spec.encryption.integrity != ""` and points the operator
   at clone-and-migrate.
6. **Snapshots.** LVM thin snapshots copy ciphertext + integrity metadata
   together, so the snapshot is internally consistent. The snapshot is
   the larger (integrity-inclusive) size.

## Enabling

Set on the StorageClass at provision time. Integrity cannot be turned on
or off after the LV is formatted without a full rewrite.

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: topolvm-encrypted-integrity
provisioner: topolvm.io
parameters:
  topolvm.io/device-class: "ssd"
  topolvm.io/encryption: "true"
  topolvm.io/key-provider: "vault"
  topolvm.io/key-ref: "transit/keys/topolvm"
  topolvm.io/integrity: "hmac-sha256"
  # Do not set integrity-no-wipe unless you understand the read-before-write caveat.
  # topolvm.io/integrity-no-wipe: "true"
  csi.storage.k8s.io/fstype: "ext4"
allowVolumeExpansion: true
volumeBindingMode: WaitForFirstConsumer
reclaimPolicy: Delete
```

## Failure modes the implementation enforces

| Scenario | Behavior |
|---|---|
| Node lacks cryptsetup integrity support | First publish fails with an actionable error; no silent fallback. |
| On-disk header integrity profile disagrees with StorageClass | Open is refused before mount. |
| Operator submits a `ReencryptRequest` matching an integrity volume | Worker marks the volume `Error` with a message pointing at clone-and-migrate. |
| Sector tampering on the underlying LV | Read fails (the goal of the option). |

## When to use it

- Compliance regimes that require authenticated encryption on data at rest.
- Untrusted storage substrates where ciphertext tampering is a credible
  attacker capability and confidentiality alone is insufficient.

Otherwise leave it off and accept LUKS-XTS confidentiality.
