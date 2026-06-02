package controller

import (
	internalController "github.com/topolvm/topolvm/internal/controller"
	"github.com/topolvm/topolvm/pkg/lvmd/proto"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SetupLogicalVolumeReconcilerWithServices creates LogicalVolumeReconciler and sets up with manager.
func SetupLogicalVolumeReconcilerWithServices(
	mgr ctrl.Manager,
	client client.Client,
	nodeName string,
	vgService proto.VGServiceClient,
	lvService proto.LVServiceClient,
) error {
	reconciler := internalController.NewLogicalVolumeReconcilerWithServices(client, nodeName, vgService, lvService)
	return reconciler.SetupWithManager(mgr)
}

// SetupSnapshotPodReconciler creates the SnapshotPodReconciler (which watches
// the snapshot executor pods and flips in-flight operations to Failed when
// the pod disappears) and sets it up with the manager.
func SetupSnapshotPodReconciler(mgr ctrl.Manager, client client.Client) error {
	reconciler := internalController.NewSnapshotPodReconciler(client)
	return reconciler.SetupWithManager(mgr)
}

