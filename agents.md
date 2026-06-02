# AGENTS.md

Notes for AI agents working on this repository. Add to this file as you discover
non-obvious things about the codebase.

## Snapshot / LogicalVolume deletion flow

### Actors

- `topolvm-node` runs the `LogicalVolumeReconciler` that reconciles the
  `LogicalVolume` CR (see `internal/controller/logicalvolume_controller.go`).
  It also creates and tears down the online-snapshot executor pods.
- `topolvm-controller` (a separate binary) only handles LV scheduling and
  capacity; the per-node LV lifecycle (including the snapshot pod) lives in
  `topolvm-node`.
- The online-snapshot executor pod runs in the `topolvm-system` namespace
  (or whatever `HOST_NAMESPACE` resolves to) and is started by
  `executor.SnapshotExecutor` in `internal/executor/backup.go`. The pod is a
  plain `corev1.Pod` (not a `Job`) with `RestartPolicy: Never`.

### Snapshot pod identity

- Name: `<operation>-<lv-name>` (lowercased), e.g. `backup-my-lv`. Built by
  `executor.BuildSnapshotPodName`.
- Namespace: `executor.GetPodNamespace()` (reads `HOST_NAMESPACE`, defaults to
  `topolvm-system`).
- Labels: `app=topolvm-snapshot`, `topolvm.io/logical-volume=<lv-name>`,
  `topolvm.io/snapshot-pod=true`. See `internal/executor/constants.go`.
- The backup pod mounts the LV via hostPath
  (`MountPath = ONLINE_SNAPSHOT_DIR/<lv-name>`). The host directory is itself
  a mount point set up by `LVMount.Mount` in `internal/mounter/mounter.go`,
  so there are two mounts sharing the device: the host bind mount and the
  pod's hostPath mount.

### Reconciliation entry points

`internal/controller/logicalvolume_controller.go`:

- `Reconcile` (L52) – reads the LV, checks nodeName filter, and dispatches:
  - `lv.DeletionTimestamp != nil` -> `handleDeletion` (L251)
  - otherwise -> `reconcile` (L88), which may call `reconcileSnapshotBackup`
    (L177) or `reconcileSnapshotRestore` (L143).
- `handleDeletion` (L251) -> `deletionWithSnapshot` (L266) or
  `deletionWithoutSnapshot` (L285).
- `deletionWithSnapshot` is taken when the snapshot has already succeeded
  (`isLVHasSnapshot` in `internal/controller/snapshot_handler.go:447`). It
  runs the snapshot-delete executor and then `removeLVIfExists`.
- `deletionWithoutSnapshot` is the path for LVs whose snapshot is still
  pending or running. **This is the path that can block indefinitely if a
  backup pod is still running.**

### Why `deletionWithoutSnapshot` used to hang

Before the fix in this repo, the flow was:

1. User deletes the `VolumeSnapshot`.
2. The CSI external-snapshotter deletes the corresponding `LogicalVolume`
   CR, which sets a `DeletionTimestamp` on it.
3. The controller goes through `handleDeletion` -> `deletionWithoutSnapshot`,
   which immediately called `removeLVIfExists` (L286) -> `lvService.RemoveLV`
   -> `lvremove -f` in `lvmd`.
4. The backup pod (`backup-<lv-name>`) is still running and holds the
   device open via its hostPath mount. The kernel reports the device as
   busy, `lvremove` fails, and the reconciler retries forever. The LV
   finalizer is never removed and the `LogicalVolume` CR sits with a
   `DeletionTimestamp` indefinitely.

### Fix: ordering teardown before `lvremove`

Implemented across two files. The deletion dispatch lives in
`internal/controller/logicalvolume_controller.go` (`handleDeletion`):

```
isLVHasSnapshot(lv)      -> deletionWithSnapshot     (snapshot already Succeeded)
not isLVHasSnapshot      -> isLVBackupCandidate
    yes                  -> deleteBackupPodAndUnMount (delete pod, then Unmount)
    no                   -> deletionWithoutSnapshot   (just lvremove + finalizer)
```

