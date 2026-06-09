# Prompt: Handle VolumeSnapshot Deletion During Ongoing Backup

## Scenario

1. A user creates a `VolumeSnapshot` for a PVC.
2. A **backup pod** is spawned that runs the `restic backup` command (via
   `topolvm-snapshotter backup`).
3. While the backup is still **in progress**, the user deletes the
   `VolumeSnapshot`.
4. As a result:
   - The reconciler of the `LogicalVolume` CR waits until the actual LV is
     removed before removing its finalizer.
   - But the actual Linux LV cannot be removed because the backup pod is
     still holding the device open (it mounts the LV via hostPath, and the
     controller also holds a host-side mount for the backup).

## Solution

Order of teardown when an LV with an in-progress backup is deleted:

1. **Delete the backup pod** (`backup-<lv-name>` in the `topolvm-system`
   namespace) and requeue. The kubelet must terminate the container and
   release the hostPath mount before the device is no longer busy.
2. **Unmount the host-side mount** held by the controller
   (`internal/mounter.LVMount.Unmount`).
3. **Remove the actual Linux LV** (`lvService.RemoveLV` → `lvremove -f`).
4. **Remove the finalizer** on the `LogicalVolume` CR so it can be garbage
   collected.

The relevant code paths are:

- `internal/controller/logicalvolume_controller.go` – `deletionWithoutSnapshot`
  implements the new order (pod → unmount → `RemoveLV` → finalizer).
- `internal/controller/snapshot_handler.go` – `deleteRunningBackupPod` is the
  helper that finds and deletes the backup pod by its deterministic name.
- `internal/executor/common.go` – `BuildSnapshotPodName` and `GetPodNamespace`
  are the new exported helpers used by the controller to locate the pod.
