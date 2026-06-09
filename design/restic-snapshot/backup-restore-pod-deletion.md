# Prompt: Handle External Deletion of the Backup / Restore Pod

## Scenario

1. A user creates a `VolumeSnapshot` for a PVC. The `LogicalVolumeReconciler`
   creates the snapshot LV, mounts it on the host, and spawns the backup
   pod `backup-<lv-name>` (or, for restore, `restore-<lv-name>`).
2. The pod sets `Status.SnapshotBackupExecutorEnsured = True` and
   `Status.Snapshot.Phase = Running`, then starts running
   `topolvm-snapshotter backup` (or `restore`).
3. **While the operation is in progress, somebody (or something —
   `kubectl delete pod`, a cluster autoscaler draining a node, an
   admission webhook, the OOM killer, the kubelet itself, a misconfigured
   `PodDisruptionBudget`, a node reboot, etc.) deletes the executor pod.**
4. Because the pod was deleted externally, `topolvm-snapshotter` never
   gets to call its `setStatusSuccess` / `setStatusFailed` update, so the
   LV status is never moved out of `Running`.

## Current behavior (as of the in-flight delete fixes)

- `Reconcile` is not retriggered by the pod's deletion (no `Watches` on
  `corev1.Pod` in `LogicalVolumeReconciler.SetupWithManager`,
  `internal/controller/logicalvolume_controller.go:354`).
- The next reconcile that does fire (e.g. periodic re-list, or because
  something else changes) walks into `reconcileSnapshotBackup`
  (L177) / `reconcileSnapshotRestore` (L143) and takes the
  "Snapshot Backup/Restore Executor triggered previously, waiting for
  completion" branch (L210-213 / L156-159), which is a no-op return.
- `Status.Snapshot.Phase` stays `Running` forever. The
  `SnapshotBackupExecutorEnsured` / `SnapshotRestoreExecutorEnsured`
  condition is still `True`. The user has no signal that anything is
  wrong; the only way to find out is to read the controller logs and
  notice the pod is missing.

The pod is gone but the LV still believes the operation is in flight, so
none of the existing cleanup paths ever run. The next time the user
deletes the `VolumeSnapshot` / PVC, the new "in-progress operation"
teardown we just added will of course handle the missing pod correctly
(`deleteRunningSnapshotPod` returns `false, nil` on `NotFound`), but
that is only triggered by the *outer* deletion. The LV itself never
recovers from the external pod deletion on its own.

## Concretely: what the user sees

- `kubectl get pods -n topolvm-system` does not show the backup/restore
  pod.
- `kubectl get logicalvolume ... -o yaml` shows
  `status.snapshot.phase: Running` indefinitely.
- `topolvm-snapshotter` was the only thing that knew whether the
  operation had reached `Succeeded` or `Failed`; with the pod gone,
  that information is lost.
- A subsequent delete of the `VolumeSnapshot` / PVC works, because the
  teardown helpers see the pod is gone and skip the delete. But the
  `Succeeded` result the user expected from the in-flight backup is
  gone too.

## Solution

Add a new `SnapshotPodReconciler` that watches the executor pods. It
is a dedicated controller with a single responsibility: detect "the
executor pod is no longer doing useful work" and reflect that into
the LV's status. The existing `LogicalVolumeReconciler` is not
changed in any structural way — it just sees an LV in `Failed` state
and runs the existing cleanup branch.

**Why a new controller and not a `Watches` on the existing one.**
Single responsibility. The LV reconciler's job is to drive an LV from
creation to deletion; mixing pod-lifecycle event handling into it would
couple two concerns. A separate `SnapshotPodReconciler` also makes
unit testing easier (one watcher, one predicate, one reconcile path)
and leaves a natural home for any future pod-level reactions
(metrics, audit logs, alerting, etc.) that don't belong on the LV
reconciler.

