# Prompt: Handle PVC Deletion During Ongoing Restore from VolumeSnapshot

## Scenario

1. A user has a `VolumeSnapshot` (a snapshot of an existing PVC).
2. The user creates a new PVC with
   `spec.dataSourceRef` pointing at that `VolumeSnapshot`.
3. `topolvm-controller` (CSI plugin) creates a new `LogicalVolume` CR with
   `spec.source = <source-lv-name>` for the new PVC and sets the
   `topolvm.io/resticRestoreRequired` annotation.
4. The per-node `LogicalVolumeReconciler` reconciles the new LV, takes a
   bind mount of it on the host, and spawns a **restore pod** named
   `restore-<lv-name>` in the `topolvm-system` namespace. The container
   runs `topolvm-snapshotter restore ...`, which writes the restic snapshot
   back into the LV.
5. While the restore is still **in progress**, the user deletes the new
   PVC (and therefore the new PV, the new LV, and the restore pod is no
   longer wanted).
6. As a result:
   - The reconciler of the new LV waits until the actual LV is removed
     before removing its finalizer.
   - But the actual Linux LV cannot be removed, because the restore pod
     is still holding the device open (hostPath mount into the container)
     and the controller is also holding a host-side mount for the restore.

This is the exact mirror of the backup case in
`volumesnapshot-deletion-during-backup-prompt.md`, but on the restore
side. Today, the same hang the backup fix just resolved is still
present here.

## Current code paths (as of the backup fix)

`internal/controller/logicalvolume_controller.go`:

- `handleDeletion` (L251) dispatches:
  1. `isLVHasSnapshot(lv)` -> `deletionWithSnapshot` (succeeded
     snapshot path)
  2. `isLVBackupCandidate(...)` -> `deleteBackupPodAndUnMount` (the
     fix we just shipped)
  3. else -> `deletionWithoutSnapshot` (just `removeLVIfExists` +
     finalizer)


Err: 
```bash
{"level":"error","ts":"2026-06-02T09:31:25Z","msg":"failed to remove LV","controller":"logicalvolume","controllerGroup":"topolvm.io","controllerKind":"LogicalVolume","LogicalVolume":{"name":"pvc-f999ca1a-c5db-40fb-88ba-a8c954e492f3"},"namespace":"","name":"pvc-f999ca1a-c5db-40fb-88ba-a8c954e492f3","reconcileID":"a1f59237-fb0c-4dc6-9d74-bdaa5ba0b334","name":"pvc-f999ca1a-c5db-40fb-88ba-a8c954e492f3","uid":"ffcc494e-b31c-48ff-a24f-1f8a2f73a9af","error":"rpc error: code = Unknown desc = exit status 5: Logical volume myvg1/ffcc494e-b31c-48ff-a24f-1f8a2f73a9af contains a filesystem in use.","stacktrace":"github.com/topolvm/topolvm/internal/controller.(*LogicalVolumeReconciler).removeLVIfExists\n\t/workdir/internal/controller/logicalvolume_controller.go:355\ngithub.com/topolvm/topolvm/internal/controller.(*LogicalVolumeReconciler).deletionWithoutSnapshot\n\t/workdir/internal/controller/logicalvolume_controller.go:318\ngithub.com/topolvm/topolvm/internal/controller.(*LogicalVolumeReconciler).handleDeletion\n\t/workdir/internal/controller/logicalvolume_controller.go:278\ngithub.com/topolvm/topolvm/internal/controller.(*LogicalVolumeReconciler).Reconcile\n\t/workdir/internal/controller/logicalvolume_controller.go:81\nsigs.k8s.io/controller-runtime/pkg/internal/controller.(*Controller[...]).Reconcile\n\t/workdir/vendor/sigs.k8s.io/controller-runtime/pkg/internal/controller/controller.go:216\nsigs.k8s.io/controller-runtime/pkg/internal/controller.(*Controller[...]).reconcileHandler\n\t/workdir/vendor/sigs.k8s.io/controller-runtime/pkg/internal/controller/controller.go:461\nsigs.k8s.io/controller-runtime/pkg/internal/controller.(*Controller[...]).processNextWorkItem\n\t/workdir/vendor/sigs.k8s.io/controller-runtime/pkg/internal/controller/controller.go:421\nsigs.k8s.io/controller-runtime/pkg/internal/controller.(*Controller[...]).Start.func1.1\n\t/workdir/vendor/sigs.k8s.io/controller-runtime/pkg/internal/controller/controller.go:296"}
```

