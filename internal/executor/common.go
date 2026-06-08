package executor

import (
	"context"
	"fmt"
	"os"
	"strings"

	topolvmv1 "github.com/topolvm/topolvm/api/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func buildObjectMeta(operation topolvmv1.OperationType, lv *topolvmv1.LogicalVolume) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:        BuildSnapshotPodName(operation, lv),
		Namespace:   getNamespace(),
		Labels:      buildLabels(operation, lv),
		Annotations: buildAnnotations(lv),
		OwnerReferences: []metav1.OwnerReference{
			*metav1.NewControllerRef(lv, topolvmv1.GroupVersion.WithKind("LogicalVolume")),
		},
	}
}

// BuildSnapshotPodName returns the deterministic name used for the snapshot
// executor pod (backup, restore, or delete) that targets the given LV.
// Example: BuildSnapshotPodName(OperationBackup, lv) -> "backup-<lv.Name>".
//
// Invariant: at most one snapshot executor pod (Backup, Restore, or Delete) is
// active for a given LV at any time. Backup and Restore are mutually exclusive
// for one LV by construction; Delete only runs after the source operation has
// reached the Succeeded terminal state, so the prior pod has already been
// cleaned up. The reconciler relies on this when ordering teardown during
// LV deletion - see internal/controller/snapshot_handler.go. New callers must
// preserve the invariant; the executors enforce it via
// failIfConflictingSnapshotPodExists.
func BuildSnapshotPodName(operation topolvmv1.OperationType, lv *topolvmv1.LogicalVolume) string {
	return fmt.Sprintf("%s-%s", strings.ToLower(string(operation)), lv.Name)
}

// allOperations is the closed set of snapshot executor operations.
var allOperations = []topolvmv1.OperationType{
	topolvmv1.OperationBackup,
	topolvmv1.OperationRestore,
	topolvmv1.OperationDelete,
}

// failIfConflictingSnapshotPodExists returns a non-nil error if a snapshot pod
// for an operation OTHER than `op` already exists for `lv`. It guards the
// uniqueness invariant documented on BuildSnapshotPodName: if (say) a Backup
// pod is still around when a Delete pod is about to be created, something has
// gone wrong upstream and creating the second pod would race the first over
// the LV's hostPath mount.
//
// The check uses Get (not List) against the deterministic pod names, so no
// list permission is required.
func failIfConflictingSnapshotPodExists(ctx context.Context, c client.Client, op topolvmv1.OperationType, lv *topolvmv1.LogicalVolume) error {
	namespace := GetPodNamespace()
	for _, other := range allOperations {
		if other == op {
			continue
		}
		pod := &corev1.Pod{}
		key := client.ObjectKey{Namespace: namespace, Name: BuildSnapshotPodName(other, lv)}
		if err := c.Get(ctx, key, pod); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("failed to check for conflicting snapshot pod %s: %w", key, err)
		}
		return fmt.Errorf("cannot start %s pod for LV %s: %s pod %s is still active",
			op, lv.Name, other, key)
	}
	return nil
}

func getNamespace() string {
	return GetPodNamespace()
}

// GetPodNamespace returns the namespace in which snapshot executor pods run.
// It reads the HOST_NAMESPACE env var and falls back to "topolvm-system".
func GetPodNamespace() string {
	namespace := os.Getenv(EnvHostNamespace)
	if namespace == "" {
		namespace = "topolvm-system"
	}
	return namespace
}

func buildLabels(operation topolvmv1.OperationType, lv *topolvmv1.LogicalVolume) map[string]string {
	labels := map[string]string{
		LabelSnapshotPodKey:       "true",
		LabelLogicalVolumeKey:     lv.Name,
		LabelSnapshotOperationKey: string(operation),
		LabelAppKey:               LabelAppValue,
	}
	return labels
}

// buildAnnotations constructs annotations for the snapshot pod.
func buildAnnotations(lv *topolvmv1.LogicalVolume) map[string]string {
	annotations := map[string]string{
		"topolvm.io/snapshot-source":  lv.Spec.Source,
		"topolvm.io/device-class":     lv.Spec.DeviceClass,
		"topolvm.io/snapshot-version": "v1",
	}
	return annotations
}

// buildPrivilegedSecurityContext returns the security context used by snapshot
// executor pods that mount the LV device via hostPath (backup and restore).
// Privileged is required to perform the bind mount inside the container;
// it already grants all capabilities, so no Capabilities.Add list is needed.
func buildPrivilegedSecurityContext() *corev1.SecurityContext {
	privileged := true
	return &corev1.SecurityContext{
		Privileged: &privileged,
	}
}

// buildUnprivilegedSecurityContext returns the security context used by the
// snapshot delete executor, which only talks to remote storage and does not
// touch any local device.
func buildUnprivilegedSecurityContext() *corev1.SecurityContext {
	privileged := false
	allowPrivilegeEscalation := false
	return &corev1.SecurityContext{
		Privileged:               &privileged,
		AllowPrivilegeEscalation: &allowPrivilegeEscalation,
	}
}

func getHostPod(ctx context.Context, rClient client.Client) (*corev1.Pod, error) {
	hostPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      os.Getenv(EnvHostName),
			Namespace: os.Getenv(EnvHostNamespace),
		},
	}

	if err := rClient.Get(ctx, client.ObjectKeyFromObject(hostPod), hostPod); err != nil {
		return nil, fmt.Errorf("failed to get host pod: %w", err)
	}

	return hostPod, nil
}