**Why we still need the probe inside the new controller.** The watch
gives us *timely* delivery of the events we care about (deletion,
phase change to Failed), but it does not by itself make the reconcile
do anything different — the reconcile has to actively look at the pod
and decide whether the executor is still there. The probe is the
read-side counterpart to the watch: the watch is the trigger, the
probe is the decision.

**Why we do not poll periodically.** Restic backups over a multi-TB
PVC can run for hours, and a healthy backup is by far the common
case. A periodic requeue would mean a `client.Get` on the executor pod
for every in-flight backup on every tick, even when nothing has
changed — wasted work on the controller, wasted API-server load, and
the risk of a transient API blip racing with a long-running backup.
The event-driven design (watch fires, reconcile probes, mark Failed)
costs zero API calls during a healthy long-running backup and reacts
within seconds when something actually goes wrong.

### 1. New file: `internal/controller/snapshot_pod_controller.go`

```go
package controller

import (
    "context"
    "fmt"

    "github.com/go-logr/logr"
    "github.com/topolvm/topolvm"
    topolvmv1 "github.com/topolvm/topolvm/api/v1"
    "github.com/topolvm/topolvm/internal/executor"
    corev1 "k8s.io/api/core/v1"
    apierrs "k8s.io/apimachinery/pkg/api/errors"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/apimachinery/pkg/types"
    ctrl "sigs.k8s.io/controller-runtime"
    "sigs.k8s.io/controller-runtime/pkg/builder"
    "sigs.k8s.io/controller-runtime/pkg/client"
    "sigs.k8s.io/controller-runtime/pkg/event"
    "sigs.k8s.io/controller-runtime/pkg/handler"
    "sigs.k8s.io/controller-runtime/pkg/log"
    "sigs.k8s.io/controller-runtime/pkg/predicate"
)

// SnapshotPodReconciler watches the snapshot executor pods (backup, restore,
// delete) and reflects "the executor is no longer doing useful work" into the
// owning LogicalVolume's status. It is intentionally narrow: it only flips an
// in-flight snapshot operation to Failed with a clear error code; the
// LogicalVolumeReconciler picks up the new status and runs the normal cleanup
// path.
type SnapshotPodReconciler struct {
    client client.Client
}

func NewSnapshotPodReconciler(c client.Client) *SnapshotPodReconciler {
    return &SnapshotPodReconciler{client: c}
}

// Reconcile is invoked with the pod's NamespacedName on every pod event that
// passes the watch predicate. We look up the owning LV, decide whether the
// pod is "gone" (deleted, or still in the API but its phase is Failed), and
// if so and the LV is still in the corresponding Running state, mark the
// operation Failed.
func (r *SnapshotPodReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    lg := log.FromContext(ctx)

    pod := &corev1.Pod{}
    if err := r.client.Get(ctx, req.NamespacedName, pod); err != nil {
        if apierrs.IsNotFound(err) {
            // Pod is gone. We still want to mark the LV failed, so we
            // derive the LV name from the pod's *intended* name (which
            // we know from the request) and proceed.
            pod = nil
        } else {
            return ctrl.Result{}, fmt.Errorf("failed to get executor pod %s: %w", req.NamespacedName, err)
        }
    }

    var lvName string
    var operation topolvmv1.OperationType
    if pod != nil {
        lvName = pod.Labels[executor.LabelLogicalVolumeKey]
        operation = topolvmv1.OperationType(pod.Labels["topolvm.io/snapshot-operation"])
        if lvName == "" || operation == "" {
            // Pod is not one of ours (shouldn't happen, predicate filters them)
            return ctrl.Result{}, nil
        }
    } else {
        // Pod is gone — parse the LV name and operation out of the
        // pod name. The naming convention is "<operation>-<lv-name>".
        op, name, ok := splitSnapshotPodName(req.Name)
        if !ok {
            return ctrl.Result{}, nil
        }
        lvName, operation = name, op
    }

    if !isExecutorPodMissingOrFailed(pod) {
        return ctrl.Result{}, nil
    }

    lv := &topolvmv1.LogicalVolume{}
    if err := r.client.Get(ctx, types.NamespacedName{Name: lvName}, lv); err != nil {
        if apierrs.IsNotFound(err) {
            return ctrl.Result{}, nil
        }
        return ctrl.Result{}, fmt.Errorf("failed to get LogicalVolume %s: %w", lvName, err)
    }

    // Only act if the LV still believes the operation is in flight.
    if lv.Status.Snapshot == nil || lv.Status.Snapshot.Operation != operation {
        return ctrl.Result{}, nil
    }
    if isSnapshotOperationComplete(lv) {
        return ctrl.Result{}, nil
    }

    var reason string
    if pod == nil {
        reason = fmt.Sprintf("%s executor pod was deleted before the operation completed", operation)
    } else {
        reason = fmt.Sprintf("%s executor pod reached PodFailed before the operation completed", operation)
    }
    lg.Info("marking snapshot operation as failed because the executor pod is gone",
        "lv", lv.Name, "operation", operation, "pod", req.NamespacedName)

    if err := failSnapshotOperation(ctx, r.client, lv, operation, reason); err != nil {
        return ctrl.Result{}, err
    }
    return ctrl.Result{}, nil
}

// SetupWithManager registers the new controller. It only watches
// corev1.Pod filtered by the snapshot labels, so the controller-runtime
// cache and the watch traffic stay as small as the actual surface area.
func (r *SnapshotPodReconciler) SetupWithManager(mgr ctrl.Manager) error {
    return ctrl.NewControllerManagedBy(mgr).
        Named("snapshotpod").
        For(&corev1.Pod{}).
        WithEventFilter(snapshotPodPredicate()).
        Watches(
            // Re-enqueue the owning LV when we update its status, so the
            // LV reconciler runs the cleanup path.
            &topolvmv1.LogicalVolume{},
            handler.EnqueueRequestForObject{},
            builder.WithPredicates(predicate.Funcs{
                UpdateFunc: func(e event.UpdateEvent) bool {
                    // We only care about status updates triggered by us.
                    oldLV, oldOK := e.ObjectOld.(*topolvmv1.LogicalVolume)
                    newLV, newOK := e.ObjectNew.(*topolvmv1.LogicalVolume)
                    if !oldOK || !newOK {
                        return false
                    }
                    return oldLV.ResourceVersion != newLV.ResourceVersion &&
                        phaseChangedToFailed(oldLV, newLV)
                },
                CreateFunc:  func(e event.CreateEvent) bool { return false },
                DeleteFunc:  func(e event.DeleteEvent) bool { return false },
                GenericFunc: func(e event.GenericEvent) bool { return false },
            }),
        ).
        Complete(r)
}
```

