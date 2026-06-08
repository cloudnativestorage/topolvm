package controller

import (
	"context"
	"encoding/base64"
	"fmt"
	"sync"

	topolvmv1 "github.com/topolvm/topolvm/api/v1"
	"github.com/topolvm/topolvm/internal/crypt"
	"github.com/topolvm/topolvm/internal/keyprovider"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crlog "sigs.k8s.io/controller-runtime/pkg/log"
)

// LVReencryptWorker is a node-scoped reconciler that runs cryptsetup
// reencrypt when a LogicalVolume's encryption.state flips to Reencrypting.
// It enforces MaxConcurrent locally so a single node can't be overwhelmed.
type LVReencryptWorker struct {
	client       client.Client
	crypt        crypt.Manager
	keyProvider  keyprovider.KeyProvider
	nodeName     string
	devicePathFn func(volumeID string) string

	mu       sync.Mutex
	inflight map[string]struct{}
	maxConc  int
}

// NewLVReencryptWorker builds a worker bound to a specific node.
// devicePathFn maps volumeID -> /dev/topolvm/<uuid> (callers wire this from
// the existing node-side LV resolution).
func NewLVReencryptWorker(c client.Client, cm crypt.Manager, kp keyprovider.KeyProvider, nodeName string, maxConc int, devicePathFn func(string) string) *LVReencryptWorker {
	if maxConc <= 0 {
		maxConc = 1
	}
	return &LVReencryptWorker{
		client:       c,
		crypt:        cm,
		keyProvider:  kp,
		nodeName:     nodeName,
		devicePathFn: devicePathFn,
		inflight:     map[string]struct{}{},
		maxConc:      maxConc,
	}
}

// Reconcile triggers reencryption when state=Reencrypting and the LV is on
// this node. cryptsetup --resilience checksum lets the operation resume
// after a crash, so an already-running reencrypt continues from header state.
func (w *LVReencryptWorker) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := crlog.FromContext(ctx).WithValues("worker", "lv-reencrypt")

	lv := &topolvmv1.LogicalVolume{}
	if err := w.client.Get(ctx, req.NamespacedName, lv); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if lv.Spec.NodeName != w.nodeName {
		return ctrl.Result{}, nil
	}
	if lv.Spec.Encryption == nil || !lv.Spec.Encryption.Enabled {
		return ctrl.Result{}, nil
	}
	if lv.Status.Encryption == nil || lv.Status.Encryption.State != topolvmv1.EncryptionReencrypting {
		return ctrl.Result{}, nil
	}

	w.mu.Lock()
	if _, ok := w.inflight[lv.Name]; ok {
		w.mu.Unlock()
		return ctrl.Result{}, nil
	}
	if len(w.inflight) >= w.maxConc {
		w.mu.Unlock()
		return ctrl.Result{Requeue: true}, nil
	}
	w.inflight[lv.Name] = struct{}{}
	w.mu.Unlock()

	go w.runReencrypt(context.Background(), lv.DeepCopy(), log)
	return ctrl.Result{}, nil
}

func (w *LVReencryptWorker) runReencrypt(ctx context.Context, lv *topolvmv1.LogicalVolume, log interface {
	Info(msg string, kv ...any)
	Error(err error, msg string, kv ...any)
}) {
	defer func() {
		w.mu.Lock()
		delete(w.inflight, lv.Name)
		w.mu.Unlock()
	}()

	dev := w.devicePathFn(lv.Status.VolumeID)
	if dev == "" {
		log.Error(fmt.Errorf("empty device path"), "reencrypt failed", "lv", lv.Name)
		_ = w.markError(ctx, lv.Name, "no device path")
		return
	}
	pass, err := w.unwrap(ctx, lv)
	if err != nil {
		log.Error(err, "reencrypt unwrap", "lv", lv.Name)
		_ = w.markError(ctx, lv.Name, err.Error())
		return
	}
	defer pass.Destroy()

	if err := w.crypt.Reencrypt(ctx, dev, pass, crypt.ReencryptOpts{Resilience: "checksum"}); err != nil {
		log.Error(err, "cryptsetup reencrypt", "lv", lv.Name)
		_ = w.markError(ctx, lv.Name, err.Error())
		return
	}
	log.Info("reencrypt completed", "lv", lv.Name)
	if err := w.markDone(ctx, lv.Name); err != nil {
		log.Error(err, "persist reencrypt completion", "lv", lv.Name)
	}
}

func (w *LVReencryptWorker) unwrap(ctx context.Context, lv *topolvmv1.LogicalVolume) (crypt.SecretBuf, error) {
	if lv.Status.Encryption == nil || lv.Status.Encryption.ActiveKeyID == "" {
		return nil, fmt.Errorf("LV %s has no ActiveKeyID", lv.Name)
	}
	ek := &topolvmv1.EncryptionKey{}
	if err := w.client.Get(ctx, client.ObjectKey{Name: lv.Status.Encryption.ActiveKeyID}, ek); err != nil {
		return nil, err
	}
	ct, err := base64.StdEncoding.DecodeString(ek.Status.WrappedDEK)
	if err != nil {
		return nil, err
	}
	return w.keyProvider.Unwrap(ctx, keyprovider.WrappedKey{
		Ciphertext: ct,
		KeyRef:     ek.Spec.KeyRef,
		KEKVersion: ek.Status.KEKVersion,
		Provider:   ek.Spec.Provider,
	}, ek.Status.BoundVolumeID)
}

func (w *LVReencryptWorker) markDone(ctx context.Context, name string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		lv := &topolvmv1.LogicalVolume{}
		if err := w.client.Get(ctx, client.ObjectKey{Name: name}, lv); err != nil {
			return err
		}
		if lv.Status.Encryption == nil {
			lv.Status.Encryption = &topolvmv1.EncryptionStatus{}
		}
		lv.Status.Encryption.MasterKeyEpoch++
		lv.Status.Encryption.State = topolvmv1.EncryptionOpened
		return w.client.Status().Update(ctx, lv)
	})
}

func (w *LVReencryptWorker) markError(ctx context.Context, name, msg string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		lv := &topolvmv1.LogicalVolume{}
		if err := w.client.Get(ctx, client.ObjectKey{Name: name}, lv); err != nil {
			return err
		}
		if lv.Status.Encryption == nil {
			lv.Status.Encryption = &topolvmv1.EncryptionStatus{}
		}
		lv.Status.Encryption.State = topolvmv1.EncryptionError
		_ = msg // message is logged separately; we do not persist arbitrary messages to status to keep the surface small.
		return w.client.Status().Update(ctx, lv)
	})
}

// SetupWithManager wires the worker to watch LogicalVolume changes.
func (w *LVReencryptWorker) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&topolvmv1.LogicalVolume{}).
		Named("lvreencryptworker").
		Complete(w)
}
