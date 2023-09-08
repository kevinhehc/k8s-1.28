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

package resourceclaim

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"

	v1 "k8s.io/api/core/v1"
	resourcev1alpha2 "k8s.io/api/resource/v1alpha2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/component-base/metrics/testutil"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/controller"
	ephemeralvolumemetrics "k8s.io/kubernetes/pkg/controller/resourceclaim/metrics"
	"k8s.io/utils/pointer"
)

var (
	testPodName          = "test-pod"
	testNamespace        = "my-namespace"
	testPodUID           = types.UID("uidpod1")
	otherNamespace       = "not-my-namespace"
	podResourceClaimName = "acme-resource"
	templateName         = "my-template"
	className            = "my-resource-class"
	nodeName             = "worker"

	testPod             = makePod(testPodName, testNamespace, testPodUID)
	testPodWithResource = makePod(testPodName, testNamespace, testPodUID, *makePodResourceClaim(podResourceClaimName, templateName))
	otherTestPod        = makePod(testPodName+"-II", testNamespace, testPodUID+"-II")

	testClaim              = makeClaim(testPodName+"-"+podResourceClaimName, testNamespace, className, makeOwnerReference(testPodWithResource, true))
	testClaimAllocated     = allocateClaim(testClaim)
	testClaimReserved      = reserveClaim(testClaimAllocated, testPodWithResource)
	testClaimReservedTwice = reserveClaim(testClaimReserved, otherTestPod)

	generatedTestClaim          = makeGeneratedClaim(podResourceClaimName, testPodName+"-"+podResourceClaimName, testNamespace, className, 1, makeOwnerReference(testPodWithResource, true))
	generatedTestClaimAllocated = allocateClaim(generatedTestClaim)
	generatedTestClaimReserved  = reserveClaim(generatedTestClaimAllocated, testPodWithResource)

	conflictingClaim    = makeClaim(testPodName+"-"+podResourceClaimName, testNamespace, className, nil)
	otherNamespaceClaim = makeClaim(testPodName+"-"+podResourceClaimName, otherNamespace, className, nil)
	template            = makeTemplate(templateName, testNamespace, className)

	testPodWithNodeName = func() *v1.Pod {
		pod := testPodWithResource.DeepCopy()
		pod.Spec.NodeName = nodeName
		pod.Status.ResourceClaimStatuses = append(pod.Status.ResourceClaimStatuses, v1.PodResourceClaimStatus{
			Name:              pod.Spec.ResourceClaims[0].Name,
			ResourceClaimName: &generatedTestClaim.Name,
		})
		return pod
	}()

	podSchedulingContext = resourcev1alpha2.PodSchedulingContext{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testPodName,
			Namespace: testNamespace,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "v1",
					Kind:       "Pod",
					Name:       testPodName,
					UID:        testPodUID,
					Controller: pointer.Bool(true),
				},
			},
		},
		Spec: resourcev1alpha2.PodSchedulingContextSpec{
			SelectedNode: nodeName,
		},
	}
)

func init() {
	klog.InitFlags(nil)
}