The wiring lives in `pkg/controller/logicalvolume_controller.go` (the
file that already constructs the `LogicalVolumeReconciler` for the
`topolvm-node` binary):

```go
// NewSnapshotPodReconcilerWithManager is a thin wrapper called from
// cmd/topolvm-node/app/run.go alongside the LV reconciler. The two
// reconcilers are independent — they share the controller-runtime
// manager and the cache, but nothing else.
func SetupSnapshotPodReconciler(mgr ctrl.Manager) error {
    return NewSnapshotPodReconciler(mgr.GetClient()).SetupWithManager(mgr)
}
```

And the predicate (lives next to the new controller):

```go
// snapshotPodPredicate returns a predicate that only fires for the events
// we actually care about on snapshot executor pods. Pod status updates fire
// frequently (kubelet pings the API server every few seconds), so we ignore
// the steady stream of Running/ContainerStatus updates and only enqueue
// when something terminal happened.
func snapshotPodPredicate() predicate.Predicate {
    return predicate.Funcs{
        CreateFunc: func(e event.CreateEvent) bool { return isSnapshotPod(e.Object) },
        UpdateFunc: func(e event.UpdateEvent) bool {
            if !isSnapshotPod(e.ObjectNew) {
                return false
            }
            oldPod, oldOK := e.ObjectOld.(*corev1.Pod)
            newPod, newOK := e.ObjectNew.(*corev1.Pod)
            if !oldOK || !newOK {
                return false
            }
            if oldPod.Status.Phase != newPod.Status.Phase &&
                newPod.Status.Phase == corev1.PodFailed {
                return true
            }
            if oldPod.DeletionTimestamp == nil && newPod.DeletionTimestamp != nil {
                return true
            }
            return false
        },
        DeleteFunc:  func(e event.DeleteEvent) bool { return isSnapshotPod(e.Object) },
        GenericFunc: func(e event.GenericEvent) bool { return false },
    }
}

func isSnapshotPod(obj client.Object) bool {
    return obj.GetLabels()[executor.LabelSnapshotPodKey] == "true"
}

func isExecutorPodMissingOrFailed(pod *corev1.Pod) bool {
    if pod == nil {
        return true
    }
    return pod.Status.Phase == corev1.PodFailed
}

func phaseChangedToFailed(oldLV, newLV *topolvmv1.LogicalVolume) bool {
    oldPhase := topolvmv1.OperationPhase("")
    newPhase := topolvmv1.OperationPhase("")
    if oldLV.Status.Snapshot != nil {
        oldPhase = oldLV.Status.Snapshot.Phase
    }
    if newLV.Status.Snapshot != nil {
        newPhase = newLV.Status.Snapshot.Phase
    }
    return oldPhase != topolvmv1.OperationPhaseFailed && newPhase == topolvmv1.OperationPhaseFailed
}

// splitSnapshotPodName parses "<operation>-<lv-name>" back into its parts.
// Returns false for anything that doesn't match the convention.
func splitSnapshotPodName(podName string) (topolvmv1.OperationType, string, bool) {
    for _, op := range []topolvmv1.OperationType{topolvmv1.OperationBackup, topolvmv1.OperationRestore, topolvmv1.OperationDelete} {
        prefix := strings.ToLower(string(op)) + "-"
        if strings.HasPrefix(podName, prefix) {
            return op, podName[len(prefix):], true
        }
    }
    return "", "", false
}
```

