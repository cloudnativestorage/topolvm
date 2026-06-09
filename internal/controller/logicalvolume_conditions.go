package controller

import (
	"context"
	"fmt"

	topolvmv1 "github.com/topolvm/topolvm/api/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func setSnapshotBackupStorageFoundToTrue(ctx context.Context, c client.Client, lv *topolvmv1.LogicalVolume) error {
	newCond := metav1.Condition{
		Type:    topolvmv1.TypeSnapshotBackupStorageFound,
		Status:  metav1.ConditionTrue,
		Reason:  topolvmv1.ReasonSnapshotBackupStorageFound,
		Message: "SnapshotBackupStorage exists and is accessible.",
	}
	return updateLVStatusCondition(ctx, c, lv, newCond)
}

func setSnapshotBackupStorageFoundToFalse(ctx context.Context, c client.Client, lv *topolvmv1.LogicalVolume, err error) error {
	newCond := metav1.Condition{
		Type:    topolvmv1.TypeSnapshotBackupStorageFound,
		Status:  metav1.ConditionFalse,
		Reason:  topolvmv1.ReasonSnapshotBackupStorageNotFound,
		Message: fmt.Sprintf("SnapshotBackupStorage not found: %q", err.Error()),
	}
	return updateLVStatusCondition(ctx, c, lv, newCond)
}

func setSnapshotBackupExecutorEnsuredToTrue(ctx context.Context, client client.Client, lv *topolvmv1.LogicalVolume) error {
	newCond := metav1.Condition{
		Type:    topolvmv1.TypeSnapshotBackupExecutorEnsured,
		Status:  metav1.ConditionTrue,
		Reason:  topolvmv1.ReasonSuccessfullyEnsuredSnapshotBackupExecutor,
		Message: "Snapshot Backup Executor has been ensured successfully.",
	}
	return updateLVStatusCondition(ctx, client, lv, newCond)
}

func setSnapshotBackupExecutorEnsuredToFalse(ctx context.Context, client client.Client, lv *topolvmv1.LogicalVolume, err error) error {
	newCond := metav1.Condition{
		Type:    topolvmv1.TypeSnapshotBackupExecutorEnsured,
		Status:  metav1.ConditionFalse,
		Reason:  topolvmv1.ReasonFailedToEnsureSnapshotBackupExecutor,
		Message: fmt.Sprintf("Failed to ensure Snapshot Backup Executor: %q", err.Error()),
	}
	return updateLVStatusCondition(ctx, client, lv, newCond)
}

func hasSnapshotBackupExecutorCondition(lv *topolvmv1.LogicalVolume) bool {
	return meta.IsStatusConditionFalse(lv.Status.Conditions, topolvmv1.TypeSnapshotBackupExecutorEnsured) ||
		meta.IsStatusConditionTrue(lv.Status.Conditions, topolvmv1.TypeSnapshotBackupExecutorEnsured)
}

func setSnapshotRestoreExecutorEnsuredToTrue(ctx context.Context, client client.Client, lv *topolvmv1.LogicalVolume) error {
	newCond := metav1.Condition{
		Type:    topolvmv1.TypeSnapshotRestoreExecutorEnsured,
		Status:  metav1.ConditionTrue,
		Reason:  topolvmv1.ReasonSuccessfullyEnsuredSnapshotRestoreExecutor,
		Message: "Snapshot Restore Executor has been ensured successfully.",
	}
	return updateLVStatusCondition(ctx, client, lv, newCond)
}

func setSnapshotRestoreExecutorEnsuredToFalse(ctx context.Context, client client.Client, lv *topolvmv1.LogicalVolume, err error) error {
	newCond := metav1.Condition{
		Type:    topolvmv1.TypeSnapshotRestoreExecutorEnsured,
		Status:  metav1.ConditionFalse,
		Reason:  topolvmv1.ReasonFailedToEnsureSnapshotRestoreExecutor,
		Message: fmt.Sprintf("Failed to ensure Snapshot Restore Executor: %q", err.Error()),
	}
	return updateLVStatusCondition(ctx, client, lv, newCond)
}

func hasSnapshotRestoreExecutorCondition(lv *topolvmv1.LogicalVolume) bool {
	return meta.IsStatusConditionFalse(lv.Status.Conditions, topolvmv1.TypeSnapshotRestoreExecutorEnsured) ||
		meta.IsStatusConditionTrue(lv.Status.Conditions, topolvmv1.TypeSnapshotRestoreExecutorEnsured)
}

