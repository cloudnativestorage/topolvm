package controller

import (
	internalController "github.com/topolvm/topolvm/internal/controller"
	ctrl "sigs.k8s.io/controller-runtime"
)

// SetupReencryptRequestReconciler wires the ReencryptRequest reconciler.
func SetupReencryptRequestReconciler(mgr ctrl.Manager) error {
	r := internalController.NewReencryptRequestReconciler(mgr.GetClient())
	return r.SetupWithManager(mgr)
}
