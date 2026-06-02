package controller

import (
	"testing"

	topolvmv1 "github.com/topolvm/topolvm/api/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestHasSnapshotExecutorPodMissing(t *testing.T) {
	cases := []struct {
		name string
		lv   *topolvmv1.LogicalVolume
		want bool
	}{
		{
			name: "no conditions",
			lv:   &topolvmv1.LogicalVolume{},
			want: false,
		},
		{
			name: "condition is True",
			lv: withCondition(metav1.Condition{
				Type:   topolvmv1.TypeSnapshotExecutorPodMissing,
				Status: metav1.ConditionTrue,
			}),
			want: true,
		},
		{
			name: "condition is False",
			lv: withCondition(metav1.Condition{
				Type:   topolvmv1.TypeSnapshotExecutorPodMissing,
				Status: metav1.ConditionFalse,
			}),
			want: false,
		},
		{
			name: "unrelated condition",
			lv: withCondition(metav1.Condition{
				Type:   topolvmv1.TypeSnapshotBackupExecutorEnsured,
				Status: metav1.ConditionTrue,
			}),
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasSnapshotExecutorPodMissing(tc.lv); got != tc.want {
				t.Errorf("hasSnapshotExecutorPodMissing = %v, want %v", got, tc.want)
			}
		})
	}
}

func withCondition(c metav1.Condition) *topolvmv1.LogicalVolume {
	lv := &topolvmv1.LogicalVolume{}
	meta.SetStatusCondition(&lv.Status.Conditions, c)
	return lv
}
