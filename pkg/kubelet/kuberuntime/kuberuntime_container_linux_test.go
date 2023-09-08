//go:build linux
// +build linux

/*
Copyright 2018 The Kubernetes Authors.

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
	"fmt"
	"k8s.io/kubernetes/pkg/kubelet/cm"
	"k8s.io/kubernetes/pkg/kubelet/types"
	"math"
	"os"
	"reflect"
	"strconv"
	"testing"

	"github.com/google/go-cmp/cmp"
	libcontainercgroups "github.com/opencontainers/runc/libcontainer/cgroups"
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/kubernetes/pkg/features"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
)

func makeExpectedConfig(m *kubeGenericRuntimeManager, pod *v1.Pod, containerIndex int, enforceMemoryQoS bool) *runtimeapi.ContainerConfig {
	ctx := context.Background()
	container := &pod.Spec.Containers[containerIndex]
	podIP := ""
	restartCount := 0
	opts, _, _ := m.runtimeHelper.GenerateRunContainerOptions(ctx, pod, container, podIP, []string{podIP})
	containerLogsPath := buildContainerLogsPath(container.Name, restartCount)
	restartCountUint32 := uint32(restartCount)
	envs := make([]*runtimeapi.KeyValue, len(opts.Envs))

	l, _ := m.generateLinuxContainerConfig(container, pod, new(int64), "", nil, enforceMemoryQoS)

	expectedConfig := &runtimeapi.ContainerConfig{
		Metadata: &runtimeapi.ContainerMetadata{
			Name:    container.Name,
			Attempt: restartCountUint32,
		},
		Image:       &runtimeapi.ImageSpec{Image: container.Image, UserSpecifiedImage: container.Image},
		Command:     container.Command,
		Args:        []string(nil),
		WorkingDir:  container.WorkingDir,
		Labels:      newContainerLabels(container, pod),
		Annotations: newContainerAnnotations(container, pod, restartCount, opts),
		Devices:     makeDevices(opts),
		Mounts:      m.makeMounts(opts, container),
		LogPath:     containerLogsPath,
		Stdin:       container.Stdin,
		StdinOnce:   container.StdinOnce,
		Tty:         container.TTY,
		Linux:       l,
		Envs:        envs,
		CDIDevices:  makeCDIDevices(opts),
	}
	return expectedConfig
}

func TestGenerateContainerConfig(t *testing.T) {
	ctx := context.Background()
	_, imageService, m, err := createTestRuntimeManager()
	assert.NoError(t, err)

	runAsUser := int64(1000)
	runAsGroup := int64(2000)
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID:       "12345678",
			Name:      "bar",
			Namespace: "new",
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:            "foo",
					Image:           "busybox",
					ImagePullPolicy: v1.PullIfNotPresent,
					Command:         []string{"testCommand"},
					WorkingDir:      "testWorkingDir",
					SecurityContext: &v1.SecurityContext{
						RunAsUser:  &runAsUser,
						RunAsGroup: &runAsGroup,
					},
				},
			},
		},
	}

	expectedConfig := makeExpectedConfig(m, pod, 0, false)
	containerConfig, _, err := m.generateContainerConfig(ctx, &pod.Spec.Containers[0], pod, 0, "", pod.Spec.Containers[0].Image, []string{}, nil)
	assert.NoError(t, err)
	assert.Equal(t, expectedConfig, containerConfig, "generate container config for kubelet runtime v1.")
	assert.Equal(t, runAsUser, containerConfig.GetLinux().GetSecurityContext().GetRunAsUser().GetValue(), "RunAsUser should be set")
	assert.Equal(t, runAsGroup, containerConfig.GetLinux().GetSecurityContext().GetRunAsGroup().GetValue(), "RunAsGroup should be set")

	runAsRoot := int64(0)
	runAsNonRootTrue := true
	podWithContainerSecurityContext := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID:       "12345678",
			Name:      "bar",
			Namespace: "new",
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:            "foo",
					Image:           "busybox",
					ImagePullPolicy: v1.PullIfNotPresent,
					Command:         []string{"testCommand"},
					WorkingDir:      "testWorkingDir",
					SecurityContext: &v1.SecurityContext{
						RunAsNonRoot: &runAsNonRootTrue,
						RunAsUser:    &runAsRoot,
					},
				},
			},
		},
	}

	_, _, err = m.generateContainerConfig(ctx, &podWithContainerSecurityContext.Spec.Containers[0], podWithContainerSecurityContext, 0, "", podWithContainerSecurityContext.Spec.Containers[0].Image, []string{}, nil)
	assert.Error(t, err)

	imageID, _ := imageService.PullImage(ctx, &runtimeapi.ImageSpec{Image: "busybox"}, nil, nil)
	resp, _ := imageService.ImageStatus(ctx, &runtimeapi.ImageSpec{Image: imageID}, false)

	resp.Image.Uid = nil
	resp.Image.Username = "test"

	podWithContainerSecurityContext.Spec.Containers[0].SecurityContext.RunAsUser = nil
	podWithContainerSecurityContext.Spec.Containers[0].SecurityContext.RunAsNonRoot = &runAsNonRootTrue

	_, _, err = m.generateContainerConfig(ctx, &podWithContainerSecurityContext.Spec.Containers[0], podWithContainerSecurityContext, 0, "", podWithContainerSecurityContext.Spec.Containers[0].Image, []string{}, nil)
	assert.Error(t, err, "RunAsNonRoot should fail for non-numeric username")
}

func TestGenerateLinuxContainerConfigResources(t *testing.T) {
	_, _, m, err := createTestRuntimeManager()
	m.cpuCFSQuota = true

	assert.NoError(t, err)

	tests := []struct {
		name         string
		podResources v1.ResourceRequirements
		expected     *runtimeapi.LinuxContainerResources
	}{
		{
			name: "Request 128M/1C, Limit 256M/3C",
			podResources: v1.ResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceMemory: resource.MustParse("128Mi"),
					v1.ResourceCPU:    resource.MustParse("1"),
				},
				Limits: v1.ResourceList{
					v1.ResourceMemory: resource.MustParse("256Mi"),
					v1.ResourceCPU:    resource.MustParse("3"),
				},
			},
			expected: &runtimeapi.LinuxContainerResources{
				CpuPeriod:          100000,
				CpuQuota:           300000,
				CpuShares:          1024,
				MemoryLimitInBytes: 256 * 1024 * 1024,
			},
		},
		{
			name: "Request 128M/2C, No Limit",
			podResources: v1.ResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceMemory: resource.MustParse("128Mi"),
					v1.ResourceCPU:    resource.MustParse("2"),
				},
			},
			expected: &runtimeapi.LinuxContainerResources{
				CpuPeriod:          100000,
				CpuQuota:           0,
				CpuShares:          2048,
				MemoryLimitInBytes: 0,
			},
		},
	}

	for _, test := range tests {
		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				UID:       "12345678",
				Name:      "bar",
				Namespace: "new",
			},
			Spec: v1.PodSpec{
				Containers: []v1.Container{
					{
						Name:            "foo",
						Image:           "busybox",
						ImagePullPolicy: v1.PullIfNotPresent,
						Command:         []string{"testCommand"},
						WorkingDir:      "testWorkingDir",
						Resources:       test.podResources,
					},
				},
			},
		}

		linuxConfig, err := m.generateLinuxContainerConfig(&pod.Spec.Containers[0], pod, new(int64), "", nil, false)
		assert.NoError(t, err)
		assert.Equal(t, test.expected.CpuPeriod, linuxConfig.GetResources().CpuPeriod, test.name)
		assert.Equal(t, test.expected.CpuQuota, linuxConfig.GetResources().CpuQuota, test.name)
		assert.Equal(t, test.expected.CpuShares, linuxConfig.GetResources().CpuShares, test.name)
		assert.Equal(t, test.expected.MemoryLimitInBytes, linuxConfig.GetResources().MemoryLimitInBytes, test.name)
	}
}

func TestCalculateLinuxResources(t *testing.T) {
	_, _, m, err := createTestRuntimeManager()
	m.cpuCFSQuota = true

	assert.NoError(t, err)

	generateResourceQuantity := func(str string) *resource.Quantity {
		quantity := resource.MustParse(str)
		return &quantity
	}

	tests := []struct {
		name          string
		cpuReq        *resource.Quantity
		cpuLim        *resource.Quantity
		memLim        *resource.Quantity
		expected      *runtimeapi.LinuxContainerResources
		cgroupVersion CgroupVersion
	}{
		{
			name:   "Request128MBLimit256MB",
			cpuReq: generateResourceQuantity("1"),
			cpuLim: generateResourceQuantity("2"),
			memLim: generateResourceQuantity("128Mi"),
			expected: &runtimeapi.LinuxContainerResources{
				CpuPeriod:          100000,
				CpuQuota:           200000,
				CpuShares:          1024,
				MemoryLimitInBytes: 134217728,
			},
			cgroupVersion: cgroupV1,
		},
		{
			name:   "RequestNoMemory",
			cpuReq: generateResourceQuantity("2"),
			cpuLim: generateResourceQuantity("8"),
			memLim: generateResourceQuantity("0"),
			expected: &runtimeapi.LinuxContainerResources{
				CpuPeriod:          100000,
				CpuQuota:           800000,
				CpuShares:          2048,
				MemoryLimitInBytes: 0,
			},
			cgroupVersion: cgroupV1,
		},
		{
			name:   "RequestNilCPU",
			cpuLim: generateResourceQuantity("2"),
			memLim: generateResourceQuantity("0"),
			expected: &runtimeapi.LinuxContainerResources{
				CpuPeriod:          100000,
				CpuQuota:           200000,
				CpuShares:          2048,
				MemoryLimitInBytes: 0,
			},
			cgroupVersion: cgroupV1,
		},
		{
			name:   "RequestZeroCPU",
			cpuReq: generateResourceQuantity("0"),
			cpuLim: generateResourceQuantity("2"),
			memLim: generateResourceQuantity("0"),
			expected: &runtimeapi.LinuxContainerResources{
				CpuPeriod:          100000,
				CpuQuota:           200000,
				CpuShares:          2,
				MemoryLimitInBytes: 0,
			},
			cgroupVersion: cgroupV1,
		},
		{
			name:   "Request128MBLimit256MB",
			cpuReq: generateResourceQuantity("1"),
			cpuLim: generateResourceQuantity("2"),
			memLim: generateResourceQuantity("128Mi"),
			expected: &runtimeapi.LinuxContainerResources{
				CpuPeriod:          100000,
				CpuQuota:           200000,
				CpuShares:          1024,
				MemoryLimitInBytes: 134217728,
				Unified:            map[string]string{"memory.oom.group": "1"},
			},
			cgroupVersion: cgroupV2,
		},
		{
			name:   "RequestNoMemory",
			cpuReq: generateResourceQuantity("2"),
			cpuLim: generateResourceQuantity("8"),
			memLim: generateResourceQuantity("0"),
			expected: &runtimeapi.LinuxContainerResources{
				CpuPeriod:          100000,
				CpuQuota:           800000,
				CpuShares:          2048,
				MemoryLimitInBytes: 0,
				Unified:            map[string]string{"memory.oom.group": "1"},
			},
			cgroupVersion: cgroupV2,
		},
		{
			name:   "RequestNilCPU",
			cpuLim: generateResourceQuantity("2"),
			memLim: generateResourceQuantity("0"),
			expected: &runtimeapi.LinuxContainerResources{
				CpuPeriod:          100000,
				CpuQuota:           200000,
				CpuShares:          2048,
				MemoryLimitInBytes: 0,
				Unified:            map[string]string{"memory.oom.group": "1"},
			},
			cgroupVersion: cgroupV2,
		},
		{
			name:   "RequestZeroCPU",
			cpuReq: generateResourceQuantity("0"),
			cpuLim: generateResourceQuantity("2"),
			memLim: generateResourceQuantity("0"),
			expected: &runtimeapi.LinuxContainerResources{
				CpuPeriod:          100000,
				CpuQuota:           200000,
				CpuShares:          2,
				MemoryLimitInBytes: 0,
				Unified:            map[string]string{"memory.oom.group": "1"},
			},
			cgroupVersion: cgroupV2,
		},
	}
	for _, test := range tests {
		setCgroupVersionDuringTest(test.cgroupVersion)
		linuxContainerResources := m.calculateLinuxResources(test.cpuReq, test.cpuLim, test.memLim)
		assert.Equal(t, test.expected, linuxContainerResources)
	}
}

func TestGenerateContainerConfigWithMemoryQoSEnforced(t *testing.T) {
	_, _, m, err := createTestRuntimeManager()
	assert.NoError(t, err)

	podRequestMemory := resource.MustParse("128Mi")
	pod1LimitMemory := resource.MustParse("256Mi")
	pod1 := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID:       "12345678",
			Name:      "bar",
			Namespace: "new",
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:            "foo",
					Image:           "busybox",
					ImagePullPolicy: v1.PullIfNotPresent,
					Command:         []string{"testCommand"},
					WorkingDir:      "testWorkingDir",
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceMemory: podRequestMemory,
						},
						Limits: v1.ResourceList{
							v1.ResourceMemory: pod1LimitMemory,
						},
					},
				},
			},
		},
	}

	pod2 := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID:       "12345678",
			Name:      "bar",
			Namespace: "new",
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:            "foo",
					Image:           "busybox",
					ImagePullPolicy: v1.PullIfNotPresent,
					Command:         []string{"testCommand"},
					WorkingDir:      "testWorkingDir",
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceMemory: podRequestMemory,
						},
					},
				},
			},
		},
	}
	pageSize := int64(os.Getpagesize())
	memoryNodeAllocatable := resource.MustParse(fakeNodeAllocatableMemory)
	pod1MemoryHigh := int64(math.Floor(
		float64(podRequestMemory.Value())+
			(float64(pod1LimitMemory.Value())-float64(podRequestMemory.Value()))*float64(m.memoryThrottlingFactor))/float64(pageSize)) * pageSize
	pod2MemoryHigh := int64(math.Floor(
		float64(podRequestMemory.Value())+
			(float64(memoryNodeAllocatable.Value())-float64(podRequestMemory.Value()))*float64(m.memoryThrottlingFactor))/float64(pageSize)) * pageSize

	type expectedResult struct {
		containerConfig *runtimeapi.LinuxContainerConfig
		memoryLow       int64
		memoryHigh      int64
	}
	l1, _ := m.generateLinuxContainerConfig(&pod1.Spec.Containers[0], pod1, new(int64), "", nil, true)
	l2, _ := m.generateLinuxContainerConfig(&pod2.Spec.Containers[0], pod2, new(int64), "", nil, true)
	tests := []struct {
		name     string
		pod      *v1.Pod
		expected *expectedResult
	}{
		{
			name: "Request128MBLimit256MB",
			pod:  pod1,
			expected: &expectedResult{
				l1,
				128 * 1024 * 1024,
				int64(pod1MemoryHigh),
			},
		},
		{
			name: "Request128MBWithoutLimit",
			pod:  pod2,
			expected: &expectedResult{
				l2,
				128 * 1024 * 1024,
				int64(pod2MemoryHigh),
			},
		},
	}

	for _, test := range tests {
		linuxConfig, err := m.generateLinuxContainerConfig(&test.pod.Spec.Containers[0], test.pod, new(int64), "", nil, true)
		assert.NoError(t, err)
		assert.Equal(t, test.expected.containerConfig, linuxConfig, test.name)
		assert.Equal(t, linuxConfig.GetResources().GetUnified()["memory.min"], strconv.FormatInt(test.expected.memoryLow, 10), test.name)
		assert.Equal(t, linuxConfig.GetResources().GetUnified()["memory.high"], strconv.FormatInt(test.expected.memoryHigh, 10), test.name)
	}
}

func TestGetHugepageLimitsFromResources(t *testing.T) {
	var baseHugepage []*runtimeapi.HugepageLimit

	// For each page size, limit to 0.
	for _, pageSize := range libcontainercgroups.HugePageSizes() {
		baseHugepage = append(baseHugepage, &runtimeapi.HugepageLimit{
			PageSize: pageSize,
			Limit:    uint64(0),
		})
	}

	tests := []struct {
		name      string
		resources v1.ResourceRequirements
		expected  []*runtimeapi.HugepageLimit
	}{
		{
			name: "Success2MB",
			resources: v1.ResourceRequirements{
				Limits: v1.ResourceList{
					"hugepages-2Mi": resource.MustParse("2Mi"),
				},
			},
			expected: []*runtimeapi.HugepageLimit{
				{
					PageSize: "2MB",
					Limit:    2097152,
				},
			},
		},
		{
			name: "Success1GB",
			resources: v1.ResourceRequirements{
				Limits: v1.ResourceList{
					"hugepages-1Gi": resource.MustParse("2Gi"),
				},
			},
			expected: []*runtimeapi.HugepageLimit{
				{
					PageSize: "1GB",
					Limit:    2147483648,
				},
			},
		},
		{
			name: "Skip2MB",
			resources: v1.ResourceRequirements{
				Limits: v1.ResourceList{
					"hugepages-2MB": resource.MustParse("2Mi"),
				},
			},
			expected: []*runtimeapi.HugepageLimit{
				{
					PageSize: "2MB",
					Limit:    0,
				},
			},
		},
		{
			name: "Skip1GB",
			resources: v1.ResourceRequirements{
				Limits: v1.ResourceList{
					"hugepages-1GB": resource.MustParse("2Gi"),
				},
			},
			expected: []*runtimeapi.HugepageLimit{
				{
					PageSize: "1GB",
					Limit:    0,
				},
			},
		},
		{
			name: "Success2MBand1GB",
			resources: v1.ResourceRequirements{
				Limits: v1.ResourceList{
					v1.ResourceName(v1.ResourceCPU): resource.MustParse("0"),
					"hugepages-2Mi":                 resource.MustParse("2Mi"),
					"hugepages-1Gi":                 resource.MustParse("2Gi"),
				},
			},
			expected: []*runtimeapi.HugepageLimit{
				{
					PageSize: "2MB",
					Limit:    2097152,
				},
				{
					PageSize: "1GB",
					Limit:    2147483648,
				},
			},
		},
		{
			name: "Skip2MBand1GB",
			resources: v1.ResourceRequirements{
				Limits: v1.ResourceList{
					v1.ResourceName(v1.ResourceCPU): resource.MustParse("0"),
					"hugepages-2MB":                 resource.MustParse("2Mi"),
					"hugepages-1GB":                 resource.MustParse("2Gi"),
				},
			},
			expected: []*runtimeapi.HugepageLimit{
				{
					PageSize: "2MB",
					Limit:    0,
				},
				{
					PageSize: "1GB",
					Limit:    0,
				},
			},
		},
	}

	for _, test := range tests {
		// Validate if machine supports hugepage size that used in test case.
		machineHugepageSupport := true
		for _, hugepageLimit := range test.expected {
			hugepageSupport := false
			for _, pageSize := range libcontainercgroups.HugePageSizes() {
				if pageSize == hugepageLimit.PageSize {
					hugepageSupport = true
					break
				}
			}

			if !hugepageSupport {
				machineHugepageSupport = false
				break
			}
		}

		// Case of machine can't support hugepage size
		if !machineHugepageSupport {
			continue
		}

		expectedHugepages := baseHugepage
		for _, hugepage := range test.expected {
			for _, expectedHugepage := range expectedHugepages {
				if expectedHugepage.PageSize == hugepage.PageSize {
					expectedHugepage.Limit = hugepage.Limit
				}
			}
		}

		results := GetHugepageLimitsFromResources(test.resources)
		if !reflect.DeepEqual(expectedHugepages, results) {
			t.Errorf("%s test failed. Expected %v but got %v", test.name, expectedHugepages, results)
		}

		for _, hugepage := range baseHugepage {
			hugepage.Limit = uint64(0)
		}
	}
}

func TestGenerateLinuxContainerConfigNamespaces(t *testing.T) {
	_, _, m, err := createTestRuntimeManager()
	if err != nil {
		t.Fatalf("error creating test RuntimeManager: %v", err)
	}

	for _, tc := range []struct {
		name   string
		pod    *v1.Pod
		target *kubecontainer.ContainerID
		want   *runtimeapi.NamespaceOption
	}{
		{
			"Default namespaces",
			&v1.Pod{
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{Name: "test"},
					},
				},
			},
			nil,
			&runtimeapi.NamespaceOption{
				Pid: runtimeapi.NamespaceMode_CONTAINER,
			},
		},
		{
			"PID Namespace POD",
			&v1.Pod{
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{Name: "test"},
					},
					ShareProcessNamespace: &[]bool{true}[0],
				},
			},
			nil,
			&runtimeapi.NamespaceOption{
				Pid: runtimeapi.NamespaceMode_POD,
			},
		},
		{
			"PID Namespace TARGET",
			&v1.Pod{
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{Name: "test"},
					},
				},
			},
			&kubecontainer.ContainerID{Type: "docker", ID: "really-long-id-string"},
			&runtimeapi.NamespaceOption{
				Pid:      runtimeapi.NamespaceMode_TARGET,
				TargetId: "really-long-id-string",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := m.generateLinuxContainerConfig(&tc.pod.Spec.Containers[0], tc.pod, nil, "", tc.target, false)
			assert.NoError(t, err)
			if diff := cmp.Diff(tc.want, got.SecurityContext.NamespaceOptions); diff != "" {
				t.Errorf("%v: diff (-want +got):\n%v", t.Name(), diff)
			}
		})
	}
}

func TestGenerateLinuxContainerResources(t *testing.T) {
	_, _, m, err := createTestRuntimeManager()
	assert.NoError(t, err)
	m.machineInfo.MemoryCapacity = 17179860387 // 16GB

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID:       "12345678",
			Name:      "foo",
			Namespace: "bar",
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:  "c1",
					Image: "busybox",
				},
			},
		},
		Status: v1.PodStatus{},
	}

	for _, tc := range []struct {
		name      string
		scalingFg bool
		limits    v1.ResourceList
		requests  v1.ResourceList
		cStatus   []v1.ContainerStatus
		expected  *runtimeapi.LinuxContainerResources
	}{
		{
			"requests & limits, cpu & memory, guaranteed qos - no container status",
			true,
			v1.ResourceList{v1.ResourceCPU: resource.MustParse("250m"), v1.ResourceMemory: resource.MustParse("500Mi")},
			v1.ResourceList{v1.ResourceCPU: resource.MustParse("250m"), v1.ResourceMemory: resource.MustParse("500Mi")},
			[]v1.ContainerStatus{},
			&runtimeapi.LinuxContainerResources{CpuShares: 256, MemoryLimitInBytes: 524288000, OomScoreAdj: -997},
		},
		{
			"requests & limits, cpu & memory, burstable qos - no container status",
			true,
			v1.ResourceList{v1.ResourceCPU: resource.MustParse("500m"), v1.ResourceMemory: resource.MustParse("750Mi")},
			v1.ResourceList{v1.ResourceCPU: resource.MustParse("250m"), v1.ResourceMemory: resource.MustParse("500Mi")},
			[]v1.ContainerStatus{},
			&runtimeapi.LinuxContainerResources{CpuShares: 256, MemoryLimitInBytes: 786432000, OomScoreAdj: 970},
		},
		{
			"best-effort qos - no container status",
			true,
			nil,
			nil,
			[]v1.ContainerStatus{},
			&runtimeapi.LinuxContainerResources{CpuShares: 2, OomScoreAdj: 1000},
		},
		{
			"requests & limits, cpu & memory, guaranteed qos - empty resources container status",
			true,
			v1.ResourceList{v1.ResourceCPU: resource.MustParse("250m"), v1.ResourceMemory: resource.MustParse("500Mi")},
			v1.ResourceList{v1.ResourceCPU: resource.MustParse("250m"), v1.ResourceMemory: resource.MustParse("500Mi")},
			[]v1.ContainerStatus{{Name: "c1"}},
			&runtimeapi.LinuxContainerResources{CpuShares: 256, MemoryLimitInBytes: 524288000, OomScoreAdj: -997},
		},
		{
			"requests & limits, cpu & memory, burstable qos - empty resources container status",
			true,
			v1.ResourceList{v1.ResourceCPU: resource.MustParse("500m"), v1.ResourceMemory: resource.MustParse("750Mi")},
			v1.ResourceList{v1.ResourceCPU: resource.MustParse("250m"), v1.ResourceMemory: resource.MustParse("500Mi")},
			[]v1.ContainerStatus{{Name: "c1"}},
			&runtimeapi.LinuxContainerResources{CpuShares: 256, MemoryLimitInBytes: 786432000, OomScoreAdj: 999},
		},
		{
			"best-effort qos - empty resources container status",
			true,
			nil,
			nil,
			[]v1.ContainerStatus{{Name: "c1"}},
			&runtimeapi.LinuxContainerResources{CpuShares: 2, OomScoreAdj: 1000},
		},
		{
			"requests & limits, cpu & memory, guaranteed qos - container status with allocatedResources",
			true,
			v1.ResourceList{v1.ResourceCPU: resource.MustParse("200m"), v1.ResourceMemory: resource.MustParse("500Mi")},
			v1.ResourceList{v1.ResourceCPU: resource.MustParse("200m"), v1.ResourceMemory: resource.MustParse("500Mi")},
			[]v1.ContainerStatus{
				{
					Name:               "c1",
					AllocatedResources: v1.ResourceList{v1.ResourceCPU: resource.MustParse("200m"), v1.ResourceMemory: resource.MustParse("500Mi")},
				},
			},
			&runtimeapi.LinuxContainerResources{CpuShares: 204, MemoryLimitInBytes: 524288000, OomScoreAdj: -997},
		},
		{
			"requests & limits, cpu & memory, burstable qos - container status with allocatedResources",
			true,
			v1.ResourceList{v1.ResourceCPU: resource.MustParse("500m"), v1.ResourceMemory: resource.MustParse("750Mi")},
			v1.ResourceList{v1.ResourceCPU: resource.MustParse("250m"), v1.ResourceMemory: resource.MustParse("500Mi")},
			[]v1.ContainerStatus{
				{
					Name:               "c1",
					AllocatedResources: v1.ResourceList{v1.ResourceCPU: resource.MustParse("250m"), v1.ResourceMemory: resource.MustParse("500Mi")},
				},
			},
			&runtimeapi.LinuxContainerResources{CpuShares: 256, MemoryLimitInBytes: 786432000, OomScoreAdj: 970},
		},
		{
			"requests & limits, cpu & memory, guaranteed qos - no container status",
			false,
			v1.ResourceList{v1.ResourceCPU: resource.MustParse("250m"), v1.ResourceMemory: resource.MustParse("500Mi")},
			v1.ResourceList{v1.ResourceCPU: resource.MustParse("250m"), v1.ResourceMemory: resource.MustParse("500Mi")},
			[]v1.ContainerStatus{},
			&runtimeapi.LinuxContainerResources{CpuShares: 256, MemoryLimitInBytes: 524288000, OomScoreAdj: -997},
		},
		{
			"requests & limits, cpu & memory, burstable qos - container status with allocatedResources",
			false,
			v1.ResourceList{v1.ResourceCPU: resource.MustParse("500m"), v1.ResourceMemory: resource.MustParse("750Mi")},
			v1.ResourceList{v1.ResourceCPU: resource.MustParse("250m"), v1.ResourceMemory: resource.MustParse("500Mi")},
			[]v1.ContainerStatus{
				{
					Name:               "c1",
					AllocatedResources: v1.ResourceList{v1.ResourceCPU: resource.MustParse("250m"), v1.ResourceMemory: resource.MustParse("500Mi")},
				},
			},
			&runtimeapi.LinuxContainerResources{CpuShares: 256, MemoryLimitInBytes: 786432000, OomScoreAdj: 970},
		},
		{
			"requests & limits, cpu & memory, guaranteed qos - container status with allocatedResources",
			false,
			v1.ResourceList{v1.ResourceCPU: resource.MustParse("200m"), v1.ResourceMemory: resource.MustParse("500Mi")},
			v1.ResourceList{v1.ResourceCPU: resource.MustParse("200m"), v1.ResourceMemory: resource.MustParse("500Mi")},
			[]v1.ContainerStatus{
				{
					Name:               "c1",
					AllocatedResources: v1.ResourceList{v1.ResourceCPU: resource.MustParse("200m"), v1.ResourceMemory: resource.MustParse("500Mi")},
				},
			},
			&runtimeapi.LinuxContainerResources{CpuShares: 204, MemoryLimitInBytes: 524288000, OomScoreAdj: -997},
		},
		{
			"best-effort qos - no container status",
			false,
			nil,
			nil,
			[]v1.ContainerStatus{},
			&runtimeapi.LinuxContainerResources{CpuShares: 2, OomScoreAdj: 1000},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.scalingFg {
				defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.InPlacePodVerticalScaling, true)()
			}

			setCgroupVersionDuringTest(cgroupV1)
			tc.expected.MemorySwapLimitInBytes = tc.expected.MemoryLimitInBytes

			pod.Spec.Containers[0].Resources = v1.ResourceRequirements{Limits: tc.limits, Requests: tc.requests}
			if len(tc.cStatus) > 0 {
				pod.Status.ContainerStatuses = tc.cStatus
			}
			resources := m.generateLinuxContainerResources(pod, &pod.Spec.Containers[0], false)
			tc.expected.HugepageLimits = resources.HugepageLimits
			if !cmp.Equal(resources, tc.expected) {
				t.Errorf("Test %s: expected resources %+v, but got %+v", tc.name, tc.expected, resources)
			}
		})
	}
	//TODO(vinaykul,InPlacePodVerticalScaling): Add unit tests for cgroup v1 & v2
}

func TestGenerateLinuxContainerResourcesWithSwap(t *testing.T) {
	_, _, m, err := createTestRuntimeManager()
	assert.NoError(t, err)
	m.machineInfo.MemoryCapacity = 42949672960 // 40Gb == 40 * 1024^3
	m.machineInfo.SwapCapacity = 5368709120    // 5Gb == 5 * 1024^3

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID:       "12345678",
			Name:      "foo",
			Namespace: "bar",
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name: "c1",
				},
				{
					Name: "c2",
				},
			},
		},
		Status: v1.PodStatus{},
	}

	expectNoSwap := func(cgroupVersion CgroupVersion, resources ...*runtimeapi.LinuxContainerResources) {
		const msg = "container is expected to not have swap access"

		for _, r := range resources {
			switch cgroupVersion {
			case cgroupV1:
				assert.Equal(t, r.MemoryLimitInBytes, r.MemorySwapLimitInBytes, msg)
			case cgroupV2:
				assert.Equal(t, "0", r.Unified[cm.Cgroup2MaxSwapFilename], msg)
			}
		}
	}

	expectUnlimitedSwap := func(cgroupVersion CgroupVersion, resources ...*runtimeapi.LinuxContainerResources) {
		const msg = "container is expected to have unlimited swap access"

		for _, r := range resources {
			switch cgroupVersion {
			case cgroupV1:
				assert.Equal(t, int64(-1), r.MemorySwapLimitInBytes, msg)
			case cgroupV2:
				assert.Equal(t, "max", r.Unified[cm.Cgroup2MaxSwapFilename], msg)
			}
		}
	}

	expectSwap := func(cgroupVersion CgroupVersion, swapBytesExpected int64, resources *runtimeapi.LinuxContainerResources) {
		msg := fmt.Sprintf("container swap is expected to be limited by %d bytes", swapBytesExpected)

		switch cgroupVersion {
		case cgroupV1:
			assert.Equal(t, resources.MemoryLimitInBytes+swapBytesExpected, resources.MemorySwapLimitInBytes, msg)
		case cgroupV2:
			assert.Equal(t, fmt.Sprintf("%d", swapBytesExpected), resources.Unified[cm.Cgroup2MaxSwapFilename], msg)
		}
	}

	calcSwapForBurstablePods := func(containerMemoryRequest int64) int64 {
		swapSize, err := calcSwapForBurstablePods(containerMemoryRequest, int64(m.machineInfo.MemoryCapacity), int64(m.machineInfo.SwapCapacity))
		assert.NoError(t, err)

		return swapSize
	}

	for _, tc := range []struct {
		name                        string
		cgroupVersion               CgroupVersion
		qosClass                    v1.PodQOSClass
		nodeSwapFeatureGateEnabled  bool
		swapBehavior                string
		addContainerWithoutRequests bool
		addGuaranteedContainer      bool
	}{
		// With cgroup v1
		{
			name:                       "cgroups v1, LimitedSwap, Burstable QoS",
			cgroupVersion:              cgroupV1,
			qosClass:                   v1.PodQOSBurstable,
			nodeSwapFeatureGateEnabled: true,
			swapBehavior:               types.LimitedSwap,
		},
		{
			name:                       "cgroups v1, UnlimitedSwap, Burstable QoS",
			cgroupVersion:              cgroupV1,
			qosClass:                   v1.PodQOSBurstable,
			nodeSwapFeatureGateEnabled: true,
			swapBehavior:               types.UnlimitedSwap,
		},
		{
			name:                       "cgroups v1, LimitedSwap, Best-effort QoS",
			cgroupVersion:              cgroupV1,
			qosClass:                   v1.PodQOSBestEffort,
			nodeSwapFeatureGateEnabled: true,
			swapBehavior:               types.LimitedSwap,
		},

		// With feature gate turned off
		{
			name:                       "NodeSwap feature gate turned off, cgroups v2, LimitedSwap",
			cgroupVersion:              cgroupV2,
			qosClass:                   v1.PodQOSBurstable,
			nodeSwapFeatureGateEnabled: false,
			swapBehavior:               types.LimitedSwap,
		},
		{
			name:                       "NodeSwap feature gate turned off, cgroups v2, UnlimitedSwap",
			cgroupVersion:              cgroupV2,
			qosClass:                   v1.PodQOSBurstable,
			nodeSwapFeatureGateEnabled: false,
			swapBehavior:               types.UnlimitedSwap,
		},

		// With no swapBehavior, UnlimitedSwap should be the default
		{
			name:                       "With no swapBehavior - UnlimitedSwap should be the default",
			cgroupVersion:              cgroupV2,
			qosClass:                   v1.PodQOSBestEffort,
			nodeSwapFeatureGateEnabled: true,
			swapBehavior:               "",
		},

		// With Guaranteed and Best-effort QoS
		{
			name:                       "Best-effort Qos, cgroups v2, LimitedSwap",
			cgroupVersion:              cgroupV2,
			qosClass:                   v1.PodQOSBurstable,
			nodeSwapFeatureGateEnabled: true,
			swapBehavior:               types.LimitedSwap,
		},
		{
			name:                       "Best-effort Qos, cgroups v2, UnlimitedSwap",
			cgroupVersion:              cgroupV2,
			qosClass:                   v1.PodQOSBurstable,
			nodeSwapFeatureGateEnabled: true,
			swapBehavior:               types.UnlimitedSwap,
		},
		{
			name:                       "Guaranteed Qos, cgroups v2, LimitedSwap",
			cgroupVersion:              cgroupV2,
			qosClass:                   v1.PodQOSGuaranteed,
			nodeSwapFeatureGateEnabled: true,
			swapBehavior:               types.LimitedSwap,
		},
		{
			name:                       "Guaranteed Qos, cgroups v2, UnlimitedSwap",
			cgroupVersion:              cgroupV2,
			qosClass:                   v1.PodQOSGuaranteed,
			nodeSwapFeatureGateEnabled: true,
			swapBehavior:               types.UnlimitedSwap,
		},

		// With a "guaranteed" container (when memory requests equal to limits)
		{
			name:                        "Burstable Qos, cgroups v2, LimitedSwap, with a guaranteed container",
			cgroupVersion:               cgroupV2,
			qosClass:                    v1.PodQOSBurstable,
			nodeSwapFeatureGateEnabled:  true,
			swapBehavior:                types.LimitedSwap,
			addContainerWithoutRequests: false,
			addGuaranteedContainer:      true,
		},
		{
			name:                        "Burstable Qos, cgroups v2, UnlimitedSwap, with a guaranteed container",
			cgroupVersion:               cgroupV2,
			qosClass:                    v1.PodQOSBurstable,
			nodeSwapFeatureGateEnabled:  true,
			swapBehavior:                types.UnlimitedSwap,
			addContainerWithoutRequests: false,
			addGuaranteedContainer:      true,
		},

		// Swap is expected to be allocated
		{
			name:                        "Burstable Qos, cgroups v2, LimitedSwap",
			cgroupVersion:               cgroupV2,
			qosClass:                    v1.PodQOSBurstable,
			nodeSwapFeatureGateEnabled:  true,
			swapBehavior:                types.LimitedSwap,
			addContainerWithoutRequests: false,
			addGuaranteedContainer:      false,
		},
		{
			name:                        "Burstable Qos, cgroups v2, UnlimitedSwap",
			cgroupVersion:               cgroupV2,
			qosClass:                    v1.PodQOSBurstable,
			nodeSwapFeatureGateEnabled:  true,
			swapBehavior:                types.UnlimitedSwap,
			addContainerWithoutRequests: false,
			addGuaranteedContainer:      false,
		},
		{
			name:                        "Burstable Qos, cgroups v2, LimitedSwap, with a container with no requests",
			cgroupVersion:               cgroupV2,
			qosClass:                    v1.PodQOSBurstable,
			nodeSwapFeatureGateEnabled:  true,
			swapBehavior:                types.LimitedSwap,
			addContainerWithoutRequests: true,
			addGuaranteedContainer:      false,
		},
		{
			name:                        "Burstable Qos, cgroups v2, UnlimitedSwap, with a container with no requests",
			cgroupVersion:               cgroupV2,
			qosClass:                    v1.PodQOSBurstable,
			nodeSwapFeatureGateEnabled:  true,
			swapBehavior:                types.UnlimitedSwap,
			addContainerWithoutRequests: true,
			addGuaranteedContainer:      false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setCgroupVersionDuringTest(tc.cgroupVersion)
			defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.NodeSwap, tc.nodeSwapFeatureGateEnabled)()
			m.memorySwapBehavior = tc.swapBehavior

			var resourceReqsC1, resourceReqsC2 v1.ResourceRequirements
			switch tc.qosClass {
			case v1.PodQOSBurstable:
				resourceReqsC1 = v1.ResourceRequirements{
					Requests: v1.ResourceList{v1.ResourceMemory: resource.MustParse("1Gi")},
				}

				if !tc.addContainerWithoutRequests {
					resourceReqsC2 = v1.ResourceRequirements{
						Requests: v1.ResourceList{v1.ResourceMemory: resource.MustParse("2Gi")},
					}

					if tc.addGuaranteedContainer {
						resourceReqsC2.Limits = v1.ResourceList{v1.ResourceMemory: resource.MustParse("2Gi")}
					}
				}
			case v1.PodQOSGuaranteed:
				resourceReqsC1 = v1.ResourceRequirements{
					Requests: v1.ResourceList{v1.ResourceMemory: resource.MustParse("1Gi"), v1.ResourceCPU: resource.MustParse("1")},
					Limits:   v1.ResourceList{v1.ResourceMemory: resource.MustParse("1Gi"), v1.ResourceCPU: resource.MustParse("1")},
				}
				resourceReqsC2 = v1.ResourceRequirements{
					Requests: v1.ResourceList{v1.ResourceMemory: resource.MustParse("2Gi"), v1.ResourceCPU: resource.MustParse("1")},
					Limits:   v1.ResourceList{v1.ResourceMemory: resource.MustParse("2Gi"), v1.ResourceCPU: resource.MustParse("1")},
				}
			}
			pod.Spec.Containers[0].Resources = resourceReqsC1
			pod.Spec.Containers[1].Resources = resourceReqsC2

			resourcesC1 := m.generateLinuxContainerResources(pod, &pod.Spec.Containers[0], false)
			resourcesC2 := m.generateLinuxContainerResources(pod, &pod.Spec.Containers[1], false)

			if !tc.nodeSwapFeatureGateEnabled || tc.cgroupVersion == cgroupV1 || (tc.swapBehavior == types.LimitedSwap && tc.qosClass != v1.PodQOSBurstable) {
				expectNoSwap(tc.cgroupVersion, resourcesC1, resourcesC2)
				return
			}

			if tc.swapBehavior == types.UnlimitedSwap || tc.swapBehavior == "" {
				expectUnlimitedSwap(tc.cgroupVersion, resourcesC1, resourcesC2)
				return
			}

			c1ExpectedSwap := calcSwapForBurstablePods(resourceReqsC1.Requests.Memory().Value())
			c2ExpectedSwap := int64(0)
			if !tc.addContainerWithoutRequests && !tc.addGuaranteedContainer {
				c2ExpectedSwap = calcSwapForBurstablePods(resourceReqsC2.Requests.Memory().Value())
			}

			expectSwap(tc.cgroupVersion, c1ExpectedSwap, resourcesC1)
			expectSwap(tc.cgroupVersion, c2ExpectedSwap, resourcesC2)
		})
	}
}

type CgroupVersion string

const (
	cgroupV1 CgroupVersion = "v1"
	cgroupV2 CgroupVersion = "v2"
)

func setCgroupVersionDuringTest(version CgroupVersion) {
	isCgroup2UnifiedMode = func() bool {
		return version == cgroupV2
	}
}
