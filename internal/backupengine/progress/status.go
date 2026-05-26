/*
Copyright AppsCode Inc. and Contributors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package progress

import (
	"context"
	"fmt"

	"github.com/docker/go-units"
	v1 "github.com/topolvm/topolvm/api/v1"
	"gomodules.xyz/restic"
	kmc "kmodules.xyz/client-go/client"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (pg *Progress) setBackupProgress(_ string, status *restic.ResticStatus) error {
	progress := &v1.OperationProgress{
		SecondsElapsed: status.SecondsElapsed,
		TotalFiles:     int64(status.TotalFiles),
		TransferDone:   units.HumanSize(float64(status.BytesDone)),
	}
	if status.TotalBytes > 0 {
		progress.Total = units.HumanSize(float64(status.TotalBytes))
	}
	if status.PercentDone*100 > 0 {
		progress.PercentDone = fmt.Sprintf("%.2f%%", status.PercentDone*100)
	}
	if status.SecondsElapsed > 0 {
		progress.Speed = units.HumanSize(float64(status.BytesDone) / float64(status.SecondsElapsed)) + "/s"
	}

	if err := pg.updateLogicalVolumeSnapshotStatus(progress); err != nil {
		return fmt.Errorf("failed to update logical volume snapshot status: %w", err)
	}

	return nil
}

func (pg *Progress) setRestoreProgress(_ string, status *restic.ResticStatus) error {
	progress := &v1.OperationProgress{
		SecondsElapsed: status.SecondsElapsed,
		TotalFiles:     int64(status.TotalFiles),
		TransferDone:   units.HumanSize(float64(status.BytesRestored)),
	}
	if status.TotalBytes > 0 {
		progress.Total = units.HumanSize(float64(status.TotalBytes))
	}
	if status.PercentDone*100 > 0 {
		progress.PercentDone = fmt.Sprintf("%.2f%%", status.PercentDone*100)
	}
	if status.TotalFiles > 0 && status.PercentDone > 0 {
		progress.FilesDone = int64(float64(status.TotalFiles) * status.PercentDone)
	}
	if status.SecondsElapsed > 0 {
		progress.Speed = units.HumanSize(float64(status.BytesRestored) / float64(status.SecondsElapsed)) + "/s"
	}

	if err := pg.updateLogicalVolumeSnapshotStatus(progress); err != nil {
		return fmt.Errorf("failed to update logical volume snapshot status: %w", err)
	}

	return nil
}

func (pg *Progress) updateLogicalVolumeSnapshotStatus(progress *v1.OperationProgress) error {
	_, err := kmc.PatchStatus(
		context.Background(),
		pg.kbClient,
		pg.logicalVol,
		func(obj client.Object) client.Object {
			in := obj.(*v1.LogicalVolume)
			if in.Status.Snapshot == nil {
				in.Status.Snapshot = &v1.SnapshotStatus{}
			}
			in.Status.Snapshot.Progress = progress
			return in
		})
	return err
}