func TestSyncHandler(t *testing.T) {
	tests := []struct {
		name                          string
		key                           string
		claims                        []*resourcev1alpha2.ResourceClaim
		claimsInCache                 []*resourcev1alpha2.ResourceClaim
		pods                          []*v1.Pod
		podsLater                     []*v1.Pod
		templates                     []*resourcev1alpha2.ResourceClaimTemplate
		expectedClaims                []resourcev1alpha2.ResourceClaim
		expectedPodSchedulingContexts []resourcev1alpha2.PodSchedulingContext
		expectedStatuses              map[string][]v1.PodResourceClaimStatus
		expectedError                 bool
		expectedMetrics               expectedMetrics
	}{
		{
			name:           "create",
			pods:           []*v1.Pod{testPodWithResource},
			templates:      []*resourcev1alpha2.ResourceClaimTemplate{template},
			key:            podKey(testPodWithResource),
			expectedClaims: []resourcev1alpha2.ResourceClaim{*generatedTestClaim},
			expectedStatuses: map[string][]v1.PodResourceClaimStatus{
				testPodWithResource.Name: {
					{Name: testPodWithResource.Spec.ResourceClaims[0].Name, ResourceClaimName: &generatedTestClaim.Name},
				},
			},
			expectedMetrics: expectedMetrics{1, 0},
		},
		{
			name: "nop",
			pods: []*v1.Pod{func() *v1.Pod {
				pod := testPodWithResource.DeepCopy()
				pod.Status.ResourceClaimStatuses = []v1.PodResourceClaimStatus{
					{Name: testPodWithResource.Spec.ResourceClaims[0].Name, ResourceClaimName: &generatedTestClaim.Name},
				}
				return pod
			}()},
			templates:      []*resourcev1alpha2.ResourceClaimTemplate{template},
			key:            podKey(testPodWithResource),
			claims:         []*resourcev1alpha2.ResourceClaim{generatedTestClaim},
			expectedClaims: []resourcev1alpha2.ResourceClaim{*generatedTestClaim},
			expectedStatuses: map[string][]v1.PodResourceClaimStatus{
				testPodWithResource.Name: {
					{Name: testPodWithResource.Spec.ResourceClaims[0].Name, ResourceClaimName: &generatedTestClaim.Name},
				},
			},
			expectedMetrics: expectedMetrics{0, 0},
		},
		{
			name: "recreate",
			pods: []*v1.Pod{func() *v1.Pod {
				pod := testPodWithResource.DeepCopy()
				pod.Status.ResourceClaimStatuses = []v1.PodResourceClaimStatus{
					{Name: testPodWithResource.Spec.ResourceClaims[0].Name, ResourceClaimName: &generatedTestClaim.Name},
				}
				return pod
			}()},
			templates:      []*resourcev1alpha2.ResourceClaimTemplate{template},
			key:            podKey(testPodWithResource),
			expectedClaims: []resourcev1alpha2.ResourceClaim{*generatedTestClaim},
			expectedStatuses: map[string][]v1.PodResourceClaimStatus{
				testPodWithResource.Name: {
					{Name: testPodWithResource.Spec.ResourceClaims[0].Name, ResourceClaimName: &generatedTestClaim.Name},
				},
			},
			expectedMetrics: expectedMetrics{1, 0},
		},
		{
			name:          "missing-template",
			pods:          []*v1.Pod{testPodWithResource},
			templates:     nil,
			key:           podKey(testPodWithResource),
			expectedError: true,
		},
		{
			name:           "find-existing-claim-by-label",
			pods:           []*v1.Pod{testPodWithResource},
			key:            podKey(testPodWithResource),
			claims:         []*resourcev1alpha2.ResourceClaim{generatedTestClaim},
			expectedClaims: []resourcev1alpha2.ResourceClaim{*generatedTestClaim},
			expectedStatuses: map[string][]v1.PodResourceClaimStatus{
				testPodWithResource.Name: {
					{Name: testPodWithResource.Spec.ResourceClaims[0].Name, ResourceClaimName: &generatedTestClaim.Name},
				},
			},
			expectedMetrics: expectedMetrics{0, 0},
		},
		{
			name:           "find-existing-claim-by-name",
			pods:           []*v1.Pod{testPodWithResource},
			key:            podKey(testPodWithResource),
			claims:         []*resourcev1alpha2.ResourceClaim{testClaim},
			expectedClaims: []resourcev1alpha2.ResourceClaim{*testClaim},
			expectedStatuses: map[string][]v1.PodResourceClaimStatus{
				testPodWithResource.Name: {
					{Name: testPodWithResource.Spec.ResourceClaims[0].Name, ResourceClaimName: &testClaim.Name},
				},
			},
			expectedMetrics: expectedMetrics{0, 0},
		},
		{
			name:          "find-created-claim-in-cache",
			pods:          []*v1.Pod{testPodWithResource},
			key:           podKey(testPodWithResource),
			claimsInCache: []*resourcev1alpha2.ResourceClaim{generatedTestClaim},
			expectedStatuses: map[string][]v1.PodResourceClaimStatus{
				testPodWithResource.Name: {
					{Name: testPodWithResource.Spec.ResourceClaims[0].Name, ResourceClaimName: &generatedTestClaim.Name},
				},
			},
			expectedMetrics: expectedMetrics{0, 0},
		},
		{
			name: "no-such-pod",
			key:  podKey(testPodWithResource),
		},
		{
			name: "pod-deleted",
			pods: func() []*v1.Pod {
				deleted := metav1.Now()
				pods := []*v1.Pod{testPodWithResource.DeepCopy()}
				pods[0].DeletionTimestamp = &deleted
				return pods
			}(),
			key: podKey(testPodWithResource),
		},
		{
			name: "no-volumes",
			pods: []*v1.Pod{testPod},
			key:  podKey(testPod),
		},
		{
			name:           "create-with-other-claim",
			pods:           []*v1.Pod{testPodWithResource},
			templates:      []*resourcev1alpha2.ResourceClaimTemplate{template},
			key:            podKey(testPodWithResource),
			claims:         []*resourcev1alpha2.ResourceClaim{otherNamespaceClaim},
			expectedClaims: []resourcev1alpha2.ResourceClaim{*otherNamespaceClaim, *generatedTestClaim},
			expectedStatuses: map[string][]v1.PodResourceClaimStatus{
				testPodWithResource.Name: {
					{Name: testPodWithResource.Spec.ResourceClaims[0].Name, ResourceClaimName: &generatedTestClaim.Name},
				},
			},
			expectedMetrics: expectedMetrics{1, 0},
		},
		{
			name:           "wrong-claim-owner",
			pods:           []*v1.Pod{testPodWithResource},
			key:            podKey(testPodWithResource),
			claims:         []*resourcev1alpha2.ResourceClaim{conflictingClaim},
			expectedClaims: []resourcev1alpha2.ResourceClaim{*conflictingClaim},
			expectedError:  true,
		},
		{
			name:            "create-conflict",
			pods:            []*v1.Pod{testPodWithResource},
			templates:       []*resourcev1alpha2.ResourceClaimTemplate{template},
			key:             podKey(testPodWithResource),
			expectedMetrics: expectedMetrics{1, 1},
			expectedError:   true,
		},
		{
			name:            "stay-reserved-seen",
			pods:            []*v1.Pod{testPodWithResource},
			key:             claimKey(testClaimReserved),
			claims:          []*resourcev1alpha2.ResourceClaim{testClaimReserved},
			expectedClaims:  []resourcev1alpha2.ResourceClaim{*testClaimReserved},
			expectedMetrics: expectedMetrics{0, 0},
		},
		{
			name:            "stay-reserved-not-seen",
			podsLater:       []*v1.Pod{testPodWithResource},
			key:             claimKey(testClaimReserved),
			claims:          []*resourcev1alpha2.ResourceClaim{testClaimReserved},
			expectedClaims:  []resourcev1alpha2.ResourceClaim{*testClaimReserved},
			expectedMetrics: expectedMetrics{0, 0},
		},
		{
			name:   "clear-reserved-delayed-allocation",
			pods:   []*v1.Pod{},
			key:    claimKey(testClaimReserved),
			claims: []*resourcev1alpha2.ResourceClaim{testClaimReserved},
			expectedClaims: func() []resourcev1alpha2.ResourceClaim {
				claim := testClaimAllocated.DeepCopy()
				claim.Status.DeallocationRequested = true
				return []resourcev1alpha2.ResourceClaim{*claim}
			}(),
			expectedMetrics: expectedMetrics{0, 0},
		},
		{
			name: "clear-reserved-immediate-allocation",
			pods: []*v1.Pod{},
			key:  claimKey(testClaimReserved),
			claims: func() []*resourcev1alpha2.ResourceClaim {
				claim := testClaimReserved.DeepCopy()
				claim.Spec.AllocationMode = resourcev1alpha2.AllocationModeImmediate
				return []*resourcev1alpha2.ResourceClaim{claim}
			}(),
			expectedClaims: func() []resourcev1alpha2.ResourceClaim {
				claim := testClaimAllocated.DeepCopy()
				claim.Spec.AllocationMode = resourcev1alpha2.AllocationModeImmediate
				return []resourcev1alpha2.ResourceClaim{*claim}
			}(),
			expectedMetrics: expectedMetrics{0, 0},
		},
		{
			name: "clear-reserved-when-done-delayed-allocation",
			pods: func() []*v1.Pod {
				pods := []*v1.Pod{testPodWithResource.DeepCopy()}
				pods[0].Status.Phase = v1.PodSucceeded
				return pods
			}(),
			key: claimKey(testClaimReserved),
			claims: func() []*resourcev1alpha2.ResourceClaim {
				claims := []*resourcev1alpha2.ResourceClaim{testClaimReserved.DeepCopy()}
				claims[0].OwnerReferences = nil
				return claims
			}(),
			expectedClaims: func() []resourcev1alpha2.ResourceClaim {
				claims := []resourcev1alpha2.ResourceClaim{*testClaimAllocated.DeepCopy()}
				claims[0].OwnerReferences = nil
				claims[0].Status.DeallocationRequested = true
				return claims
			}(),
			expectedMetrics: expectedMetrics{0, 0},
		},
		{
			name: "clear-reserved-when-done-immediate-allocation",
			pods: func() []*v1.Pod {
				pods := []*v1.Pod{testPodWithResource.DeepCopy()}
				pods[0].Status.Phase = v1.PodSucceeded
				return pods
			}(),
			key: claimKey(testClaimReserved),
			claims: func() []*resourcev1alpha2.ResourceClaim {
				claims := []*resourcev1alpha2.ResourceClaim{testClaimReserved.DeepCopy()}
				claims[0].OwnerReferences = nil
				claims[0].Spec.AllocationMode = resourcev1alpha2.AllocationModeImmediate
				return claims
			}(),
			expectedClaims: func() []resourcev1alpha2.ResourceClaim {
				claims := []resourcev1alpha2.ResourceClaim{*testClaimAllocated.DeepCopy()}
				claims[0].OwnerReferences = nil
				claims[0].Spec.AllocationMode = resourcev1alpha2.AllocationModeImmediate
				return claims
			}(),
			expectedMetrics: expectedMetrics{0, 0},
		},
		{
			name:            "remove-reserved",
			pods:            []*v1.Pod{testPod},
			key:             claimKey(testClaimReservedTwice),
			claims:          []*resourcev1alpha2.ResourceClaim{testClaimReservedTwice},
			expectedClaims:  []resourcev1alpha2.ResourceClaim{*testClaimReserved},
			expectedMetrics: expectedMetrics{0, 0},
		},
		{
			name: "delete-claim-when-done",
			pods: func() []*v1.Pod {
				pods := []*v1.Pod{testPodWithResource.DeepCopy()}
				pods[0].Status.Phase = v1.PodSucceeded
				return pods
			}(),
			key:             claimKey(testClaimReserved),
			claims:          []*resourcev1alpha2.ResourceClaim{testClaimReserved},
			expectedClaims:  nil,
			expectedMetrics: expectedMetrics{0, 0},
		},
		{
			name:           "trigger-allocation",
			pods:           []*v1.Pod{testPodWithNodeName},
			key:            podKey(testPodWithNodeName),
			templates:      []*resourcev1alpha2.ResourceClaimTemplate{template},
			claims:         []*resourcev1alpha2.ResourceClaim{generatedTestClaim},
			expectedClaims: []resourcev1alpha2.ResourceClaim{*generatedTestClaim},
			expectedStatuses: map[string][]v1.PodResourceClaimStatus{
				testPodWithNodeName.Name: {
					{Name: testPodWithNodeName.Spec.ResourceClaims[0].Name, ResourceClaimName: &generatedTestClaim.Name},
				},
			},
			expectedPodSchedulingContexts: []resourcev1alpha2.PodSchedulingContext{podSchedulingContext},
			expectedMetrics:               expectedMetrics{0, 0},
		},
		{
			name:           "add-reserved",
			pods:           []*v1.Pod{testPodWithNodeName},
			key:            podKey(testPodWithNodeName),
			templates:      []*resourcev1alpha2.ResourceClaimTemplate{template},
			claims:         []*resourcev1alpha2.ResourceClaim{generatedTestClaimAllocated},
			expectedClaims: []resourcev1alpha2.ResourceClaim{*generatedTestClaimReserved},
			expectedStatuses: map[string][]v1.PodResourceClaimStatus{
				testPodWithNodeName.Name: {
					{Name: testPodWithNodeName.Spec.ResourceClaims[0].Name, ResourceClaimName: &generatedTestClaim.Name},
				},
			},
			expectedMetrics: expectedMetrics{0, 0},
		},
	}

	for _, tc := range tests {
		// Run sequentially because of global logging and global metrics.
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			var objects []runtime.Object
			for _, pod := range tc.pods {
				objects = append(objects, pod)
			}
			for _, claim := range tc.claims {
				objects = append(objects, claim)
			}
			for _, template := range tc.templates {
				objects = append(objects, template)
			}

			fakeKubeClient := createTestClient(objects...)
			if tc.expectedMetrics.numFailures > 0 {
				fakeKubeClient.PrependReactor("create", "resourceclaims", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
					return true, nil, apierrors.NewConflict(action.GetResource().GroupResource(), "fake name", errors.New("fake conflict"))
				})
			}
			setupMetrics()
			informerFactory := informers.NewSharedInformerFactory(fakeKubeClient, controller.NoResyncPeriodFunc())
			podInformer := informerFactory.Core().V1().Pods()
			podSchedulingInformer := informerFactory.Resource().V1alpha2().PodSchedulingContexts()
			claimInformer := informerFactory.Resource().V1alpha2().ResourceClaims()
			templateInformer := informerFactory.Resource().V1alpha2().ResourceClaimTemplates()

			ec, err := NewController(klog.FromContext(ctx), fakeKubeClient, podInformer, podSchedulingInformer, claimInformer, templateInformer)
			if err != nil {
				t.Fatalf("error creating ephemeral controller : %v", err)
			}

			// Ensure informers are up-to-date.
			go informerFactory.Start(ctx.Done())
			stopInformers := func() {
				cancel()
				informerFactory.Shutdown()
			}
			defer stopInformers()
			informerFactory.WaitForCacheSync(ctx.Done())
			cache.WaitForCacheSync(ctx.Done(), podInformer.Informer().HasSynced, claimInformer.Informer().HasSynced, templateInformer.Informer().HasSynced)

			// Add claims that only exist in the mutation cache.
			for _, claim := range tc.claimsInCache {
				ec.claimCache.Mutation(claim)
			}

			// Simulate race: stop informers, add more pods that the controller doesn't know about.
			stopInformers()
			for _, pod := range tc.podsLater {
				_, err := fakeKubeClient.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("unexpected error while creating pod: %v", err)
				}
			}

			err = ec.syncHandler(context.TODO(), tc.key)
			if err != nil && !tc.expectedError {
				t.Fatalf("unexpected error while running handler: %v", err)
			}
			if err == nil && tc.expectedError {
				t.Fatalf("unexpected success")
			}

			claims, err := fakeKubeClient.ResourceV1alpha2().ResourceClaims("").List(ctx, metav1.ListOptions{})
			if err != nil {
				t.Fatalf("unexpected error while listing claims: %v", err)
			}
			assert.Equal(t, normalizeClaims(tc.expectedClaims), normalizeClaims(claims.Items))

			pods, err := fakeKubeClient.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
			if err != nil {
				t.Fatalf("unexpected error while listing pods: %v", err)
			}
			var actualStatuses map[string][]v1.PodResourceClaimStatus
			for _, pod := range pods.Items {
				if len(pod.Status.ResourceClaimStatuses) == 0 {
					continue
				}
				if actualStatuses == nil {
					actualStatuses = make(map[string][]v1.PodResourceClaimStatus)
				}
				actualStatuses[pod.Name] = pod.Status.ResourceClaimStatuses
			}
			assert.Equal(t, tc.expectedStatuses, actualStatuses, "pod resource claim statuses")

			scheduling, err := fakeKubeClient.ResourceV1alpha2().PodSchedulingContexts("").List(ctx, metav1.ListOptions{})
			if err != nil {
				t.Fatalf("unexpected error while listing claims: %v", err)
			}
			assert.Equal(t, normalizeScheduling(tc.expectedPodSchedulingContexts), normalizeScheduling(scheduling.Items))

			expectMetrics(t, tc.expectedMetrics)
		})
	}
}

