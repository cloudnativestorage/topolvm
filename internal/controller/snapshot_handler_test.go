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
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("corev1.AddToScheme: %v", err)
	}
	if err := topolvmv1.AddToScheme(s); err != nil {
		t.Fatalf("topolvmv1.AddToScheme: %v", err)
	}
	return s
}

// recordingClient wraps a fake client.Client and counts Delete calls so the
// tests can assert that deleteRunningSnapshotPod issues Delete exactly once
// per running pod and never against an already-terminating pod.
type recordingClient struct {
	client.Client
	deleteCalls int
}

func (r *recordingClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	r.deleteCalls++
	return r.Client.Delete(ctx, obj, opts...)
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
	terminating.Finalizers = []string{"kubernetes"} // required for the fake to retain a DeletionTimestamp

	cases := []struct {
		name        string
		seed        *corev1.Pod // nil = pod not present in the API
		getErr      error
		wantRequeue bool
		wantErr     bool
		wantDeletes int
	}{
		{
			name:        "pod gone from API; proceed to unmount",
			seed:        nil,
			wantRequeue: false,
			wantDeletes: 0,
		},
		{
			name:        "pod running; issue Delete and requeue",
			seed:        running.DeepCopy(),
			wantRequeue: true,
			wantDeletes: 1,
		},
		{
			name:        "pod already terminating; do not re-issue Delete",
			seed:        terminating.DeepCopy(),
			wantRequeue: true,
			wantDeletes: 0,
		},
		{
			name:    "Get returns non-NotFound; surface the error",
			getErr:  errors.New("apiserver down"),
			wantErr: true,
		},
	}

	scheme := newScheme(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			builder := fake.NewClientBuilder().WithScheme(scheme)
			if tc.seed != nil {
				builder = builder.WithObjects(tc.seed)
			}
			if tc.getErr != nil {
				builder = builder.WithInterceptorFuncs(interceptor.Funcs{
					Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
						return tc.getErr
					},
				})
			}
			rc := &recordingClient{Client: builder.Build()}
			h := newHandlerWithClient(rc)
			requeue, err := h.deleteRunningSnapshotPod(context.Background(), logr.Discard(), lv, topolvmv1.OperationBackup)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if requeue != tc.wantRequeue {
				t.Errorf("requeue = %v, want %v", requeue, tc.wantRequeue)
			}
			if rc.deleteCalls != tc.wantDeletes {
				t.Errorf("Delete calls = %d, want %d", rc.deleteCalls, tc.wantDeletes)
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
	podKey := client.ObjectKey{Namespace: executor.GetPodNamespace(), Name: podName}

	// Step 1: seed a running pod with a finalizer so the fake retains the
	// object after Delete (it'll just set DeletionTimestamp, mimicking real
	// kubelet behavior).
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:       podName,
			Namespace:  executor.GetPodNamespace(),
			Finalizers: []string{"test/keep-around"},
		},
	}
	scheme := newScheme(t)
	rc := &recordingClient{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()}
	h := newHandlerWithClient(rc)
	ctx := context.Background()

	requeue, err := h.deleteRunningSnapshotPod(ctx, logr.Discard(), lv, topolvmv1.OperationBackup)
	if err != nil || !requeue || rc.deleteCalls != 1 {
		t.Fatalf("step1: requeue=%v err=%v deletes=%d; want requeue=true err=nil deletes=1", requeue, err, rc.deleteCalls)
	}

	// Verify the fake recorded DeletionTimestamp on the pod - this is what
	// the real apiserver does when a finalizer keeps the object pinned.
	got := &corev1.Pod{}
	if err := rc.Get(ctx, podKey, got); err != nil {
		t.Fatalf("step1: re-get pod: %v", err)
	}
	if got.DeletionTimestamp.IsZero() {
		t.Fatalf("step1: expected DeletionTimestamp set after Delete")
	}

	// Step 2: pod is terminating. The function must NOT call Delete again.
	requeue, err = h.deleteRunningSnapshotPod(ctx, logr.Discard(), lv, topolvmv1.OperationBackup)
	if err != nil || !requeue || rc.deleteCalls != 1 {
		t.Fatalf("step2: requeue=%v err=%v deletes=%d; want requeue=true err=nil deletes=1 (no re-issue)", requeue, err, rc.deleteCalls)
	}

	// Step 3: kubelet finalized teardown. Drop the finalizer so the fake
	// finishes the deletion, then call again - this time we should get
	// requeue=false meaning the LV controller is cleared to unmount.
	got.Finalizers = nil
	if err := rc.Update(ctx, got); err != nil {
		t.Fatalf("step3: clear finalizer: %v", err)
	}
	if err := rc.Get(ctx, podKey, &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("step3: expected pod NotFound after finalizer removal, got %v", err)
	}

	requeue, err = h.deleteRunningSnapshotPod(ctx, logr.Discard(), lv, topolvmv1.OperationBackup)
	if err != nil || requeue {
		t.Fatalf("step3: requeue=%v err=%v; want requeue=false err=nil", requeue, err)
	}
}
