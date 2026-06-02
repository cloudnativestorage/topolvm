package executor

import (
	"testing"

	topolvmv1 "github.com/topolvm/topolvm/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestBuildSnapshotPodName(t *testing.T) {
	tests := []struct {
		name      string
		operation topolvmv1.OperationType
		lvName    string
		want      string
	}{
		{
			name:      "backup operation is lowercased",
			operation: topolvmv1.OperationBackup,
			lvName:    "my-lv",
			want:      "backup-my-lv",
		},
		{
			name:      "restore operation is lowercased",
			operation: topolvmv1.OperationRestore,
			lvName:    "my-lv",
			want:      "restore-my-lv",
		},
		{
			name:      "delete operation is lowercased",
			operation: topolvmv1.OperationDelete,
			lvName:    "snapshot-abc",
			want:      "delete-snapshot-abc",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lv := &topolvmv1.LogicalVolume{
				ObjectMeta: metav1.ObjectMeta{Name: tt.lvName},
			}
			if got := BuildSnapshotPodName(tt.operation, lv); got != tt.want {
				t.Errorf("BuildSnapshotPodName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildLabelsIncludesOperation(t *testing.T) {
	lv := &topolvmv1.LogicalVolume{ObjectMeta: metav1.ObjectMeta{Name: "my-lv"}}
	labels := buildLabels(topolvmv1.OperationRestore, lv)

	if labels[LabelAppKey] != LabelAppValue {
		t.Errorf("LabelApp = %q, want %q", labels[LabelAppKey], LabelAppValue)
	}
	if labels[LabelLogicalVolumeKey] != "my-lv" {
		t.Errorf("LabelLogicalVolume = %q, want %q", labels[LabelLogicalVolumeKey], "my-lv")
	}
	if labels[LabelSnapshotPodKey] != "true" {
		t.Errorf("LabelSnapshotPod = %q, want %q", labels[LabelSnapshotPodKey], "true")
	}
	if labels[LabelSnapshotOperationKey] != string(topolvmv1.OperationRestore) {
		t.Errorf("LabelSnapshotOperation = %q, want %q", labels[LabelSnapshotOperationKey], topolvmv1.OperationRestore)
	}
}

func TestGetPodNamespaceDefaultsToTopolvmSystem(t *testing.T) {
	t.Setenv(EnvHostNamespace, "")
	if got := GetPodNamespace(); got != "topolvm-system" {
		t.Errorf("GetPodNamespace() with empty env = %q, want %q", got, "topolvm-system")
	}
}

func TestGetPodNamespaceHonorsEnv(t *testing.T) {
	t.Setenv(EnvHostNamespace, "custom-ns")
	if got := GetPodNamespace(); got != "custom-ns" {
		t.Errorf("GetPodNamespace() with env set = %q, want %q", got, "custom-ns")
	}
}