func makeClaim(name, namespace, classname string, owner *metav1.OwnerReference) *resourcev1alpha2.ResourceClaim {
	claim := &resourcev1alpha2.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: resourcev1alpha2.ResourceClaimSpec{
			ResourceClassName: classname,
			AllocationMode:    resourcev1alpha2.AllocationModeWaitForFirstConsumer,
		},
	}
	if owner != nil {
		claim.OwnerReferences = []metav1.OwnerReference{*owner}
	}

	return claim
}

func makeGeneratedClaim(podClaimName, generateName, namespace, classname string, createCounter int, owner *metav1.OwnerReference) *resourcev1alpha2.ResourceClaim {
	claim := &resourcev1alpha2.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:         fmt.Sprintf("%s-%d", generateName, createCounter),
			GenerateName: generateName,
			Namespace:    namespace,
			Annotations:  map[string]string{"resource.kubernetes.io/pod-claim-name": podClaimName},
		},
		Spec: resourcev1alpha2.ResourceClaimSpec{
			ResourceClassName: classname,
			AllocationMode:    resourcev1alpha2.AllocationModeWaitForFirstConsumer,
		},
	}
	if owner != nil {
		claim.OwnerReferences = []metav1.OwnerReference{*owner}
	}

	return claim
}

func allocateClaim(claim *resourcev1alpha2.ResourceClaim) *resourcev1alpha2.ResourceClaim {
	claim = claim.DeepCopy()
	claim.Status.Allocation = &resourcev1alpha2.AllocationResult{
		Shareable: true,
	}
	return claim
}

