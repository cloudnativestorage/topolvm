package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	v1 "github.com/topolvm/topolvm/api/v1"
	"github.com/topolvm/topolvm/internal/backupengine/progress"
	"gomodules.xyz/restic"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	defaultScratchDir = "/tmp"
)

// NewResticProvider creates a new Restic repository provider
func NewResticProvider(client client.Client, snapStorage *v1.SnapshotBackupStorage, ri *RepoInf, lv *v1.LogicalVolume) (Provider, error) {
	backend := &restic.Backend{ConfigResolver: v1.NewSnapshotStorageResolver(client, snapStorage)}
	if ri != nil {
		backend.Repository = ri.Name
		backend.Directory = ri.Path
	}
	setupOptions := &restic.SetupOptions{
		ScratchDir: defaultScratchDir,
		Backends:   []*restic.Backend{backend},
	}
	wrapper, err := restic.NewResticWrapper(setupOptions)
	if err != nil {
		return nil, fmt.Errorf("failed to create restic wrapper: %w", err)
	}
	if ri != nil {
		ri.Repository = wrapper.Config.GetBackend(ri.Name).Envs[restic.RESTIC_REPOSITORY]
	}
	return &resticProvider{
		logiclVol: lv,
		client:    client,
		wrapper:   wrapper,
	}, nil
}

type resticProvider struct {
	client    client.Client
	wrapper   *restic.ResticWrapper
	logiclVol *v1.LogicalVolume
}

// Backup creates a new snapshot
func (r *resticProvider) Backup(ctx context.Context, param *BackupParam) (*BackupResult, error) {
	fail := func(err error, msg string) (*BackupResult, error) {
		return &BackupResult{
			Phase:        BackupPhaseFailed,
			ErrorMessage: fmt.Sprintf(msg, err),
			Provider:     string(v1.EngineRestic),
			Hostname:     param.Repo.Hostname,
			Path:         param.Repo.Path,
			Paths:        param.BackupPaths,
			Repository:   param.Repo.Repository,
		}, err
	}

	if exist := r.wrapper.RepositoryAlreadyExist(param.Repo.Name); !exist {
		err := r.wrapper.InitializeRepository(param.Repo.Name)
		if err != nil {
			return fail(err, "failed to initialize repository: %v")
		}
	}
	err := r.wrapper.EnsureNoExclusiveLock(param.Client, param.Namespace)
	if err != nil {
		return fail(err, "failed to ensure to no exclusive lock: %v")
	}

	backupOptions := restic.BackupOptions{
		Host:        param.Repo.Hostname,
		BackupPaths: param.BackupPaths,
		Exclude:     param.Exclude,
		Args:        param.Args,
	}
	fmt.Println("######################### Running with Progress Reporter")
	progressRptr := progress.NewProgressReporter(r.client, r.wrapper, r.logiclVol, v1.OperationBackup)
	progressRptr.Start()
	defer progressRptr.Stop()

	out, err := r.wrapper.RunBackup(backupOptions)
	if err != nil {
		return fail(err, "failed to execute backup: %v")
	}
	result := convertOutputToBackupResult(out, param)
	return result, nil
}

// Restore restores files from a snapshot
func (r *resticProvider) Restore(ctx context.Context, param *RestoreParam) (*RestoreResult, error) {
	restoreOpt := restic.RestoreOptions{
		Host:         param.Repo.Hostname,
		Destination:  param.Destination,
		RestorePaths: param.RestorePaths,
		Snapshots: []string{
			param.SnapshotID,
		},
		Exclude: param.Exclude,
		Include: param.Include,
		Args:    param.Args,
	}

	fmt.Println("######################### Running Restore with Progress Reporter")
	progressRptr := progress.NewProgressReporter(r.client, r.wrapper, r.logiclVol, v1.OperationRestore)
	progressRptr.Start()
	defer progressRptr.Stop()

	out, err := r.wrapper.RunRestore(param.Repo.Name, restoreOpt)
	if err != nil {
		return &RestoreResult{
			Phase:        RestoreFailed,
			ErrorMessage: fmt.Sprintf("restore failed: %v", err),
			Provider:     string(v1.EngineRestic),
			Hostname:     param.Repo.Hostname,
			Repository:   param.Repo.Repository,
		}, err
	}
	result := convertOutputToRestoreResult(out, param)
	return result, nil
}

