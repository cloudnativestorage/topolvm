package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/go-logr/logr"
	topolvmv1 "github.com/topolvm/topolvm/api/v1"
	"github.com/topolvm/topolvm/internal/executor"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// podClient is a hand-rolled client.Client double scoped to the surface area
// of (*snapshotHandler).deleteRunningSnapshotPod, which only calls Get and
// Delete on *corev1.Pod. Embedding client.Client as a nil interface means any
// other method call will panic - that's deliberate so the test surfaces
// unintended dependencies immediately.
type podClient struct {
	client.Client
	pod         *corev1.Pod // nil = NotFound
	getErr      error       // if non-nil, returned from Get instead of the pod
	deleteCalls int
	deleteErr   error
}

var _ client.Client = (*podClient)(nil)

func (f *podClient) Get(_ context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	if f.getErr != nil {
		return f.getErr
	}
	if f.pod == nil {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, key.Name)
	}
	p, ok := obj.(*corev1.Pod)
	if !ok {
		return errors.New("podClient.Get only supports *corev1.Pod")
	}
	*p = *f.pod.DeepCopy()
	return nil
}

func (f *podClient) Delete(_ context.Context, _ client.Object, _ ...client.DeleteOption) error {
	f.deleteCalls++
	return f.deleteErr
}

func newHandlerWithClient(c client.Client) *snapshotHandler {
	return &snapshotHandler{
		LogicalVolumeReconciler: &LogicalVolumeReconciler{
			client: c,
		},
	}
}

func TestDeleteRunningSnapshotPod(t *testing.T) {
	lv := &topolvmv1.LogicalVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "my-lv"},
	}
	podName := executor.BuildSnapshotPodName(topolvmv1.OperationBackup, lv)
	podNamespace := executor.GetPodNamespace()
	deletionTime := metav1.Now()

	running := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: podNamespace,
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	terminating := running.DeepCopy()
	terminating.DeletionTimestamp = &deletionTime
	terminating.Finalizers = []string{"kubernetes"} // required for DeletionTimestamp to round-trip

	cases := []struct {
		name        string
		pod         *corev1.Pod
		getErr      error
		wantRequeue bool
		wantErr     bool
		wantDeletes int
	}{
		{
			name:        "pod gone from API; proceed to unmount",
			pod:         nil,
			wantRequeue: false,
			wantDeletes: 0,
		},
		{
			name:        "pod running; issue Delete and requeue",
			pod:         running,
			wantRequeue: true,
			wantDeletes: 1,
		},
		{
			name:        "pod already terminating; do not re-issue Delete",
			pod:         terminating,
			wantRequeue: true,
			wantDeletes: 0,
		},
		{
			name:    "Get returns non-NotFound; surface the error",
			getErr:  errors.New("apiserver down"),
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := &podClient{pod: tc.pod, getErr: tc.getErr}
			h := newHandlerWithClient(fc)
			requeue, err := h.deleteRunningSnapshotPod(context.Background(), logr.Discard(), lv, topolvmv1.OperationBackup)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if requeue != tc.wantRequeue {
				t.Errorf("requeue = %v, want %v", requeue, tc.wantRequeue)
			}
			if fc.deleteCalls != tc.wantDeletes {
				t.Errorf("Delete calls = %d, want %d", fc.deleteCalls, tc.wantDeletes)
			}
		})
	}
}

// TestDeleteRunningSnapshotPod_TerminationOrdering pins the invariant called
// out in AGENTS.md: as long as the pod is observable in the API, the function
// must report requeue=true so the LV controller waits before unmounting. Only
// once the API reports NotFound (kubelet finished tearing the pod down and
// released its hostPath mount) does it return requeue=false. A regression
// here would resurrect the original "lvremove fails because device is busy"
// hang.
func TestDeleteRunningSnapshotPod_TerminationOrdering(t *testing.T) {
	lv := &topolvmv1.LogicalVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "my-lv"},
	}
	podName := executor.BuildSnapshotPodName(topolvmv1.OperationBackup, lv)

	// Step 1: pod is running. Expect Delete + requeue.
	now := metav1.Now()
	fc := &podClient{pod: &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: executor.GetPodNamespace()},
	}}
	h := newHandlerWithClient(fc)
	requeue, err := h.deleteRunningSnapshotPod(context.Background(), logr.Discard(), lv, topolvmv1.OperationBackup)
	if err != nil || !requeue || fc.deleteCalls != 1 {
		t.Fatalf("step1: requeue=%v err=%v deletes=%d; want requeue=true err=nil deletes=1", requeue, err, fc.deleteCalls)
	}

	// Step 2: kubelet observed the delete; pod has DeletionTimestamp set.
	// We must NOT call Delete again and must keep requeueing.
	fc.pod.DeletionTimestamp = &now
	fc.pod.Finalizers = []string{"kubernetes"}
	requeue, err = h.deleteRunningSnapshotPod(context.Background(), logr.Discard(), lv, topolvmv1.OperationBackup)
	if err != nil || !requeue || fc.deleteCalls != 1 {
		t.Fatalf("step2: requeue=%v err=%v deletes=%d; want requeue=true err=nil deletes=1 (no re-issue)", requeue, err, fc.deleteCalls)
	}

	// Step 3: kubelet finalized teardown; pod is gone from the API. Only now
	// is the LV controller cleared to proceed to unmount + lvremove.
	fc.pod = nil
	requeue, err = h.deleteRunningSnapshotPod(context.Background(), logr.Discard(), lv, topolvmv1.OperationBackup)
	if err != nil || requeue {
		t.Fatalf("step3: requeue=%v err=%v; want requeue=false err=nil", requeue, err)
	}
}