func reserveClaim(claim *resourcev1alpha2.ResourceClaim, pod *v1.Pod) *resourcev1alpha2.ResourceClaim {
	claim = claim.DeepCopy()
	claim.Status.ReservedFor = append(claim.Status.ReservedFor,
		resourcev1alpha2.ResourceClaimConsumerReference{
			Resource: "pods",
			Name:     pod.Name,
			UID:      pod.UID,
		},
	)
	return claim
}

func makePodResourceClaim(name, templateName string) *v1.PodResourceClaim {
	return &v1.PodResourceClaim{
		Name: name,
		Source: v1.ClaimSource{
			ResourceClaimTemplateName: &templateName,
		},
	}
}

func makePod(name, namespace string, uid types.UID, podClaims ...v1.PodResourceClaim) *v1.Pod {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: uid},
		Spec: v1.PodSpec{
			ResourceClaims: podClaims,
		},
	}

	return pod
}

func makeTemplate(name, namespace, classname string) *resourcev1alpha2.ResourceClaimTemplate {
	template := &resourcev1alpha2.ResourceClaimTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: resourcev1alpha2.ResourceClaimTemplateSpec{
			Spec: resourcev1alpha2.ResourceClaimSpec{
				ResourceClassName: classname,
			},
		},
	}
	return template
}