func (r *resticProvider) Delete(ctx context.Context, param *DeleteParam) ([]byte, error) {
	err := r.wrapper.EnsureNoExclusiveLock(param.Client, param.Namespace)
	if err != nil {
		return nil, fmt.Errorf("failed to ensure to no exclusive lock: %v", err)
	}
	return r.wrapper.DeleteSnapshots(param.Repo.Name, param.SnapshotIDs)
}

func (r *resticProvider) ValidateConnection(ctx context.Context) error {
	type resticErrorJSON struct {
		MessageType string `json:"message_type"`
		Code        int    `json:"code"`
		Message     string `json:"message"`
	}

	//r.wrapper.SetCombineOutput(true)
	//out, err := r.wrapper.ValidateConnection()
	var resticErr resticErrorJSON
	out := []byte(`{"message_type":"error","code":1,"message":"error message"}`)
	jsonErr := json.Unmarshal(out, &resticErr)

	// It approach will work only once this PR gets merged: https://github.com/restic/restic/pull/5570
	if jsonErr == nil && resticErr.Message != "" {
		msg := resticErr.Message
		switch {
		case strings.Contains(msg, "repository does not exist"),
			strings.Contains(msg, "The specified key does not exist"):
			return nil

		case strings.Contains(msg, "Access Denied"):
			return fmt.Errorf("invalid credentials: %s", msg)

		case strings.Contains(msg, "no such host"):
			return fmt.Errorf("invalid endpoint or DNS resolution failed: %s", msg)

		case strings.Contains(msg, "The specified bucket does not exist"):
			return fmt.Errorf("bucket not found: %s", msg)

		default:
			return fmt.Errorf("backend verification failed: %s", msg)
		}
	}
	//if err != nil || len(out) == 0 || jsonErr != nil {
	//	return fmt.Errorf("restic command failed: %v\nOutput: %s\nJson extract failed:%v", err, string(out), jsonErr)
	//}

	return nil
}

func safeInt64(val *int64) int64 {
	if val == nil {
		return 0
	}
	return *val
}

func convertOutputToRestoreResult(output *restic.RestoreOutput, param *RestoreParam) *RestoreResult {
	result := &RestoreResult{
		Provider:    string(v1.EngineRestic),
		Hostname:    param.Repo.Hostname,
		Repository:  param.Repo.Repository,
		RestoreTime: time.Now(),
	}

	if output == nil || len(output.Stats) == 0 {
		result.Phase = RestoreFailed
		result.ErrorMessage = "no restore statistics available"
		return result
	}

	hostStats := output.Stats[0]
	if hostStats.Phase == restic.HostRestoreSucceeded {
		result.Phase = RestoreSucceeded
	} else {
		result.Phase = RestoreFailed
		result.ErrorMessage = hostStats.Error
	}
	result.Duration = hostStats.Duration
	return result
}

func convertOutputToBackupResult(output []restic.BackupOutput, param *BackupParam) *BackupResult {
	result := &BackupResult{
		Provider:   string(v1.EngineRestic),
		Hostname:   param.Repo.Hostname,
		Path:       param.Repo.Path,
		Paths:      param.BackupPaths,
		Repository: param.Repo.Repository,
		BackupTime: time.Now(),
	}

	if output == nil || len(output[0].Stats) == 0 {
		result.Phase = BackupPhaseFailed
		result.ErrorMessage = "no backup statistics available"
		return result
	}

	hostStats := output[0].Stats[0]
	if hostStats.Phase == restic.HostBackupSucceeded {
		result.Phase = BackupPhaseSucceeded
	} else {
		result.Phase = BackupPhaseFailed
		result.ErrorMessage = hostStats.Error
	}
	result.Duration = hostStats.Duration

	// Extract snapshot information (use first snapshot if multiple)
	if len(hostStats.Snapshots) > 0 {
		snapshot := hostStats.Snapshots[0]
		result.SnapshotID = snapshot.Name
		// Parse size information
		result.Size = BackupSizeInfo{
			TotalFormatted:    snapshot.TotalSize,
			UploadedFormatted: snapshot.Uploaded,
		}

		// Parse file statistics
		result.Files = BackupFileInfo{
			Total:      safeInt64(snapshot.FileStats.TotalFiles),
			New:        safeInt64(snapshot.FileStats.NewFiles),
			Modified:   safeInt64(snapshot.FileStats.ModifiedFiles),
			Unmodified: safeInt64(snapshot.FileStats.UnmodifiedFiles),
		}
	}

	return result
}