func setSnapshotExecutorCleanupToTrue(ctx context.Context, client client.Client, operation topolvmv1.OperationType, lv *topolvmv1.LogicalVolume) error {
	var newCond metav1.Condition
	switch operation {
	case topolvmv1.OperationBackup:
		newCond = metav1.Condition{
			Type:    topolvmv1.TypeSnapshotBackupExecutorCleaned,
			Status:  metav1.ConditionTrue,
			Reason:  topolvmv1.ReasonSuccessfullyCleanedSnapshotBackupExecutor,
			Message: "Snapshot Backup Executor has been cleaned successfully.",
		}
	case topolvmv1.OperationRestore:
		newCond = metav1.Condition{
			Type:    topolvmv1.TypeSnapshotRestoreExecutorCleaned,
			Status:  metav1.ConditionTrue,
			Reason:  topolvmv1.ReasonSuccessfullyCleanedSnapshotRestoreExecutor,
			Message: "Snapshot Restore Executor has been cleaned successfully.",
		}
	}
	return updateLVStatusCondition(ctx, client, lv, newCond)
}

func setSnapshotExecutorCleanupToFalse(ctx context.Context, client client.Client, operation topolvmv1.OperationType, lv *topolvmv1.LogicalVolume, err error) error {
	var newCond metav1.Condition
	switch operation {
	case topolvmv1.OperationBackup:
		newCond = metav1.Condition{
			Type:    topolvmv1.TypeSnapshotBackupExecutorCleaned,
			Status:  metav1.ConditionFalse,
			Reason:  topolvmv1.ReasonFailedToCleanedSnapshotBackupExecutor,
			Message: fmt.Sprintf("Failed to clean Snapshot Backup Executor: %q", err.Error()),
		}
	case topolvmv1.OperationRestore:
		newCond = metav1.Condition{
			Type:    topolvmv1.TypeSnapshotRestoreExecutorCleaned,
			Status:  metav1.ConditionFalse,
			Reason:  topolvmv1.ReasonFailedToCleanedSnapshotRestoreExecutor,
			Message: fmt.Sprintf("Failed to clean Snapshot Restore Executor: %q", err.Error()),
		}
	}
	return updateLVStatusCondition(ctx, client, lv, newCond)
}

func setSnapshotDeleteExecutorEnsuredToTrue(ctx context.Context, client client.Client, lv *topolvmv1.LogicalVolume) error {
	newCond := metav1.Condition{
		Type:    topolvmv1.TypeSnapshotDeleteExecutorEnsured,
		Status:  metav1.ConditionTrue,
		Reason:  topolvmv1.ReasonSuccessfullyEnsuredSnapshotDeleteExecutor,
		Message: "Snapshot Delete Executor has been ensured successfully.",
	}
	return updateLVStatusCondition(ctx, client, lv, newCond)
}

func setSnapshotDeleteExecutorEnsuredToFalse(ctx context.Context, client client.Client, lv *topolvmv1.LogicalVolume, err error) error {
	newCond := metav1.Condition{
		Type:    topolvmv1.TypeSnapshotDeleteExecutorEnsured,
		Status:  metav1.ConditionFalse,
		Reason:  topolvmv1.ReasonFailedToEnsureSnapshotDeleteExecutor,
		Message: fmt.Sprintf("Failed to ensure Snapshot Delete Executor: %q", err.Error()),
	}
	return updateLVStatusCondition(ctx, client, lv, newCond)
}

func hasSnapshotDeleteExecutorCondition(lv *topolvmv1.LogicalVolume) bool {
	return meta.IsStatusConditionFalse(lv.Status.Conditions, topolvmv1.TypeSnapshotDeleteExecutorEnsured) ||
		meta.IsStatusConditionTrue(lv.Status.Conditions, topolvmv1.TypeSnapshotDeleteExecutorEnsured)
}

func hasSnapshotExecutorPodMissing(lv *topolvmv1.LogicalVolume) bool {
	return meta.IsStatusConditionTrue(lv.Status.Conditions, topolvmv1.TypeSnapshotExecutorPodMissing)
}

func hasConditionSnapshotDeleteSucceeded(lv *topolvmv1.LogicalVolume) bool {
	for _, condition := range lv.Status.Conditions {
		if condition.Type == topolvmv1.TypeSnapshotDeleteEnsured &&
			condition.Status == metav1.ConditionTrue &&
			condition.Reason == topolvmv1.ConditionSnapshotDeleteSucceeded {
			return true
		}
	}
	return false
}