> Note: the `topolvm.io/snapshot-operation` label is not set today. The
> executor's `buildLabels` would need to add it (one line, same place
> where `topolvm.io/logical-volume` is added — see
> `internal/executor/common.go:35-43`). Alternatively the
> `SnapshotPodReconciler` can map the pod's "operation" by inspecting
> the LV's `Status.Snapshot.Operation` after fetching the LV — slightly
> more work per event but no new label needed. Pick one consistently;
> the prompt's sketch assumes the label.

### 2. Promote `updateSnapshotOperationStatus` and add `failSnapshotOperation`

Today `updateSnapshotOperationStatus` is a method on `*snapshotHandler`
in `internal/controller/snapshot_handler.go`. The new
`SnapshotPodReconciler` does not have a `snapshotHandler`, so the
helper needs to take the client directly. Two options:

- Promote it to a free function
  (`updateSnapshotOperationStatus(ctx, client, lv, operation, phase, msg, snapErr)`)
  and update the two existing callers in `snapshot_handler.go`.
- Or add a thin `*snapshotHandler`-free wrapper for the new caller.

The first option is cleaner; do that as part of the same change. With
that in place, add:

```go
// failSnapshotOperation marks the in-flight snapshot operation as Failed
// with a clear error code so the LV reconciler can run the normal cleanup
// path. Shared between SnapshotPodReconciler (external pod deletion /
// crash) and any future caller that needs to mark the operation failed.
func failSnapshotOperation(ctx context.Context, c client.Client, lv *topolvmv1.LogicalVolume, operation topolvmv1.OperationType, message string) error {
    snapErr := &topolvmv1.SnapshotError{
        Code:    "SnapshotExecutorPodMissing",
        Message: message,
    }
    return updateSnapshotOperationStatus(ctx, c, lv, operation, topolvmv1.OperationPhaseFailed, message, snapErr)
}
```

