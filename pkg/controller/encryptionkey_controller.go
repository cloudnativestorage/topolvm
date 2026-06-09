package controller

import (
	"time"

	internalController "github.com/topolvm/topolvm/internal/controller"
	"github.com/topolvm/topolvm/internal/keyprovider"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SetupEncryptionKeyReconciler wires the EncryptionKey rewrap reconciler.
func SetupEncryptionKeyReconciler(mgr ctrl.Manager, _ client.Client, kp keyprovider.KeyProvider, requeue time.Duration) error {
	r := internalController.NewEncryptionKeyReconciler(mgr.GetClient(), kp, requeue)
	return r.SetupWithManager(mgr)
}
