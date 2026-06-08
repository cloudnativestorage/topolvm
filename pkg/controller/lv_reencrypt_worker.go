package controller

import (
	internalController "github.com/topolvm/topolvm/internal/controller"
	"github.com/topolvm/topolvm/internal/crypt"
	"github.com/topolvm/topolvm/internal/keyprovider"
	ctrl "sigs.k8s.io/controller-runtime"
)

// SetupLVReencryptWorker registers the node-scoped reencrypt worker.
func SetupLVReencryptWorker(mgr ctrl.Manager, cm crypt.Manager, kp keyprovider.KeyProvider, nodeName string, maxConcurrentPerNode int, devicePathFn func(volumeID string) string) error {
	w := internalController.NewLVReencryptWorker(mgr.GetClient(), cm, kp, nodeName, maxConcurrentPerNode, devicePathFn)
	return w.SetupWithManager(mgr)
}