### 3. Wire the new controller in `cmd/topolvm-node/app/run.go`

Right next to the existing `SetupLogicalVolumeReconcilerWithServices`
call (currently around line 124 of `run.go`):

```go
if err := controller.SetupSnapshotPodReconciler(mgr); err != nil {
    setupLog.Error(err, "unable to create controller", "controller", "SnapshotPod")
}
```

The two reconcilers share the controller-runtime manager and cache; no
extra setup is required.

### 4. No changes to `LogicalVolumeReconciler`

The whole point of the new controller is to leave the LV reconciler
alone. The new flow is:

1. Pod is deleted or transitions to `PodFailed` →
   `SnapshotPodReconciler.Reconcile` runs.
2. It calls `failSnapshotOperation` which writes
   `lv.Status.Snapshot.Phase = Failed` and sets
   `Status.Snapshot.Error.Code = SnapshotExecutorPodMissing`.
3. The LV's own watch (in `LogicalVolumeReconciler`) sees the status
   update and re-reconciles. The `isSnapshotOperationComplete` branch
   in `reconcileSnapshotBackup` / `reconcileSnapshotRestore` is now
   true, so the cleaner runs:
   - `executeCleanerOperation` unmounts (idempotent — pod is already
     gone) and best-effort deletes the pod (no-op — pod is already
     gone).
   - For backup with `Succeeded`-only LVM-snapshot cleanup, that
     branch is skipped because `Phase == Failed`, not `Succeeded`.
4. The LV sits in `Failed` state until the user acts (delete the
   `VolumeSnapshot`, or trigger a new backup/restore by recreating the
   source).

## Edge cases / open questions

- **Pod deleted during the legitimate cleanup path.** When the backup
  finishes successfully, the snapshotter sets `Phase = Succeeded` and
  the LV controller later calls `executeCleanerOperation` which
  deletes the pod. By the time the pod is deleted the LV is already
  in `Succeeded` (or `Failed`), so `failSnapshotOperation`'s
  `isSnapshotOperationComplete` guard short-circuits and we don't
  double-mark. No false positive.
- **Pod deleted while the LV is being deleted.** `handleDeletion` is
  driven by the LV's own `DeletionTimestamp`; the executor pod being
  gone in the meantime is irrelevant — the dispatcher just goes
  through `deletionWithSnapshot` / `deletionWithoutSnapshot` /
  `deleteSnapshotPodAndUnMount` and the pod is a no-op. The pod
  controller's status update lands during this window, but the LV
  controller is in the deletion path and the new `Phase = Failed`
  status is overwritten by the finalizer removal.
- **Transient API errors when probing the pod.** `client.Get` returning
  a non-NotFound error inside `Reconcile` is propagated as an error
  and the controller-runtime workqueue retries with backoff. The
  reconcile is not idempotent in the strictest sense (a successful
  retry could double-mark `Failed` after the snapshotter), but
  `meta.SetStatusCondition` is idempotent and `updateSnapshotOperationStatus`
  reads the latest version before writing, so the worst case is a
  benign no-op.
- **Status update races with the snapshotter's status update.** If
  `topolvm-snapshotter` was racing to write `Succeeded` at the same
  moment the pod is killed, we might write `Failed` after a `Succeeded`
  was already persisted. The resulting Phase will be `Failed` (last
  write wins), which is the safer outcome. The snapshot is still in
  the restic repository regardless; the user can read the snapshot
  status to know whether the data made it.
