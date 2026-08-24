package controller

import (
	internalController "github.com/topolvm/topolvm/internal/controller"
	"github.com/topolvm/topolvm/internal/crypt"
	"github.com/topolvm/topolvm/internal/keyprovider"
	"k8s.io/utils/keymutex"
	ctrl "sigs.k8s.io/controller-runtime"
)

// SetupLVReencryptWorker registers the node-scoped reencrypt worker.
// deviceLocks must be the same instance passed to the node server so the
// worker's cryptsetup reencrypt calls serialize against NodePublishVolume /
// NodeUnpublishVolume for the same volume.
func SetupLVReencryptWorker(
	mgr ctrl.Manager,
	cm crypt.Manager,
	kp keyprovider.KeyProvider,
	nodeName string,
	maxConcurrentPerNode int,
	devicePathFn func(volumeID string) string,
	deviceLocks keymutex.KeyMutex,
) error {
	w := internalController.NewLVReencryptWorker(
		mgr.GetClient(), cm, kp, nodeName, maxConcurrentPerNode, devicePathFn, deviceLocks,
	)
	return w.SetupWithManager(mgr)
}
