/*
Copyright 2022 The Kruise Authors.

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

package protomanager

import (
	"fmt"
	"io/ioutil"
	"os"

	"k8s.io/klog/v2"

	"github.com/gogo/protobuf/proto"

	ctrlmeshproto "github.com/openkruise/controllermesh/apis/ctrlmesh/proto"
)

const (
	expectedSpecFilePath    = "/ctrlmesh/expected-spec"
	currentSpecFilePath     = "/ctrlmesh/current-spec"
	historyRequestsFilePath = "/ctrlmesh/history-requests"

	testBlockLoadingFilePath = "/ctrlmesh/mock-proto-manage-failure"
)

type storage struct {
	expectedSpecFile    *os.File
	currentSpecFile     *os.File
	historyRequestsFile *os.File
}

func newStorage() (*storage, error) {
	var err error
	s := &storage{}
	if err = s.mockFailure(); err != nil {
		// block here
		klog.Warningf("Block new storage: %v", err)
		ch := make(chan struct{})
		<-ch
	}
	s.expectedSpecFile, err = os.OpenFile(expectedSpecFilePath, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return nil, err
	}
	s.currentSpecFile, err = os.OpenFile(currentSpecFilePath, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return nil, err
	}
	s.historyRequestsFile, err = os.OpenFile(historyRequestsFilePath, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (s *storage) loadData(reqStore *requestsStore) (expectedSpec, currentSpec *ctrlmeshproto.ProxySpecV1, err error) {
	expectedSpecBytes, err := ioutil.ReadAll(s.expectedSpecFile)
	if err != nil {
		return nil, nil, err
	}
	currentSpecBytes, err := ioutil.ReadAll(s.currentSpecFile)
	if err != nil {
		return nil, nil, err
	}
	historyRequestsBytes, err := ioutil.ReadAll(s.historyRequestsFile)
	if err != nil {
		return nil, nil, err
	}
	if len(expectedSpecBytes) > 0 {
		expectedSpec = &ctrlmeshproto.ProxySpecV1{}
		if err = proto.Unmarshal(expectedSpecBytes, expectedSpec); err != nil {
			return nil, nil, err
		}
	}
	if len(currentSpecBytes) > 0 {
		currentSpec = &ctrlmeshproto.ProxySpecV1{}
		if err = proto.Unmarshal(currentSpecBytes, currentSpec); err != nil {
			return nil, nil, err
		}
	}
	if len(historyRequestsBytes) > 0 {
		if err = reqStore.unmarshal(historyRequestsBytes); err != nil {
			return nil, nil, err
		}
	}
	return
}

func (s *storage) writeExpectedSpec(spec *ctrlmeshproto.ProxySpecV1) error {
	var err error
	if err = s.mockFailure(); err != nil {
		return err
	}
	b, err := proto.Marshal(spec)
	if err != nil {
		return err
	}
	_, err = s.expectedSpecFile.Write(b)
	return err
}

func (s *storage) writeCurrentSpec(spec *ctrlmeshproto.ProxySpecV1) error {
	var err error
	if err = s.mockFailure(); err != nil {
		return err
	}
	b, err := proto.Marshal(spec)
	if err != nil {
		return err
	}
	_, err = s.currentSpecFile.Write(b)
	return err
}

func (s *storage) writeHistoryRequests(reqStore *requestsStore) error {
	var err error
	if err = s.mockFailure(); err != nil {
		return err
	}
	b, err := reqStore.marshal()
	if err != nil {
		return err
	}
	_, err = s.historyRequestsFile.Write(b)
	return err
}

func (s *storage) mockFailure() error {
	if _, err := os.Stat(testBlockLoadingFilePath); err == nil {
		return fmt.Errorf("mock failure for %s exists", testBlockLoadingFilePath)
	}
	return nil
}
