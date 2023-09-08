/*
Copyright 2014 The Kubernetes Authors.

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

package pod

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/tools/cache"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	utilpointer "k8s.io/utils/pointer"

	apitesting "k8s.io/kubernetes/pkg/api/testing"
	api "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/kubelet/client"

	// ensure types are installed
	_ "k8s.io/kubernetes/pkg/apis/core/install"
)

func TestMatchPod(t *testing.T) {
	testCases := []struct {
		in            *api.Pod
		fieldSelector fields.Selector
		expectMatch   bool
	}{
		{
			in: &api.Pod{
				Spec: api.PodSpec{NodeName: "nodeA"},
			},
			fieldSelector: fields.ParseSelectorOrDie("spec.nodeName=nodeA"),
			expectMatch:   true,
		},
		{
			in: &api.Pod{
				Spec: api.PodSpec{NodeName: "nodeB"},
			},
			fieldSelector: fields.ParseSelectorOrDie("spec.nodeName=nodeA"),
			expectMatch:   false,
		},
		{
			in: &api.Pod{
				Spec: api.PodSpec{RestartPolicy: api.RestartPolicyAlways},
			},
			fieldSelector: fields.ParseSelectorOrDie("spec.restartPolicy=Always"),
			expectMatch:   true,
		},
		{
			in: &api.Pod{
				Spec: api.PodSpec{RestartPolicy: api.RestartPolicyAlways},
			},
			fieldSelector: fields.ParseSelectorOrDie("spec.restartPolicy=Never"),
			expectMatch:   false,
		},
		{
			in: &api.Pod{
				Spec: api.PodSpec{SchedulerName: "scheduler1"},
			},
			fieldSelector: fields.ParseSelectorOrDie("spec.schedulerName=scheduler1"),
			expectMatch:   true,
		},
		{
			in: &api.Pod{
				Spec: api.PodSpec{SchedulerName: "scheduler1"},
			},
			fieldSelector: fields.ParseSelectorOrDie("spec.schedulerName=scheduler2"),
			expectMatch:   false,
		},
		{
			in: &api.Pod{
				Spec: api.PodSpec{ServiceAccountName: "serviceAccount1"},
			},
			fieldSelector: fields.ParseSelectorOrDie("spec.serviceAccountName=serviceAccount1"),
			expectMatch:   true,
		},
		{
			in: &api.Pod{
				Spec: api.PodSpec{SchedulerName: "serviceAccount1"},
			},
			fieldSelector: fields.ParseSelectorOrDie("spec.serviceAccountName=serviceAccount2"),
			expectMatch:   false,
		},
		{
			in: &api.Pod{
				Status: api.PodStatus{Phase: api.PodRunning},
			},
			fieldSelector: fields.ParseSelectorOrDie("status.phase=Running"),
			expectMatch:   true,
		},
		{
			in: &api.Pod{
				Status: api.PodStatus{Phase: api.PodRunning},
			},
			fieldSelector: fields.ParseSelectorOrDie("status.phase=Pending"),
			expectMatch:   false,
		},
		{
			in: &api.Pod{
				Status: api.PodStatus{
					PodIPs: []api.PodIP{
						{IP: "1.2.3.4"},
					},
				},
			},
			fieldSelector: fields.ParseSelectorOrDie("status.podIP=1.2.3.4"),
			expectMatch:   true,
		},
		{
			in: &api.Pod{
				Status: api.PodStatus{
					PodIPs: []api.PodIP{
						{IP: "1.2.3.4"},
					},
				},
			},
			fieldSelector: fields.ParseSelectorOrDie("status.podIP=4.3.2.1"),
			expectMatch:   false,
		},
		{
			in: &api.Pod{
				Status: api.PodStatus{NominatedNodeName: "node1"},
			},
			fieldSelector: fields.ParseSelectorOrDie("status.nominatedNodeName=node1"),
			expectMatch:   true,
		},
		{
			in: &api.Pod{
				Status: api.PodStatus{NominatedNodeName: "node1"},
			},
			fieldSelector: fields.ParseSelectorOrDie("status.nominatedNodeName=node2"),
			expectMatch:   false,
		},
		{
			in: &api.Pod{
				Status: api.PodStatus{
					PodIPs: []api.PodIP{
						{IP: "2001:db8::"},
					},
				},
			},
			fieldSelector: fields.ParseSelectorOrDie("status.podIP=2001:db8::"),
			expectMatch:   true,
		},
		{
			in: &api.Pod{
				Status: api.PodStatus{
					PodIPs: []api.PodIP{
						{IP: "2001:db8::"},
					},
				},
			},
			fieldSelector: fields.ParseSelectorOrDie("status.podIP=2001:db7::"),
			expectMatch:   false,
		},
		{
			in: &api.Pod{
				Spec: api.PodSpec{
					SecurityContext: &api.PodSecurityContext{
						HostNetwork: true,
					},
				},
			},
			fieldSelector: fields.ParseSelectorOrDie("spec.hostNetwork=true"),
			expectMatch:   true,
		},
		{
			in: &api.Pod{
				Spec: api.PodSpec{
					SecurityContext: &api.PodSecurityContext{
						HostNetwork: true,
					},
				},
			},
			fieldSelector: fields.ParseSelectorOrDie("spec.hostNetwork=false"),
			expectMatch:   false,
		},
		{
			in: &api.Pod{
				Spec: api.PodSpec{
					SecurityContext: &api.PodSecurityContext{
						HostNetwork: false,
					},
				},
			},
			fieldSelector: fields.ParseSelectorOrDie("spec.hostNetwork=false"),
			expectMatch:   true,
		},
		{
			in: &api.Pod{
				Spec: api.PodSpec{},
			},
			fieldSelector: fields.ParseSelectorOrDie("spec.hostNetwork=false"),
			expectMatch:   true,
		},
		{
			in: &api.Pod{
				Spec: api.PodSpec{},
			},
			fieldSelector: fields.ParseSelectorOrDie("spec.hostNetwork=true"),
			expectMatch:   false,
		},
	}
	for _, testCase := range testCases {
		m := MatchPod(labels.Everything(), testCase.fieldSelector)
		result, err := m.Matches(testCase.in)
		if err != nil {
			t.Errorf("Unexpected error %v", err)
		}
		if result != testCase.expectMatch {
			t.Errorf("Result %v, Expected %v, Selector: %v, Pod: %v", result, testCase.expectMatch, testCase.fieldSelector.String(), testCase.in)
		}
	}
}

func getResourceList(cpu, memory string) api.ResourceList {
	res := api.ResourceList{}
	if cpu != "" {
		res[api.ResourceCPU] = resource.MustParse(cpu)
	}
	if memory != "" {
		res[api.ResourceMemory] = resource.MustParse(memory)
	}
	return res
}

func getResourceRequirements(requests, limits api.ResourceList) api.ResourceRequirements {
	res := api.ResourceRequirements{}
	res.Requests = requests
	res.Limits = limits
	return res
}

func newContainer(name string, requests api.ResourceList, limits api.ResourceList) api.Container {
	return api.Container{
		Name:      name,
		Resources: getResourceRequirements(requests, limits),
	}
}

func newPod(name string, containers []api.Container) *api.Pod {
	return &api.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: api.PodSpec{
			Containers: containers,
		},
	}
}

func TestGetPodQOS(t *testing.T) {
	testCases := []struct {
		pod      *api.Pod
		expected api.PodQOSClass
	}{
		{
			pod: newPod("guaranteed", []api.Container{
				newContainer("guaranteed", getResourceList("100m", "100Mi"), getResourceList("100m", "100Mi")),
			}),
			expected: api.PodQOSGuaranteed,
		},
		{
			pod: newPod("best-effort", []api.Container{
				newContainer("best-effort", getResourceList("", ""), getResourceList("", "")),
			}),
			expected: api.PodQOSBestEffort,
		},
		{
			pod: newPod("burstable", []api.Container{
				newContainer("burstable", getResourceList("100m", "100Mi"), getResourceList("", "")),
			}),
			expected: api.PodQOSBurstable,
		},
	}
	for id, testCase := range testCases {
		Strategy.PrepareForCreate(genericapirequest.NewContext(), testCase.pod)
		actual := testCase.pod.Status.QOSClass
		if actual != testCase.expected {
			t.Errorf("[%d]: invalid qos pod %s, expected: %s, actual: %s", id, testCase.pod.Name, testCase.expected, actual)
		}
	}
}

func TestWaitingForGatesCondition(t *testing.T) {
	tests := []struct {
		name           string
		pod            *api.Pod
		featureEnabled bool
		want           api.PodCondition
	}{
		{
			name:           "pod without .spec.schedulingGates, feature disabled",
			pod:            &api.Pod{},
			featureEnabled: false,
			want:           api.PodCondition{},
		},
		{
			name:           "pod without .spec.schedulingGates, feature enabled",
			pod:            &api.Pod{},
			featureEnabled: true,
			want:           api.PodCondition{},
		},
		{
			name: "pod with .spec.schedulingGates, feature disabled",
			pod: &api.Pod{
				Spec: api.PodSpec{
					SchedulingGates: []api.PodSchedulingGate{{Name: "foo"}},
				},
			},
			featureEnabled: false,
			want:           api.PodCondition{},
		},
		{
			name: "pod with .spec.schedulingGates, feature enabled",
			pod: &api.Pod{
				Spec: api.PodSpec{
					SchedulingGates: []api.PodSchedulingGate{{Name: "foo"}},
				},
			},
			featureEnabled: true,
			want: api.PodCondition{
				Type:    api.PodScheduled,
				Status:  api.ConditionFalse,
				Reason:  api.PodReasonSchedulingGated,
				Message: "Scheduling is blocked due to non-empty scheduling gates",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.PodSchedulingReadiness, tt.featureEnabled)()

			Strategy.PrepareForCreate(genericapirequest.NewContext(), tt.pod)
			var got api.PodCondition
			for _, condition := range tt.pod.Status.Conditions {
				if condition.Type == api.PodScheduled {
					got = condition
					break
				}
			}

			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("unexpected field errors (-want, +got):\n%s", diff)
			}
		})
	}
}

func TestCheckGracefulDelete(t *testing.T) {
	defaultGracePeriod := int64(30)
	tcs := []struct {
		name              string
		pod               *api.Pod
		deleteGracePeriod *int64
		gracePeriod       int64
	}{
		{
			name: "in pending phase with has node name",
			pod: &api.Pod{
				Spec:   api.PodSpec{NodeName: "something"},
				Status: api.PodStatus{Phase: api.PodPending},
			},
			deleteGracePeriod: &defaultGracePeriod,
			gracePeriod:       defaultGracePeriod,
		},
		{
			name: "in failed phase with has node name",
			pod: &api.Pod{
				Spec:   api.PodSpec{NodeName: "something"},
				Status: api.PodStatus{Phase: api.PodFailed},
			},
			deleteGracePeriod: &defaultGracePeriod,
			gracePeriod:       0,
		},
		{
			name: "in failed phase",
			pod: &api.Pod{
				Spec:   api.PodSpec{},
				Status: api.PodStatus{Phase: api.PodPending},
			},
			deleteGracePeriod: &defaultGracePeriod,
			gracePeriod:       0,
		},
		{
			name: "in succeeded phase",
			pod: &api.Pod{
				Spec:   api.PodSpec{},
				Status: api.PodStatus{Phase: api.PodSucceeded},
			},
			deleteGracePeriod: &defaultGracePeriod,
			gracePeriod:       0,
		},
		{
			name: "no phase",
			pod: &api.Pod{
				Spec:   api.PodSpec{},
				Status: api.PodStatus{},
			},
			deleteGracePeriod: &defaultGracePeriod,
			gracePeriod:       0,
		},
		{
			name: "has negative grace period",
			pod: &api.Pod{
				Spec: api.PodSpec{
					NodeName:                      "something",
					TerminationGracePeriodSeconds: utilpointer.Int64(-1),
				},
				Status: api.PodStatus{},
			},
			gracePeriod: 1,
		},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			out := &metav1.DeleteOptions{}
			if tc.deleteGracePeriod != nil {
				out.GracePeriodSeconds = utilpointer.Int64(*tc.deleteGracePeriod)
			}
			Strategy.CheckGracefulDelete(genericapirequest.NewContext(), tc.pod, out)
			if out.GracePeriodSeconds == nil {
				t.Errorf("out grace period was nil but supposed to be %v", tc.gracePeriod)
			}
			if *(out.GracePeriodSeconds) != tc.gracePeriod {
				t.Errorf("out grace period was %v but was expected to be %v", *out, tc.gracePeriod)
			}
		})
	}
}

type mockPodGetter struct {
	pod *api.Pod
}

func (g mockPodGetter) Get(context.Context, string, *metav1.GetOptions) (runtime.Object, error) {
	return g.pod, nil
}

func TestCheckLogLocation(t *testing.T) {
	ctx := genericapirequest.NewDefaultContext()
	fakePodName := "test"
	tcs := []struct {
		name              string
		in                *api.Pod
		opts              *api.PodLogOptions
		expectedErr       error
		expectedTransport http.RoundTripper
	}{
		{
			name: "simple",
			in: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: fakePodName},
				Spec: api.PodSpec{
					Containers: []api.Container{
						{Name: "mycontainer"},
					},
					NodeName: "foo",
				},
				Status: api.PodStatus{},
			},
			opts:              &api.PodLogOptions{},
			expectedErr:       nil,
			expectedTransport: fakeSecureRoundTripper,
		},
		{
			name: "insecure",
			in: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: fakePodName},
				Spec: api.PodSpec{
					Containers: []api.Container{
						{Name: "mycontainer"},
					},
					NodeName: "foo",
				},
				Status: api.PodStatus{},
			},
			opts: &api.PodLogOptions{
				InsecureSkipTLSVerifyBackend: true,
			},
			expectedErr:       nil,
			expectedTransport: fakeInsecureRoundTripper,
		},
		{
			name: "missing container",
			in: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: fakePodName},
				Spec:       api.PodSpec{},
				Status:     api.PodStatus{},
			},
			opts:              &api.PodLogOptions{},
			expectedErr:       errors.NewBadRequest("a container name must be specified for pod test"),
			expectedTransport: nil,
		},
		{
			name: "choice of two containers",
			in: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: fakePodName},
				Spec: api.PodSpec{
					Containers: []api.Container{
						{Name: "container1"},
						{Name: "container2"},
					},
				},
				Status: api.PodStatus{},
			},
			opts:              &api.PodLogOptions{},
			expectedErr:       errors.NewBadRequest("a container name must be specified for pod test, choose one of: [container1 container2]"),
			expectedTransport: nil,
		},
		{
			name: "initcontainers",
			in: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: fakePodName},
				Spec: api.PodSpec{
					Containers: []api.Container{
						{Name: "container1"},
						{Name: "container2"},
					},
					InitContainers: []api.Container{
						{Name: "initcontainer1"},
					},
				},
				Status: api.PodStatus{},
			},
			opts:              &api.PodLogOptions{},
			expectedErr:       errors.NewBadRequest("a container name must be specified for pod test, choose one of: [initcontainer1 container1 container2]"),
			expectedTransport: nil,
		},
		{
			in: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: fakePodName},
				Spec: api.PodSpec{
					Containers: []api.Container{
						{Name: "container1"},
						{Name: "container2"},
					},
					InitContainers: []api.Container{
						{Name: "initcontainer1"},
					},
					EphemeralContainers: []api.EphemeralContainer{
						{EphemeralContainerCommon: api.EphemeralContainerCommon{Name: "debugger"}},
					},
				},
				Status: api.PodStatus{},
			},
			opts:              &api.PodLogOptions{},
			expectedErr:       errors.NewBadRequest("a container name must be specified for pod test, choose one of: [initcontainer1 container1 container2 debugger]"),
			expectedTransport: nil,
		},
		{
			name: "bad container",
			in: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: fakePodName},
				Spec: api.PodSpec{
					Containers: []api.Container{
						{Name: "container1"},
						{Name: "container2"},
					},
				},
				Status: api.PodStatus{},
			},
			opts: &api.PodLogOptions{
				Container: "unknown",
			},
			expectedErr:       errors.NewBadRequest("container unknown is not valid for pod test"),
			expectedTransport: nil,
		},
		{
			name: "good with two containers",
			in: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: fakePodName},
				Spec: api.PodSpec{
					Containers: []api.Container{
						{Name: "container1"},
						{Name: "container2"},
					},
					NodeName: "foo",
				},
				Status: api.PodStatus{},
			},
			opts: &api.PodLogOptions{
				Container: "container2",
			},
			expectedErr:       nil,
			expectedTransport: fakeSecureRoundTripper,
		},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			getter := &mockPodGetter{tc.in}
			connectionGetter := &mockConnectionInfoGetter{&client.ConnectionInfo{
				Transport:                      fakeSecureRoundTripper,
				InsecureSkipTLSVerifyTransport: fakeInsecureRoundTripper,
			}}

			_, actualTransport, err := LogLocation(ctx, getter, connectionGetter, fakePodName, tc.opts)
			if !reflect.DeepEqual(err, tc.expectedErr) {
				t.Errorf("expected %q, got %q", tc.expectedErr, err)
			}
			if actualTransport != tc.expectedTransport {
				t.Errorf("expected %q, got %q", tc.expectedTransport, actualTransport)
			}
		})
	}
}

func TestSelectableFieldLabelConversions(t *testing.T) {
	apitesting.TestSelectableFieldLabelConversionsOfKind(t,
		"v1",
		"Pod",
		ToSelectableFields(&api.Pod{}),
		nil,
	)
}

type mockConnectionInfoGetter struct {
	info *client.ConnectionInfo
}

func (g mockConnectionInfoGetter) GetConnectionInfo(ctx context.Context, nodeName types.NodeName) (*client.ConnectionInfo, error) {
	return g.info, nil
}

func TestPortForwardLocation(t *testing.T) {
	ctx := genericapirequest.NewDefaultContext()
	tcs := []struct {
		in          *api.Pod
		info        *client.ConnectionInfo
		opts        *api.PodPortForwardOptions
		expectedErr error
		expectedURL *url.URL
	}{
		{
			in: &api.Pod{
				Spec: api.PodSpec{},
			},
			opts:        &api.PodPortForwardOptions{},
			expectedErr: errors.NewBadRequest("pod test does not have a host assigned"),
		},
		{
			in: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "ns",
					Name:      "pod1",
				},
				Spec: api.PodSpec{
					NodeName: "node1",
				},
			},
			info:        &client.ConnectionInfo{},
			opts:        &api.PodPortForwardOptions{},
			expectedURL: &url.URL{Host: ":", Path: "/portForward/ns/pod1"},
		},
		{
			in: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "ns",
					Name:      "pod1",
				},
				Spec: api.PodSpec{
					NodeName: "node1",
				},
			},
			info:        &client.ConnectionInfo{},
			opts:        &api.PodPortForwardOptions{Ports: []int32{80}},
			expectedURL: &url.URL{Host: ":", Path: "/portForward/ns/pod1", RawQuery: "port=80"},
		},
	}
	for _, tc := range tcs {
		getter := &mockPodGetter{tc.in}
		connectionGetter := &mockConnectionInfoGetter{tc.info}
		loc, _, err := PortForwardLocation(ctx, getter, connectionGetter, "test", tc.opts)
		if !reflect.DeepEqual(err, tc.expectedErr) {
			t.Errorf("expected %v, got %v", tc.expectedErr, err)
		}
		if !reflect.DeepEqual(loc, tc.expectedURL) {
			t.Errorf("expected %v, got %v", tc.expectedURL, loc)
		}
	}
}

func TestGetPodIP(t *testing.T) {
	testCases := []struct {
		name       string
		pod        *api.Pod
		expectedIP string
	}{
		{
			name:       "nil pod",
			pod:        nil,
			expectedIP: "",
		},
		{
			name: "no status object",
			pod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "pod1"},
				Spec:       api.PodSpec{},
			},
			expectedIP: "",
		},
		{
			name: "no pod ips",
			pod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "pod1"},
				Spec:       api.PodSpec{},
				Status:     api.PodStatus{},
			},
			expectedIP: "",
		},
		{
			name: "empty list",
			pod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "pod1"},
				Spec:       api.PodSpec{},
				Status: api.PodStatus{
					PodIPs: []api.PodIP{},
				},
			},
			expectedIP: "",
		},
		{
			name: "1 ip",
			pod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "pod1"},
				Spec:       api.PodSpec{},
				Status: api.PodStatus{
					PodIPs: []api.PodIP{
						{IP: "10.0.0.10"},
					},
				},
			},
			expectedIP: "10.0.0.10",
		},
		{
			name: "multiple ips",
			pod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "pod1"},
				Spec:       api.PodSpec{},
				Status: api.PodStatus{
					PodIPs: []api.PodIP{
						{IP: "10.0.0.10"},
						{IP: "10.0.0.20"},
					},
				},
			},
			expectedIP: "10.0.0.10",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			podIP := getPodIP(tc.pod)
			if podIP != tc.expectedIP {
				t.Errorf("expected pod ip:%v does not match actual %v", tc.expectedIP, podIP)
			}
		})
	}
}

type fakeTransport struct {
	val string
}

func (f fakeTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, nil
}

var (
	fakeSecureRoundTripper   = fakeTransport{val: "secure"}
	fakeInsecureRoundTripper = fakeTransport{val: "insecure"}
)

func TestPodIndexFunc(t *testing.T) {
	tcs := []struct {
		name          string
		indexFunc     cache.IndexFunc
		pod           interface{}
		expectedValue string
		expectedErr   error
	}{
		{
			name:      "node name index",
			indexFunc: NodeNameIndexFunc,
			pod: &api.Pod{
				Spec: api.PodSpec{
					NodeName: "test-pod",
				},
			},
			expectedValue: "test-pod",
			expectedErr:   nil,
		},
		{
			name:          "not a pod failed",
			indexFunc:     NodeNameIndexFunc,
			pod:           "not a pod object",
			expectedValue: "test-pod",
			expectedErr:   fmt.Errorf("not a pod"),
		},
	}

	for _, tc := range tcs {
		indexValues, err := tc.indexFunc(tc.pod)
		if !reflect.DeepEqual(err, tc.expectedErr) {
			t.Errorf("name %v, expected %v, got %v", tc.name, tc.expectedErr, err)
		}
		if err == nil && len(indexValues) != 1 && indexValues[0] != tc.expectedValue {
			t.Errorf("name %v, expected %v, got %v", tc.name, tc.expectedValue, indexValues)
		}

	}
}

func newPodWithHugePageValue(resourceName api.ResourceName, value resource.Quantity) *api.Pod {
	return &api.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       "default",
			Name:            "foo",
			ResourceVersion: "1",
		},
		Spec: api.PodSpec{
			RestartPolicy: api.RestartPolicyAlways,
			DNSPolicy:     api.DNSDefault,
			Containers: []api.Container{{
				Name:                     "foo",
				Image:                    "image",
				ImagePullPolicy:          "IfNotPresent",
				TerminationMessagePolicy: "File",
				Resources: api.ResourceRequirements{
					Requests: api.ResourceList{
						api.ResourceCPU: resource.MustParse("10"),
						resourceName:    value,
					},
					Limits: api.ResourceList{
						api.ResourceCPU: resource.MustParse("10"),
						resourceName:    value,
					},
				}},
			},
		},
	}
}

func TestPodStrategyValidate(t *testing.T) {
	const containerName = "container"

	tests := []struct {
		name    string
		pod     *api.Pod
		wantErr bool
	}{
		{
			name:    "a new pod setting container with indivisible hugepages values",
			pod:     newPodWithHugePageValue(api.ResourceHugePagesPrefix+"1Mi", resource.MustParse("1.1Mi")),
			wantErr: true,
		},
		{
			name: "a new pod setting init-container with indivisible hugepages values",
			pod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      "foo",
				},
				Spec: api.PodSpec{
					RestartPolicy: api.RestartPolicyAlways,
					DNSPolicy:     api.DNSDefault,
					InitContainers: []api.Container{{
						Name:                     containerName,
						Image:                    "image",
						ImagePullPolicy:          "IfNotPresent",
						TerminationMessagePolicy: "File",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{
								api.ResourceName(api.ResourceHugePagesPrefix + "64Ki"): resource.MustParse("127Ki"),
							},
							Limits: api.ResourceList{
								api.ResourceName(api.ResourceHugePagesPrefix + "64Ki"): resource.MustParse("127Ki"),
							},
						}},
					},
				},
			},
			wantErr: true,
		},
		{
			name: "a new pod setting init-container with indivisible hugepages values while container with divisible hugepages values",
			pod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      "foo",
				},
				Spec: api.PodSpec{
					RestartPolicy: api.RestartPolicyAlways,
					DNSPolicy:     api.DNSDefault,
					InitContainers: []api.Container{{
						Name:                     containerName,
						Image:                    "image",
						ImagePullPolicy:          "IfNotPresent",
						TerminationMessagePolicy: "File",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{
								api.ResourceName(api.ResourceHugePagesPrefix + "2Mi"): resource.MustParse("5.1Mi"),
							},
							Limits: api.ResourceList{
								api.ResourceName(api.ResourceHugePagesPrefix + "2Mi"): resource.MustParse("5.1Mi"),
							},
						}},
					},
					Containers: []api.Container{{
						Name:                     containerName,
						Image:                    "image",
						ImagePullPolicy:          "IfNotPresent",
						TerminationMessagePolicy: "File",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{
								api.ResourceName(api.ResourceHugePagesPrefix + "1Gi"): resource.MustParse("2Gi"),
							},
							Limits: api.ResourceList{
								api.ResourceName(api.ResourceHugePagesPrefix + "1Gi"): resource.MustParse("2Gi"),
							},
						}},
					},
				},
			},
			wantErr: true,
		},
		{
			name: "a new pod setting container with divisible hugepages values",
			pod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      "foo",
				},
				Spec: api.PodSpec{
					RestartPolicy: api.RestartPolicyAlways,
					DNSPolicy:     api.DNSDefault,
					Containers: []api.Container{{
						Name:                     containerName,
						Image:                    "image",
						ImagePullPolicy:          "IfNotPresent",
						TerminationMessagePolicy: "File",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{
								api.ResourceName(api.ResourceCPU):                     resource.MustParse("10"),
								api.ResourceName(api.ResourceHugePagesPrefix + "1Mi"): resource.MustParse("2Mi"),
							},
							Limits: api.ResourceList{
								api.ResourceName(api.ResourceCPU):                     resource.MustParse("10"),
								api.ResourceName(api.ResourceHugePagesPrefix + "1Mi"): resource.MustParse("2Mi"),
							},
						}},
					},
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			errs := Strategy.Validate(genericapirequest.NewContext(), tc.pod)
			if tc.wantErr && len(errs) == 0 {
				t.Errorf("expected errors but got none")
			}
			if !tc.wantErr && len(errs) != 0 {
				t.Errorf("unexpected errors: %v", errs.ToAggregate())
			}
		})
	}
}

func TestEphemeralContainerStrategyValidateUpdate(t *testing.T) {

	test := []struct {
		name   string
		newPod *api.Pod
		oldPod *api.Pod
	}{
		{
			name: "add ephemeral container to regular pod and expect success",
			oldPod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "test-pod",
					Namespace:       "test-ns",
					ResourceVersion: "1",
				},
				Spec: api.PodSpec{
					RestartPolicy: api.RestartPolicyAlways,
					DNSPolicy:     api.DNSDefault,
					Containers: []api.Container{
						{
							Name:                     "container",
							Image:                    "image",
							ImagePullPolicy:          "IfNotPresent",
							TerminationMessagePolicy: "File",
						},
					},
				},
			},
			newPod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "test-pod",
					Namespace:       "test-ns",
					ResourceVersion: "1",
				},
				Spec: api.PodSpec{
					RestartPolicy: api.RestartPolicyAlways,
					DNSPolicy:     api.DNSDefault,
					Containers: []api.Container{
						{
							Name:                     "container",
							Image:                    "image",
							ImagePullPolicy:          "IfNotPresent",
							TerminationMessagePolicy: "File",
						},
					},
					EphemeralContainers: []api.EphemeralContainer{
						{
							EphemeralContainerCommon: api.EphemeralContainerCommon{
								Name:                     "debugger",
								Image:                    "image",
								ImagePullPolicy:          "IfNotPresent",
								TerminationMessagePolicy: "File",
							},
						},
					},
				},
			},
		},
	}

	// expect no errors
	for _, tc := range test {
		t.Run(tc.name, func(t *testing.T) {
			if errs := EphemeralContainersStrategy.ValidateUpdate(genericapirequest.NewContext(), tc.newPod, tc.oldPod); len(errs) != 0 {
				t.Errorf("unexpected error:%v", errs)
			}
		})
	}

	test = []struct {
		name   string
		newPod *api.Pod
		oldPod *api.Pod
	}{
		{
			name: "add ephemeral container to static pod and expect failure",
			oldPod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "test-pod",
					Namespace:       "test-ns",
					ResourceVersion: "1",
					Annotations:     map[string]string{api.MirrorPodAnnotationKey: "someVal"},
				},
				Spec: api.PodSpec{
					RestartPolicy: api.RestartPolicyAlways,
					DNSPolicy:     api.DNSDefault,
					Containers: []api.Container{
						{
							Name:                     "container",
							Image:                    "image",
							ImagePullPolicy:          "IfNotPresent",
							TerminationMessagePolicy: "File",
						},
					},
					NodeName: "example.com",
				},
			},
			newPod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "test-pod",
					Namespace:       "test-ns",
					ResourceVersion: "1",
					Annotations:     map[string]string{api.MirrorPodAnnotationKey: "someVal"},
				},
				Spec: api.PodSpec{
					RestartPolicy: api.RestartPolicyAlways,
					DNSPolicy:     api.DNSDefault,
					Containers: []api.Container{
						{
							Name:                     "container",
							Image:                    "image",
							ImagePullPolicy:          "IfNotPresent",
							TerminationMessagePolicy: "File",
						},
					},
					EphemeralContainers: []api.EphemeralContainer{
						{
							EphemeralContainerCommon: api.EphemeralContainerCommon{
								Name:                     "debugger",
								Image:                    "image",
								ImagePullPolicy:          "IfNotPresent",
								TerminationMessagePolicy: "File",
							},
						},
					},
					NodeName: "example.com",
				},
			},
		},
		{
			name: "remove ephemeral container from regular pod and expect failure",
			newPod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "test-pod",
					Namespace:       "test-ns",
					ResourceVersion: "1",
				},
				Spec: api.PodSpec{
					RestartPolicy: api.RestartPolicyAlways,
					DNSPolicy:     api.DNSDefault,
					Containers: []api.Container{
						{
							Name:                     "container",
							Image:                    "image",
							ImagePullPolicy:          "IfNotPresent",
							TerminationMessagePolicy: "File",
						},
					},
				},
			},
			oldPod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "test-pod",
					Namespace:       "test-ns",
					ResourceVersion: "1",
				},
				Spec: api.PodSpec{
					RestartPolicy: api.RestartPolicyAlways,
					DNSPolicy:     api.DNSDefault,
					Containers: []api.Container{
						{
							Name:                     "container",
							Image:                    "image",
							ImagePullPolicy:          "IfNotPresent",
							TerminationMessagePolicy: "File",
						},
					},
					EphemeralContainers: []api.EphemeralContainer{
						{
							EphemeralContainerCommon: api.EphemeralContainerCommon{
								Name:                     "debugger",
								Image:                    "image",
								ImagePullPolicy:          "IfNotPresent",
								TerminationMessagePolicy: "File",
							},
						},
					},
				},
			},
		},
		{
			name: "change ephemeral container from regular pod and expect failure",
			newPod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "test-pod",
					Namespace:       "test-ns",
					ResourceVersion: "1",
				},
				Spec: api.PodSpec{
					RestartPolicy: api.RestartPolicyAlways,
					DNSPolicy:     api.DNSDefault,
					Containers: []api.Container{
						{
							Name:                     "container",
							Image:                    "image",
							ImagePullPolicy:          "IfNotPresent",
							TerminationMessagePolicy: "File",
						},
					},
					EphemeralContainers: []api.EphemeralContainer{
						{
							EphemeralContainerCommon: api.EphemeralContainerCommon{
								Name:                     "debugger",
								Image:                    "image2",
								ImagePullPolicy:          "IfNotPresent",
								TerminationMessagePolicy: "File",
							},
						},
					},
				},
			},
			oldPod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "test-pod",
					Namespace:       "test-ns",
					ResourceVersion: "1",
				},
				Spec: api.PodSpec{
					RestartPolicy: api.RestartPolicyAlways,
					DNSPolicy:     api.DNSDefault,
					Containers: []api.Container{
						{
							Name:                     "container",
							Image:                    "image",
							ImagePullPolicy:          "IfNotPresent",
							TerminationMessagePolicy: "File",
						},
					},
					EphemeralContainers: []api.EphemeralContainer{
						{
							EphemeralContainerCommon: api.EphemeralContainerCommon{
								Name:                     "debugger",
								Image:                    "image",
								ImagePullPolicy:          "IfNotPresent",
								TerminationMessagePolicy: "File",
							},
						},
					},
				},
			},
		},
	}

	// expect one error
	for _, tc := range test {
		t.Run(tc.name, func(t *testing.T) {
			errs := EphemeralContainersStrategy.ValidateUpdate(genericapirequest.NewContext(), tc.newPod, tc.oldPod)
			if len(errs) == 0 {
				t.Errorf("unexpected success:ephemeral containers are not supported for static pods")
			} else if len(errs) != 1 {
				t.Errorf("unexpected errors:expected one error about ephemeral containers are not supported for static pods:got:%v:", errs)
			}
		})
	}
}

func TestPodStrategyValidateUpdate(t *testing.T) {
	test := []struct {
		name   string
		newPod *api.Pod
		oldPod *api.Pod
	}{
		{
			name:   "an existing pod with indivisible hugepages values to a new pod with indivisible hugepages values",
			newPod: newPodWithHugePageValue(api.ResourceHugePagesPrefix+"2Mi", resource.MustParse("2.1Mi")),
			oldPod: newPodWithHugePageValue(api.ResourceHugePagesPrefix+"2Mi", resource.MustParse("2.1Mi")),
		},
	}

	for _, tc := range test {
		t.Run(tc.name, func(t *testing.T) {
			if errs := Strategy.ValidateUpdate(genericapirequest.NewContext(), tc.newPod, tc.oldPod); len(errs) != 0 {
				t.Errorf("unexpected error:%v", errs)
			}
		})
	}
}

func TestDropNonEphemeralContainerUpdates(t *testing.T) {
	tests := []struct {
		name                    string
		oldPod, newPod, wantPod *api.Pod
	}{
		{
			name: "simple ephemeral container append",
			oldPod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "test-pod",
					Namespace:       "test-ns",
					ResourceVersion: "1",
				},
				Spec: api.PodSpec{
					Containers: []api.Container{
						{
							Name:  "container",
							Image: "image",
						},
					},
				},
			},
			newPod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "test-pod",
					Namespace:       "test-ns",
					ResourceVersion: "1",
				},
				Spec: api.PodSpec{
					Containers: []api.Container{
						{
							Name:  "container",
							Image: "image",
						},
					},
					EphemeralContainers: []api.EphemeralContainer{
						{
							EphemeralContainerCommon: api.EphemeralContainerCommon{
								Name:  "container",
								Image: "image",
							},
						},
					},
				},
			},
			wantPod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "test-pod",
					Namespace:       "test-ns",
					ResourceVersion: "1",
				},
				Spec: api.PodSpec{
					Containers: []api.Container{
						{
							Name:  "container",
							Image: "image",
						},
					},
					EphemeralContainers: []api.EphemeralContainer{
						{
							EphemeralContainerCommon: api.EphemeralContainerCommon{
								Name:  "container",
								Image: "image",
							},
						},
					},
				},
			},
		},
		{
			name: "whoops wrong pod",
			oldPod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "test-pod",
					Namespace:       "test-ns",
					ResourceVersion: "1",
					UID:             "blue",
				},
			},
			newPod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "new-pod",
					Namespace:       "new-ns",
					ResourceVersion: "1",
					UID:             "green",
				},
			},
			wantPod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "new-pod",
					Namespace:       "new-ns",
					ResourceVersion: "1",
					UID:             "green",
				},
			},
		},
		{
			name: "resource conflict during update",
			oldPod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "test-pod",
					Namespace:       "test-ns",
					ResourceVersion: "2",
					UID:             "blue",
				},
			},
			newPod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "test-pod",
					Namespace:       "test-ns",
					ResourceVersion: "1",
					UID:             "blue",
				},
			},
			wantPod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "test-pod",
					Namespace:       "test-ns",
					ResourceVersion: "1",
					UID:             "blue",
				},
			},
		},
		{
			name: "drop non-ephemeral container changes",
			oldPod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "test-pod",
					Namespace:       "test-ns",
					ResourceVersion: "1",
					Annotations:     map[string]string{"foo": "bar"},
				},
				Spec: api.PodSpec{
					Containers: []api.Container{
						{
							Name:  "container",
							Image: "image",
						},
					},
				},
			},
			newPod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "test-pod",
					Namespace:       "test-ns",
					ResourceVersion: "1",
					Annotations:     map[string]string{"foo": "bar", "whiz": "pop"},
					Finalizers:      []string{"milo"},
				},
				Spec: api.PodSpec{
					Containers: []api.Container{
						{
							Name:  "container",
							Image: "newimage",
						},
					},
					EphemeralContainers: []api.EphemeralContainer{
						{
							EphemeralContainerCommon: api.EphemeralContainerCommon{
								Name:  "container1",
								Image: "image",
							},
						},
						{
							EphemeralContainerCommon: api.EphemeralContainerCommon{
								Name:  "container2",
								Image: "image",
							},
						},
					},
				},
				Status: api.PodStatus{
					Message: "hi.",
				},
			},
			wantPod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "test-pod",
					Namespace:       "test-ns",
					ResourceVersion: "1",
					Annotations:     map[string]string{"foo": "bar"},
				},
				Spec: api.PodSpec{
					Containers: []api.Container{
						{
							Name:  "container",
							Image: "image",
						},
					},
					EphemeralContainers: []api.EphemeralContainer{
						{
							EphemeralContainerCommon: api.EphemeralContainerCommon{
								Name:  "container1",
								Image: "image",
							},
						},
						{
							EphemeralContainerCommon: api.EphemeralContainerCommon{
								Name:  "container2",
								Image: "image",
							},
						},
					},
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotPod := dropNonEphemeralContainerUpdates(tc.newPod, tc.oldPod)
			if diff := cmp.Diff(tc.wantPod, gotPod); diff != "" {
				t.Errorf("unexpected diff when dropping fields (-want, +got):\n%s", diff)
			}
		})
	}
}

func TestNodeInclusionPolicyEnablementInCreating(t *testing.T) {
	var (
		honor            = api.NodeInclusionPolicyHonor
		ignore           = api.NodeInclusionPolicyIgnore
		emptyConstraints = []api.TopologySpreadConstraint{
			{
				WhenUnsatisfiable: api.DoNotSchedule,
				TopologyKey:       "kubernetes.io/hostname",
				MaxSkew:           1,
			},
		}
		defaultConstraints = []api.TopologySpreadConstraint{
			{
				NodeAffinityPolicy: &honor,
				NodeTaintsPolicy:   &ignore,
				WhenUnsatisfiable:  api.DoNotSchedule,
				TopologyKey:        "kubernetes.io/hostname",
				MaxSkew:            1,
			},
		}
	)

	tests := []struct {
		name                          string
		topologySpreadConstraints     []api.TopologySpreadConstraint
		wantTopologySpreadConstraints []api.TopologySpreadConstraint
		enableNodeInclusionPolicy     bool
	}{
		{
			name:                          "nodeInclusionPolicy enabled with topology unset",
			topologySpreadConstraints:     emptyConstraints,
			wantTopologySpreadConstraints: emptyConstraints,
			enableNodeInclusionPolicy:     true,
		},
		{
			name:                          "nodeInclusionPolicy enabled with topology configured",
			topologySpreadConstraints:     defaultConstraints,
			wantTopologySpreadConstraints: defaultConstraints,
			enableNodeInclusionPolicy:     true,
		},
		{
			name:                          "nodeInclusionPolicy disabled with topology configured",
			topologySpreadConstraints:     defaultConstraints,
			wantTopologySpreadConstraints: emptyConstraints,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.NodeInclusionPolicyInPodTopologySpread, tc.enableNodeInclusionPolicy)()

			pod := &api.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      "foo",
				},
				Spec: api.PodSpec{
					RestartPolicy: api.RestartPolicyAlways,
					DNSPolicy:     api.DNSDefault,
					Containers: []api.Container{
						{
							Name:                     "container",
							Image:                    "image",
							ImagePullPolicy:          "IfNotPresent",
							TerminationMessagePolicy: "File",
						},
					},
				},
			}
			wantPod := pod.DeepCopy()
			pod.Spec.TopologySpreadConstraints = append(pod.Spec.TopologySpreadConstraints, tc.topologySpreadConstraints...)

			errs := Strategy.Validate(genericapirequest.NewContext(), pod)
			if len(errs) != 0 {
				t.Errorf("Unexpected error: %v", errs.ToAggregate())
			}

			Strategy.PrepareForCreate(genericapirequest.NewContext(), pod)
			wantPod.Spec.TopologySpreadConstraints = append(wantPod.Spec.TopologySpreadConstraints, tc.wantTopologySpreadConstraints...)
			if diff := cmp.Diff(wantPod, pod, cmpopts.IgnoreFields(pod.Status, "Phase", "QOSClass")); diff != "" {
				t.Errorf("%s unexpected result (-want, +got): %s", tc.name, diff)
			}
		})
	}
}

func TestNodeInclusionPolicyEnablementInUpdating(t *testing.T) {
	var (
		honor  = api.NodeInclusionPolicyHonor
		ignore = api.NodeInclusionPolicyIgnore
	)

	// Enable the Feature Gate during the first rule creation
	defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.NodeInclusionPolicyInPodTopologySpread, true)()
	ctx := genericapirequest.NewDefaultContext()

	pod := &api.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       "default",
			Name:            "foo",
			ResourceVersion: "1",
		},
		Spec: api.PodSpec{
			RestartPolicy: api.RestartPolicyAlways,
			DNSPolicy:     api.DNSDefault,
			Containers: []api.Container{
				{
					Name:                     "container",
					Image:                    "image",
					ImagePullPolicy:          "IfNotPresent",
					TerminationMessagePolicy: "File",
				},
			},
			TopologySpreadConstraints: []api.TopologySpreadConstraint{
				{
					NodeAffinityPolicy: &ignore,
					NodeTaintsPolicy:   &honor,
					WhenUnsatisfiable:  api.DoNotSchedule,
					TopologyKey:        "kubernetes.io/hostname",
					MaxSkew:            1,
				},
			},
		},
	}

	errs := Strategy.Validate(ctx, pod)
	if len(errs) != 0 {
		t.Errorf("Unexpected error: %v", errs.ToAggregate())
	}

	createdPod := pod.DeepCopy()
	Strategy.PrepareForCreate(ctx, createdPod)

	if len(createdPod.Spec.TopologySpreadConstraints) != 1 ||
		*createdPod.Spec.TopologySpreadConstraints[0].NodeAffinityPolicy != ignore ||
		*createdPod.Spec.TopologySpreadConstraints[0].NodeTaintsPolicy != honor {
		t.Error("NodeInclusionPolicy created with unexpected result")
	}

	// Disable the Feature Gate and expect these fields still exist after updating.
	defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.NodeInclusionPolicyInPodTopologySpread, false)()

	updatedPod := createdPod.DeepCopy()
	updatedPod.Labels = map[string]string{"foo": "bar"}
	updatedPod.ResourceVersion = "2"

	errs = Strategy.ValidateUpdate(ctx, updatedPod, createdPod)
	if len(errs) != 0 {
		t.Errorf("Unexpected error: %v", errs.ToAggregate())
	}

	Strategy.PrepareForUpdate(ctx, updatedPod, createdPod)

	if len(updatedPod.Spec.TopologySpreadConstraints) != 1 ||
		*updatedPod.Spec.TopologySpreadConstraints[0].NodeAffinityPolicy != ignore ||
		*updatedPod.Spec.TopologySpreadConstraints[0].NodeTaintsPolicy != honor {
		t.Error("NodeInclusionPolicy updated with unexpected result")
	}

	// Enable the Feature Gate again to check whether configured fields still exist after updating.
	defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.NodeInclusionPolicyInPodTopologySpread, true)()

	updatedPod2 := updatedPod.DeepCopy()
	updatedPod2.Labels = map[string]string{"foo": "fuz"}
	updatedPod2.ResourceVersion = "3"

	errs = Strategy.ValidateUpdate(ctx, updatedPod2, updatedPod)
	if len(errs) != 0 {
		t.Errorf("Unexpected error: %v", errs.ToAggregate())
	}

	Strategy.PrepareForUpdate(ctx, updatedPod2, updatedPod)
	if len(updatedPod2.Spec.TopologySpreadConstraints) != 1 ||
		*updatedPod2.Spec.TopologySpreadConstraints[0].NodeAffinityPolicy != ignore ||
		*updatedPod2.Spec.TopologySpreadConstraints[0].NodeTaintsPolicy != honor {
		t.Error("NodeInclusionPolicy updated with unexpected result")
	}
}
