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
	"sync"

	v1 "github.com/topolvm/topolvm/api/v1"
	"gomodules.xyz/restic"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Progress struct {
	kbClient      client.Client
	wg            sync.WaitGroup
	ctx           context.Context
	logicalVol    *v1.LogicalVolume
	cancel        context.CancelFunc
	wrapper       *restic.ResticWrapper
	operationType v1.OperationType
}

func NewProgressReporter(kClient client.Client, wrapper *restic.ResticWrapper, lv *v1.LogicalVolume, opType v1.OperationType) *Progress {
	ctx, cancel := context.WithCancel(context.Background())
	pgTyp := &Progress{
		logicalVol:    lv,
		ctx:           ctx,
		cancel:        cancel,
		kbClient:      kClient,
		wrapper:       wrapper,
		operationType: opType,
	}
	return pgTyp
}
