# Prompt: Implement Restic Progress in Status for Restore Operation

## Context
We already have real-time progress reporting during **backup** (commit `5f270391`). The same must now be implemented for the **restore** operation.

## Goal
Mirror the backup progress pattern so that during `restic restore`, the `LogicalVolume.Status.Snapshot.Progress` field is periodically updated with restore progress (elapsed time, percent done, files processed, bytes transferred, etc.).

## Reference: What Was Done for Backup (Commit `5f270391`)

1. **`internal/backupengine/progress/`** — New package introduced:
   - `types.go`: `Progress` struct that holds `kbClient`, `wrapper` (`*restic.ResticWrapper`), `logicalVol`, and context/cancel logic.
   - `progress.go`: `Start()` spawns goroutines per backend repository that poll `wrapper.StatusSince(repo, cursor)` every 10s, and call `setBackupProgress()`.
   - `status.go`: `setBackupProgress()` translates `restic.ResticStatus` into `v1.OperationProgress` and patches `LogicalVolume.Status.Snapshot.Progress` via `kmc.PatchStatus`.

2. **`internal/backupengine/provider/restic.go`** — In `Backup()`:
   - Creates `progress.NewProgressReporter(r.client, r.wrapper, r.logiclVol)`
   - Calls `progressRptr.Start()` before `r.wrapper.RunBackup()`
   - `defer progressRptr.Stop()` after backup completes

3. **API / CRDs** — `OperationProgress` fields (`SecondsElapsed`, `PercentDone`, `TotalFiles`, `TransferDone`, `Total`) were added to `LogicalVolumeStatus.Snapshot.Progress`.

---

## Files to Modify / Create

What you need understand youself and do this

---

## Behavioral Requirements
- The progress polling interval should remain `10 * time.Second` (reuse `progressPollInterval`).
- The progress must update the same `LogicalVolume.Status.Snapshot.Progress` field that backup uses.
- When restore completes (success or failure), the progress reporter must be stopped so goroutines do not leak.
- The `OperationProgress` fields populated should include: `SecondsElapsed`, `TotalFiles`, `FilesDone`, `TransferDone`, `Total`, `PercentDone`, and `Speed` where applicable.

---

## Acceptance Criteria
- During a restore operation, `kubectl get logicalvolume <lv-name> -o yaml` shows `.status.snapshot.progress` updating periodically.
- No goroutine leaks after restore finishes.
- Existing backup progress functionality remains untouched and working.
- Code follows the existing style in the `internal/backupengine/progress/` package.
