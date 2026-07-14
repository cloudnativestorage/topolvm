package controller

import (
	"context"
	"fmt"
	"time"

	topolvmv1 "github.com/topolvm/topolvm/api/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crlog "sigs.k8s.io/controller-runtime/pkg/log"
)

// ReencryptRequestReconciler expands a ReencryptRequest selector into the
// concrete set of LogicalVolume objects and drives master-key rotation by
// flipping each LV's encryption state to Reencrypting. The owning node's
// LogicalVolume reconciler observes the state, runs cryptsetup reencrypt with
// --resilience checksum (so the operation resumes after a node restart), and
// reports completion back. The progress is accumulated on the
// ReencryptRequest's status.
type ReencryptRequestReconciler struct {
	client client.Client
}

// NewReencryptRequestReconciler returns a ReencryptRequestReconciler.
func NewReencryptRequestReconciler(c client.Client) *ReencryptRequestReconciler {
	return &ReencryptRequestReconciler{client: c}
}

//+kubebuilder:rbac:groups=topolvm.io,resources=reencryptrequests,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=topolvm.io,resources=reencryptrequests/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=topolvm.io,resources=logicalvolumes,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=topolvm.io,resources=logicalvolumes/status,verbs=get;update;patch

// Reconcile resolves the request's selector, schedules per-volume work, and
// reflects progress on status.
func (r *ReencryptRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := crlog.FromContext(ctx)

	rr := &topolvmv1.ReencryptRequest{}
	if err := r.client.Get(ctx, req.NamespacedName, rr); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if rr.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}
	if rr.Status.Phase == topolvmv1.ReencryptPhaseCompleted || rr.Status.Phase == topolvmv1.ReencryptPhaseFailed {
		return ctrl.Result{}, nil
	}

	sel, err := metav1.LabelSelectorAsSelector(rr.Spec.Selector)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("invalid selector: %w", err)
	}
	if sel == labels.Nothing() {
		return ctrl.Result{}, fmt.Errorf("nil selector rejected to prevent cluster-wide rotation")
	}

	var lvs topolvmv1.LogicalVolumeList
	if err := r.client.List(ctx, &lvs, &client.ListOptions{LabelSelector: sel}); err != nil {
		return ctrl.Result{}, err
	}

	// Filter to encrypted volumes only and tally.
	var work []topolvmv1.LogicalVolume
	for _, lv := range lvs.Items {
		if lv.Spec.Encryption != nil && lv.Spec.Encryption.Enabled {
			work = append(work, lv)
		}
	}

	// Schedule per-volume reencrypt by setting state on each LV. Honor
	// MaxConcurrentPerNode: count currently Running on each node and skip
	// when at cap.
	cap := int(rr.Spec.MaxConcurrentPerNode)
	if cap <= 0 {
		cap = 1
	}
	running := perNodeRunningCount(work)
	scheduled := 0
	for _, lv := range work {
		if lv.Status.Encryption != nil && lv.Status.Encryption.State == topolvmv1.EncryptionReencrypting {
			continue
		}
		if lv.Status.Encryption != nil && lv.Status.Encryption.MasterKeyEpoch > 0 && rr.Status.Phase == topolvmv1.ReencryptPhaseRunning {
			// Already reencrypted under this request; tracked via status.completed below.
			continue
		}
		if running[lv.Spec.NodeName] >= cap {
			continue
		}
		if err := r.markReencrypting(ctx, lv.Name); err != nil {
			log.Error(err, "mark LV reencrypting", "name", lv.Name)
			continue
		}
		running[lv.Spec.NodeName]++
		scheduled++
	}

	// Update aggregate progress.
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &topolvmv1.ReencryptRequest{}
		if err := r.client.Get(ctx, client.ObjectKey{Name: rr.Name}, fresh); err != nil {
			return err
		}
		fresh.Status.Total = int32(len(work))
		fresh.Status.Completed = countCompleted(work)
		fresh.Status.Failed = countFailed(work)
		switch {
		case fresh.Status.Total == 0:
			fresh.Status.Phase = topolvmv1.ReencryptPhaseCompleted
		case fresh.Status.Completed >= fresh.Status.Total && fresh.Status.Failed == 0:
			fresh.Status.Phase = topolvmv1.ReencryptPhaseCompleted
		case fresh.Status.Failed > 0 && fresh.Status.Completed+fresh.Status.Failed >= fresh.Status.Total:
			fresh.Status.Phase = topolvmv1.ReencryptPhaseFailed
		default:
			fresh.Status.Phase = topolvmv1.ReencryptPhaseRunning
		}
		return r.client.Status().Update(ctx, fresh)
	}); err != nil {
		return ctrl.Result{}, err
	}

	// Requeue while there is still work outstanding.
	if rr.Status.Phase != topolvmv1.ReencryptPhaseCompleted && rr.Status.Phase != topolvmv1.ReencryptPhaseFailed {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

func (r *ReencryptRequestReconciler) markReencrypting(ctx context.Context, lvName string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		lv := &topolvmv1.LogicalVolume{}
		if err := r.client.Get(ctx, client.ObjectKey{Name: lvName}, lv); err != nil {
			return err
		}
		if lv.Status.Encryption == nil {
			lv.Status.Encryption = &topolvmv1.EncryptionStatus{}
		}
		lv.Status.Encryption.State = topolvmv1.EncryptionReencrypting
		return r.client.Status().Update(ctx, lv)
	})
}

func perNodeRunningCount(lvs []topolvmv1.LogicalVolume) map[string]int {
	out := map[string]int{}
	for _, lv := range lvs {
		if lv.Status.Encryption != nil && lv.Status.Encryption.State == topolvmv1.EncryptionReencrypting {
			out[lv.Spec.NodeName]++
		}
	}
	return out
}

func countCompleted(lvs []topolvmv1.LogicalVolume) int32 {
	var n int32
	for _, lv := range lvs {
		if lv.Status.Encryption != nil && lv.Status.Encryption.MasterKeyEpoch > 0 && lv.Status.Encryption.State == topolvmv1.EncryptionOpened {
			n++
		}
	}
	return n
}

func countFailed(lvs []topolvmv1.LogicalVolume) int32 {
	var n int32
	for _, lv := range lvs {
		if lv.Status.Encryption != nil && lv.Status.Encryption.State == topolvmv1.EncryptionError {
			n++
		}
	}
	return n
}

// SetupWithManager sets up the controller with the Manager.
func (r *ReencryptRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&topolvmv1.ReencryptRequest{}).
		Complete(r)
}
