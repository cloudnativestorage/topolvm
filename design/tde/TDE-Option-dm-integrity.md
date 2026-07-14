# TDE Option: dm-integrity (authenticated encryption)

**Companion to:** `TopoLVM-TDE-Implementation-Spec.md` (Sections 4.1, 6.2, 7, 9).
**Goal:** offer authenticated encryption (confidentiality + integrity) via LUKS2 with dm-integrity, gated off by default, with the constraints documented so an operator opts in knowingly.
**Files:** `internal/crypt/crypt.go` (FormatOpts + Format), `api/v1/logicalvolume_types.go` (EncryptionSpec), StorageClass parameter handling, docs.

---

## 1. What it adds and what it costs

Plain LUKS2 (AES-XTS) gives confidentiality only: an attacker who can flip ciphertext bits causes undetectable plaintext changes. dm-integrity adds a per-sector authentication tag (HMAC-SHA256 by default with `aes-xts` + `--integrity`), turning it into authenticated encryption (AEAD-like), so tampering is detected and reads of corrupted sectors fail.

Costs and constraints, all of which the spec must surface:

1. **Write amplification:** every write updates a separate integrity journal, then the data. Roughly halves write throughput and adds latency. Benchmark before enabling.
2. **Capacity overhead:** integrity tags consume a few percent of the device; usable size is smaller than the LV.
3. **Format cost:** the integrity area must be initialized. A full wipe is slow on large volumes; `--integrity-no-wipe` skips it but then reads of never-written sectors fail authentication until written.
4. **Resize:** growing must reinitialize the new integrity region. `cryptsetup resize` handles the crypt mapping, but the integrity device geometry changes; verify on the target cryptsetup version.
5. **Reencrypt:** LUKS2 online reencrypt support for integrity-enabled devices is limited and version dependent. For integrity volumes, prefer clone-and-migrate (the migration tool) over in-place reencrypt for cipher changes. Gate `ReencryptRequest` to refuse integrity volumes unless the node's cryptsetup reports support.
6. **Snapshots:** the LVM thin snapshot still copies ciphertext + integrity metadata together, so it stays self-consistent, but the snapshot is the larger (integrity-inclusive) size. Restore works; verify size accounting.

## 2. Gating

- StorageClass parameter `topolvm.io/integrity: none|hmac-sha256` (default `none`). Set only at provision time; it is part of `luksFormat` and cannot be added to an existing volume without a full reencrypt/clone.
- `EncryptionSpec.Integrity string` carries it to the node.
- Refuse mismatches: if a volume was formatted without integrity, the node must not try to open it as integrity, and vice versa. `cryptsetup` reports the header's integrity profile; read it and fail clearly on mismatch.
- A cluster guard: the node checks `cryptsetup --version` and the kernel `dm-integrity` module at startup; if integrity is requested but unsupported, fail provisioning of integrity StorageClasses with an actionable error (do not silently fall back to no integrity).

## 3. crypt.Manager changes

```go
type FormatOpts struct {
	Cipher    string
	KeySize   int
	PBKDF     string
	Integrity string // "" | "hmac-sha256"
	NoWipe    bool   // default false; true skips integrity wipe (faster format, see Section 1.3)
}
```

Format command when integrity is set:

```
cryptsetup luksFormat --type luks2 \
  --cipher aes-xts-plain64 --key-size 512 --pbkdf argon2id \
  --integrity hmac-sha256 [--integrity-no-wipe] --batch-mode <device> --key-file=-
```

Open is unchanged (`cryptsetup open` activates the integrity layer automatically for an integrity-formatted LUKS2 header). `HeaderProfile(device)` helper returns `{cipher, integrity}` for the mismatch guard.

Default `NoWipe=false` (correct, slower). Expose `topolvm.io/integrity-no-wipe: "true"` for operators who accept the unwritten-sector caveat in exchange for fast formatting; document that mkfs immediately after format initializes most of the device.

## 4. Node flow deltas

- `NodeStageVolume`: pass `Integrity` from `lv.Spec.Encryption` into `FormatOpts`. After open, run the header-profile mismatch guard.
- `NodeExpandVolume`: after `cryptsetup resize`, verify integrity geometry; on the cryptsetup versions where integrity resize is unsupported, return a clear "not supported, use clone-migrate" error rather than corrupting the device.
- Reencrypt worker: refuse integrity volumes unless supported (Section 1.5).

## 5. Tests

- Integration (loop device, tag `integrity`): format with `--integrity hmac-sha256`, open, mkfs, write, close, reopen, read back (proves tags validate). Then flip a ciphertext byte on the underlying device and assert the read fails (integrity catches tampering).
- Size accounting test: assert usable size < LV size and that the PVC reports the post-integrity capacity.
- Guard test: request integrity on a node without support -> provisioning fails with the actionable error; open an integrity volume as non-integrity -> fails the mismatch guard.
- Benchmark (informational, not gating): fio write throughput integrity vs no-integrity, recorded in `docs/tde/benchmarks.md`.

## 6. Documentation deliverable

Add `docs/tde/integrity.md` covering: when to use it (regulatory integrity requirements, untrusted-storage threat models), the throughput/capacity costs with measured numbers, the reencrypt limitation and clone-migrate workaround, and the no-wipe tradeoff. The StorageClass example must carry a comment that integrity cannot be toggled after provisioning.

## 7. Acceptance checklist

- [ ] Integrity off by default; only `topolvm.io/integrity: hmac-sha256` enables it.
- [ ] Tamper test: corrupting a ciphertext sector makes the read fail.
- [ ] Mismatch guard prevents opening a volume with the wrong integrity profile.
- [ ] Unsupported-node guard fails provisioning with an actionable message (no silent fallback).
- [ ] Reencrypt refuses integrity volumes where unsupported and points to clone-migrate.
- [ ] `docs/tde/integrity.md` and a benchmark recorded.