For an in-progress restore LV:

- `Status.Snapshot.Phase` is `Running` and `Status.Snapshot.SnapshotID`
  is empty, so `isLVHasSnapshot` returns `false`.
- The LV's own `VolumeSnapshotContent` does **not** exist (the new PVC
  is not itself a snapshot; the snapshot lives on the source LV), so
  `isLVBackupCandidate` -> `buildSnapshotContextFrBackup` resolves
  `vsClass = nil` and returns `false`.
- Therefore we fall into `deletionWithoutSnapshot`, which calls
  `removeLVIfExists` immediately. `lvremove -f` fails because the
  restore pod's container is still holding the device open, the
  reconciler retries forever, and the LV finalizer is never removed.

## Solution (sketch)

Mirror the backup fix on the restore side, sharing as much code as
reasonable.

### 1. Generic snapshot-pod teardown

Refactor `snapshotHandler.deleteRunningBackupPod` to take the operation
type as a parameter so the same logic can target any of the three
executor pods:

```go
// in internal/controller/snapshot_handler.go
func (h *snapshotHandler) deleteRunningSnapshotPod(
    ctx context.Context, log logr.Logger, lv *topolvmv1.LogicalVolume,
    operation topolvmv1.OperationType,
) (bool, error) {
    podName := executor.BuildSnapshotPodName(operation, lv)
    namespace := executor.GetPodNamespace()
    pod := &corev1.Pod{
        ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: namespace},
    }
    if err := h.client.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
        if apierrs.IsNotFound(err) {
            return false, nil
        }
        return false, fmt.Errorf("failed to get %s pod %s/%s: %w", operation, namespace, podName, err)
    }
    log.Info("deleting running snapshot pod before LV removal",
        "name", lv.Name, "pod", podName, "namespace", namespace, "operation", operation, "phase", pod.Status.Phase)
    if err := h.client.Delete(ctx, pod); err != nil && !apierrs.IsNotFound(err) {
        return false, fmt.Errorf("failed to delete %s pod %s/%s: %w", operation, namespace, podName, err)
    }
    return true, nil
}
```

`deleteRunningBackupPod` can either be deleted in favour of this, or
kept as a thin wrapper that passes `topolvmv1.OperationBackup`.

### 2. Generic teardown helper in the controller

Replace the current `deleteBackupPodAndUnMount` (or extend it) with a
helper that takes the operation type, deletes the pod, and unmounts:

```go
// in internal/controller/logicalvolume_controller.go
func (r *LogicalVolumeReconciler) deleteSnapshotPodAndUnMount(
    ctx context.Context, lv *topolvmv1.LogicalVolume, log logr.Logger,
    operation topolvmv1.OperationType,
) (bool, error) {
    log.Info("LV has an in-progress snapshot operation, deleting pod first",
        "name", lv.Name, "operation", operation)
    requeue, err := r.snapshot.deleteRunningSnapshotPod(ctx, log, lv, operation)
    if err != nil {
        return false, err
    }
    if requeue {
        return true, nil
    }
    if err := r.lvMount.Unmount(ctx, lv); err != nil {
        log.Error(err, "failed to unmount LV during deletion", "name", lv.Name)
        return false, err
    }
    return false, nil
}
```

### 3. New predicate `isLVRestoreCandidate`

Add a sibling to `isLVBackupCandidate` in `snapshot_handler.go`. It
must call `buildSnapshotContextFrRestore` so the `shouldRestore` field
is actually populated (the same gotcha we just fixed in the backup
predicate):

```go
func isLVRestoreCandidate(ctx context.Context, log logr.Logger, snapHandler *snapshotHandler, lv *topolvmv1.LogicalVolume) (bool, error) {
    if err := snapHandler.buildSnapshotContextFrRestore(ctx, log, lv); err != nil {
        return false, fmt.Errorf("failed to build snapshot context for restore: %w", err)
    }
    return snapHandler.shouldRestore, nil
}
```

The two predicates are mutually exclusive: `buildSnapshotContextFrBackup`
returns `false` for an LV without a `VolumeSnapshotContent` of its own
(the restore case), and `buildSnapshotContextFrRestore` returns `false`
for an LV without `Spec.Source` (the backup case).

### 4. Update `handleDeletion`

In `internal/controller/logicalvolume_controller.go`, after the
existing `isLVBackupCandidate` branch, add the restore branch:

```go
func (r *LogicalVolumeReconciler) handleDeletion(ctx context.Context, lv *topolvmv1.LogicalVolume, log logr.Logger) (ctrl.Result, error) {
    if !controllerutil.ContainsFinalizer(lv, topolvm.GetLogicalVolumeFinalizer()) {
        return ctrl.Result{}, nil
    }
    log.Info("finalizing LogicalVolume", "name", lv.Name)

    if isLVHasSnapshot(lv) {
        return r.deletionWithSnapshot(ctx, lv, log)
    }

    if yes, err := isLVBackupCandidate(ctx, log, r.snapshot, lv); err != nil {
        log.Error(err, "failed to determine if LV is a backup candidate", "name", lv.Name)
        return ctrl.Result{}, err
    } else if yes {
        requeue, err := r.deleteSnapshotPodAndUnMount(ctx, lv, log, topolvmv1.OperationBackup)
        if err != nil {
            return ctrl.Result{}, err
        }
        if requeue {
            return ctrl.Result{RequeueAfter: requeueIntervalForSimpleUpdate}, nil
        }
    } else if yes, err := isLVRestoreCandidate(ctx, log, r.snapshot, lv); err != nil {
        log.Error(err, "failed to determine if LV is a restore candidate", "name", lv.Name)
        return ctrl.Result{}, err
    } else if yes {
        requeue, err := r.deleteSnapshotPodAndUnMount(ctx, lv, log, topolvmv1.OperationRestore)
        if err != nil {
            return ctrl.Result{}, err
        }
        if requeue {
            return ctrl.Result{RequeueAfter: requeueIntervalForSimpleUpdate}, nil
        }
    }

    return r.deletionWithoutSnapshot(ctx, lv, log)
}
```

### 5. Test plan

`internal/executor/common_test.go` already covers the generic helpers
(`BuildSnapshotPodName`, `GetPodNamespace`) and will keep covering the
restore case (`BuildSnapshotPodName(Restore, lv) == "restore-<lv>"`).

Add a small unit test for `isLVRestoreCandidate`:

- "returns true when the LV has a Source LV with a Succeeded online
  snapshot"
- "returns false when the LV has no Source"
- "returns false when the source snapshot is not online"
- "returns the same value as buildSnapshotContextFrRestore populates"
  (the regression we just fixed for the backup predicate)

A full envtest scenario would: create a source snapshot LV, create a
restore-target LV with `Spec.Source` set and a `Running` status, create
a `restore-<lv>` pod, set `DeletionTimestamp` on the LV, and verify
the pod gets deleted and the LV is eventually removed. This is gated
on the same envtest setup that the existing controller tests use; the
existing `MockLVServiceClient.RemoveLV` will need to be extended to
return success instead of `panic("unimplemented")`.

## Edge cases / open questions

- **Pod still terminating**: `deleteSnapshotPodAndUnMount` returns
  `requeue=true` when the pod has a `DeletionTimestamp` but the kubelet
  has not yet finished terminating the containers. The reconciler will
  re-queue with `requeueIntervalForSimpleUpdate` (1s) until the pod is
  fully gone. This is the same pattern as the backup fix.
- **What if both predicates are true?** Mutually exclusive today
  (backup LV has no source; restore LV has no VSContent of its own),
  but the `if/else if` chain above documents the assumption. If that
  ever stops being true (e.g. a future change lets a restore target
  also be a snapshot), we'd want a single combined teardown that
  deletes every relevant pod.
- **Failure modes for `isLVRestoreCandidate`**: if
  `buildSnapshotContextFrRestore` errors (e.g. transient API error
  fetching the source LV), we return the error and requeue via the
  controller-runtime retry, same as the backup predicate.
- **No change to the bind-mount subtlety**: the current code calls
  `LVMount.Mount` (not `BindMount`) for restore, despite a comment
  suggesting bind mount. This prompt does not change that behaviour;
  it is a separate concern and `Unmount` is idempotent enough for our
  purposes.
- **`deletionWithSnapshot` and the delete pod**: a snapshot that has
  been `Succeeded` will go through `deletionWithSnapshot`, which
  creates a `delete-<lv-name>` pod via `executeSnapshotDeleteOperation`.
  The delete pod does **not** mount the LV (`delete.go:78`: `podSpec.Volumes = []corev1.Volume{}`),
  so it does not block `lvremove`. The snapshot-delete path is
  therefore not affected by this bug. No change needed there.