func podKey(pod *v1.Pod) string {
	return podKeyPrefix + pod.Namespace + "/" + pod.Name
}

func claimKey(claim *resourcev1alpha2.ResourceClaim) string {
	return claimKeyPrefix + claim.Namespace + "/" + claim.Name
}

func makeOwnerReference(pod *v1.Pod, isController bool) *metav1.OwnerReference {
	isTrue := true
	return &metav1.OwnerReference{
		APIVersion:         "v1",
		Kind:               "Pod",
		Name:               pod.Name,
		UID:                pod.UID,
		Controller:         &isController,
		BlockOwnerDeletion: &isTrue,
	}
}

func normalizeClaims(claims []resourcev1alpha2.ResourceClaim) []resourcev1alpha2.ResourceClaim {
	sort.Slice(claims, func(i, j int) bool {
		if claims[i].Namespace < claims[j].Namespace {
			return true
		}
		if claims[i].Namespace > claims[j].Namespace {
			return false
		}
		return claims[i].Name < claims[j].Name
	})
	for i := range claims {
		if len(claims[i].Status.ReservedFor) == 0 {
			claims[i].Status.ReservedFor = nil
		}
		if claims[i].Spec.AllocationMode == "" {
			// This emulates defaulting.
			claims[i].Spec.AllocationMode = resourcev1alpha2.AllocationModeWaitForFirstConsumer
		}
	}
	return claims
}

