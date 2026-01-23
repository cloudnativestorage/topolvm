package v1

import (
	"context"
	"fmt"

	core "k8s.io/api/core/v1"
	"kubestash.dev/apimachinery/pkg/restic"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// NewSnapshotStorageResolver creates a StorageConfigResolver that resolves storage configuration
func NewSnapshotStorageResolver(kbClient client.Client, bs *SnapshotBackupStorage) restic.StorageConfigResolver {
	return func(backend *restic.Backend) error {
		var storageSecretName string
		switch {
		case bs.Spec.Storage.S3 != nil:
			s3 := bs.Spec.Storage.S3
			storageSecretName = s3.SecretName
			backend.StorageConfig = &restic.StorageConfig{
				Provider:       string(ProviderS3),
				Bucket:         s3.Bucket,
				Endpoint:       s3.Endpoint,
				Region:         s3.Region,
				Prefix:         s3.Prefix,
				InsecureTLS:    s3.InsecureTLS,
				MaxConnections: s3.MaxConnections,
			}
		case bs.Spec.Storage.GCS != nil:
			gcs := bs.Spec.Storage.GCS
			storageSecretName = gcs.SecretName
			backend.StorageConfig = &restic.StorageConfig{
				Provider:       string(ProviderGCS),
				Bucket:         gcs.Bucket,
				Prefix:         gcs.Prefix,
				MaxConnections: gcs.MaxConnections,
			}
		case bs.Spec.Storage.Azure != nil:
			azure := bs.Spec.Storage.Azure
			storageSecretName = azure.SecretName
			backend.StorageConfig = &restic.StorageConfig{
				Provider:            string(ProviderAzure),
				Bucket:              azure.Container,
				Prefix:              azure.Prefix,
				AzureStorageAccount: azure.StorageAccount,
				MaxConnections:      azure.MaxConnections,
			}
		default:
			return fmt.Errorf("no storage backend configured in BackupStorage %s/%s", bs.Namespace, bs.Name)
		}

		if storageSecretName != "" {
			secret := &core.Secret{}
			if err := kbClient.Get(context.Background(), client.ObjectKey{Name: storageSecretName, Namespace: bs.Namespace}, secret); err != nil {
				return fmt.Errorf("failed to get storage Secret %s/%s: %w", bs.Namespace, storageSecretName, err)
			}
			backend.StorageSecret = secret
			backend.EncryptionSecret = secret
		}
		return nil
	}
}
