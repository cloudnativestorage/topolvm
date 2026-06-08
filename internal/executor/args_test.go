package executor

import (
	"reflect"
	"slices"
	"testing"

	snapshot_api "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	topolvmv1 "github.com/topolvm/topolvm/api/v1"
	"github.com/topolvm/topolvm/internal/mounter"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// These tests pin the exact CLI flag set the snapshot executor pods pass to
// the topolvm-snapshotter binary. The values flow from the LogicalVolume,
// VolumeSnapshotClass, and Repo metadata; the snapshotter binary parses them
// with cobra/pflag, so any drift between the flag name a builder emits and the
// flag the binary declares would silently break backups/restores. The shape is
// load-bearing in another way too: changes here are version-skew exposure
// between the topolvm-node controller (which builds the pod) and the
// topolvm-snapshotter image (which interprets the flags).

func vsClass(namespace string) *snapshot_api.VolumeSnapshotClass {
	return &snapshot_api.VolumeSnapshotClass{
		Parameters: map[string]string{
			SnapshotStorageName:      "backup-store",
			SnapshotStorageNamespace: namespace,
		},
	}
}

func TestBuildSnapshotArgs(t *testing.T) {
	lv := &topolvmv1.LogicalVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "my-lv"},
		Spec:       topolvmv1.LogicalVolumeSpec{NodeName: "node-1"},
	}
	cases := []struct {
		name string
		e    *SnapshotExecutor
		want []string
	}{
		{
			name: "all parameters present",
			e: &SnapshotExecutor{
				logicalVolume: lv,
				targetPVCInfo: types.NamespacedName{Namespace: "app", Name: "data"},
				vsClass:       vsClass("topolvm-system"),
				namespace:     "topolvm-system",
			},
			want: []string{
				"--lv-name=my-lv",
				"--node-name=node-1",
				"--mount-path=" + SnapshotData,
				"--targeted-pvc-namespace=app",
				"--targeted-pvc-name=data",
				"--snapshot-storage-name=backup-store",
				"--snapshot-storage-namespace=topolvm-system",
			},
		},
		{
			name: "missing storage-namespace falls back to executor namespace",
			e: &SnapshotExecutor{
				logicalVolume: lv,
				targetPVCInfo: types.NamespacedName{Namespace: "app", Name: "data"},
				vsClass:       vsClass(""),
				namespace:     "host-ns",
			},
			want: []string{
				"--lv-name=my-lv",
				"--node-name=node-1",
				"--mount-path=" + SnapshotData,
				"--targeted-pvc-namespace=app",
				"--targeted-pvc-name=data",
				"--snapshot-storage-name=backup-store",
				"--snapshot-storage-namespace=host-ns",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.e.buildSnapshotArgs()
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("buildSnapshotArgs() mismatch\n got: %#v\nwant: %#v", got, tc.want)
			}
		})
	}
}

func TestBuildRestoreArgs(t *testing.T) {
	lv := &topolvmv1.LogicalVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "my-lv"},
		Spec:       topolvmv1.LogicalVolumeSpec{NodeName: "node-1"},
	}
	snapLV := &topolvmv1.LogicalVolume{
		Status: topolvmv1.LogicalVolumeStatus{
			Snapshot: &topolvmv1.SnapshotStatus{
				Path:       "/some/repo/path",
				SnapshotID: "abc123",
			},
		},
	}
	e := &RestoreExecutor{
		lv:            lv,
		snapshotLV:    snapLV,
		mountResponse: &mounter.MountResponse{},
		vsClass:       vsClass("topolvm-system"),
		namespace:     "topolvm-system",
	}
	got := e.buildRestoreArgs()
	want := []string{
		"--lv-name=my-lv",
		"--node-name=node-1",
		"--repo-path=/some/repo/path",
		"--snapshot-id=abc123",
		"--snapshot-storage-name=backup-store",
		"--snapshot-storage-namespace=topolvm-system",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildRestoreArgs() mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestBuildDeleteArgs(t *testing.T) {
	lv := &topolvmv1.LogicalVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "my-lv"},
		Status: topolvmv1.LogicalVolumeStatus{
			Snapshot: &topolvmv1.SnapshotStatus{Path: "/some/repo/path"},
		},
	}
	e := &DeleteExecutor{
		lv:        lv,
		vsClass:   vsClass("topolvm-system"),
		namespace: "topolvm-system",
	}
	got := e.buildDeleteArgs()
	want := []string{
		"--lv-name=my-lv",
		"--repo-path=/some/repo/path",
		"--snapshot-storage-name=backup-store",
		"--snapshot-storage-namespace=topolvm-system",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildDeleteArgs() mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

// TestBuildArgs_StorageNamespaceDefaultIsApplied ensures the
// defaultNamespaceIfEmpty helper is wired into both the namespace AND name
// flags consistently. (The current implementation defaults both to
// e.namespace when the parameter is empty - that's surprising but it's what
// the snapshotter binary depends on; pin the behavior.)
func TestBuildArgs_StorageNamespaceDefaultIsApplied(t *testing.T) {
	lv := &topolvmv1.LogicalVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "my-lv"},
		Status: topolvmv1.LogicalVolumeStatus{
			Snapshot: &topolvmv1.SnapshotStatus{Path: "/p"},
		},
	}
	e := &DeleteExecutor{
		lv: lv,
		vsClass: &snapshot_api.VolumeSnapshotClass{
			Parameters: map[string]string{}, // both keys missing
		},
		namespace: "fallback",
	}
	got := e.buildDeleteArgs()
	for _, want := range []string{"--snapshot-storage-name=fallback", "--snapshot-storage-namespace=fallback"} {
		if !slices.Contains(got, want) {
			t.Errorf("expected fallback arg %q in %#v", want, got)
		}
	}
}