Important: `isLVBackupCandidate` is a real predicate. It calls
`snapshotHandler.buildSnapshotContextFrBackup`, which is the same function
the normal reconcile path uses to populate `shouldBackup` from the
`VolumeSnapshotClass` parameters. Calling only
`snapshotHandler.setVolumeSnapshotInfo` would leave `shouldBackup` as the
zero value (`false`), because the deletion path never runs
`buildSnapshotContextFrBackup` the way `reconcile()` does. Always go
through `buildSnapshotContextFrBackup` here so the candidate check actually
reflects the VSClass.

`deleteBackupPodAndUnMount` (in `logicalvolume_controller.go`) does:

1. **Delete the backup pod** (if it exists) and requeue. The kubelet must
   terminate the container and release the pod's hostPath mount before the
   device is no longer busy. `deleteRunningBackupPod` (in
   `snapshot_handler.go`) looks up the pod by its deterministic name and
   namespace and calls `client.Delete`.
2. **`LVMount.Unmount`** to release the host-side bind mount the
   controller set up for the backup. `Unmount` is already mostly
   idempotent (returns nil when the LV is gone or the path is not a
   mount point), so a reconciler retry after a partial teardown is safe.
3. Fall through to `deletionWithoutSnapshot` which does
   `lvService.RemoveLV` (NotFound is treated as success) and
   `removeFinalizer` so the CR can be garbage collected.

The new helpers in the executor package are exported so the controller does
not have to duplicate the pod-naming convention:

- `executor.BuildSnapshotPodName(operation, lv) string`
- `executor.GetPodNamespace() string`

### Conditions on `LogicalVolume.Status`

The controller tracks per-step state via `meta.SetStatusCondition` entries
on the LV status. The relevant types live in `api/v1/constants.go`:

- `SnapshotBackupExecutorEnsured` – the backup pod has been created.
- `SnapshotBackupExecutorCleaned` – the backup pod has been cleaned up
  (used by the normal completion path in `reconcileSnapshotBackup`).
- `LVMSnapshotCleaned` – the LVM snapshot LV has been removed after a
  successful backup.
- `SnapshotDeleteExecutorEnsured` / `SnapshotDeleteEnsured` – used by the
  snapshot-delete path in `deletionWithSnapshot`.

The new fix intentionally does not add a new condition; it reuses the
existing pod-lookup and idempotency semantics.

## Testing notes

- `internal/controller/*_test.go` is Ginkgo + envtest, requires
  `ENVTEST_KUBERNETES_VERSION` and downloaded kubebuilder assets.
- `internal/executor/common_test.go` is a small stdlib test for the new
  pure helpers (`BuildSnapshotPodName`, `GetPodNamespace`).
- The vendored controller-runtime does not include the fake client
  (`sigs.k8s.io/controller-runtime/pkg/client/fake`), so adding tests
  that need it requires updating `go.mod` and re-running `go mod vendor`.
  Be aware that `go.mod` and `vendor/modules.txt` are currently slightly
  out of sync (lots of "explicitly required ... not marked as explicit in
  vendor/modules.txt" warnings on a plain `go list`); a re-vendor changes
  many files and should be done in its own commit.
- Run lint with `make lint` (uses golangci-lint). Run unit tests with
  `go test ./...`.

## Other things worth knowing

- The mock LV service in `internal/controller/logicalvolume_controller_test.go`
  has `RemoveLV` set to `panic("unimplemented")`. To extend the Ginkgo
  tests to exercise the deletion path you will need to add a real
  implementation to the mock (or have the mock track an "exists" flag and
  return success).
- `LVMount.Unmount` is mostly idempotent (returns nil for
  `IsMountPoint: not exist` or `not a mount point`, and for `LV not found`),
  but it can still return a real error if the device is busy. That's the
  exact case the new fix is designed to avoid; the `IsMounted` probe in
  the controller gives us a second, explicit guard against retry-induced
  surprises.
- Pod name format `<operation>-<lv-name>` is also used for the restore and
  delete executor pods (`restore-...`, `delete-...`). If you ever need to
  tear those down at deletion time as well, the same `BuildSnapshotPodName`
  helper works for all three.