- **What if the pod was deleted *after* the snapshotter wrote
  `Succeeded`?** The pod controller sees the pod event, calls
  `failSnapshotOperation`, but the `isSnapshotOperationComplete`
  guard inside the helper sees `Phase = Succeeded` already and
  short-circuits. The cleaner runs as part of the existing
  completion flow. No new code path is needed.
- **Should we also watch the snapshot-delete pod?** The delete pod
  does not mount the LV (`delete.go:78`,
  `podSpec.Volumes = []corev1.Volume{}`), so an external deletion
  cannot cause the same hang. Not in scope for this prompt.
- **What if the pod is stuck in `PodRunning` but the container is
  deadlocked?** We can't tell from the pod's `Status.Phase` alone, and
  per the "no polling" rule we won't be checking liveness timers. This
  is a known limitation; the snapshotter would need to add a heartbeat
  to its own status updates (e.g. bump a `LastUpdated` timestamp on
  `Status.Snapshot`) for the controller to detect a frozen in-flight
  operation. Out of scope for this prompt, but worth noting as a
  follow-up.
- **Why we never poll.** A multi-TB PVC backed up by restic can run
  for hours. A periodic requeue would mean a `client.Get` on the
  executor pod for every in-flight backup on every tick, even when
  nothing has changed — wasted work on the controller and the API
  server, and a source of false positives if a long-running backup
  happens to coincide with a transient API blip. The watch is the
  trigger, `isExecutorPodMissingOrFailed` is the decision, and the
  cost during a healthy long backup is exactly zero API calls.

## Test plan

Pure unit tests (no client needed) for the new helpers in
`internal/controller/snapshot_pod_controller_test.go`:

- `isSnapshotPod` returns `true` for an object with
  `topolvm.io/snapshot-pod=true` and `false` otherwise.
- `snapshotPodPredicate` returns `false` for `Running -> Running`
  updates and `true` for `Running -> Failed`, `Running -> deletion
  started`, and `Create`/`Delete` events.
- `splitSnapshotPodName` parses `backup-my-lv` correctly and returns
  `false` for `random-name`.
- `isExecutorPodMissingOrFailed` returns `true` for `nil`, `true` for
  `PodFailed`, `false` for `Running`/`Pending`/`Succeeded`.
- `phaseChangedToFailed` returns `true` only when the new phase is
  `Failed` and the old phase was not.

Envtest (gated on the existing controller test setup + the fake-client
work noted in `agents.md`):

- Start a backup or restore (the LV is in `Running`), then
  `k8sClient.Delete` the executor pod. Assert that the LV's
  `Status.Snapshot.Phase` flips to `Failed` and
  `Status.Snapshot.Error.Code = "SnapshotExecutorPodMissing"` within
  the controller's reconcile window. Then assert that the LV
  controller's existing cleanup path runs (unmount, finalizer
  removal) without us having to add anything to the LV reconciler.
- Same scenario but instead of deleting the pod, update its
  `Status.Phase` to `PodFailed` (e.g. simulate an OOMKilled
  container). Same assertions should hold.
- Negative case: the LV is in `Succeeded` (backup finished cleanly),
  then the pod is deleted by the cleaner. The pod controller should
  see the event, but `isSnapshotOperationComplete` inside
  `failSnapshotOperation`'s caller guard short-circuits and the LV
  status is not touched.

## Why not just put a finalizer on the pod?

A pod finalizer would block `kubectl delete pod` until the controller
removes it, which is heavy-handed — the user explicitly asked for the
pod to be gone. The watch-and-detect design is the lighter-weight
intervention: we don't fight the user, we just reflect the loss of
the executor into the LV's status so the rest of the system can
react.

The other reason a finalizer is not enough on its own: it only covers
*external* deletion. A finalizer cannot help with a crashed container
(`PodFailed`), an OOMKilled container that the kubelet will not
restart (`RestartPolicy: Never`), or a `PodPending` stuck on
ImagePullBackOff — in all those cases the pod is still there in the
API, no finalizer is involved, and a finalizer-based design would
still need the phase-probe machinery. The watch + phase-aware probe
in the new `SnapshotPodReconciler` covers every "executor is no
longer doing useful work" case in one mechanism, which is why this
prompt commits fully to the event-driven design.

