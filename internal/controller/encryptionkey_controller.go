package controller

import (
	"context"
	"encoding/base64"
	"fmt"
	"time"

	topolvmv1 "github.com/topolvm/topolvm/api/v1"
	"github.com/topolvm/topolvm/internal/keyprovider"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crlog "sigs.k8s.io/controller-runtime/pkg/log"
)

// EncryptionKeyReconciler watches EncryptionKey objects and, when the
// provider's current KEK version is newer than the version stored on a key,
// rewraps the blob so the volume stays unlockable under the new KEK without
// any downtime. This is the cheap rotation path; the master key in the
// LUKS header is untouched.
type EncryptionKeyReconciler struct {
	client       client.Client
	keyProvider  keyprovider.KeyProvider
	requeueAfter time.Duration
}

// NewEncryptionKeyReconciler returns an EncryptionKeyReconciler. requeue
// controls the cadence at which keys are revisited; spec recommends 6h.
func NewEncryptionKeyReconciler(c client.Client, kp keyprovider.KeyProvider, requeue time.Duration) *EncryptionKeyReconciler {
	if requeue == 0 {
		requeue = 6 * time.Hour
	}
	return &EncryptionKeyReconciler{client: c, keyProvider: kp, requeueAfter: requeue}
}

//+kubebuilder:rbac:groups=topolvm.io,resources=encryptionkeys,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=topolvm.io,resources=encryptionkeys/status,verbs=get;update;patch

// Reconcile compares stored KEKVersion to the provider's current one and
// calls Rewrap when stale.
func (r *EncryptionKeyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := crlog.FromContext(ctx)

	ek := &topolvmv1.EncryptionKey{}
	if err := r.client.Get(ctx, req.NamespacedName, ek); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if ek.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}

	if r.keyProvider == nil {
		log.Info("no key provider configured; skipping rewrap")
		return ctrl.Result{RequeueAfter: r.requeueAfter}, nil
	}
	if ek.Spec.Provider != r.keyProvider.Name() {
		// Different provider; ignore. A future change may dispatch to a
		// per-provider reconciler.
		return ctrl.Result{RequeueAfter: r.requeueAfter}, nil
	}
	if ek.Status.WrappedDEK == "" {
		// Not yet populated by the controller; revisit later.
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	current, err := r.keyProvider.KEKVersion(ctx, ek.Spec.KeyRef)
	if err != nil {
		log.Error(err, "read provider KEK version", "keyRef", ek.Spec.KeyRef)
		return ctrl.Result{RequeueAfter: r.requeueAfter}, nil
	}
	if current == ek.Status.KEKVersion {
		return ctrl.Result{RequeueAfter: r.requeueAfter}, nil
	}

	ct, err := base64.StdEncoding.DecodeString(ek.Status.WrappedDEK)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("decode wrappedDEK on %s: %w", ek.Name, err)
	}
	rewrapped, err := r.keyProvider.Rewrap(ctx, keyprovider.WrappedKey{
		Ciphertext: ct,
		KeyRef:     ek.Spec.KeyRef,
		KEKVersion: ek.Status.KEKVersion,
		Provider:   ek.Spec.Provider,
	}, ek.Status.BoundVolumeID)
	if err != nil {
		log.Error(err, "rewrap failed", "key", ek.Name)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	log.Info("rewrapped EncryptionKey", "name", ek.Name, "from", ek.Status.KEKVersion, "to", rewrapped.KEKVersion)

	now := metav1.NewTime(time.Now())
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &topolvmv1.EncryptionKey{}
		if err := r.client.Get(ctx, client.ObjectKey{Name: ek.Name}, fresh); err != nil {
			return err
		}
		fresh.Status.WrappedDEK = base64.StdEncoding.EncodeToString(rewrapped.Ciphertext)
		fresh.Status.KEKVersion = rewrapped.KEKVersion
		fresh.Status.LastRewrapAt = &now
		return r.client.Status().Update(ctx, fresh)
	}); err != nil {
		log.Error(err, "persist rewrapped EncryptionKey", "name", ek.Name)
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: r.requeueAfter}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *EncryptionKeyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&topolvmv1.EncryptionKey{}).
		Complete(r)
}
