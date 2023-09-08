/*
Copyright 2016 The Kubernetes Authors.

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

package kuberuntime

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	runtimetesting "k8s.io/cri-api/pkg/apis/testing"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
)

type podStatusProviderFunc func(uid types.UID, name, namespace string) (*kubecontainer.PodStatus, error)

func (f podStatusProviderFunc) GetPodStatus(_ context.Context, uid types.UID, name, namespace string) (*kubecontainer.PodStatus, error) {
	return f(uid, name, namespace)
}

func TestIsInitContainerFailed(t *testing.T) {
	tests := []struct {
		status      *kubecontainer.Status
		isFailed    bool
		description string
	}{
		{
			status: &kubecontainer.Status{
				State:    kubecontainer.ContainerStateExited,
				ExitCode: 1,
			},
			isFailed:    true,
			description: "Init container in exited state and non-zero exit code should return true",
		},
		{
			status: &kubecontainer.Status{
				State: kubecontainer.ContainerStateUnknown,
			},
			isFailed:    true,
			description: "Init container in unknown state should return true",
		},
		{
			status: &kubecontainer.Status{
				Reason:   "OOMKilled",
				ExitCode: 0,
			},
			isFailed:    true,
			description: "Init container which reason is OOMKilled should return true",
		},
		{
			status: &kubecontainer.Status{
				State:    kubecontainer.ContainerStateExited,
				ExitCode: 0,
			},
			isFailed:    false,
			description: "Init container in exited state and zero exit code should return false",
		},
		{
			status: &kubecontainer.Status{
				State: kubecontainer.ContainerStateRunning,
			},
			isFailed:    false,
			description: "Init container in running state should return false",
		},
		{
			status: &kubecontainer.Status{
				State: kubecontainer.ContainerStateCreated,
			},
			isFailed:    false,
			description: "Init container in created state should return false",
		},
	}
	for i, test := range tests {
		isFailed := isInitContainerFailed(test.status)
		assert.Equal(t, test.isFailed, isFailed, "TestCase[%d]: %s", i, test.description)
	}
}

func TestStableKey(t *testing.T) {
	container := &v1.Container{
		Name:  "test_container",
		Image: "foo/image:v1",
	}
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test_pod",
			Namespace: "test_pod_namespace",
			UID:       "test_pod_uid",
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{*container},
		},
	}
	oldKey := getStableKey(pod, container)

	// Updating the container image should change the key.
	container.Image = "foo/image:v2"
	newKey := getStableKey(pod, container)
	assert.NotEqual(t, oldKey, newKey)
}

func TestToKubeContainer(t *testing.T) {
	c := &runtimeapi.Container{
		Id: "test-id",
		Metadata: &runtimeapi.ContainerMetadata{
			Name:    "test-name",
			Attempt: 1,
		},
		Image:    &runtimeapi.ImageSpec{Image: "test-image"},
		ImageRef: "test-image-ref",
		State:    runtimeapi.ContainerState_CONTAINER_RUNNING,
		Annotations: map[string]string{
			containerHashLabel: "1234",
		},
	}
	expect := &kubecontainer.Container{
		ID: kubecontainer.ContainerID{
			Type: runtimetesting.FakeRuntimeName,
			ID:   "test-id",
		},
		Name:    "test-name",
		ImageID: "test-image-ref",
		Image:   "test-image",
		Hash:    uint64(0x1234),
		State:   kubecontainer.ContainerStateRunning,
	}

	_, _, m, err := createTestRuntimeManager()
	assert.NoError(t, err)
	got, err := m.toKubeContainer(c)
	assert.NoError(t, err)
	assert.Equal(t, expect, got)

	// unable to convert a nil pointer to a runtime container
	_, err = m.toKubeContainer(nil)
	assert.Error(t, err)
	_, err = m.sandboxToKubeContainer(nil)
	assert.Error(t, err)
}

func TestGetImageUser(t *testing.T) {
	_, i, m, err := createTestRuntimeManager()
	assert.NoError(t, err)

	type image struct {
		name     string
		uid      *runtimeapi.Int64Value
		username string
	}

	type imageUserValues struct {
		// getImageUser can return (*int64)(nil) so comparing with *uid will break
		// type cannot be *int64 as Golang does not allow to take the address of a numeric constant"
		uid      interface{}
		username string
		err      error
	}

	tests := []struct {
		description             string
		originalImage           image
		expectedImageUserValues imageUserValues
	}{
		{
			"image without username and uid should return (new(int64), \"\", nil)",
			image{
				name:     "test-image-ref1",
				uid:      (*runtimeapi.Int64Value)(nil),
				username: "",
			},
			imageUserValues{
				uid:      int64(0),
				username: "",
				err:      nil,
			},
		},
		{
			"image with username and no uid should return ((*int64)nil, imageStatus.Username, nil)",
			image{
				name:     "test-image-ref2",
				uid:      (*runtimeapi.Int64Value)(nil),
				username: "testUser",
			},
			imageUserValues{
				uid:      (*int64)(nil),
				username: "testUser",
				err:      nil,
			},
		},
		{
			"image with uid should return (*int64, \"\", nil)",
			image{
				name: "test-image-ref3",
				uid: &runtimeapi.Int64Value{
					Value: 2,
				},
				username: "whatever",
			},
			imageUserValues{
				uid:      int64(2),
				username: "",
				err:      nil,
			},
		},
	}

	i.SetFakeImages([]string{"test-image-ref1", "test-image-ref2", "test-image-ref3"})
	for j, test := range tests {
		ctx := context.Background()
		i.Images[test.originalImage.name].Username = test.originalImage.username
		i.Images[test.originalImage.name].Uid = test.originalImage.uid

		uid, username, err := m.getImageUser(ctx, test.originalImage.name)
		assert.NoError(t, err, "TestCase[%d]", j)

		if test.expectedImageUserValues.uid == (*int64)(nil) {
			assert.Equal(t, test.expectedImageUserValues.uid, uid, "TestCase[%d]", j)
		} else {
			assert.Equal(t, test.expectedImageUserValues.uid, *uid, "TestCase[%d]", j)
		}
		assert.Equal(t, test.expectedImageUserValues.username, username, "TestCase[%d]", j)
	}
}

func TestToRuntimeProtocol(t *testing.T) {
	for _, test := range []struct {
		name     string
		protocol string
		expected runtimeapi.Protocol
	}{
		{
			name:     "TCP protocol",
			protocol: "TCP",
			expected: runtimeapi.Protocol_TCP,
		},
		{
			name:     "UDP protocol",
			protocol: "UDP",
			expected: runtimeapi.Protocol_UDP,
		},
		{
			name:     "SCTP protocol",
			protocol: "SCTP",
			expected: runtimeapi.Protocol_SCTP,
		},
		{
			name:     "unknown protocol",
			protocol: "unknown",
			expected: runtimeapi.Protocol_TCP,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if result := toRuntimeProtocol(v1.Protocol(test.protocol)); result != test.expected {
				t.Errorf("expected %d but got %d", test.expected, result)
			}
		})
	}
}

func TestToKubeContainerState(t *testing.T) {
	for _, test := range []struct {
		name     string
		state    int32
		expected kubecontainer.State
	}{
		{
			name:     "container created",
			state:    0,
			expected: kubecontainer.ContainerStateCreated,
		},
		{
			name:     "container running",
			state:    1,
			expected: kubecontainer.ContainerStateRunning,
		},
		{
			name:     "container exited",
			state:    2,
			expected: kubecontainer.ContainerStateExited,
		},
		{
			name:     "unknown state",
			state:    3,
			expected: kubecontainer.ContainerStateUnknown,
		},
		{
			name:     "not supported state",
			state:    4,
			expected: kubecontainer.ContainerStateUnknown,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if result := toKubeContainerState(runtimeapi.ContainerState(test.state)); result != test.expected {
				t.Errorf("expected %s but got %s", test.expected, result)
			}
		})
	}
}
