package provider

import (
	"context"
	"fmt"
)

// NewKopiaProvider creates a new Kopia repository provider
func NewKopiaProvider() (Provider, error) {
	return &kopiaProvider{}, nil
}

type kopiaProvider struct {
}

// Backup creates a new snapshot
func (k *kopiaProvider) Backup(_ context.Context, param *BackupParam) (*BackupResult, error) {
	// TODO: Implement Kopia backup
	return &BackupResult{
		Phase:        BackupPhaseFailed,
		ErrorMessage: "kopia provider not implemented yet",
		Provider:     "kopia",
		Hostname:     param.Repo.Hostname,
		Paths:        param.BackupPaths,
		Repository:   param.Repo.Name,
	}, fmt.Errorf("kopia provider not implemented yet")
}

// Restore restores files from a snapshot
func (k *kopiaProvider) Restore(_ context.Context, _ *RestoreParam) (*RestoreResult, error) {
	return nil, fmt.Errorf("kopia provider not implemented yet")
}

func (k *kopiaProvider) Delete(ctx context.Context, param *DeleteParam) ([]byte, error) {
	//TODO implement me
	panic("implement me")
}

func (k *kopiaProvider) ValidateConnection(ctx context.Context) error {
	//TODO implement me
	panic("implement me")
}
