/*
Copyright 2020 The Kubernetes Authors.

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

package state

import (
	"os"
	"path"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	resourcev1alpha2 "k8s.io/api/resource/v1alpha2"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/kubernetes/pkg/kubelet/checkpointmanager"
	testutil "k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/state/testing"
)

const testingCheckpoint = "dramanager_checkpoint_test"

// assertStateEqual marks provided test as failed if provided states differ
func assertStateEqual(t *testing.T, restoredState, expectedState ClaimInfoStateList) {
	assert.Equal(t, expectedState, restoredState, "expected ClaimInfoState does not equal to restored one")
}

func TestCheckpointGetOrCreate(t *testing.T) {
	testCases := []struct {
		description       string
		checkpointContent string
		expectedError     string
		expectedState     ClaimInfoStateList
	}{
		{
			"Create non-existing checkpoint",
			"",
			"",
			[]ClaimInfoState{},
		},
		{
			"Restore checkpoint - single claim",
			`{"version":"v1","entries":[{"DriverName":"test-driver.cdi.k8s.io","ClassName":"class-name","ClaimUID":"067798be-454e-4be4-9047-1aa06aea63f7","ClaimName":"example","Namespace":"default","PodUIDs":{"139cdb46-f989-4f17-9561-ca10cfb509a6":{}},"ResourceHandles":[{"driverName":"test-driver.cdi.k8s.io","data":"{\"a\": \"b\"}"}],"CDIDevices":{"test-driver.cdi.k8s.io":["example.com/example=cdi-example"]}}],"checksum":4194867564}`,
			"",
			[]ClaimInfoState{
				{
					DriverName: "test-driver.cdi.k8s.io",
					ClassName:  "class-name",
					ClaimUID:   "067798be-454e-4be4-9047-1aa06aea63f7",
					ClaimName:  "example",
					Namespace:  "default",
					PodUIDs:    sets.New("139cdb46-f989-4f17-9561-ca10cfb509a6"),
					ResourceHandles: []resourcev1alpha2.ResourceHandle{
						{
							DriverName: "test-driver.cdi.k8s.io",
							Data:       `{"a": "b"}`,
						},
					},
					CDIDevices: map[string][]string{
						"test-driver.cdi.k8s.io": {"example.com/example=cdi-example"},
					},
				},
			},
		},
		{
			"Restore checkpoint - single claim - multiple devices",
			`{"version":"v1","entries":[{"DriverName":"meta-test-driver.cdi.k8s.io","ClassName":"class-name","ClaimUID":"067798be-454e-4be4-9047-1aa06aea63f7","ClaimName":"example","Namespace":"default","PodUIDs":{"139cdb46-f989-4f17-9561-ca10cfb509a6":{}},"ResourceHandles":[{"driverName":"test-driver-1.cdi.k8s.io","data":"{\"a\": \"b\"}"},{"driverName":"test-driver-2.cdi.k8s.io","data":"{\"c\": \"d\"}"}],"CDIDevices":{"test-driver-1.cdi.k8s.io":["example-1.com/example-1=cdi-example-1"],"test-driver-2.cdi.k8s.io":["example-2.com/example-2=cdi-example-2"]}}],"checksum":360176657}`,
			"",
			[]ClaimInfoState{
				{
					DriverName: "meta-test-driver.cdi.k8s.io",
					ClassName:  "class-name",
					ClaimUID:   "067798be-454e-4be4-9047-1aa06aea63f7",
					ClaimName:  "example",
					Namespace:  "default",
					PodUIDs:    sets.New("139cdb46-f989-4f17-9561-ca10cfb509a6"),
					ResourceHandles: []resourcev1alpha2.ResourceHandle{
						{
							DriverName: "test-driver-1.cdi.k8s.io",
							Data:       `{"a": "b"}`,
						},
						{
							DriverName: "test-driver-2.cdi.k8s.io",
							Data:       `{"c": "d"}`,
						},
					},
					CDIDevices: map[string][]string{
						"test-driver-1.cdi.k8s.io": {"example-1.com/example-1=cdi-example-1"},
						"test-driver-2.cdi.k8s.io": {"example-2.com/example-2=cdi-example-2"},
					},
				},
			},
		},
		{
			"Restore checkpoint - multiple claims",
			`{"version":"v1","entries":[{"DriverName":"test-driver.cdi.k8s.io","ClassName":"class-name-1","ClaimUID":"067798be-454e-4be4-9047-1aa06aea63f7","ClaimName":"example-1","Namespace":"default","PodUIDs":{"139cdb46-f989-4f17-9561-ca10cfb509a6":{}},"ResourceHandles":[{"driverName":"test-driver.cdi.k8s.io","data":"{\"a\": \"b\"}"}],"CDIDevices":{"test-driver.cdi.k8s.io":["example.com/example=cdi-example-1"]}},{"DriverName":"test-driver.cdi.k8s.io","ClassName":"class-name-2","ClaimUID":"4cf8db2d-06c0-7d70-1a51-e59b25b2c16c","ClaimName":"example-2","Namespace":"default","PodUIDs":{"139cdb46-f989-4f17-9561-ca10cfb509a6":{}},"ResourceHandles":[{"driverName":"test-driver.cdi.k8s.io","data":"{\"c\": \"d\"}"}],"CDIDevices":{"test-driver.cdi.k8s.io":["example.com/example=cdi-example-2"]}}],"checksum":103176902}`,
			"",
			[]ClaimInfoState{
				{
					DriverName: "test-driver.cdi.k8s.io",
					ClassName:  "class-name-1",
					ClaimUID:   "067798be-454e-4be4-9047-1aa06aea63f7",
					ClaimName:  "example-1",
					Namespace:  "default",
					PodUIDs:    sets.New("139cdb46-f989-4f17-9561-ca10cfb509a6"),
					ResourceHandles: []resourcev1alpha2.ResourceHandle{
						{
							DriverName: "test-driver.cdi.k8s.io",
							Data:       `{"a": "b"}`,
						},
					},
					CDIDevices: map[string][]string{
						"test-driver.cdi.k8s.io": {"example.com/example=cdi-example-1"},
					},
				},
				{
					DriverName: "test-driver.cdi.k8s.io",
					ClassName:  "class-name-2",
					ClaimUID:   "4cf8db2d-06c0-7d70-1a51-e59b25b2c16c",
					ClaimName:  "example-2",
					Namespace:  "default",
					PodUIDs:    sets.New("139cdb46-f989-4f17-9561-ca10cfb509a6"),
					ResourceHandles: []resourcev1alpha2.ResourceHandle{
						{
							DriverName: "test-driver.cdi.k8s.io",
							Data:       `{"c": "d"}`,
						},
					},
					CDIDevices: map[string][]string{
						"test-driver.cdi.k8s.io": {"example.com/example=cdi-example-2"},
					},
				},
			},
		},
		{
			"Restore checkpoint - invalid checksum",
			`{"version":"v1","entries":[{"DriverName":"test-driver.cdi.k8s.io","ClassName":"class-name","ClaimUID":"067798be-454e-4be4-9047-1aa06aea63f7","ClaimName":"example","Namespace":"default","PodUIDs":{"139cdb46-f989-4f17-9561-ca10cfb509a6":{}},"CDIDevices":{"test-driver.cdi.k8s.io":["example.com/example=cdi-example"]}}],"checksum":1988120168}`,
			"checkpoint is corrupted",
			[]ClaimInfoState{},
		},
		{
			"Restore checkpoint with invalid JSON",
			`{`,
			"unexpected end of JSON input",
			[]ClaimInfoState{},
		},
	}

	// create temp dir
	testingDir, err := os.MkdirTemp("", "dramanager_state_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(testingDir)

	// create checkpoint manager for testing
	cpm, err := checkpointmanager.NewCheckpointManager(testingDir)
	assert.NoError(t, err, "could not create testing checkpoint manager")

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			// ensure there is no previous checkpoint
			assert.NoError(t, cpm.RemoveCheckpoint(testingCheckpoint), "could not remove testing checkpoint")

			// prepare checkpoint for testing
			if strings.TrimSpace(tc.checkpointContent) != "" {
				checkpoint := &testutil.MockCheckpoint{Content: tc.checkpointContent}
				assert.NoError(t, cpm.CreateCheckpoint(testingCheckpoint, checkpoint), "could not create testing checkpoint")
			}

			var state ClaimInfoStateList

			checkpointState, err := NewCheckpointState(testingDir, testingCheckpoint)

			if err == nil {
				state, err = checkpointState.GetOrCreate()
			}
			if strings.TrimSpace(tc.expectedError) != "" {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tc.expectedError)
			} else {
				assert.NoError(t, err, "unexpected error while creating checkpointState")
				// compare state after restoration with the one expected
				assertStateEqual(t, state, tc.expectedState)
			}
		})
	}
}

func TestCheckpointStateStore(t *testing.T) {
	claimInfoState := ClaimInfoState{
		DriverName: "test-driver.cdi.k8s.io",
		ClassName:  "class-name",
		ClaimUID:   "067798be-454e-4be4-9047-1aa06aea63f7",
		ClaimName:  "example",
		Namespace:  "default",
		PodUIDs:    sets.New("139cdb46-f989-4f17-9561-ca10cfb509a6"),
		ResourceHandles: []resourcev1alpha2.ResourceHandle{
			{
				DriverName: "test-driver.cdi.k8s.io",
				Data:       `{"a": "b"}`,
			},
		},
		CDIDevices: map[string][]string{
			"test-driver.cdi.k8s.io": {"example.com/example=cdi-example"},
		},
	}

	expectedCheckpoint := `{"version":"v1","entries":[{"DriverName":"test-driver.cdi.k8s.io","ClassName":"class-name","ClaimUID":"067798be-454e-4be4-9047-1aa06aea63f7","ClaimName":"example","Namespace":"default","PodUIDs":{"139cdb46-f989-4f17-9561-ca10cfb509a6":{}},"ResourceHandles":[{"driverName":"test-driver.cdi.k8s.io","data":"{\"a\": \"b\"}"}],"CDIDevices":{"test-driver.cdi.k8s.io":["example.com/example=cdi-example"]}}],"checksum":4194867564}`

	// Should return an error, stateDir cannot be an empty string
	if _, err := NewCheckpointState("", testingCheckpoint); err == nil {
		t.Fatal("expected error but got nil")
	}

	// create temp dir
	testingDir, err := os.MkdirTemp("", "dramanager_state_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(testingDir)

	cpm, err := checkpointmanager.NewCheckpointManager(testingDir)
	assert.NoError(t, err, "could not create testing checkpoint manager")
	assert.NoError(t, cpm.RemoveCheckpoint(testingCheckpoint), "could not remove testing checkpoint")

	cs, err := NewCheckpointState(testingDir, testingCheckpoint)
	assert.NoError(t, err, "could not create testing checkpointState instance")
	err = cs.Store(ClaimInfoStateList{claimInfoState})
	assert.NoError(t, err, "could not store ClaimInfoState")
	checkpoint := NewDRAManagerCheckpoint()
	cpm.GetCheckpoint(testingCheckpoint, checkpoint)
	checkpointData, err := checkpoint.MarshalCheckpoint()
	assert.NoError(t, err, "could not Marshal Checkpoint")
	assert.Equal(t, expectedCheckpoint, string(checkpointData), "expected ClaimInfoState does not equal to restored one")

	// NewCheckpointState with an empty checkpointName should return an error
	if _, err = NewCheckpointState(testingDir, ""); err == nil {
		t.Fatal("expected error but got nil")
	}
}

func TestOldCheckpointRestore(t *testing.T) {
	testingDir := t.TempDir()
	cpm, err := checkpointmanager.NewCheckpointManager(testingDir)
	assert.NoError(t, err, "could not create testing checkpoint manager")

	oldCheckpointData := `{"version":"v1","entries":[{"DriverName":"test-driver.cdi.k8s.io","ClassName":"class-name","ClaimUID":"067798be-454e-4be4-9047-1aa06aea63f7","ClaimName":"example","Namespace":"default","PodUIDs":{"139cdb46-f989-4f17-9561-ca10cfb509a6":{}},"CDIDevices":{"test-driver.cdi.k8s.io":["example.com/example=cdi-example"]}}],"checksum":153446146}`
	err = os.WriteFile(path.Join(testingDir, testingCheckpoint), []byte(oldCheckpointData), 0644)
	assert.NoError(t, err, "could not store checkpoint data")

	checkpoint := NewDRAManagerCheckpoint()
	err = cpm.GetCheckpoint(testingCheckpoint, checkpoint)
	assert.NoError(t, err, "could not restore checkpoint")

	checkpointData, err := checkpoint.MarshalCheckpoint()
	assert.NoError(t, err, "could not Marshal Checkpoint")

	expectedData := `{"version":"v1","entries":[{"DriverName":"test-driver.cdi.k8s.io","ClassName":"class-name","ClaimUID":"067798be-454e-4be4-9047-1aa06aea63f7","ClaimName":"example","Namespace":"default","PodUIDs":{"139cdb46-f989-4f17-9561-ca10cfb509a6":{}},"ResourceHandles":null,"CDIDevices":{"test-driver.cdi.k8s.io":["example.com/example=cdi-example"]}}],"checksum":453625682}`
	assert.Equal(t, expectedData, string(checkpointData), "expected ClaimInfoState does not equal to restored one")
}
