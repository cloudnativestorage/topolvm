# Transparent Data Encryption (TDE)

TopoLVM TDE encrypts every volume at rest with LUKS2 / dm-crypt, with the
data encryption key (DEK) wrapped by an external key management system
(KMS, Vault Transit, or PKCS#11/HSM). The design document is
`TopoLVM-TDE-Design.md` at the repo root; this file is the operator
runbook.

## Architecture (one sentence per layer)

```
workload  -- plaintext I/O --
filesystem (ext4/xfs)                   <- mkfs and mount target = mapper
/dev/mapper/topolvm-<short>             <- dm-crypt plaintext view
LUKS2 header + dm-crypt (master key in kernel keyring)
/dev/topolvm/<uuid>                     <- ciphertext logical volume
LVM thin/thick pool, VG, physical disk
```

The master key lives only in the kernel device-mapper context between
`luksOpen` and `luksClose`. The DEK is wrapped under the provider's KEK
and stored as ciphertext in an `EncryptionKey` CR; the KEK never leaves
the provider.

## Enabling encryption

1. Build and deploy topolvm-controller and topolvm-node with the
   `--encryption-enabled --key-provider=vault --key-provider-config=/etc/topolvm/vault.yaml`
   flags (or `--key-provider=fake` for testing).
2. Provide `cryptsetup` on the node host or in the node container image.
3. Apply an encrypted StorageClass (see `example/tde/storageclass-encrypted.yaml`).
4. Create PVCs against the encrypted StorageClass as usual.

## Operations

| Operation | Mechanism | Downtime |
|---|---|---|
| KEK rotation | EncryptionKey reconciler calls `provider.Rewrap` | none |
| Passphrase / keyslot rotation | node-side `RotateKeyslot` (luksAddKey + luksKillSlot) | none, online |
| Master-key reencryption | `ReencryptRequest` -> per-LV state=Reencrypting -> `cryptsetup reencrypt --resilience checksum` | online, I/O-bound |

## Snapshot / restore

A thin snapshot inherits the origin's LUKS2 header (it is a block-level
copy of ciphertext). The snapshot's `EncryptionKey` is a pinned copy of
the origin's at the moment of snapshot, so the snapshot is openable with
the wrapped DEK that unlocked the origin at that point. Pre-rotation
snapshots stay openable through their pinned key even after the origin
rotates.

## Threat model recap

Protects: stolen disks, raw LV backups, offline etcd, decommissioned
media. Does NOT protect: root on a node while a volume is open
(plaintext is necessarily in kernel memory), a compromised KMS/HSM
(the KEK is the root of trust).

## Secret hygiene

- Plaintext passphrases live only in `crypt.SecretBuf` (mlock'd, zeroized
  on Destroy).
- Cryptsetup receives the passphrase via stdin only (`--key-file=-`);
  the new passphrase to `luksAddKey` is fed via `/dev/fd/3` so it
  never appears in argv.
- The `EncryptionKey` CR stores only ciphertext.
- The CI secret-leak regression grep must find nothing:
  ```bash
  rg -n "passphrase|--key=|MASTER_KEY" --glob '!**/*_test.go' internal/ | rg -v "key-file=-"
  ```
