package controller

import (
	"context"
	"fmt"
	"strings"

	topolvmv1 "github.com/topolvm/topolvm/api/v1"
	"github.com/topolvm/topolvm/internal/executor"
	corev1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch

type SnapshotPodReconciler struct {
	client client.Client
}

func NewSnapshotPodReconciler(c client.Client) *SnapshotPodReconciler {
	return &SnapshotPodReconciler{client: c}
}

func (r *SnapshotPodReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	lg := log.FromContext(ctx)

	pod := &corev1.Pod{}
	podFound := true
	if err := r.client.Get(ctx, req.NamespacedName, pod); err != nil {
		if !apierrs.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("failed to get executor pod %s: %w", req.NamespacedName, err)
		}
		podFound = false
	}

	lvName, operation, ok := lvNameAndOperation(pod, podFound, req.Name)
	if !ok {
		return ctrl.Result{}, nil
	}

	if !isExecutorPodMissingOrFailed(podFound, pod) { // Neither missing nor failed
		return ctrl.Result{}, nil
	}

	lv := &topolvmv1.LogicalVolume{}
	if err := r.client.Get(ctx, types.NamespacedName{Name: lvName}, lv); err != nil {
		if apierrs.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get LogicalVolume %s: %w", lvName, err)
	}

	if !lv.DeletionTimestamp.IsZero() {
		lg.Info("skipping snapshot-failed status update; LV is being deleted",
			"lv", lv.Name, "operation", operation, "pod", req.NamespacedName)
		return ctrl.Result{}, nil
	}

	if isSnapshotOperationComplete(lv) {
		lg.Info("skipping pod-missing failure: snapshot operation already complete",
			"lv", lv.Name, "operation", operation, "phase", lv.Status.Snapshot.Phase, "pod", req.NamespacedName)
		return ctrl.Result{}, nil
	}

	var reason string
	if !podFound {
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

func (r *SnapshotPodReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("snapshotpod").
		For(&corev1.Pod{}).
		WithEventFilter(snapshotPodPredicate()).
		Complete(r)
}

func lvNameAndOperation(pod *corev1.Pod, podFound bool, reqName string) (string, topolvmv1.OperationType, bool) {
	if podFound {
		lvName := pod.Labels[executor.LabelLogicalVolumeKey]
		op := topolvmv1.OperationType(pod.Labels[executor.LabelSnapshotOperationKey])
		if lvName == "" || op == "" {
			return "", "", false
		}
		return lvName, op, true
	}
	return splitSnapshotPodName(reqName)
}

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

func isExecutorPodMissingOrFailed(podFound bool, pod *corev1.Pod) bool {
	if !podFound {
		return true
	}
	return pod.Status.Phase == corev1.PodFailed
}

func splitSnapshotPodName(podName string) (string, topolvmv1.OperationType, bool) {
	for _, op := range []topolvmv1.OperationType{topolvmv1.OperationBackup, topolvmv1.OperationRestore, topolvmv1.OperationDelete} {
		prefix := strings.ToLower(string(op)) + "-"
		if strings.HasPrefix(podName, prefix) {
			return podName[len(prefix):], op, true
		}
	}
	return "", "", false
}
