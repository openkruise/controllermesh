/*
Copyright 2021 The Kruise Authors.

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

package client

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/openkruise/controllermesh/apis/ctrlmesh/constants"
	ctrlmeshproto "github.com/openkruise/controllermesh/apis/ctrlmesh/proto"
)

var (
	selfInfo = &ctrlmeshproto.SelfInfo{Namespace: os.Getenv(constants.EnvPodNamespace), Name: os.Getenv(constants.EnvPodName)}
	onceInit sync.Once
)

type Client interface {
	Start(ctx context.Context) error
	GetProtoSpec() (*ctrlmeshproto.InternalRoute, []*ctrlmeshproto.Endpoint, *ctrlmeshproto.ControlInstruction, *ctrlmeshproto.SpecHash, time.Time)
	GetProtoSpecSnapshot() *ProtoSpecSnapshot
}

type ProtoSpecSnapshot struct {
	Ctx    context.Context
	Cancel context.CancelFunc
	Closed chan struct{}
	Mutex  sync.Mutex

	Route              *ctrlmeshproto.InternalRoute
	Endpoints          []*ctrlmeshproto.Endpoint
	SpecHash           *ctrlmeshproto.SpecHash
	ControlInstruction *ctrlmeshproto.ControlInstruction
	RefreshTime        time.Time
}
