package provider

import (
	"context"
	"fmt"

	v1 "github.com/topolvm/topolvm/api/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// GetProvider returns the appropriate snapshot provider based on the storage configuration
func GetProvider(ctx context.Context, client client.Client, snapStorage *v1.SnapshotBackupStorage, ri *RepoInf, lv *v1.LogicalVolume) (Provider, error) {
	switch snapStorage.Spec.Engine {
	case v1.EngineRestic:
		return NewResticProvider(ctx, client, snapStorage, ri, lv)
	case v1.EngineKopia:
		return NewKopiaProvider()
	default:
		return nil, fmt.Errorf("unsupported snapshot engine: %s", snapStorage.Spec.Engine)
	}
}