func normalizeScheduling(scheduling []resourcev1alpha2.PodSchedulingContext) []resourcev1alpha2.PodSchedulingContext {
	sort.Slice(scheduling, func(i, j int) bool {
		return scheduling[i].Namespace < scheduling[j].Namespace ||
			scheduling[i].Name < scheduling[j].Name
	})
	return scheduling
}

func createTestClient(objects ...runtime.Object) *fake.Clientset {
	fakeClient := fake.NewSimpleClientset(objects...)
	fakeClient.PrependReactor("create", "resourceclaims", createResourceClaimReactor())
	return fakeClient
}

// createResourceClaimReactor implements the logic required for the GenerateName field to work when using
// the fake client. Add it with client.PrependReactor to your fake client.
func createResourceClaimReactor() func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
	nameCounter := 1
	var mutex sync.Mutex
	return func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		mutex.Lock()
		defer mutex.Unlock()
		claim := action.(k8stesting.CreateAction).GetObject().(*resourcev1alpha2.ResourceClaim)
		if claim.Name == "" && claim.GenerateName != "" {
			claim.Name = fmt.Sprintf("%s-%d", claim.GenerateName, nameCounter)
		}
		nameCounter++
		return false, nil, nil
	}
}

// Metrics helpers

type expectedMetrics struct {
	numCreated  int
	numFailures int
}

func expectMetrics(t *testing.T, em expectedMetrics) {
	t.Helper()

	actualCreated, err := testutil.GetCounterMetricValue(ephemeralvolumemetrics.ResourceClaimCreateAttempts)
	handleErr(t, err, "ResourceClaimCreate")
	if actualCreated != float64(em.numCreated) {
		t.Errorf("Expected claims to be created %d, got %v", em.numCreated, actualCreated)
	}
	actualConflicts, err := testutil.GetCounterMetricValue(ephemeralvolumemetrics.ResourceClaimCreateFailures)
	handleErr(t, err, "ResourceClaimCreate/Conflict")
	if actualConflicts != float64(em.numFailures) {
		t.Errorf("Expected claims to have conflicts %d, got %v", em.numFailures, actualConflicts)
	}
}

func handleErr(t *testing.T, err error, metricName string) {
	if err != nil {
		t.Errorf("Failed to get %s value, err: %v", metricName, err)
	}
}

func setupMetrics() {
	ephemeralvolumemetrics.RegisterMetrics()
	ephemeralvolumemetrics.ResourceClaimCreateAttempts.Reset()
	ephemeralvolumemetrics.ResourceClaimCreateFailures.Reset()
}