func hasSnapshotBackupExecutorCleanupCondition(lv *topolvmv1.LogicalVolume) bool {
	return meta.IsStatusConditionFalse(lv.Status.Conditions, topolvmv1.TypeSnapshotBackupExecutorCleaned) ||
		meta.IsStatusConditionTrue(lv.Status.Conditions, topolvmv1.TypeSnapshotBackupExecutorCleaned)
}

func hasSnapshotRestoreExecutorCleanupCondition(lv *topolvmv1.LogicalVolume) bool {
	return meta.IsStatusConditionFalse(lv.Status.Conditions, topolvmv1.TypeSnapshotRestoreExecutorCleaned) ||
		meta.IsStatusConditionTrue(lv.Status.Conditions, topolvmv1.TypeSnapshotRestoreExecutorCleaned)
}

func setLVMSnapshotCleanedToTrue(ctx context.Context, client client.Client, lv *topolvmv1.LogicalVolume) error {
	newCond := metav1.Condition{
		Type:    topolvmv1.TypeLVMSnapshotCleaned,
		Status:  metav1.ConditionTrue,
		Reason:  topolvmv1.ReasonSuccessfullyCleanedLVMSnapshot,
		Message: "LVM snapshot volume has been removed successfully after backup completion.",
	}
	return updateLVStatusCondition(ctx, client, lv, newCond)
}

func setLVMSnapshotCleanedToFalse(ctx context.Context, client client.Client, lv *topolvmv1.LogicalVolume, err error) error {
	newCond := metav1.Condition{
		Type:    topolvmv1.TypeLVMSnapshotCleaned,
		Status:  metav1.ConditionFalse,
		Reason:  topolvmv1.ReasonFailedToCleanLVMSnapshot,
		Message: fmt.Sprintf("Failed to clean LVM snapshot: %q", err.Error()),
	}
	return updateLVStatusCondition(ctx, client, lv, newCond)
}

func hasLVMSnapshotCleanupCondition(lv *topolvmv1.LogicalVolume) bool {
	return meta.IsStatusConditionFalse(lv.Status.Conditions, topolvmv1.TypeLVMSnapshotCleaned) ||
		meta.IsStatusConditionTrue(lv.Status.Conditions, topolvmv1.TypeLVMSnapshotCleaned)
}

func updateLVStatus(ctx context.Context, kClient client.Client, lv *topolvmv1.LogicalVolume) error {
	// Refresh the LogicalVolume to get the latest version
	freshLV := &topolvmv1.LogicalVolume{}
	if err := kClient.Get(ctx, client.ObjectKeyFromObject(lv), freshLV); err != nil {
		return fmt.Errorf("failed to get latest LogicalVolume: %w", err)
	}
	freshLV.Status = lv.Status
	if err := kClient.Status().Update(ctx, freshLV); err != nil {
		return fmt.Errorf("failed to update snapshot status: %w", err)
	}
	lv.Status = freshLV.Status
	lv.ResourceVersion = freshLV.ResourceVersion
	return nil
}

// updateLVStatusCondition re-fetches the LV, applies `cond` on top of the
// currently-stored Conditions slice via meta.SetStatusCondition, and Updates
// the status subresource. The previous implementation wholesale-replaced
// freshLV.Status.Conditions with the in-memory slice, which silently dropped
// any condition another reconciler (or another helper in the same reconcile)
// wrote between the original Reconcile-time Get and this call.
func updateLVStatusCondition(ctx context.Context, kClient client.Client, lv *topolvmv1.LogicalVolume, cond metav1.Condition) error {
	freshLV := &topolvmv1.LogicalVolume{}
	if err := kClient.Get(ctx, client.ObjectKeyFromObject(lv), freshLV); err != nil {
		return fmt.Errorf("failed to get latest LogicalVolume: %w", err)
	}
	if freshLV.Status.Snapshot == nil {
		freshLV.Status.Snapshot = &topolvmv1.SnapshotStatus{
			StartTime: metav1.Now(),
		}
	}
	meta.SetStatusCondition(&freshLV.Status.Conditions, cond)
	if err := kClient.Status().Update(ctx, freshLV); err != nil {
		return fmt.Errorf("failed to update snapshot status: %w", err)
	}
	lv.Status = freshLV.Status
	lv.ResourceVersion = freshLV.ResourceVersion
	return nil
}
