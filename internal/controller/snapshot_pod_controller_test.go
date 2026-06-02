package controller

import (
	"testing"

	topolvmv1 "github.com/topolvm/topolvm/api/v1"
	"github.com/topolvm/topolvm/internal/executor"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

func TestIsSnapshotPod(t *testing.T) {
	withLabel := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{executor.LabelSnapshotPodKey: "true"},
		},
	}
	withoutLabel := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{}}
	otherValue := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{executor.LabelSnapshotPodKey: "false"},
		},
	}

	if !isSnapshotPod(withLabel) {
		t.Errorf("isSnapshotPod(withLabel) = false, want true")
	}
	if isSnapshotPod(withoutLabel) {
		t.Errorf("isSnapshotPod(withoutLabel) = true, want false")
	}
	if isSnapshotPod(otherValue) {
		t.Errorf("isSnapshotPod(otherValue) = true, want false")
	}
}

func TestIsExecutorPodMissingOrFailed(t *testing.T) {
	running := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	failed := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodFailed}}
	pending := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending}}
	succeeded := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodSucceeded}}

	cases := []struct {
		name     string
		podFound bool
		pod      *corev1.Pod
		want     bool
	}{
		{"pod missing", false, nil, true},
		{"pod failed", true, failed, true},
		{"pod running", true, running, false},
		{"pod pending", true, pending, false},
		{"pod succeeded", true, succeeded, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isExecutorPodMissingOrFailed(tc.podFound, tc.pod); got != tc.want {
				t.Errorf("isExecutorPodMissingOrFailed(%v, %+v) = %v, want %v", tc.podFound, tc.pod, got, tc.want)
			}
		})
	}
}

func TestSplitSnapshotPodName(t *testing.T) {
	cases := []struct {
		name     string
		podName  string
		wantName string
		wantOp   topolvmv1.OperationType
		wantOK   bool
	}{
		{"backup", "backup-my-lv", "my-lv", topolvmv1.OperationBackup, true},
		{"restore", "restore-snapshot-abc", "snapshot-abc", topolvmv1.OperationRestore, true},
		{"delete", "delete-foo", "foo", topolvmv1.OperationDelete, true},
		{"unknown prefix", "reindex-foo", "", "", false},
		{"empty", "", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lvName, op, ok := splitSnapshotPodName(tc.podName)
			if lvName != tc.wantName || op != tc.wantOp || ok != tc.wantOK {
				t.Errorf("splitSnapshotPodName(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tc.podName, lvName, op, ok, tc.wantName, tc.wantOp, tc.wantOK)
			}
		})
	}
}

func TestPhaseChangedToFailed(t *testing.T) {
	makeLV := func(phase topolvmv1.OperationPhase) *topolvmv1.LogicalVolume {
		lv := &topolvmv1.LogicalVolume{}
		if phase != "" {
			lv.Status.Snapshot = &topolvmv1.SnapshotStatus{Phase: phase}
		}
		return lv
	}

	cases := []struct {
		name string
		oldL *topolvmv1.LogicalVolume
		newL *topolvmv1.LogicalVolume
		want bool
	}{
		{"running to failed", makeLV(topolvmv1.OperationPhaseRunning), makeLV(topolvmv1.OperationPhaseFailed), true},
		{"pending to failed", makeLV(topolvmv1.OperationPhasePending), makeLV(topolvmv1.OperationPhaseFailed), true},
		{"no status to failed", makeLV(""), makeLV(topolvmv1.OperationPhaseFailed), true},
		{"failed to failed (idempotent)", makeLV(topolvmv1.OperationPhaseFailed), makeLV(topolvmv1.OperationPhaseFailed), false},
		{"running to running", makeLV(topolvmv1.OperationPhaseRunning), makeLV(topolvmv1.OperationPhaseRunning), false},
		{"running to succeeded", makeLV(topolvmv1.OperationPhaseRunning), makeLV(topolvmv1.OperationPhaseSucceeded), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := phaseChangedToFailed(tc.oldL, tc.newL); got != tc.want {
				t.Errorf("phaseChangedToFailed = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSnapshotPodPredicate(t *testing.T) {
	snapshotPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "backup-my-lv",
			Namespace: "topolvm-system",
			Labels:    map[string]string{executor.LabelSnapshotPodKey: "true"},
		},
	}
	notSnapshotPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "unrelated"},
	}

	pred := snapshotPodPredicate()

	// Create: only fires for snapshot pods.
	if !pred.Create(event.CreateEvent{Object: snapshotPod}) {
		t.Errorf("Create(snapshotPod) = false, want true")
	}
	if pred.Create(event.CreateEvent{Object: notSnapshotPod}) {
		t.Errorf("Create(notSnapshotPod) = true, want false")
	}

	// Delete: only fires for snapshot pods.
	if !pred.Delete(event.DeleteEvent{Object: snapshotPod}) {
		t.Errorf("Delete(snapshotPod) = false, want true")
	}
	if pred.Delete(event.DeleteEvent{Object: notSnapshotPod}) {
		t.Errorf("Delete(notSnapshotPod) = true, want false")
	}

	// Update: Running -> Failed fires.
	failed := snapshotPod.DeepCopy()
	failed.Status.Phase = corev1.PodFailed
	if !pred.Update(event.UpdateEvent{ObjectOld: snapshotPod, ObjectNew: failed}) {
		t.Errorf("Update(Running -> Failed) = false, want true")
	}

	// Update: Running -> Running does not.
	if pred.Update(event.UpdateEvent{ObjectOld: snapshotPod, ObjectNew: snapshotPod}) {
		t.Errorf("Update(Running -> Running) = true, want false")
	}

	// Update: DeletionTimestamp becomes set fires.
	deleting := snapshotPod.DeepCopy()
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	if !pred.Update(event.UpdateEvent{ObjectOld: snapshotPod, ObjectNew: deleting}) {
		t.Errorf("Update(DeletionTimestamp set) = false, want true")
	}
}