## Why a new controller and not a `Watches` on `LogicalVolumeReconciler`?

Two controllers, one shared controller-runtime manager/cache, with a
narrow hand-off: `SnapshotPodReconciler` writes a status, the
`LogicalVolumeReconciler`'s existing watch picks it up. Reasons:

- **Single responsibility.** The LV reconciler's job is to drive an LV
  from creation to deletion. Mixing pod-lifecycle event handling into
  it would couple two concerns that happen to share a CR but not a
  state machine. The new controller has one input (pod events) and
  one output (LV status update).
- **Testability.** The new reconciler is one watcher, one predicate,
  one reconcile path, easy to unit-test in isolation without
  spinning up the full LV lifecycle.
- **Extensibility.** If we ever want metrics, audit logs, or alerts
  on "executor pod died", the new controller is the natural home.
  The LV controller stays focused on LV state.
- **Operational separation.** If the pod controller is broken or
  slow, the LV controller is unaffected (and vice versa). With a
  `Watches` on the LV controller, a bug in the predicate or the
  pod-handling code would impact LV reconciliation, which is the
  hot path for every LV on the node.

## Race we hit on the first run, and the fix

When this was first wired up the logs looked like:

```
"marking snapshot operation as failed because the executor pod is gone" controller=snapshotpod
"reconciling LogicalVolume" controller=logicalvolume
"snapshot LV found" controller=logicalvolume
"Processing snapshot backup" controller=logicalvolume
"Snapshot Backup Executor triggered previously, waiting for completion" controller=logicalvolume
"Reconciler error" controller=snapshotpod
  error="failed to update snapshot status: ... the object has been modified;
         please apply your changes to the latest version and try again"
"marking snapshot operation as failed because the executor pod is gone" controller=snapshotpod
```

Root cause: `failSnapshotOperation` was two writes back-to-back —
`setSnapshotExecutorPodMissing` (Get + `Status().Update`) and then
`updateSnapshotOperationStatus` (another Get + `Status().Update`).
The LV controller's `ensureFinalizerAndLabels` does a `Patch` on the
LV (to add the finalizer / created-by label) that bumps
`ResourceVersion`; if that lands between the two writes, the second
update loses the optimistic-concurrency check. The workqueue retried
and the second attempt succeeded, but every in-flight pod deletion
now generates a noisy `Reconciler error` log line.

Fix in `internal/controller/snapshot_handler.go`: collapse the two
writes into a single Get + `Status().Update` so the LV controller's
`Patch` cannot interleave. Re-check the `Operation` and
`isSnapshotOperationComplete` guards after the Get (using the fresh
object) to keep the snapshotter-races-snapshotter-write guarantee
intact.

## Final state of the LV after a successful run

`kubectl describe logicalvolume ...` shows both the dedicated
condition and the high-level `Status.Snapshot` fields populated
together, with the same message in both:

```yaml
status:
  conditions:
  - type: SnapshotBackupExecutorEnsured
    status: "True"
    reason: SuccessfullyEnsuredSnapshotBackupExecutor
  - type: SnapshotExecutorPodMissing
    status: "True"
    reason: ExecutorPodMissing
    message: Backup executor pod was deleted before the operation completed
  - type: SnapshotBackupExecutorCleaned
    status: "True"
    reason: SuccessfullyCleanedSnapshotBackupExecutor
  snapshot:
    phase: Failed
    operation: Backup
    error:
      code: ExecutorPodMissing
      message: Backup executor pod was deleted before the operation completed
```

(The `SnapshotExecutorPodMissing` condition and the `Phase = Failed`
write are atomic — the operator never sees a state where one is set
but not the other.)

