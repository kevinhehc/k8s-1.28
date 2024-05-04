/*
Copyright 2021 The Kubernetes Authors.

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

package job

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	typedv1 "k8s.io/client-go/kubernetes/typed/batch/v1"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	basemetrics "k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/testutil"
	"k8s.io/klog/v2"
	kubeapiservertesting "k8s.io/kubernetes/cmd/kube-apiserver/app/testing"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	jobcontroller "k8s.io/kubernetes/pkg/controller/job"
	"k8s.io/kubernetes/pkg/controller/job/metrics"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/test/integration/framework"
	"k8s.io/kubernetes/test/integration/util"
	"k8s.io/utils/pointer"
)

const waitInterval = time.Second
const fastPodFailureBackoff = 100 * time.Millisecond

type metricLabelsWithValue struct {
	Labels []string
	Value  int
}

func TestMetricsOnSuccesses(t *testing.T) {
	nonIndexedCompletion := batchv1.NonIndexedCompletion
	indexedCompletion := batchv1.IndexedCompletion

	// setup the job controller
	closeFn, restConfig, clientSet, ns := setup(t, "simple")
	defer closeFn()
	ctx, cancel := startJobControllerAndWaitForCaches(restConfig)
	defer cancel()

	testCases := map[string]struct {
		job                       *batchv1.Job
		wantJobFinishedNumMetric  metricLabelsWithValue
		wantJobPodsFinishedMetric metricLabelsWithValue
	}{
		"non-indexed job": {
			job: &batchv1.Job{
				Spec: batchv1.JobSpec{
					Completions:    pointer.Int32(2),
					Parallelism:    pointer.Int32(2),
					CompletionMode: &nonIndexedCompletion,
				},
			},
			wantJobFinishedNumMetric: metricLabelsWithValue{
				Labels: []string{"NonIndexed", "succeeded", ""},
				Value:  1,
			},
			wantJobPodsFinishedMetric: metricLabelsWithValue{
				Labels: []string{"NonIndexed", "succeeded"},
				Value:  2,
			},
		},
		"indexed job": {
			job: &batchv1.Job{
				Spec: batchv1.JobSpec{
					Completions:    pointer.Int32(2),
					Parallelism:    pointer.Int32(2),
					CompletionMode: &indexedCompletion,
				},
			},
			wantJobFinishedNumMetric: metricLabelsWithValue{
				Labels: []string{"Indexed", "succeeded", ""},
				Value:  1,
			},
			wantJobPodsFinishedMetric: metricLabelsWithValue{
				Labels: []string{"Indexed", "succeeded"},
				Value:  2,
			},
		},
	}
	job_index := 0 // job index to avoid collisions between job names created by different test cases
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			resetMetrics()
			// create a single job and wait for its completion
			job := tc.job.DeepCopy()
			job.Name = fmt.Sprintf("job-%v", job_index)
			job_index++
			jobObj, err := createJobWithDefaults(ctx, clientSet, ns.Name, job)
			if err != nil {
				t.Fatalf("Failed to create Job: %v", err)
			}
			validateJobPodsStatus(ctx, t, clientSet, jobObj, podsByStatus{
				Active: int(*jobObj.Spec.Parallelism),
				Ready:  pointer.Int32(0),
			})
			if err, _ := setJobPodsPhase(ctx, clientSet, jobObj, v1.PodSucceeded, int(*jobObj.Spec.Parallelism)); err != nil {
				t.Fatalf("Failed setting phase %s on Job Pod: %v", v1.PodSucceeded, err)
			}
			validateJobSucceeded(ctx, t, clientSet, jobObj)

			// verify metric values after the job is finished
			validateCounterMetric(t, metrics.JobFinishedNum, tc.wantJobFinishedNumMetric)
			validateCounterMetric(t, metrics.JobPodsFinished, tc.wantJobPodsFinishedMetric)
			validateTerminatedPodsTrackingFinalizerMetric(t, int(*jobObj.Spec.Parallelism))
		})
	}
}

func TestJobFinishedNumReasonMetric(t *testing.T) {
	// setup the job controller
	closeFn, restConfig, clientSet, ns := setup(t, "simple")
	defer closeFn()
	ctx, cancel := startJobControllerAndWaitForCaches(restConfig)
	defer cancel()

	testCases := map[string]struct {
		job                       batchv1.Job
		podStatus                 v1.PodStatus
		enableJobPodFailurePolicy bool
		wantJobFinishedNumMetric  metricLabelsWithValue
	}{
		"non-indexed job; failed pod handled by FailJob action; JobPodFailurePolicy enabled": {
			enableJobPodFailurePolicy: true,
			job: batchv1.Job{
				Spec: batchv1.JobSpec{
					Completions:  pointer.Int32(1),
					Parallelism:  pointer.Int32(1),
					BackoffLimit: pointer.Int32(1),
					PodFailurePolicy: &batchv1.PodFailurePolicy{
						Rules: []batchv1.PodFailurePolicyRule{
							{
								Action: batchv1.PodFailurePolicyActionFailJob,
								OnExitCodes: &batchv1.PodFailurePolicyOnExitCodesRequirement{
									Operator: batchv1.PodFailurePolicyOnExitCodesOpIn,
									Values:   []int32{5},
								},
							},
						},
					},
				},
			},
			podStatus: v1.PodStatus{
				Phase: v1.PodFailed,
				ContainerStatuses: []v1.ContainerStatus{
					{
						State: v1.ContainerState{
							Terminated: &v1.ContainerStateTerminated{
								ExitCode: 5,
							},
						},
					},
				},
			},
			wantJobFinishedNumMetric: metricLabelsWithValue{
				Labels: []string{"NonIndexed", "failed", "PodFailurePolicy"},
				Value:  1,
			},
		},
		"non-indexed job; failed pod handled by Count action; JobPodFailurePolicy enabled": {
			enableJobPodFailurePolicy: true,
			job: batchv1.Job{
				Spec: batchv1.JobSpec{
					Completions:  pointer.Int32(1),
					Parallelism:  pointer.Int32(1),
					BackoffLimit: pointer.Int32(0),
					PodFailurePolicy: &batchv1.PodFailurePolicy{
						Rules: []batchv1.PodFailurePolicyRule{
							{
								Action: batchv1.PodFailurePolicyActionCount,
								OnExitCodes: &batchv1.PodFailurePolicyOnExitCodesRequirement{
									Operator: batchv1.PodFailurePolicyOnExitCodesOpIn,
									Values:   []int32{5},
								},
							},
						},
					},
				},
			},
			podStatus: v1.PodStatus{
				Phase: v1.PodFailed,
				ContainerStatuses: []v1.ContainerStatus{
					{
						State: v1.ContainerState{
							Terminated: &v1.ContainerStateTerminated{
								ExitCode: 5,
							},
						},
					},
				},
			},
			wantJobFinishedNumMetric: metricLabelsWithValue{
				Labels: []string{"NonIndexed", "failed", "BackoffLimitExceeded"},
				Value:  1,
			},
		},
		"non-indexed job; failed": {
			job: batchv1.Job{
				Spec: batchv1.JobSpec{
					Completions:  pointer.Int32(1),
					Parallelism:  pointer.Int32(1),
					BackoffLimit: pointer.Int32(0),
				},
			},
			podStatus: v1.PodStatus{
				Phase: v1.PodFailed,
			},
			wantJobFinishedNumMetric: metricLabelsWithValue{
				Labels: []string{"NonIndexed", "failed", "BackoffLimitExceeded"},
				Value:  1,
			},
		},
	}
	job_index := 0 // job index to avoid collisions between job names created by different test cases
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			defer featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.JobPodFailurePolicy, tc.enableJobPodFailurePolicy)()
			resetMetrics()
			// create a single job and wait for its completion
			job := tc.job.DeepCopy()
			job.Name = fmt.Sprintf("job-%v", job_index)
			job_index++
			jobObj, err := createJobWithDefaults(ctx, clientSet, ns.Name, job)
			if err != nil {
				t.Fatalf("Failed to create Job: %v", err)
			}
			validateJobPodsStatus(ctx, t, clientSet, jobObj, podsByStatus{
				Active: int(*jobObj.Spec.Parallelism),
				Ready:  pointer.Int32(0),
			})

			op := func(p *v1.Pod) bool {
				p.Status = tc.podStatus
				return true
			}
			if err, _ := updateJobPodsStatus(ctx, clientSet, jobObj, op, 1); err != nil {
				t.Fatalf("Error %q while updating pod status for Job: %q", err, jobObj.Name)
			}

			validateJobFailed(ctx, t, clientSet, jobObj)

			// verify metric values after the job is finished
			validateCounterMetric(t, metrics.JobFinishedNum, tc.wantJobFinishedNumMetric)
		})
	}
}

func validateCounterMetric(t *testing.T, counterVec *basemetrics.CounterVec, wantMetric metricLabelsWithValue) {
	t.Helper()
	var cmpErr error
	err := wait.PollImmediate(10*time.Millisecond, 10*time.Second, func() (bool, error) {
		cmpErr = nil
		value, err := testutil.GetCounterMetricValue(counterVec.WithLabelValues(wantMetric.Labels...))
		if err != nil {
			return true, fmt.Errorf("collecting the %q metric: %q", counterVec.Name, err)
		}
		if wantMetric.Value != int(value) {
			cmpErr = fmt.Errorf("Unexpected metric delta for %q metric with labels %q. want: %v, got: %v", counterVec.Name, wantMetric.Labels, wantMetric.Value, int(value))
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		t.Errorf("Failed waiting for expected metric: %q", err)
	}
	if cmpErr != nil {
		t.Error(cmpErr)
	}
}

func validateTerminatedPodsTrackingFinalizerMetric(t *testing.T, want int) {
	validateCounterMetric(t, metrics.TerminatedPodsTrackingFinalizerTotal, metricLabelsWithValue{
		Value:  want,
		Labels: []string{metrics.Add},
	})
	validateCounterMetric(t, metrics.TerminatedPodsTrackingFinalizerTotal, metricLabelsWithValue{
		Value:  want,
		Labels: []string{metrics.Delete},
	})
}

// TestJobPodFailurePolicyWithFailedPodDeletedDuringControllerRestart verifies that the job is properly marked as Failed
// in a scenario when the job controller crashes between removing pod finalizers and marking the job as Failed (based on
// the pod failure policy). After the finalizer for the failed pod is removed we remove the failed pod. This step is
// done to simulate what PodGC would do. Then, the test spawns the second instance of the controller to check that it
// will pick up the job state properly and will mark it as Failed, even if th pod triggering the pod failure policy is
// already deleted.
// Note: this scenario requires the use of finalizers. Without finalizers there is no guarantee a failed pod would be
// checked against the pod failure policy rules before its removal by PodGC.
func TestJobPodFailurePolicyWithFailedPodDeletedDuringControllerRestart(t *testing.T) {
	count := 3
	job := batchv1.Job{
		Spec: batchv1.JobSpec{
			Template: v1.PodTemplateSpec{
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Name:                     "main-container",
							Image:                    "foo",
							ImagePullPolicy:          v1.PullIfNotPresent,
							TerminationMessagePolicy: v1.TerminationMessageFallbackToLogsOnError,
						},
					},
				},
			},
			Parallelism: pointer.Int32(int32(count)),
			Completions: pointer.Int32(int32(count)),
			PodFailurePolicy: &batchv1.PodFailurePolicy{
				Rules: []batchv1.PodFailurePolicyRule{
					{
						Action: batchv1.PodFailurePolicyActionFailJob,
						OnExitCodes: &batchv1.PodFailurePolicyOnExitCodesRequirement{
							Operator: batchv1.PodFailurePolicyOnExitCodesOpIn,
							Values:   []int32{5},
						},
					},
				},
			},
		},
	}
	podStatusMatchingOnExitCodesTerminateRule := v1.PodStatus{
		Phase: v1.PodFailed,
		ContainerStatuses: []v1.ContainerStatus{
			{
				Name: "main-container",
				State: v1.ContainerState{
					Terminated: &v1.ContainerStateTerminated{
						ExitCode: 5,
					},
				},
			},
		},
	}
	defer featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.JobPodFailurePolicy, true)()
	closeFn, restConfig, cs, ns := setup(t, "simple")
	defer closeFn()

	// Make the job controller significantly slower to trigger race condition.
	restConfig.QPS = 1
	restConfig.Burst = 1
	ctx, cancel := startJobControllerAndWaitForCaches(restConfig)
	defer func() {
		cancel()
	}()
	resetMetrics()
	restConfig.QPS = 200
	restConfig.Burst = 200

	// create a job with a failed pod matching the exit code rule and a couple of successful pods
	jobObj, err := createJobWithDefaults(ctx, cs, ns.Name, &job)
	if err != nil {
		t.Fatalf("Failed to create Job: %v", err)
	}
	validateJobPodsStatus(ctx, t, cs, jobObj, podsByStatus{
		Active: count,
		Ready:  pointer.Int32(0),
	})

	jobPods, err := getJobPods(ctx, t, cs, jobObj, func(s v1.PodStatus) bool {
		return (s.Phase == v1.PodPending || s.Phase == v1.PodRunning)
	})
	if err != nil {
		t.Fatalf("Failed to list Job Pods: %v", err)
	}

	failedIndex := 1
	wg := sync.WaitGroup{}
	wg.Add(1)

	// Await for the failed pod (with index failedIndex) to have its finalizer
	// removed. The finalizer will be removed by the job controller just after
	// appending the FailureTarget condition to the job to mark it as targeted
	// for failure.
	go func() {
		err := wait.PollImmediate(10*time.Millisecond, time.Minute, func() (bool, error) {
			failedPodUpdated, err := cs.CoreV1().Pods(jobObj.Namespace).Get(ctx, jobPods[failedIndex].Name, metav1.GetOptions{})
			if err != nil {
				return true, err
			}
			if len(failedPodUpdated.Finalizers) == 0 {
				return true, nil
			}
			return false, nil
		})
		if err != nil {
			t.Logf("Failed awaiting for the finalizer removal for pod %v", klog.KObj(jobPods[failedIndex]))
		}
		wg.Done()
	}()

	// We update one pod as failed with state matching the pod failure policy rule. This results in removal
	// of the pod finalizer from the pod by the job controller.
	failedPod := jobPods[failedIndex]
	updatedPod := failedPod.DeepCopy()
	updatedPod.Status = podStatusMatchingOnExitCodesTerminateRule
	_, err = updatePodStatuses(ctx, cs, []v1.Pod{*updatedPod})
	if err != nil {
		t.Fatalf("Failed to update pod statuses %q for pods of job %q", err, klog.KObj(jobObj))
	}
	wg.Wait()

	t.Logf("Finalizer is removed for the failed pod %q. Shutting down the controller.", klog.KObj(failedPod))
	// shut down the first job controller as soon as it removed the finalizer for the failed pod. This will
	// likely happen before the first controller is able to mark the job as Failed.
	cancel()

	// Delete the failed pod to make sure it is not used by the second instance of the controller
	ctx, cancel = context.WithCancel(context.Background())
	err = cs.CoreV1().Pods(failedPod.Namespace).Delete(ctx, failedPod.Name, metav1.DeleteOptions{GracePeriodSeconds: pointer.Int64(0)})
	if err != nil {
		t.Fatalf("Error: '%v' while deleting pod: '%v'", err, klog.KObj(failedPod))
	}
	t.Logf("The failed pod %q is deleted", klog.KObj(failedPod))
	cancel()

	// start the second controller to promote the interim FailureTarget job condition as Failed
	ctx, cancel = startJobControllerAndWaitForCaches(restConfig)
	// verify the job is correctly marked as Failed
	validateJobFailed(ctx, t, cs, jobObj)
	validateNoOrphanPodsWithFinalizers(ctx, t, cs, jobObj)
}

// TestJobPodFailurePolicy tests handling of pod failures with respect to the
// configured pod failure policy rules
func TestJobPodFailurePolicy(t *testing.T) {
	t.Cleanup(setDurationDuringTest(&jobcontroller.DefaultJobPodFailureBackOff, fastPodFailureBackoff))
	job := batchv1.Job{
		Spec: batchv1.JobSpec{
			Template: v1.PodTemplateSpec{
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Name:                     "main-container",
							Image:                    "foo",
							ImagePullPolicy:          v1.PullIfNotPresent,
							TerminationMessagePolicy: v1.TerminationMessageFallbackToLogsOnError,
						},
					},
				},
			},
			PodFailurePolicy: &batchv1.PodFailurePolicy{
				Rules: []batchv1.PodFailurePolicyRule{
					{
						Action: batchv1.PodFailurePolicyActionIgnore,
						OnPodConditions: []batchv1.PodFailurePolicyOnPodConditionsPattern{
							{
								Type: v1.DisruptionTarget,
							},
						},
					},
					{
						Action: batchv1.PodFailurePolicyActionCount,
						OnExitCodes: &batchv1.PodFailurePolicyOnExitCodesRequirement{
							Operator: batchv1.PodFailurePolicyOnExitCodesOpIn,
							Values:   []int32{10},
						},
					},
					{
						Action: batchv1.PodFailurePolicyActionFailJob,
						OnExitCodes: &batchv1.PodFailurePolicyOnExitCodesRequirement{
							Operator: batchv1.PodFailurePolicyOnExitCodesOpIn,
							Values:   []int32{5, 6, 7},
						},
					},
				},
			},
		},
	}
	podStatusMatchingOnExitCodesTerminateRule := v1.PodStatus{
		Phase: v1.PodFailed,
		ContainerStatuses: []v1.ContainerStatus{
			{
				Name: "main-container",
				State: v1.ContainerState{
					Terminated: &v1.ContainerStateTerminated{
						ExitCode: 5,
					},
				},
			},
		},
	}
	podStatusMatchingOnExitCodesCountRule := v1.PodStatus{
		Phase: v1.PodFailed,
		ContainerStatuses: []v1.ContainerStatus{
			{
				Name: "main-container",
				State: v1.ContainerState{
					Terminated: &v1.ContainerStateTerminated{
						ExitCode: 10,
					},
				},
			},
		},
	}
	podStatusMatchingOnPodConditionsIgnoreRule := v1.PodStatus{
		Phase: v1.PodFailed,
		Conditions: []v1.PodCondition{
			{
				Type:   v1.DisruptionTarget,
				Status: v1.ConditionTrue,
			},
		},
	}
	podStatusNotMatchingAnyRule := v1.PodStatus{
		Phase: v1.PodFailed,
		ContainerStatuses: []v1.ContainerStatus{
			{
				State: v1.ContainerState{
					Terminated: &v1.ContainerStateTerminated{},
				},
			},
		},
	}
	testCases := map[string]struct {
		enableJobPodFailurePolicy                bool
		restartController                        bool
		job                                      batchv1.Job
		podStatus                                v1.PodStatus
		wantActive                               int
		wantFailed                               int
		wantJobConditionType                     batchv1.JobConditionType
		wantPodFailuresHandledByPolicyRuleMetric *metricLabelsWithValue
	}{
		"pod status matching the configured FailJob rule on exit codes; job terminated when JobPodFailurePolicy enabled": {
			enableJobPodFailurePolicy: true,
			job:                       job,
			podStatus:                 podStatusMatchingOnExitCodesTerminateRule,
			wantActive:                0,
			wantFailed:                1,
			wantJobConditionType:      batchv1.JobFailed,
			wantPodFailuresHandledByPolicyRuleMetric: &metricLabelsWithValue{
				Labels: []string{"FailJob"},
				Value:  1,
			},
		},
		"pod status matching the configured FailJob rule on exit codes; with controller restart; job terminated when JobPodFailurePolicy enabled": {
			enableJobPodFailurePolicy: true,
			restartController:         true,
			job:                       job,
			podStatus:                 podStatusMatchingOnExitCodesTerminateRule,
			wantActive:                0,
			wantFailed:                1,
			wantJobConditionType:      batchv1.JobFailed,
		},
		"pod status matching the configured FailJob rule on exit codes; default handling when JobPodFailurePolicy disabled": {
			enableJobPodFailurePolicy: false,
			job:                       job,
			podStatus:                 podStatusMatchingOnExitCodesTerminateRule,
			wantActive:                1,
			wantFailed:                1,
			wantJobConditionType:      batchv1.JobComplete,
		},
		"pod status matching the configured Ignore rule on pod conditions; pod failure not counted when JobPodFailurePolicy enabled": {
			enableJobPodFailurePolicy: true,
			job:                       job,
			podStatus:                 podStatusMatchingOnPodConditionsIgnoreRule,
			wantActive:                1,
			wantFailed:                0,
			wantJobConditionType:      batchv1.JobComplete,
			wantPodFailuresHandledByPolicyRuleMetric: &metricLabelsWithValue{
				Labels: []string{"Ignore"},
				Value:  1,
			},
		},
		"pod status matching the configured Count rule on exit codes; pod failure counted when JobPodFailurePolicy enabled": {
			enableJobPodFailurePolicy: true,
			job:                       job,
			podStatus:                 podStatusMatchingOnExitCodesCountRule,
			wantActive:                1,
			wantFailed:                1,
			wantJobConditionType:      batchv1.JobComplete,
			wantPodFailuresHandledByPolicyRuleMetric: &metricLabelsWithValue{
				Labels: []string{"Count"},
				Value:  1,
			},
		},
		"pod status non-matching any configured rule; pod failure counted when JobPodFailurePolicy enabled": {
			enableJobPodFailurePolicy: true,
			job:                       job,
			podStatus:                 podStatusNotMatchingAnyRule,
			wantActive:                1,
			wantFailed:                1,
			wantJobConditionType:      batchv1.JobComplete,
			wantPodFailuresHandledByPolicyRuleMetric: &metricLabelsWithValue{
				Labels: []string{"Count"},
				Value:  0,
			},
		},
	}
	for name, test := range testCases {
		t.Run(name, func(t *testing.T) {
			resetMetrics()
			defer featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.JobPodFailurePolicy, test.enableJobPodFailurePolicy)()

			closeFn, restConfig, clientSet, ns := setup(t, "simple")
			defer closeFn()
			ctx, cancel := startJobControllerAndWaitForCaches(restConfig)
			defer func() {
				cancel()
			}()

			jobObj, err := createJobWithDefaults(ctx, clientSet, ns.Name, &test.job)
			if err != nil {
				t.Fatalf("Error %q while creating the job %q", err, jobObj.Name)
			}
			validateJobPodsStatus(ctx, t, clientSet, jobObj, podsByStatus{
				Active: 1,
				Ready:  pointer.Int32(0),
			})

			op := func(p *v1.Pod) bool {
				p.Status = test.podStatus
				return true
			}

			if err, _ := updateJobPodsStatus(ctx, clientSet, jobObj, op, 1); err != nil {
				t.Fatalf("Error %q while updating pod status for Job: %q", err, jobObj.Name)
			}

			if test.restartController {
				cancel()
				ctx, cancel = startJobControllerAndWaitForCaches(restConfig)
			}

			validateJobPodsStatus(ctx, t, clientSet, jobObj, podsByStatus{
				Active: test.wantActive,
				Failed: test.wantFailed,
				Ready:  pointer.Int32(0),
			})

			if test.wantJobConditionType == batchv1.JobComplete {
				if err, _ := setJobPodsPhase(ctx, clientSet, jobObj, v1.PodSucceeded, 1); err != nil {
					t.Fatalf("Failed setting phase %q on Job Pod: %q", v1.PodSucceeded, err)
				}
			}
			validateJobCondition(ctx, t, clientSet, jobObj, test.wantJobConditionType)
			if test.wantPodFailuresHandledByPolicyRuleMetric != nil {
				validateCounterMetric(t, metrics.PodFailuresHandledByFailurePolicy, *test.wantPodFailuresHandledByPolicyRuleMetric)
			}
			validateFinishedPodsNoFinalizer(ctx, t, clientSet, jobObj)
		})
	}
}

// TestBackoffLimitPerIndex_DelayedPodDeletion tests the pod deletion is delayed
// until the replacement pod is created, so that the replacement pod has the
// index-failure-count annotation bumped, when BackoffLimitPerIndex is used.
func TestBackoffLimitPerIndex_DelayedPodDeletion(t *testing.T) {
	t.Cleanup(setDurationDuringTest(&jobcontroller.DefaultJobPodFailureBackOff, fastPodFailureBackoff))

	defer featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.JobBackoffLimitPerIndex, true)()
	closeFn, restConfig, clientSet, ns := setup(t, "backoff-limit-per-index-failed")
	defer closeFn()
	ctx, cancel := startJobControllerAndWaitForCaches(restConfig)
	defer func() {
		cancel()
	}()

	jobObj, err := createJobWithDefaults(ctx, clientSet, ns.Name, &batchv1.Job{
		Spec: batchv1.JobSpec{
			Parallelism:          pointer.Int32(1),
			Completions:          pointer.Int32(1),
			BackoffLimitPerIndex: pointer.Int32(1),
			CompletionMode:       completionModePtr(batchv1.IndexedCompletion),
		},
	})
	if err != nil {
		t.Fatalf("Failed to create Job: %v", err)
	}
	validateJobPodsStatus(ctx, t, clientSet, jobObj, podsByStatus{
		Active: 1,
		Ready:  pointer.Int32(0),
	})
	validateIndexedJobPods(ctx, t, clientSet, jobObj, sets.New(0), "", pointer.String(""))

	// First pod from index 0 failed.
	if err := setJobPhaseForIndex(ctx, clientSet, jobObj, v1.PodFailed, 0); err != nil {
		t.Fatal("Failed trying to fail pod with index 0")
	}
	// Delete the failed pod
	pod, err := getJobPodForIndex(ctx, clientSet, jobObj, 0, func(_ *v1.Pod) bool { return true })
	if err != nil {
		t.Fatalf("failed to get terminal pod for index: %v", 0)
	}
	if err := clientSet.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("failed to delete pod: %v, error: %v", klog.KObj(pod), err)
	}

	validateJobPodsStatus(ctx, t, clientSet, jobObj, podsByStatus{
		Active: 1,
		Failed: 1,
		Ready:  pointer.Int32(0),
	})
	validateIndexedJobPods(ctx, t, clientSet, jobObj, sets.New(0), "", pointer.String(""))

	// Verify the replacement pod is created and has the index-failure-count
	// annotation bumped.
	replacement, err := getActivePodForIndex(ctx, clientSet, jobObj, 0)
	if err != nil {
		t.Fatalf("Failed to get active replacement pod for index: %v, error: %v", 0, err)
	}
	gotIndexFailureCount, err := getIndexFailureCount(replacement)
	if err != nil {
		t.Fatalf("Failed read the index failure count annotation for pod: %v, error: %v", klog.KObj(replacement), err)
	}
	if diff := cmp.Diff(1, gotIndexFailureCount); diff != "" {
		t.Errorf("Unexpected index failure count for the replacement pod: %s", diff)
	}
	if err := setJobPhaseForIndex(ctx, clientSet, jobObj, v1.PodSucceeded, 0); err != nil {
		t.Fatal("Failed trying to fail pod with index 0")
	}
	validateJobPodsStatus(ctx, t, clientSet, jobObj, podsByStatus{
		Active:    0,
		Succeeded: 1,
		Failed:    1,
		Ready:     pointer.Int32(0),
	})
	validateJobSucceeded(ctx, t, clientSet, jobObj)
}

// TestBackoffLimitPerIndex_Reenabling tests handling of pod failures when
// reenabling the BackoffLimitPerIndex feature.
func TestBackoffLimitPerIndex_Reenabling(t *testing.T) {
	t.Cleanup(setDurationDuringTest(&jobcontroller.DefaultJobPodFailureBackOff, fastPodFailureBackoff))

	defer featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.JobBackoffLimitPerIndex, true)()
	closeFn, restConfig, clientSet, ns := setup(t, "backoff-limit-per-index-reenabled")
	defer closeFn()
	ctx, cancel := startJobControllerAndWaitForCaches(restConfig)
	defer cancel()
	resetMetrics()

	jobObj, err := createJobWithDefaults(ctx, clientSet, ns.Name, &batchv1.Job{
		Spec: batchv1.JobSpec{
			Parallelism:          pointer.Int32(3),
			Completions:          pointer.Int32(3),
			BackoffLimitPerIndex: pointer.Int32(0),
			CompletionMode:       completionModePtr(batchv1.IndexedCompletion),
		},
	})
	if err != nil {
		t.Fatalf("Failed to create Job: %v", err)
	}
	validateJobPodsStatus(ctx, t, clientSet, jobObj, podsByStatus{
		Active: 3,
		Ready:  pointer.Int32(0),
	})
	validateIndexedJobPods(ctx, t, clientSet, jobObj, sets.New(0, 1, 2), "", pointer.String(""))

	// First pod from index 0 failed
	if err := setJobPhaseForIndex(ctx, clientSet, jobObj, v1.PodFailed, 0); err != nil {
		t.Fatal("Failed trying to fail pod with index 0")
	}
	validateJobPodsStatus(ctx, t, clientSet, jobObj, podsByStatus{
		Active: 2,
		Failed: 1,
		Ready:  pointer.Int32(0),
	})
	validateIndexedJobPods(ctx, t, clientSet, jobObj, sets.New(1, 2), "", pointer.String("0"))

	// Disable the feature
	defer featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.JobBackoffLimitPerIndex, false)()

	// First pod from index 1 failed
	if err := setJobPhaseForIndex(ctx, clientSet, jobObj, v1.PodFailed, 1); err != nil {
		t.Fatal("Failed trying to fail pod with index 1")
	}
	validateJobPodsStatus(ctx, t, clientSet, jobObj, podsByStatus{
		Active: 3,
		Failed: 2,
		Ready:  pointer.Int32(0),
	})
	validateIndexedJobPods(ctx, t, clientSet, jobObj, sets.New(0, 1, 2), "", nil)

	// Reenable the feature
	defer featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.JobBackoffLimitPerIndex, true)()

	// First pod from index 2 failed
	if err := setJobPhaseForIndex(ctx, clientSet, jobObj, v1.PodFailed, 2); err != nil {
		t.Fatal("Failed trying to fail pod with index 2")
	}

	// Verify the indexes 0 and 1 are active as the failed pods don't have
	// finalizers at this point, so they are ignored.
	validateJobPodsStatus(ctx, t, clientSet, jobObj, podsByStatus{
		Active: 2,
		Failed: 3,
		Ready:  pointer.Int32(0),
	})
	validateIndexedJobPods(ctx, t, clientSet, jobObj, sets.New(0, 1), "", pointer.String("2"))

	// mark remaining pods are Succeeded and verify Job status
	if err, _ := setJobPodsPhase(ctx, clientSet, jobObj, v1.PodSucceeded, 2); err != nil {
		t.Fatalf("Failed setting phase %q on Job Pod: %q", v1.PodSucceeded, err)
	}
	validateJobFailed(ctx, t, clientSet, jobObj)
	validateFinishedPodsNoFinalizer(ctx, t, clientSet, jobObj)
}

// TestBackoffLimitPerIndex_JobPodsCreatedWithExponentialBackoff tests that the
// pods are recreated with expotential backoff delay computed independently
// per index. Scenario:
// - fail index 0
// - fail index 0
// - fail index 1
// - succeed index 0
// - fail index 1
// - succeed index 1
func TestBackoffLimitPerIndex_JobPodsCreatedWithExponentialBackoff(t *testing.T) {
	defer featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.JobBackoffLimitPerIndex, true)()
	t.Cleanup(setDurationDuringTest(&jobcontroller.DefaultJobPodFailureBackOff, 2*time.Second))

	closeFn, restConfig, clientSet, ns := setup(t, "simple")
	defer closeFn()
	ctx, cancel := startJobControllerAndWaitForCaches(restConfig)
	defer cancel()

	jobObj, err := createJobWithDefaults(ctx, clientSet, ns.Name, &batchv1.Job{
		Spec: batchv1.JobSpec{
			Completions:          pointer.Int32(2),
			Parallelism:          pointer.Int32(2),
			BackoffLimitPerIndex: pointer.Int32(2),
			CompletionMode:       completionModePtr(batchv1.IndexedCompletion),
		},
	})
	if err != nil {
		t.Fatalf("Could not create job: %v", err)
	}
	validateJobPodsStatus(ctx, t, clientSet, jobObj, podsByStatus{
		Active: 2,
		Ready:  pointer.Int32(0),
	})
	validateIndexedJobPods(ctx, t, clientSet, jobObj, sets.New(0, 1), "", pointer.String(""))

	// Fail the first pod for index 0
	if err := setJobPhaseForIndex(ctx, clientSet, jobObj, v1.PodFailed, 0); err != nil {
		t.Fatalf("Failed setting phase %s on Job Pod: %v", v1.PodFailed, err)
	}
	validateJobPodsStatus(ctx, t, clientSet, jobObj, podsByStatus{
		Active: 2,
		Failed: 1,
		Ready:  pointer.Int32(0),
	})
	validateIndexedJobPods(ctx, t, clientSet, jobObj, sets.New(0, 1), "", pointer.String(""))

	// Fail the second pod for index 0
	if err := setJobPhaseForIndex(ctx, clientSet, jobObj, v1.PodFailed, 0); err != nil {
		t.Fatalf("Failed setting phase %s on Job Pod: %v", v1.PodFailed, err)
	}
	validateJobPodsStatus(ctx, t, clientSet, jobObj, podsByStatus{
		Active: 2,
		Failed: 2,
		Ready:  pointer.Int32(0),
	})
	validateIndexedJobPods(ctx, t, clientSet, jobObj, sets.New(0, 1), "", pointer.String(""))

	// Fail the first pod for index 1
	if err := setJobPhaseForIndex(ctx, clientSet, jobObj, v1.PodFailed, 1); err != nil {
		t.Fatalf("Failed setting phase %s on Job Pod: %v", v1.PodFailed, err)
	}
	validateJobPodsStatus(ctx, t, clientSet, jobObj, podsByStatus{
		Active: 2,
		Failed: 3,
		Ready:  pointer.Int32(0),
	})
	validateIndexedJobPods(ctx, t, clientSet, jobObj, sets.New(0, 1), "", pointer.String(""))

	// Succeed the third pod for index 0
	if err := setJobPhaseForIndex(ctx, clientSet, jobObj, v1.PodSucceeded, 0); err != nil {
		t.Fatalf("Failed setting phase %s on Job Pod: %v", v1.PodSucceeded, err)
	}
	validateJobPodsStatus(ctx, t, clientSet, jobObj, podsByStatus{
		Active:    1,
		Failed:    3,
		Succeeded: 1,
		Ready:     pointer.Int32(0),
	})
	validateIndexedJobPods(ctx, t, clientSet, jobObj, sets.New(1), "0", pointer.String(""))

	// Fail the second pod for index 1
	if err := setJobPhaseForIndex(ctx, clientSet, jobObj, v1.PodFailed, 1); err != nil {
		t.Fatalf("Failed setting phase %s on Job Pod: %v", v1.PodFailed, err)
	}
	validateJobPodsStatus(ctx, t, clientSet, jobObj, podsByStatus{
		Active:    1,
		Failed:    4,
		Succeeded: 1,
		Ready:     pointer.Int32(0),
	})
	validateIndexedJobPods(ctx, t, clientSet, jobObj, sets.New(1), "0", pointer.String(""))

	// Succeed the third pod for index 1
	if err := setJobPhaseForIndex(ctx, clientSet, jobObj, v1.PodSucceeded, 1); err != nil {
		t.Fatalf("Failed setting phase %s on Job Pod: %v", v1.PodSucceeded, err)
	}
	validateJobPodsStatus(ctx, t, clientSet, jobObj, podsByStatus{
		Active:    0,
		Failed:    4,
		Succeeded: 2,
		Ready:     pointer.Int32(0),
	})
	validateIndexedJobPods(ctx, t, clientSet, jobObj, sets.New[int](), "0,1", pointer.String(""))
	validateJobSucceeded(ctx, t, clientSet, jobObj)

	for index := 0; index < int(*jobObj.Spec.Completions); index++ {
		podsForIndex, err := getJobPodsForIndex(ctx, clientSet, jobObj, index, func(_ *v1.Pod) bool { return true })
		if err != nil {
			t.Fatalf("Failed to list job %q pods for index %v, error: %v", klog.KObj(jobObj), index, err)
		}
		validateExpotentialBackoffDelay(t, jobcontroller.DefaultJobPodFailureBackOff, podsForIndex)
	}
}

// TestBackoffLimitPerIndex tests handling of job and its pods when
// backoff limit per index is used.
func TestBackoffLimitPerIndex(t *testing.T) {
	t.Cleanup(setDurationDuringTest(&jobcontroller.DefaultJobPodFailureBackOff, fastPodFailureBackoff))

	type podTerminationWithExpectations struct {
		index                          int
		status                         v1.PodStatus
		wantActive                     int
		wantFailed                     int
		wantSucceeded                  int
		wantActiveIndexes              sets.Set[int]
		wantCompletedIndexes           string
		wantFailedIndexes              *string
		wantReplacementPodFailureCount *int
	}

	podTemplateSpec := v1.PodTemplateSpec{
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:                     "main-container",
					Image:                    "foo",
					ImagePullPolicy:          v1.PullIfNotPresent,
					TerminationMessagePolicy: v1.TerminationMessageFallbackToLogsOnError,
				},
			},
		},
	}
	testCases := map[string]struct {
		job                  batchv1.Job
		podTerminations      []podTerminationWithExpectations
		wantJobConditionType batchv1.JobConditionType
	}{
		"job succeeded": {
			job: batchv1.Job{
				Spec: batchv1.JobSpec{
					Parallelism:          pointer.Int32(2),
					Completions:          pointer.Int32(2),
					CompletionMode:       completionModePtr(batchv1.IndexedCompletion),
					BackoffLimitPerIndex: pointer.Int32(1),
					Template:             podTemplateSpec,
				},
			},
			podTerminations: []podTerminationWithExpectations{
				{
					status: v1.PodStatus{
						Phase: v1.PodFailed,
					},
					wantActive:                     2,
					wantFailed:                     1,
					wantActiveIndexes:              sets.New(0, 1),
					wantFailedIndexes:              pointer.String(""),
					wantReplacementPodFailureCount: pointer.Int(1),
				},
			},
			wantJobConditionType: batchv1.JobComplete,
		},
		"job index fails due to exceeding backoff limit per index": {
			job: batchv1.Job{
				Spec: batchv1.JobSpec{
					Parallelism:          pointer.Int32(2),
					Completions:          pointer.Int32(2),
					CompletionMode:       completionModePtr(batchv1.IndexedCompletion),
					BackoffLimitPerIndex: pointer.Int32(2),
					Template:             podTemplateSpec,
				},
			},
			podTerminations: []podTerminationWithExpectations{
				{
					status: v1.PodStatus{
						Phase: v1.PodFailed,
					},
					wantActive:                     2,
					wantFailed:                     1,
					wantActiveIndexes:              sets.New(0, 1),
					wantFailedIndexes:              pointer.String(""),
					wantReplacementPodFailureCount: pointer.Int(1),
				},
				{
					status: v1.PodStatus{
						Phase: v1.PodFailed,
					},
					wantActive:                     2,
					wantFailed:                     2,
					wantActiveIndexes:              sets.New(0, 1),
					wantFailedIndexes:              pointer.String(""),
					wantReplacementPodFailureCount: pointer.Int(2),
				},
				{
					status: v1.PodStatus{
						Phase: v1.PodFailed,
					},
					wantActive:        1,
					wantFailed:        3,
					wantActiveIndexes: sets.New(1),
					wantFailedIndexes: pointer.String("0"),
				},
			},
			wantJobConditionType: batchv1.JobFailed,
		},
		"job index fails due to exceeding the global backoff limit first": {
			job: batchv1.Job{
				Spec: batchv1.JobSpec{
					Parallelism:          pointer.Int32(3),
					Completions:          pointer.Int32(3),
					CompletionMode:       completionModePtr(batchv1.IndexedCompletion),
					BackoffLimitPerIndex: pointer.Int32(1),
					BackoffLimit:         pointer.Int32(2),
					Template:             podTemplateSpec,
				},
			},
			podTerminations: []podTerminationWithExpectations{
				{
					index: 0,
					status: v1.PodStatus{
						Phase: v1.PodFailed,
					},
					wantActive:        3,
					wantFailed:        1,
					wantActiveIndexes: sets.New(0, 1, 2),
					wantFailedIndexes: pointer.String(""),
				},
				{
					index: 1,
					status: v1.PodStatus{
						Phase: v1.PodFailed,
					},
					wantActive:        3,
					wantFailed:        2,
					wantActiveIndexes: sets.New(0, 1, 2),
					wantFailedIndexes: pointer.String(""),
				},
				{
					index: 2,
					status: v1.PodStatus{
						Phase: v1.PodFailed,
					},
					wantFailed:        5,
					wantFailedIndexes: pointer.String(""),
				},
			},
			wantJobConditionType: batchv1.JobFailed,
		},
		"job continues execution after a failed index, the job is marked Failed due to the failed index": {
			job: batchv1.Job{
				Spec: batchv1.JobSpec{
					Parallelism:          pointer.Int32(2),
					Completions:          pointer.Int32(2),
					CompletionMode:       completionModePtr(batchv1.IndexedCompletion),
					BackoffLimitPerIndex: pointer.Int32(0),
					Template:             podTemplateSpec,
				},
			},
			podTerminations: []podTerminationWithExpectations{
				{
					index: 0,
					status: v1.PodStatus{
						Phase: v1.PodFailed,
					},
					wantActive:        1,
					wantFailed:        1,
					wantActiveIndexes: sets.New(1),
					wantFailedIndexes: pointer.String("0"),
				},
				{
					index: 1,
					status: v1.PodStatus{
						Phase: v1.PodSucceeded,
					},
					wantFailed:           1,
					wantSucceeded:        1,
					wantFailedIndexes:    pointer.String("0"),
					wantCompletedIndexes: "1",
				},
			},
			wantJobConditionType: batchv1.JobFailed,
		},
		"job execution terminated early due to exceeding max failed indexes": {
			job: batchv1.Job{
				Spec: batchv1.JobSpec{
					Parallelism:          pointer.Int32(3),
					Completions:          pointer.Int32(3),
					CompletionMode:       completionModePtr(batchv1.IndexedCompletion),
					BackoffLimitPerIndex: pointer.Int32(0),
					MaxFailedIndexes:     pointer.Int32(1),
					Template:             podTemplateSpec,
				},
			},
			podTerminations: []podTerminationWithExpectations{
				{
					index: 0,
					status: v1.PodStatus{
						Phase: v1.PodFailed,
					},
					wantActive:        2,
					wantFailed:        1,
					wantActiveIndexes: sets.New(1, 2),
					wantFailedIndexes: pointer.String("0"),
				},
				{
					index: 1,
					status: v1.PodStatus{
						Phase: v1.PodFailed,
					},
					wantActive:        0,
					wantFailed:        3,
					wantFailedIndexes: pointer.String("0,1"),
				},
			},
			wantJobConditionType: batchv1.JobFailed,
		},
		"pod failure matching pod failure policy rule with FailIndex action": {
			job: batchv1.Job{
				Spec: batchv1.JobSpec{
					Parallelism:          pointer.Int32(2),
					Completions:          pointer.Int32(2),
					CompletionMode:       completionModePtr(batchv1.IndexedCompletion),
					BackoffLimitPerIndex: pointer.Int32(1),
					Template:             podTemplateSpec,
					PodFailurePolicy: &batchv1.PodFailurePolicy{
						Rules: []batchv1.PodFailurePolicyRule{
							{
								Action: batchv1.PodFailurePolicyActionFailIndex,
								OnExitCodes: &batchv1.PodFailurePolicyOnExitCodesRequirement{
									Operator: batchv1.PodFailurePolicyOnExitCodesOpIn,
									Values:   []int32{13},
								},
							},
							{
								Action: batchv1.PodFailurePolicyActionFailIndex,
								OnPodConditions: []batchv1.PodFailurePolicyOnPodConditionsPattern{
									{
										Type:   v1.DisruptionTarget,
										Status: v1.ConditionTrue,
									},
								},
							},
						},
					},
				},
			},
			podTerminations: []podTerminationWithExpectations{
				{
					index: 0,
					status: v1.PodStatus{
						Phase: v1.PodFailed,
						ContainerStatuses: []v1.ContainerStatus{
							{
								State: v1.ContainerState{
									Terminated: &v1.ContainerStateTerminated{
										ExitCode: 13,
									},
								},
							},
						},
					},
					wantActive:        1,
					wantFailed:        1,
					wantActiveIndexes: sets.New(1),
					wantFailedIndexes: pointer.String("0"),
				},
				{
					index: 1,
					status: v1.PodStatus{
						Phase: v1.PodFailed,
						Conditions: []v1.PodCondition{
							{
								Type:   v1.DisruptionTarget,
								Status: v1.ConditionTrue,
							},
						},
					},
					wantFailed:        2,
					wantFailedIndexes: pointer.String("0,1"),
				},
			},
			wantJobConditionType: batchv1.JobFailed,
		},
	}
	for name, test := range testCases {
		t.Run(name, func(t *testing.T) {
			resetMetrics()
			defer featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.JobPodFailurePolicy, true)()
			defer featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.JobBackoffLimitPerIndex, true)()

			closeFn, restConfig, clientSet, ns := setup(t, "simple")
			defer closeFn()
			ctx, cancel := startJobControllerAndWaitForCaches(restConfig)
			defer func() {
				cancel()
			}()
			jobObj, err := createJobWithDefaults(ctx, clientSet, ns.Name, &test.job)
			if err != nil {
				t.Fatalf("Error %q while creating the job %q", err, jobObj.Name)
			}
			validateJobPodsStatus(ctx, t, clientSet, jobObj, podsByStatus{
				Active: int(*test.job.Spec.Parallelism),
				Ready:  pointer.Int32(0),
			})
			for _, podTermination := range test.podTerminations {
				pod, err := getActivePodForIndex(ctx, clientSet, jobObj, podTermination.index)
				if err != nil {
					t.Fatalf("listing Job Pods: %q", err)
				}
				pod.Status = podTermination.status
				if _, err = clientSet.CoreV1().Pods(pod.Namespace).UpdateStatus(ctx, pod, metav1.UpdateOptions{}); err != nil {
					t.Fatalf("Error updating the pod %q: %q", klog.KObj(pod), err)
				}
				validateJobPodsStatus(ctx, t, clientSet, jobObj, podsByStatus{
					Active:    podTermination.wantActive,
					Succeeded: podTermination.wantSucceeded,
					Failed:    podTermination.wantFailed,
					Ready:     pointer.Int32(0),
				})
				validateIndexedJobPods(ctx, t, clientSet, jobObj, podTermination.wantActiveIndexes, podTermination.wantCompletedIndexes, podTermination.wantFailedIndexes)
				if podTermination.wantReplacementPodFailureCount != nil {
					replacement, err := getActivePodForIndex(ctx, clientSet, jobObj, podTermination.index)
					if err != nil {
						t.Fatalf("Failed to get active replacement pod for index: %v, error: %v", podTermination.index, err)
					}
					gotReplacementPodFailureCount, err := getIndexFailureCount(replacement)
					if err != nil {
						t.Fatalf("Failed read the index failure count annotation for pod: %v, error: %v", klog.KObj(replacement), err)
					}
					if *podTermination.wantReplacementPodFailureCount != gotReplacementPodFailureCount {
						t.Fatalf("Unexpected value of the index failure count annotation. Want: %v, got: %v", *podTermination.wantReplacementPodFailureCount, gotReplacementPodFailureCount)
					}
				}
			}

			remainingActive := test.podTerminations[len(test.podTerminations)-1].wantActive
			if remainingActive > 0 {
				if err, _ := setJobPodsPhase(ctx, clientSet, jobObj, v1.PodSucceeded, remainingActive); err != nil {
					t.Fatalf("Failed setting phase %q on Job Pod: %q", v1.PodSucceeded, err)
				}
			}
			validateJobCondition(ctx, t, clientSet, jobObj, test.wantJobConditionType)
			validateFinishedPodsNoFinalizer(ctx, t, clientSet, jobObj)
		})
	}
}

func getIndexFailureCount(p *v1.Pod) (int, error) {
	if p.Annotations == nil {
		return 0, errors.New("no annotations found")
	}
	v, ok := p.Annotations[batchv1.JobIndexFailureCountAnnotation]
	if !ok {
		return 0, fmt.Errorf("annotation %s not found", batchv1.JobIndexFailureCountAnnotation)
	}
	return strconv.Atoi(v)
}

func completionModePtr(cm batchv1.CompletionMode) *batchv1.CompletionMode {
	return &cm
}

// TestNonParallelJob tests that a Job that only executes one Pod. The test
// recreates the Job controller at some points to make sure a new controller
// is able to pickup.
func TestNonParallelJob(t *testing.T) {
	t.Cleanup(setDurationDuringTest(&jobcontroller.DefaultJobPodFailureBackOff, fastPodFailureBackoff))
	closeFn, restConfig, clientSet, ns := setup(t, "simple")
	defer closeFn()
	ctx, cancel := startJobControllerAndWaitForCaches(restConfig)
	defer func() {
		cancel()
	}()

	jobObj, err := createJobWithDefaults(ctx, clientSet, ns.Name, &batchv1.Job{})
	if err != nil {
		t.Fatalf("Failed to create Job: %v", err)
	}
	validateJobPodsStatus(ctx, t, clientSet, jobObj, podsByStatus{
		Active: 1,
		Ready:  pointer.Int32(0),
	})

	// Restarting controller.
	cancel()
	ctx, cancel = startJobControllerAndWaitForCaches(restConfig)

	// Failed Pod is replaced.
	if err, _ := setJobPodsPhase(ctx, clientSet, jobObj, v1.PodFailed, 1); err != nil {
		t.Fatalf("Failed setting phase %s on Job Pod: %v", v1.PodFailed, err)
	}
	validateJobPodsStatus(ctx, t, clientSet, jobObj, podsByStatus{
		Active: 1,
		Failed: 1,
		Ready:  pointer.Int32(0),
	})

	// Restarting controller.
	cancel()
	ctx, cancel = startJobControllerAndWaitForCaches(restConfig)

	// No more Pods are created after the Pod succeeds.
	if err, _ := setJobPodsPhase(ctx, clientSet, jobObj, v1.PodSucceeded, 1); err != nil {
		t.Fatalf("Failed setting phase %s on Job Pod: %v", v1.PodSucceeded, err)
	}
	validateJobSucceeded(ctx, t, clientSet, jobObj)
	validateJobPodsStatus(ctx, t, clientSet, jobObj, podsByStatus{
		Failed:    1,
		Succeeded: 1,
		Ready:     pointer.Int32(0),
	})
	validateFinishedPodsNoFinalizer(ctx, t, clientSet, jobObj)
}

func TestParallelJob(t *testing.T) {
	t.Cleanup(setDurationDuringTest(&jobcontroller.DefaultJobPodFailureBackOff, fastPodFailureBackoff))
	cases := map[string]struct {
		enableReadyPods bool
	}{
		"none": {},
		"ready pods": {
			enableReadyPods: true,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			defer featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.JobReadyPods, tc.enableReadyPods)()

			closeFn, restConfig, clientSet, ns := setup(t, "parallel")
			defer closeFn()
			ctx, cancel := startJobControllerAndWaitForCaches(restConfig)
			defer cancel()
			resetMetrics()

			jobObj, err := createJobWithDefaults(ctx, clientSet, ns.Name, &batchv1.Job{
				Spec: batchv1.JobSpec{
					Parallelism: pointer.Int32(5),
				},
			})
			if err != nil {
				t.Fatalf("Failed to create Job: %v", err)
			}
			want := podsByStatus{Active: 5}
			if tc.enableReadyPods {
				want.Ready = pointer.Int32(0)
			}
			validateJobPodsStatus(ctx, t, clientSet, jobObj, want)

			// Tracks ready pods, if enabled.
			if err, _ := setJobPodsReady(ctx, clientSet, jobObj, 2); err != nil {
				t.Fatalf("Failed Marking Pods as ready: %v", err)
			}
			if tc.enableReadyPods {
				*want.Ready = 2
			}
			validateJobPodsStatus(ctx, t, clientSet, jobObj, want)

			// Failed Pods are replaced.
			if err, _ := setJobPodsPhase(ctx, clientSet, jobObj, v1.PodFailed, 2); err != nil {
				t.Fatalf("Failed setting phase %s on Job Pods: %v", v1.PodFailed, err)
			}
			want = podsByStatus{
				Active: 5,
				Failed: 2,
			}
			if tc.enableReadyPods {
				want.Ready = pointer.Int32(0)
			}
			validateJobPodsStatus(ctx, t, clientSet, jobObj, want)
			// Once one Pod succeeds, no more Pods are created, even if some fail.
			if err, _ := setJobPodsPhase(ctx, clientSet, jobObj, v1.PodSucceeded, 1); err != nil {
				t.Fatalf("Failed setting phase %s on Job Pod: %v", v1.PodSucceeded, err)
			}
			want = podsByStatus{
				Failed:    2,
				Succeeded: 1,
				Active:    4,
			}
			if tc.enableReadyPods {
				want.Ready = pointer.Int32(0)
			}
			validateJobPodsStatus(ctx, t, clientSet, jobObj, want)
			if err, _ := setJobPodsPhase(ctx, clientSet, jobObj, v1.PodFailed, 2); err != nil {
				t.Fatalf("Failed setting phase %s on Job Pods: %v", v1.PodFailed, err)
			}
			want = podsByStatus{
				Failed:    4,
				Succeeded: 1,
				Active:    2,
			}
			if tc.enableReadyPods {
				want.Ready = pointer.Int32(0)
			}
			validateJobPodsStatus(ctx, t, clientSet, jobObj, want)
			// No more Pods are created after remaining Pods succeed.
			if err, _ := setJobPodsPhase(ctx, clientSet, jobObj, v1.PodSucceeded, 2); err != nil {
				t.Fatalf("Failed setting phase %s on Job Pods: %v", v1.PodSucceeded, err)
			}
			validateJobSucceeded(ctx, t, clientSet, jobObj)
			want = podsByStatus{
				Failed:    4,
				Succeeded: 3,
			}
			if tc.enableReadyPods {
				want.Ready = pointer.Int32(0)
			}
			validateJobPodsStatus(ctx, t, clientSet, jobObj, want)
			validateFinishedPodsNoFinalizer(ctx, t, clientSet, jobObj)
			validateTerminatedPodsTrackingFinalizerMetric(t, 7)
		})
	}
}

func TestParallelJobParallelism(t *testing.T) {
	closeFn, restConfig, clientSet, ns := setup(t, "parallel")
	defer closeFn()
	ctx, cancel := startJobControllerAndWaitForCaches(restConfig)
	defer cancel()

	jobObj, err := createJobWithDefaults(ctx, clientSet, ns.Name, &batchv1.Job{
		Spec: batchv1.JobSpec{
			BackoffLimit: pointer.Int32(2),
			Parallelism:  pointer.Int32(5),
		},
	})
	if err != nil {
		t.Fatalf("Failed to create Job: %v", err)
	}
	validateJobPodsStatus(ctx, t, clientSet, jobObj, podsByStatus{
		Active: 5,
		Ready:  pointer.Int32(0),
	})

	// Reduce parallelism by a number greater than backoffLimit.
	patch := []byte(`{"spec":{"parallelism":2}}`)
	jobObj, err = clientSet.BatchV1().Jobs(ns.Name).Patch(ctx, jobObj.Name, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		t.Fatalf("Updating Job: %v", err)
	}
	validateJobPodsStatus(ctx, t, clientSet, jobObj, podsByStatus{
		Active: 2,
		Ready:  pointer.Int32(0),
	})

	// Increase parallelism again.
	patch = []byte(`{"spec":{"parallelism":4}}`)
	jobObj, err = clientSet.BatchV1().Jobs(ns.Name).Patch(ctx, jobObj.Name, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		t.Fatalf("Updating Job: %v", err)
	}
	validateJobPodsStatus(ctx, t, clientSet, jobObj, podsByStatus{
		Active: 4,
		Ready:  pointer.Int32(0),
	})

	// Succeed Job
	if err, _ := setJobPodsPhase(ctx, clientSet, jobObj, v1.PodSucceeded, 4); err != nil {
		t.Fatalf("Failed setting phase %s on Job Pods: %v", v1.PodFailed, err)
	}
	validateJobSucceeded(ctx, t, clientSet, jobObj)
	validateJobPodsStatus(ctx, t, clientSet, jobObj, podsByStatus{
		Succeeded: 4,
		Ready:     pointer.Int32(0),
	})
	validateFinishedPodsNoFinalizer(ctx, t, clientSet, jobObj)
}

func TestParallelJobWithCompletions(t *testing.T) {
	// Lower limits for a job sync so that we can test partial updates with a low
	// number of pods.
	t.Cleanup(setDuringTest(&jobcontroller.MaxUncountedPods, 10))
	t.Cleanup(setDuringTest(&jobcontroller.MaxPodCreateDeletePerSync, 10))
	t.Cleanup(setDurationDuringTest(&jobcontroller.DefaultJobPodFailureBackOff, fastPodFailureBackoff))
	cases := map[string]struct {
		enableReadyPods bool
	}{
		"none": {},
		"ready pods": {
			enableReadyPods: true,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			defer featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.JobReadyPods, tc.enableReadyPods)()
			closeFn, restConfig, clientSet, ns := setup(t, "completions")
			defer closeFn()
			ctx, cancel := startJobControllerAndWaitForCaches(restConfig)
			defer cancel()

			jobObj, err := createJobWithDefaults(ctx, clientSet, ns.Name, &batchv1.Job{
				Spec: batchv1.JobSpec{
					Parallelism: pointer.Int32(54),
					Completions: pointer.Int32(56),
				},
			})
			if err != nil {
				t.Fatalf("Failed to create Job: %v", err)
			}
			want := podsByStatus{Active: 54}
			if tc.enableReadyPods {
				want.Ready = pointer.Int32(0)
			}
			validateJobPodsStatus(ctx, t, clientSet, jobObj, want)

			// Tracks ready pods, if enabled.
			if err, _ := setJobPodsReady(ctx, clientSet, jobObj, 52); err != nil {
				t.Fatalf("Failed Marking Pods as ready: %v", err)
			}
			if tc.enableReadyPods {
				want.Ready = pointer.Int32(52)
			}
			validateJobPodsStatus(ctx, t, clientSet, jobObj, want)

			// Failed Pods are replaced.
			if err, _ := setJobPodsPhase(ctx, clientSet, jobObj, v1.PodFailed, 2); err != nil {
				t.Fatalf("Failed setting phase %s on Job Pods: %v", v1.PodFailed, err)
			}
			want = podsByStatus{
				Active: 54,
				Failed: 2,
			}
			if tc.enableReadyPods {
				want.Ready = pointer.Int32(50)
			}
			validateJobPodsStatus(ctx, t, clientSet, jobObj, want)
			// Pods are created until the number of succeeded Pods equals completions.
			if err, _ := setJobPodsPhase(ctx, clientSet, jobObj, v1.PodSucceeded, 53); err != nil {
				t.Fatalf("Failed setting phase %s on Job Pod: %v", v1.PodSucceeded, err)
			}
			want = podsByStatus{
				Failed:    2,
				Succeeded: 53,
				Active:    3,
			}
			if tc.enableReadyPods {
				want.Ready = pointer.Int32(0)
			}
			validateJobPodsStatus(ctx, t, clientSet, jobObj, want)
			// No more Pods are created after the Job completes.
			if err, _ := setJobPodsPhase(ctx, clientSet, jobObj, v1.PodSucceeded, 3); err != nil {
				t.Fatalf("Failed setting phase %s on Job Pods: %v", v1.PodSucceeded, err)
			}
			validateJobSucceeded(ctx, t, clientSet, jobObj)
			want = podsByStatus{
				Failed:    2,
				Succeeded: 56,
			}
			if tc.enableReadyPods {
				want.Ready = pointer.Int32(0)
			}
			validateJobPodsStatus(ctx, t, clientSet, jobObj, want)
			validateFinishedPodsNoFinalizer(ctx, t, clientSet, jobObj)
		})
	}
}

func TestIndexedJob(t *testing.T) {
	t.Cleanup(setDurationDuringTest(&jobcontroller.DefaultJobPodFailureBackOff, fastPodFailureBackoff))
	closeFn, restConfig, clientSet, ns := setup(t, "indexed")
	defer closeFn()
	ctx, cancel := startJobControllerAndWaitForCaches(restConfig)
	defer cancel()
	resetMetrics()

	mode := batchv1.IndexedCompletion
	jobObj, err := createJobWithDefaults(ctx, clientSet, ns.Name, &batchv1.Job{
		Spec: batchv1.JobSpec{
			Parallelism:    pointer.Int32(3),
			Completions:    pointer.Int32(4),
			CompletionMode: &mode,
		},
	})
	if err != nil {
		t.Fatalf("Failed to create Job: %v", err)
	}
	validateJobPodsStatus(ctx, t, clientSet, jobObj, podsByStatus{
		Active: 3,
		Ready:  pointer.Int32(0),
	})
	validateIndexedJobPods(ctx, t, clientSet, jobObj, sets.New(0, 1, 2), "", nil)

	// One Pod succeeds.
	if err := setJobPhaseForIndex(ctx, clientSet, jobObj, v1.PodSucceeded, 1); err != nil {
		t.Fatal("Failed trying to succeed pod with index 1")
	}
	validateJobPodsStatus(ctx, t, clientSet, jobObj, podsByStatus{
		Active:    3,
		Succeeded: 1,
		Ready:     pointer.Int32(0),
	})
	validateIndexedJobPods(ctx, t, clientSet, jobObj, sets.New(0, 2, 3), "1", nil)

	// One Pod fails, which should be recreated.
	if err := setJobPhaseForIndex(ctx, clientSet, jobObj, v1.PodFailed, 2); err != nil {
		t.Fatal("Failed trying to succeed pod with index 2")
	}
	validateJobPodsStatus(ctx, t, clientSet, jobObj, podsByStatus{
		Active:    3,
		Failed:    1,
		Succeeded: 1,
		Ready:     pointer.Int32(0),
	})
	validateIndexedJobPods(ctx, t, clientSet, jobObj, sets.New(0, 2, 3), "1", nil)

	// Remaining Pods succeed.
	if err, _ := setJobPodsPhase(ctx, clientSet, jobObj, v1.PodSucceeded, 3); err != nil {
		t.Fatal("Failed trying to succeed remaining pods")
	}
	validateJobPodsStatus(ctx, t, clientSet, jobObj, podsByStatus{
		Active:    0,
		Failed:    1,
		Succeeded: 4,
		Ready:     pointer.Int32(0),
	})
	validateIndexedJobPods(ctx, t, clientSet, jobObj, nil, "0-3", nil)
	validateJobSucceeded(ctx, t, clientSet, jobObj)
	validateFinishedPodsNoFinalizer(ctx, t, clientSet, jobObj)
	validateTerminatedPodsTrackingFinalizerMetric(t, 5)
}

func TestJobPodReplacementPolicy(t *testing.T) {
	const podCount int32 = 2
	indexedCompletion := batchv1.IndexedCompletion
	nonIndexedCompletion := batchv1.NonIndexedCompletion
	var podReplacementPolicy = func(obj batchv1.PodReplacementPolicy) *batchv1.PodReplacementPolicy {
		return &obj
	}
	jobSpecIndexedDefault := &batchv1.JobSpec{
		Parallelism:    pointer.Int32Ptr(podCount),
		Completions:    pointer.Int32Ptr(podCount),
		CompletionMode: &indexedCompletion,
	}
	cases := map[string]struct {
		podReplacementPolicyEnabled bool
		wantTerminating             *int32
		wantFailed                  int
		wantActive                  int
		jobSpec                     *batchv1.JobSpec
	}{
		"feature flag off, delete pods and verify no terminating status": {
			jobSpec:    jobSpecIndexedDefault,
			wantActive: int(podCount),
			wantFailed: int(podCount),
		},
		"feature flag true, delete pods and verify terminating status": {
			podReplacementPolicyEnabled: true,
			jobSpec:                     jobSpecIndexedDefault,
			wantTerminating:             pointer.Int32(podCount),
			wantFailed:                  int(podCount),
		},
		"feature flag true, delete pods, verify terminating status and recreate upon terminating": {
			podReplacementPolicyEnabled: true,
			jobSpec: &batchv1.JobSpec{
				Parallelism:          pointer.Int32Ptr(podCount),
				Completions:          pointer.Int32Ptr(podCount),
				CompletionMode:       &indexedCompletion,
				PodReplacementPolicy: podReplacementPolicy(batchv1.TerminatingOrFailed),
			},
			wantTerminating: pointer.Int32(podCount),
			wantFailed:      int(podCount),
		},
		"feature flag true, delete pods, verify terminating status and recreate once failed": {
			podReplacementPolicyEnabled: true,
			jobSpec: &batchv1.JobSpec{
				Parallelism:          pointer.Int32Ptr(podCount),
				Completions:          pointer.Int32Ptr(podCount),
				CompletionMode:       &nonIndexedCompletion,
				PodReplacementPolicy: podReplacementPolicy(batchv1.Failed),
			},
			wantTerminating: pointer.Int32(podCount),
		},
		"feature flag true with NonIndexedJob, delete pods, verify terminating status and recreate once failed": {
			podReplacementPolicyEnabled: true,
			jobSpec: &batchv1.JobSpec{
				Parallelism:          pointer.Int32Ptr(podCount),
				Completions:          pointer.Int32Ptr(podCount),
				CompletionMode:       &nonIndexedCompletion,
				PodReplacementPolicy: podReplacementPolicy(batchv1.Failed),
			},
			wantTerminating: pointer.Int32(podCount),
		},
		"feature flag false, podFailurePolicy enabled, delete pods, verify terminating status and recreate once failed": {
			podReplacementPolicyEnabled: false,
			jobSpec: &batchv1.JobSpec{
				Parallelism:          pointer.Int32Ptr(podCount),
				Completions:          pointer.Int32Ptr(podCount),
				CompletionMode:       &nonIndexedCompletion,
				PodReplacementPolicy: podReplacementPolicy(batchv1.Failed),
				PodFailurePolicy: &batchv1.PodFailurePolicy{
					Rules: []batchv1.PodFailurePolicyRule{
						{
							Action: batchv1.PodFailurePolicyActionFailJob,
							OnExitCodes: &batchv1.PodFailurePolicyOnExitCodesRequirement{
								Operator: batchv1.PodFailurePolicyOnExitCodesOpIn,
								Values:   []int32{5},
							},
						},
					},
				},
			},
			wantActive: int(podCount),
		},
	}
	for name, tc := range cases {
		tc := tc
		t.Run(name, func(t *testing.T) {
			defer featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.JobPodReplacementPolicy, tc.podReplacementPolicyEnabled)()
			defer featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.JobPodFailurePolicy, tc.jobSpec.PodFailurePolicy != nil)()

			closeFn, restConfig, clientSet, ns := setup(t, "pod-replacement-policy")
			defer closeFn()
			ctx, cancel := startJobControllerAndWaitForCaches(restConfig)
			defer cancel()
			resetMetrics()

			jobObj, err := createJobWithDefaults(ctx, clientSet, ns.Name, &batchv1.Job{
				Spec: *tc.jobSpec,
			})
			if err != nil {
				t.Fatalf("Failed to create Job: %v", err)
			}
			jobClient := clientSet.BatchV1().Jobs(jobObj.Namespace)

			// Wait for pods to start up.
			err = wait.PollImmediate(5*time.Millisecond, wait.ForeverTestTimeout, func() (done bool, err error) {
				job, err := jobClient.Get(ctx, jobObj.Name, metav1.GetOptions{})
				if err != nil {
					return false, err
				}
				if job.Status.Active == int32(podCount) {
					return true, nil
				}
				return false, nil
			})
			if err != nil {
				t.Fatalf("Error waiting for Job pods to become active: %v", err)
			}
			pods, errList := clientSet.CoreV1().Pods(ns.Namespace).List(ctx, metav1.ListOptions{})
			if errList != nil {
				t.Fatalf("Failed to list pods: %v", errList)
			}
			updatePod(t, clientSet, pods.Items, func(pod *v1.Pod) {
				pod.Finalizers = append(pod.Finalizers, "fake.example.com/blockDeletion")
			})
			err = clientSet.CoreV1().Pods(ns.Name).DeleteCollection(ctx,
				metav1.DeleteOptions{},
				metav1.ListOptions{
					Limit: 1000,
				})
			if err != nil {
				t.Fatalf("Failed to cleanup Pods: %v", err)
			}

			podsDelete, errList2 := clientSet.CoreV1().Pods(ns.Namespace).List(ctx, metav1.ListOptions{})
			if errList != nil {
				t.Fatalf("Failed to list pods: %v", errList2)
			}
			for _, val := range podsDelete.Items {
				if val.DeletionTimestamp == nil {
					t.Fatalf("Deletion not registered.")
				}
			}

			validateJobsPodsStatusOnly(ctx, t, clientSet, jobObj, podsByStatus{
				Terminating: tc.wantTerminating,
				Failed:      tc.wantFailed,
				Active:      tc.wantActive,
				Ready:       pointer.Int32(0),
			})
		})
	}
}

func TestElasticIndexedJob(t *testing.T) {
	const initialCompletions int32 = 3
	type jobUpdate struct {
		completions          *int32
		succeedIndexes       []int
		failIndexes          []int
		wantSucceededIndexes string
		wantFailed           int
		wantRemainingIndexes sets.Set[int]
		wantActivePods       int
	}
	cases := map[string]struct {
		featureGate bool
		jobUpdates  []jobUpdate
		wantErr     *apierrors.StatusError
	}{
		"feature flag off, mutation not allowed": {
			jobUpdates: []jobUpdate{
				{
					completions: pointer.Int32Ptr(4),
				},
			},
			wantErr: apierrors.NewInvalid(
				schema.GroupKind{Group: "batch", Kind: "Job"},
				"test-job",
				field.ErrorList{field.Invalid(field.NewPath("spec", "completions"), 4, "field is immutable")},
			),
		},
		"scale up": {
			featureGate: true,
			jobUpdates: []jobUpdate{
				{
					// Scale up completions 3->4 then succeed indexes 0-3
					completions:          pointer.Int32Ptr(4),
					succeedIndexes:       []int{0, 1, 2, 3},
					wantSucceededIndexes: "0-3",
				},
			},
		},
		"scale down": {
			featureGate: true,
			jobUpdates: []jobUpdate{
				// First succeed index 1 and fail index 2 while completions is still original value (3).
				{
					succeedIndexes:       []int{1},
					failIndexes:          []int{2},
					wantSucceededIndexes: "1",
					wantFailed:           1,
					wantRemainingIndexes: sets.New(0, 2),
					wantActivePods:       2,
				},
				// Scale down completions 3->1, verify prev failure out of range still counts
				// but succeeded out of range does not.
				{
					completions:          pointer.Int32Ptr(1),
					succeedIndexes:       []int{0},
					wantSucceededIndexes: "0",
					wantFailed:           1,
				},
			},
		},
		"index finishes successfully, scale down, scale up": {
			featureGate: true,
			jobUpdates: []jobUpdate{
				// First succeed index 2 while completions is still original value (3).
				{
					succeedIndexes:       []int{2},
					wantSucceededIndexes: "2",
					wantRemainingIndexes: sets.New(0, 1),
					wantActivePods:       2,
				},
				// Scale completions down 3->2 to exclude previously succeeded index.
				{
					completions:          pointer.Int32Ptr(2),
					wantRemainingIndexes: sets.New(0, 1),
					wantActivePods:       2,
				},
				// Scale completions back up to include previously succeeded index that was temporarily out of range.
				{
					completions:          pointer.Int32Ptr(3),
					succeedIndexes:       []int{0, 1, 2},
					wantSucceededIndexes: "0-2",
				},
			},
		},
		"scale down to 0, verify that the job succeeds": {
			featureGate: true,
			jobUpdates: []jobUpdate{
				{
					completions: pointer.Int32Ptr(0),
				},
			},
		},
	}

	for name, tc := range cases {
		tc := tc
		t.Run(name, func(t *testing.T) {
			defer featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.ElasticIndexedJob, tc.featureGate)()
			closeFn, restConfig, clientSet, ns := setup(t, "indexed")
			defer closeFn()
			ctx, cancel := startJobControllerAndWaitForCaches(restConfig)
			defer cancel()
			resetMetrics()

			// Set up initial Job in Indexed completion mode.
			mode := batchv1.IndexedCompletion
			jobObj, err := createJobWithDefaults(ctx, clientSet, ns.Name, &batchv1.Job{
				Spec: batchv1.JobSpec{
					Parallelism:    pointer.Int32Ptr(initialCompletions),
					Completions:    pointer.Int32Ptr(initialCompletions),
					CompletionMode: &mode,
				},
			})
			if err != nil {
				t.Fatalf("Failed to create Job: %v", err)
			}
			jobClient := clientSet.BatchV1().Jobs(jobObj.Namespace)

			// Wait for pods to start up.
			err = wait.PollImmediate(5*time.Millisecond, wait.ForeverTestTimeout, func() (done bool, err error) {
				job, err := jobClient.Get(ctx, jobObj.Name, metav1.GetOptions{})
				if err != nil {
					return false, err
				}
				if job.Status.Active == int32(initialCompletions) {
					return true, nil
				}
				return false, nil
			})
			if err != nil {
				t.Fatalf("Error waiting for Job pods to become active: %v", err)
			}

			for _, update := range tc.jobUpdates {
				// Update Job spec if necessary.
				if update.completions != nil {
					if jobObj, err = updateJob(ctx, jobClient, jobObj.Name, func(j *batchv1.Job) {
						j.Spec.Completions = update.completions
						j.Spec.Parallelism = update.completions
					}); err != nil {
						if diff := cmp.Diff(tc.wantErr, err); diff != "" {
							t.Fatalf("Unexpected or missing errors (-want/+got): %s", diff)
						}
						return
					}
				}

				// Succeed specified indexes.
				for _, idx := range update.succeedIndexes {
					if err := setJobPhaseForIndex(ctx, clientSet, jobObj, v1.PodSucceeded, idx); err != nil {
						t.Fatalf("Failed trying to succeed pod with index %d: %v", idx, err)
					}
				}

				// Fail specified indexes.
				for _, idx := range update.failIndexes {
					if err := setJobPhaseForIndex(ctx, clientSet, jobObj, v1.PodFailed, idx); err != nil {
						t.Fatalf("Failed trying to fail pod with index %d: %v", idx, err)
					}
				}

				validateJobPodsStatus(ctx, t, clientSet, jobObj, podsByStatus{
					Active:    update.wantActivePods,
					Succeeded: len(update.succeedIndexes),
					Failed:    update.wantFailed,
					Ready:     pointer.Int32(0),
				})
				validateIndexedJobPods(ctx, t, clientSet, jobObj, update.wantRemainingIndexes, update.wantSucceededIndexes, nil)
			}

			validateJobSucceeded(ctx, t, clientSet, jobObj)
		})
	}
}

// BenchmarkLargeIndexedJob benchmarks the completion of an Indexed Job.
// We expect that large jobs are more commonly used as Indexed. And they are
// also faster to track, as they need less API calls.
func BenchmarkLargeIndexedJob(b *testing.B) {
	closeFn, restConfig, clientSet, ns := setup(b, "indexed")
	restConfig.QPS = 100
	restConfig.Burst = 100
	defer closeFn()
	ctx, cancel := startJobControllerAndWaitForCaches(restConfig)
	defer cancel()
	backoff := wait.Backoff{
		Duration: time.Second,
		Factor:   1.5,
		Steps:    30,
		Cap:      5 * time.Minute,
	}
	mode := batchv1.IndexedCompletion
	for _, nPods := range []int32{1000, 10_000} {
		b.Run(fmt.Sprintf("nPods=%d", nPods), func(b *testing.B) {
			b.ResetTimer()
			for n := 0; n < b.N; n++ {
				jobObj, err := createJobWithDefaults(ctx, clientSet, ns.Name, &batchv1.Job{
					ObjectMeta: metav1.ObjectMeta{
						Name: fmt.Sprintf("npods-%d-%d", nPods, n),
					},
					Spec: batchv1.JobSpec{
						Parallelism:    pointer.Int32(nPods),
						Completions:    pointer.Int32(nPods),
						CompletionMode: &mode,
					},
				})
				if err != nil {
					b.Fatalf("Failed to create Job: %v", err)
				}
				remaining := int(nPods)
				if err := wait.ExponentialBackoff(backoff, func() (done bool, err error) {
					if err, succ := setJobPodsPhase(ctx, clientSet, jobObj, v1.PodSucceeded, remaining); err != nil {
						remaining -= succ
						b.Logf("Transient failure succeeding pods: %v", err)
						return false, nil
					}
					return true, nil
				}); err != nil {
					b.Fatalf("Could not succeed the remaining %d pods: %v", remaining, err)
				}
				validateJobSucceeded(ctx, b, clientSet, jobObj)

				// Cleanup Pods and Job.
				b.StopTimer()
				// Clean up pods in pages, because DeleteCollection might timeout.
				// #90743
				for {
					pods, err := clientSet.CoreV1().Pods(ns.Name).List(ctx, metav1.ListOptions{Limit: 1})
					if err != nil {
						b.Fatalf("Failed to list Pods for cleanup: %v", err)
					}
					if len(pods.Items) == 0 {
						break
					}
					err = clientSet.CoreV1().Pods(ns.Name).DeleteCollection(ctx,
						metav1.DeleteOptions{},
						metav1.ListOptions{
							Limit: 1000,
						})
					if err != nil {
						b.Fatalf("Failed to cleanup Pods: %v", err)
					}
				}
				err = clientSet.BatchV1().Jobs(jobObj.Namespace).Delete(ctx, jobObj.Name, metav1.DeleteOptions{})
				if err != nil {
					b.Fatalf("Failed to cleanup Job: %v", err)
				}
				b.StartTimer()
			}
		})
	}
}

func TestOrphanPodsFinalizersClearedWithGC(t *testing.T) {
	for _, policy := range []metav1.DeletionPropagation{metav1.DeletePropagationOrphan, metav1.DeletePropagationBackground, metav1.DeletePropagationForeground} {
		t.Run(string(policy), func(t *testing.T) {
			closeFn, restConfig, clientSet, ns := setup(t, "simple")
			defer closeFn()
			informerSet := informers.NewSharedInformerFactory(clientset.NewForConfigOrDie(restclient.AddUserAgent(restConfig, "controller-informers")), 0)
			// Make the job controller significantly slower to trigger race condition.
			restConfig.QPS = 1
			restConfig.Burst = 1
			jc, ctx, cancel := createJobControllerWithSharedInformers(restConfig, informerSet)
			resetMetrics()
			defer cancel()
			restConfig.QPS = 200
			restConfig.Burst = 200
			runGC := util.CreateGCController(ctx, t, *restConfig, informerSet)
			informerSet.Start(ctx.Done())
			go jc.Run(ctx, 1)
			runGC()

			jobObj, err := createJobWithDefaults(ctx, clientSet, ns.Name, &batchv1.Job{
				Spec: batchv1.JobSpec{
					Parallelism: pointer.Int32(2),
				},
			})
			if err != nil {
				t.Fatalf("Failed to create Job: %v", err)
			}
			validateJobPodsStatus(ctx, t, clientSet, jobObj, podsByStatus{
				Active: 2,
				Ready:  pointer.Int32(0),
			})

			// Delete Job. The GC should delete the pods in cascade.
			err = clientSet.BatchV1().Jobs(jobObj.Namespace).Delete(ctx, jobObj.Name, metav1.DeleteOptions{
				PropagationPolicy: &policy,
			})
			if err != nil {
				t.Fatalf("Failed to delete job: %v", err)
			}
			validateNoOrphanPodsWithFinalizers(ctx, t, clientSet, jobObj)
			// Pods never finished, so they are not counted in the metric.
			validateTerminatedPodsTrackingFinalizerMetric(t, 0)
		})
	}
}

func TestFinalizersClearedWhenBackoffLimitExceeded(t *testing.T) {
	// Set a maximum number of uncounted pods below parallelism, to ensure it
	// doesn't affect the termination of pods.
	t.Cleanup(setDuringTest(&jobcontroller.MaxUncountedPods, 50))
	closeFn, restConfig, clientSet, ns := setup(t, "simple")
	defer closeFn()
	ctx, cancel := startJobControllerAndWaitForCaches(restConfig)
	defer cancel()

	// Job tracking with finalizers requires less calls in Indexed mode,
	// so it's more likely to process all finalizers before all the pods
	// are visible.
	mode := batchv1.IndexedCompletion
	jobObj, err := createJobWithDefaults(ctx, clientSet, ns.Name, &batchv1.Job{
		Spec: batchv1.JobSpec{
			CompletionMode: &mode,
			Completions:    pointer.Int32(100),
			Parallelism:    pointer.Int32(100),
			BackoffLimit:   pointer.Int32(0),
		},
	})
	if err != nil {
		t.Fatalf("Could not create job: %v", err)
	}

	// Fail a pod ASAP.
	err = wait.PollImmediate(time.Millisecond, wait.ForeverTestTimeout, func() (done bool, err error) {
		if err, _ := setJobPodsPhase(ctx, clientSet, jobObj, v1.PodFailed, 1); err != nil {
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("Could not fail pod: %v", err)
	}

	validateJobFailed(ctx, t, clientSet, jobObj)

	validateNoOrphanPodsWithFinalizers(ctx, t, clientSet, jobObj)
}

func TestJobPodsCreatedWithExponentialBackoff(t *testing.T) {
	t.Cleanup(setDurationDuringTest(&jobcontroller.DefaultJobPodFailureBackOff, 2*time.Second))
	closeFn, restConfig, clientSet, ns := setup(t, "simple")
	defer closeFn()
	ctx, cancel := startJobControllerAndWaitForCaches(restConfig)
	defer cancel()

	jobObj, err := createJobWithDefaults(ctx, clientSet, ns.Name, &batchv1.Job{})
	if err != nil {
		t.Fatalf("Could not create job: %v", err)
	}
	validateJobPodsStatus(ctx, t, clientSet, jobObj, podsByStatus{
		Active: 1,
		Ready:  pointer.Int32(0),
	})

	// Fail the first pod
	if err, _ := setJobPodsPhase(ctx, clientSet, jobObj, v1.PodFailed, 1); err != nil {
		t.Fatalf("Failed setting phase %s on Job Pod: %v", v1.PodFailed, err)
	}
	validateJobPodsStatus(ctx, t, clientSet, jobObj, podsByStatus{
		Active: 1,
		Ready:  pointer.Int32(0),
		Failed: 1,
	})

	// Fail the second pod
	if err, _ := setJobPodsPhase(ctx, clientSet, jobObj, v1.PodFailed, 1); err != nil {
		t.Fatalf("Failed setting phase %s on Job Pod: %v", v1.PodFailed, err)
	}
	validateJobPodsStatus(ctx, t, clientSet, jobObj, podsByStatus{
		Active: 1,
		Ready:  pointer.Int32(0),
		Failed: 2,
	})

	jobPods, err := getJobPods(ctx, t, clientSet, jobObj, func(ps v1.PodStatus) bool { return true })
	if err != nil {
		t.Fatalf("Failed to list Job Pods: %v", err)
	}
	if len(jobPods) != 3 {
		t.Fatalf("Expected to get %v pods, received %v", 4, len(jobPods))
	}
	validateExpotentialBackoffDelay(t, jobcontroller.DefaultJobPodFailureBackOff, jobPods)
}

func validateExpotentialBackoffDelay(t *testing.T, defaultPodFailureBackoff time.Duration, pods []*v1.Pod) {
	t.Helper()
	creationTime := []time.Time{}
	finishTime := []time.Time{}
	for _, pod := range pods {
		creationTime = append(creationTime, pod.CreationTimestamp.Time)
		if len(pod.Status.ContainerStatuses) > 0 {
			finishTime = append(finishTime, pod.Status.ContainerStatuses[0].State.Terminated.FinishedAt.Time)
		}
	}

	sort.Slice(creationTime, func(i, j int) bool {
		return creationTime[i].Before(creationTime[j])
	})
	sort.Slice(finishTime, func(i, j int) bool {
		return finishTime[i].Before(finishTime[j])
	})

	diff := creationTime[1].Sub(finishTime[0])

	if diff < defaultPodFailureBackoff {
		t.Fatalf("Second pod should be created at least %v seconds after the first pod, time difference: %v", defaultPodFailureBackoff, diff)
	}

	if diff >= 2*defaultPodFailureBackoff {
		t.Fatalf("Second pod should be created before %v seconds after the first pod, time difference: %v", 2*defaultPodFailureBackoff, diff)
	}

	diff = creationTime[2].Sub(finishTime[1])

	if diff < 2*defaultPodFailureBackoff {
		t.Fatalf("Third pod should be created at least %v seconds after the second pod, time difference: %v", 2*defaultPodFailureBackoff, diff)
	}

	if diff >= 4*defaultPodFailureBackoff {
		t.Fatalf("Third pod should be created before %v seconds after the second pod, time difference: %v", 4*defaultPodFailureBackoff, diff)
	}
}

// TestJobFailedWithInterrupts tests that a job were one pod fails and the rest
// succeed is marked as Failed, even if the controller fails in the middle.
func TestJobFailedWithInterrupts(t *testing.T) {
	closeFn, restConfig, clientSet, ns := setup(t, "simple")
	defer closeFn()
	ctx, cancel := startJobControllerAndWaitForCaches(restConfig)
	defer func() {
		cancel()
	}()
	jobObj, err := createJobWithDefaults(ctx, clientSet, ns.Name, &batchv1.Job{
		Spec: batchv1.JobSpec{
			Completions:  pointer.Int32(10),
			Parallelism:  pointer.Int32(10),
			BackoffLimit: pointer.Int32(0),
			Template: v1.PodTemplateSpec{
				Spec: v1.PodSpec{
					NodeName: "foo", // Scheduled pods are not deleted immediately.
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Could not create job: %v", err)
	}
	validateJobPodsStatus(ctx, t, clientSet, jobObj, podsByStatus{
		Active: 10,
		Ready:  pointer.Int32(0),
	})
	t.Log("Finishing pods")
	if err, _ := setJobPodsPhase(ctx, clientSet, jobObj, v1.PodFailed, 1); err != nil {
		t.Fatalf("Could not fail a pod: %v", err)
	}
	remaining := 9
	if err := wait.PollImmediate(5*time.Millisecond, wait.ForeverTestTimeout, func() (done bool, err error) {
		if err, succ := setJobPodsPhase(ctx, clientSet, jobObj, v1.PodSucceeded, remaining); err != nil {
			remaining -= succ
			t.Logf("Transient failure succeeding pods: %v", err)
			return false, nil
		}
		return true, nil
	}); err != nil {
		t.Fatalf("Could not succeed the remaining %d pods: %v", remaining, err)
	}
	t.Log("Recreating job controller")
	cancel()
	ctx, cancel = startJobControllerAndWaitForCaches(restConfig)
	validateJobCondition(ctx, t, clientSet, jobObj, batchv1.JobFailed)
}

func validateNoOrphanPodsWithFinalizers(ctx context.Context, t *testing.T, clientSet clientset.Interface, jobObj *batchv1.Job) {
	t.Helper()
	orphanPods := 0
	if err := wait.PollImmediate(waitInterval, wait.ForeverTestTimeout, func() (done bool, err error) {
		pods, err := clientSet.CoreV1().Pods(jobObj.Namespace).List(ctx, metav1.ListOptions{
			LabelSelector: metav1.FormatLabelSelector(jobObj.Spec.Selector),
		})
		if err != nil {
			return false, err
		}
		orphanPods = 0
		for _, pod := range pods.Items {
			if hasJobTrackingFinalizer(&pod) {
				orphanPods++
			}
		}
		return orphanPods == 0, nil
	}); err != nil {
		t.Errorf("Failed waiting for pods to be freed from finalizer: %v", err)
		t.Logf("Last saw %d orphan pods", orphanPods)
	}
}

func TestOrphanPodsFinalizersClearedOnRestart(t *testing.T) {
	// Step 0: create job.
	closeFn, restConfig, clientSet, ns := setup(t, "simple")
	defer closeFn()
	ctx, cancel := startJobControllerAndWaitForCaches(restConfig)
	defer func() {
		cancel()
	}()

	jobObj, err := createJobWithDefaults(ctx, clientSet, ns.Name, &batchv1.Job{
		Spec: batchv1.JobSpec{
			Parallelism: pointer.Int32(1),
		},
	})
	if err != nil {
		t.Fatalf("Failed to create Job: %v", err)
	}
	validateJobPodsStatus(ctx, t, clientSet, jobObj, podsByStatus{
		Active: 1,
		Ready:  pointer.Int32(0),
	})

	// Step 2: Delete the Job while the controller is stopped.
	cancel()

	err = clientSet.BatchV1().Jobs(jobObj.Namespace).Delete(context.Background(), jobObj.Name, metav1.DeleteOptions{})
	if err != nil {
		t.Fatalf("Failed to delete job: %v", err)
	}

	// Step 3: Restart controller.
	ctx, cancel = startJobControllerAndWaitForCaches(restConfig)
	validateNoOrphanPodsWithFinalizers(ctx, t, clientSet, jobObj)
}

func TestSuspendJob(t *testing.T) {
	type step struct {
		flag       bool
		wantActive int
		wantStatus v1.ConditionStatus
		wantReason string
	}
	testCases := []struct {
		featureGate bool
		create      step
		update      step
	}{
		// Exhaustively test all combinations other than trivial true->true and
		// false->false cases.
		{
			create: step{flag: false, wantActive: 2},
			update: step{flag: true, wantActive: 0, wantStatus: v1.ConditionTrue, wantReason: "Suspended"},
		},
		{
			create: step{flag: true, wantActive: 0, wantStatus: v1.ConditionTrue, wantReason: "Suspended"},
			update: step{flag: false, wantActive: 2, wantStatus: v1.ConditionFalse, wantReason: "Resumed"},
		},
	}

	for _, tc := range testCases {
		name := fmt.Sprintf("feature=%v,create=%v,update=%v", tc.featureGate, tc.create.flag, tc.update.flag)
		t.Run(name, func(t *testing.T) {
			closeFn, restConfig, clientSet, ns := setup(t, "suspend")
			defer closeFn()
			ctx, cancel := startJobControllerAndWaitForCaches(restConfig)
			defer cancel()
			events, err := clientSet.EventsV1().Events(ns.Name).Watch(ctx, metav1.ListOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer events.Stop()

			parallelism := int32(2)
			job, err := createJobWithDefaults(ctx, clientSet, ns.Name, &batchv1.Job{
				Spec: batchv1.JobSpec{
					Parallelism: pointer.Int32(parallelism),
					Completions: pointer.Int32(4),
					Suspend:     pointer.BoolPtr(tc.create.flag),
				},
			})
			if err != nil {
				t.Fatalf("Failed to create Job: %v", err)
			}

			validate := func(s string, active int, status v1.ConditionStatus, reason string) {
				validateJobPodsStatus(ctx, t, clientSet, job, podsByStatus{
					Active: active,
					Ready:  pointer.Int32(0),
				})
				job, err = clientSet.BatchV1().Jobs(ns.Name).Get(ctx, job.Name, metav1.GetOptions{})
				if err != nil {
					t.Fatalf("Failed to get Job after %s: %v", s, err)
				}
				if got, want := getJobConditionStatus(ctx, job, batchv1.JobSuspended), status; got != want {
					t.Errorf("Unexpected Job condition %q status after %s: got %q, want %q", batchv1.JobSuspended, s, got, want)
				}
				if err := waitForEvent(events, job.UID, reason); err != nil {
					t.Errorf("Waiting for event with reason %q after %s: %v", reason, s, err)
				}
			}
			validate("create", tc.create.wantActive, tc.create.wantStatus, tc.create.wantReason)

			job.Spec.Suspend = pointer.BoolPtr(tc.update.flag)
			job, err = clientSet.BatchV1().Jobs(ns.Name).Update(ctx, job, metav1.UpdateOptions{})
			if err != nil {
				t.Fatalf("Failed to update Job: %v", err)
			}
			validate("update", tc.update.wantActive, tc.update.wantStatus, tc.update.wantReason)
		})
	}
}

func TestSuspendJobControllerRestart(t *testing.T) {
	closeFn, restConfig, clientSet, ns := setup(t, "suspend")
	defer closeFn()
	ctx, cancel := startJobControllerAndWaitForCaches(restConfig)
	defer cancel()

	job, err := createJobWithDefaults(ctx, clientSet, ns.Name, &batchv1.Job{
		Spec: batchv1.JobSpec{
			Parallelism: pointer.Int32(2),
			Completions: pointer.Int32(4),
			Suspend:     pointer.BoolPtr(true),
		},
	})
	if err != nil {
		t.Fatalf("Failed to create Job: %v", err)
	}
	validateJobPodsStatus(ctx, t, clientSet, job, podsByStatus{
		Active: 0,
		Ready:  pointer.Int32(0),
	})
}

func TestNodeSelectorUpdate(t *testing.T) {
	closeFn, restConfig, clientSet, ns := setup(t, "suspend")
	defer closeFn()
	ctx, cancel := startJobControllerAndWaitForCaches(restConfig)
	defer cancel()

	job, err := createJobWithDefaults(ctx, clientSet, ns.Name, &batchv1.Job{Spec: batchv1.JobSpec{
		Parallelism: pointer.Int32(1),
		Suspend:     pointer.BoolPtr(true),
	}})
	if err != nil {
		t.Fatalf("Failed to create Job: %v", err)
	}
	jobName := job.Name
	jobNamespace := job.Namespace
	jobClient := clientSet.BatchV1().Jobs(jobNamespace)

	// (1) Unsuspend and set node selector in the same update.
	nodeSelector := map[string]string{"foo": "bar"}
	if _, err := updateJob(ctx, jobClient, jobName, func(j *batchv1.Job) {
		j.Spec.Template.Spec.NodeSelector = nodeSelector
		j.Spec.Suspend = pointer.BoolPtr(false)
	}); err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	// (2) Check that the pod was created using the expected node selector.

	var pod *v1.Pod
	if err := wait.PollImmediate(waitInterval, wait.ForeverTestTimeout, func() (bool, error) {
		pods, err := clientSet.CoreV1().Pods(jobNamespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			t.Fatalf("Failed to list Job Pods: %v", err)
		}
		if len(pods.Items) == 0 {
			return false, nil
		}
		pod = &pods.Items[0]
		return true, nil
	}); err != nil || pod == nil {
		t.Fatalf("pod not found: %v", err)
	}

	// if the feature gate is enabled, then the job should now be unsuspended and
	// the pod has the node selector.
	if diff := cmp.Diff(nodeSelector, pod.Spec.NodeSelector); diff != "" {
		t.Errorf("Unexpected nodeSelector (-want,+got):\n%s", diff)
	}

	// (3) Update node selector again. It should fail since the job is unsuspended.
	_, err = updateJob(ctx, jobClient, jobName, func(j *batchv1.Job) {
		j.Spec.Template.Spec.NodeSelector = map[string]string{"foo": "baz"}
	})

	if err == nil || !strings.Contains(err.Error(), "spec.template: Invalid value") {
		t.Errorf("Expected \"spec.template: Invalid value\" error, got: %v", err)
	}

}

type podsByStatus struct {
	Active      int
	Ready       *int32
	Failed      int
	Succeeded   int
	Terminating *int32
}

func validateJobsPodsStatusOnly(ctx context.Context, t *testing.T, clientSet clientset.Interface, jobObj *batchv1.Job, desired podsByStatus) {
	t.Helper()
	var actualCounts podsByStatus
	if err := wait.PollImmediate(waitInterval, wait.ForeverTestTimeout, func() (bool, error) {
		updatedJob, err := clientSet.BatchV1().Jobs(jobObj.Namespace).Get(ctx, jobObj.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("Failed to get updated Job: %v", err)
		}
		actualCounts = podsByStatus{
			Active:      int(updatedJob.Status.Active),
			Ready:       updatedJob.Status.Ready,
			Succeeded:   int(updatedJob.Status.Succeeded),
			Failed:      int(updatedJob.Status.Failed),
			Terminating: updatedJob.Status.Terminating,
		}
		return cmp.Equal(actualCounts, desired), nil
	}); err != nil {
		diff := cmp.Diff(desired, actualCounts)
		t.Errorf("Waiting for Job Status: %v\nPods (-want,+got):\n%s", err, diff)
	}
}
func validateJobPodsStatus(ctx context.Context, t *testing.T, clientSet clientset.Interface, jobObj *batchv1.Job, desired podsByStatus) {
	t.Helper()
	validateJobsPodsStatusOnly(ctx, t, clientSet, jobObj, desired)
	var active []*v1.Pod
	if err := wait.PollImmediate(waitInterval, wait.ForeverTestTimeout, func() (bool, error) {
		pods, err := clientSet.CoreV1().Pods(jobObj.Namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			t.Fatalf("Failed to list Job Pods: %v", err)
		}
		active = nil
		for _, pod := range pods.Items {
			phase := pod.Status.Phase
			if metav1.IsControlledBy(&pod, jobObj) && (phase == v1.PodPending || phase == v1.PodRunning) {
				p := pod
				active = append(active, &p)
			}
		}
		return len(active) == desired.Active, nil
	}); err != nil {
		if len(active) != desired.Active {
			t.Errorf("Found %d active Pods, want %d", len(active), desired.Active)
		}
	}
	for _, p := range active {
		if !hasJobTrackingFinalizer(p) {
			t.Errorf("Active pod %s doesn't have tracking finalizer", p.Name)
		}
	}
}

func getJobPods(ctx context.Context, t *testing.T, clientSet clientset.Interface, jobObj *batchv1.Job, filter func(v1.PodStatus) bool) ([]*v1.Pod, error) {
	t.Helper()
	allPods, err := clientSet.CoreV1().Pods(jobObj.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	jobPods := make([]*v1.Pod, 0, 0)
	for _, pod := range allPods.Items {
		if metav1.IsControlledBy(&pod, jobObj) && filter(pod.Status) {
			p := pod
			jobPods = append(jobPods, &p)
		}
	}
	return jobPods, nil
}

func validateFinishedPodsNoFinalizer(ctx context.Context, t *testing.T, clientSet clientset.Interface, jobObj *batchv1.Job) {
	t.Helper()
	pods, err := clientSet.CoreV1().Pods(jobObj.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("Failed to list Job Pods: %v", err)
	}
	for _, pod := range pods.Items {
		phase := pod.Status.Phase
		if metav1.IsControlledBy(&pod, jobObj) && (phase == v1.PodPending || phase == v1.PodRunning) && hasJobTrackingFinalizer(&pod) {
			t.Errorf("Finished pod %s still has a tracking finalizer", pod.Name)
		}
	}
}

// validateIndexedJobPods validates indexes and hostname of
// active and completed Pods of an Indexed Job.
// Call after validateJobPodsStatus
func validateIndexedJobPods(ctx context.Context, t *testing.T, clientSet clientset.Interface, jobObj *batchv1.Job, wantActive sets.Set[int], gotCompleted string, wantFailed *string) {
	t.Helper()
	updatedJob, err := clientSet.BatchV1().Jobs(jobObj.Namespace).Get(ctx, jobObj.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get updated Job: %v", err)
	}
	if updatedJob.Status.CompletedIndexes != gotCompleted {
		t.Errorf("Got completed indexes %q, want %q", updatedJob.Status.CompletedIndexes, gotCompleted)
	}
	if diff := cmp.Diff(wantFailed, updatedJob.Status.FailedIndexes); diff != "" {
		t.Errorf("Got unexpected failed indexes: %s", diff)
	}
	pods, err := clientSet.CoreV1().Pods(jobObj.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("Failed to list Job Pods: %v", err)
	}
	gotActive := sets.New[int]()
	for _, pod := range pods.Items {
		if metav1.IsControlledBy(&pod, jobObj) {
			if pod.Status.Phase == v1.PodPending || pod.Status.Phase == v1.PodRunning {
				ix, err := getCompletionIndex(&pod)
				if err != nil {
					t.Errorf("Failed getting completion index for pod %s: %v", pod.Name, err)
				} else {
					gotActive.Insert(ix)
				}
				expectedName := fmt.Sprintf("%s-%d", jobObj.Name, ix)
				if diff := cmp.Equal(expectedName, pod.Spec.Hostname); !diff {
					t.Errorf("Got pod hostname %s, want %s", pod.Spec.Hostname, expectedName)
				}
			}
		}
	}
	if wantActive == nil {
		wantActive = sets.New[int]()
	}
	if diff := cmp.Diff(sets.List(wantActive), sets.List(gotActive)); diff != "" {
		t.Errorf("Unexpected active indexes (-want,+got):\n%s", diff)
	}
}

func waitForEvent(events watch.Interface, uid types.UID, reason string) error {
	if reason == "" {
		return nil
	}
	return wait.PollImmediate(waitInterval, wait.ForeverTestTimeout, func() (bool, error) {
		for {
			var ev watch.Event
			select {
			case ev = <-events.ResultChan():
			default:
				return false, nil
			}
			e, ok := ev.Object.(*eventsv1.Event)
			if !ok {
				continue
			}
			ctrl := "job-controller"
			if (e.ReportingController == ctrl || e.DeprecatedSource.Component == ctrl) && e.Reason == reason && e.Regarding.UID == uid {
				return true, nil
			}
		}
	})
}

func getJobConditionStatus(ctx context.Context, job *batchv1.Job, cType batchv1.JobConditionType) v1.ConditionStatus {
	for _, cond := range job.Status.Conditions {
		if cond.Type == cType {
			return cond.Status
		}
	}
	return ""
}

func validateJobFailed(ctx context.Context, t *testing.T, clientSet clientset.Interface, jobObj *batchv1.Job) {
	t.Helper()
	validateJobCondition(ctx, t, clientSet, jobObj, batchv1.JobFailed)
}

func validateJobSucceeded(ctx context.Context, t testing.TB, clientSet clientset.Interface, jobObj *batchv1.Job) {
	t.Helper()
	validateJobCondition(ctx, t, clientSet, jobObj, batchv1.JobComplete)
}

func validateJobCondition(ctx context.Context, t testing.TB, clientSet clientset.Interface, jobObj *batchv1.Job, cond batchv1.JobConditionType) {
	t.Helper()
	if err := wait.PollImmediate(waitInterval, wait.ForeverTestTimeout, func() (bool, error) {
		j, err := clientSet.BatchV1().Jobs(jobObj.Namespace).Get(ctx, jobObj.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("Failed to obtain updated Job: %v", err)
		}
		return getJobConditionStatus(ctx, j, cond) == v1.ConditionTrue, nil
	}); err != nil {
		t.Errorf("Waiting for Job to have condition %s: %v", cond, err)
	}
}

func setJobPodsPhase(ctx context.Context, clientSet clientset.Interface, jobObj *batchv1.Job, phase v1.PodPhase, cnt int) (error, int) {
	op := func(p *v1.Pod) bool {
		p.Status.Phase = phase
		if phase == v1.PodFailed || phase == v1.PodSucceeded {
			p.Status.ContainerStatuses = []v1.ContainerStatus{
				{
					State: v1.ContainerState{
						Terminated: &v1.ContainerStateTerminated{
							FinishedAt: metav1.Now(),
						},
					},
				},
			}
		}
		return true
	}
	return updateJobPodsStatus(ctx, clientSet, jobObj, op, cnt)
}

func setJobPodsReady(ctx context.Context, clientSet clientset.Interface, jobObj *batchv1.Job, cnt int) (error, int) {
	op := func(p *v1.Pod) bool {
		if podutil.IsPodReady(p) {
			return false
		}
		p.Status.Conditions = append(p.Status.Conditions, v1.PodCondition{
			Type:   v1.PodReady,
			Status: v1.ConditionTrue,
		})
		return true
	}
	return updateJobPodsStatus(ctx, clientSet, jobObj, op, cnt)
}

func updateJobPodsStatus(ctx context.Context, clientSet clientset.Interface, jobObj *batchv1.Job, op func(*v1.Pod) bool, cnt int) (error, int) {
	pods, err := clientSet.CoreV1().Pods(jobObj.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing Job Pods: %w", err), 0
	}
	updates := make([]v1.Pod, 0, cnt)
	for _, pod := range pods.Items {
		if len(updates) == cnt {
			break
		}
		if p := pod.Status.Phase; metav1.IsControlledBy(&pod, jobObj) && p != v1.PodFailed && p != v1.PodSucceeded {
			if !op(&pod) {
				continue
			}
			updates = append(updates, pod)
		}
	}
	successful, err := updatePodStatuses(ctx, clientSet, updates)
	if successful != cnt {
		return fmt.Errorf("couldn't set phase on %d Job pods", cnt-successful), successful
	}
	return err, successful
}

func updatePodStatuses(ctx context.Context, clientSet clientset.Interface, updates []v1.Pod) (int, error) {
	wg := sync.WaitGroup{}
	wg.Add(len(updates))
	errCh := make(chan error, len(updates))
	var updated int32

	for _, pod := range updates {
		pod := pod
		go func() {
			_, err := clientSet.CoreV1().Pods(pod.Namespace).UpdateStatus(ctx, &pod, metav1.UpdateOptions{})
			if err != nil {
				errCh <- err
			} else {
				atomic.AddInt32(&updated, 1)
			}
			wg.Done()
		}()
	}
	wg.Wait()

	select {
	case err := <-errCh:
		return int(updated), fmt.Errorf("updating Pod status: %w", err)
	default:
	}
	return int(updated), nil
}

func updatePod(t *testing.T, clientSet clientset.Interface, pods []v1.Pod, updateFunc func(*v1.Pod)) {
	for _, val := range pods {
		if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			newPod, err := clientSet.CoreV1().Pods(val.Namespace).Get(context.TODO(), val.Name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			updateFunc(newPod)
			_, err = clientSet.CoreV1().Pods(val.Namespace).Update(context.TODO(), newPod, metav1.UpdateOptions{})
			return err
		}); err != nil {
			t.Fatalf("Failed to update pod %s: %v", val.Name, err)
		}
	}
}

func setJobPhaseForIndex(ctx context.Context, clientSet clientset.Interface, jobObj *batchv1.Job, phase v1.PodPhase, ix int) error {
	pods, err := clientSet.CoreV1().Pods(jobObj.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing Job Pods: %w", err)
	}
	for _, pod := range pods.Items {
		if p := pod.Status.Phase; !metav1.IsControlledBy(&pod, jobObj) || p == v1.PodFailed || p == v1.PodSucceeded {
			continue
		}
		if pix, err := getCompletionIndex(&pod); err == nil && pix == ix {
			pod.Status.Phase = phase
			if phase == v1.PodFailed || phase == v1.PodSucceeded {
				pod.Status.ContainerStatuses = []v1.ContainerStatus{
					{
						State: v1.ContainerState{
							Terminated: &v1.ContainerStateTerminated{
								FinishedAt: metav1.Now(),
							},
						},
					},
				}
			}
			_, err := clientSet.CoreV1().Pods(pod.Namespace).UpdateStatus(ctx, &pod, metav1.UpdateOptions{})
			if err != nil {
				return fmt.Errorf("updating pod %s status: %w", pod.Name, err)
			}
			return nil
		}
	}
	return errors.New("no pod matching index found")
}

func getActivePodForIndex(ctx context.Context, clientSet clientset.Interface, jobObj *batchv1.Job, ix int) (*v1.Pod, error) {
	return getJobPodForIndex(ctx, clientSet, jobObj, ix, func(p *v1.Pod) bool {
		return !podutil.IsPodTerminal(p)
	})
}

func getJobPodForIndex(ctx context.Context, clientSet clientset.Interface, jobObj *batchv1.Job, ix int, filter func(*v1.Pod) bool) (*v1.Pod, error) {
	pods, err := getJobPodsForIndex(ctx, clientSet, jobObj, ix, filter)
	if err != nil {
		return nil, err
	}
	if len(pods) == 0 {
		return nil, fmt.Errorf("Pod not found for index: %v", ix)
	}
	return pods[0], nil
}

func getJobPodsForIndex(ctx context.Context, clientSet clientset.Interface, jobObj *batchv1.Job, ix int, filter func(*v1.Pod) bool) ([]*v1.Pod, error) {
	pods, err := clientSet.CoreV1().Pods(jobObj.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing Job Pods: %w", err)
	}
	var result []*v1.Pod
	for _, pod := range pods.Items {
		pod := pod
		if !metav1.IsControlledBy(&pod, jobObj) {
			continue
		}
		if !filter(&pod) {
			continue
		}
		if pix, err := getCompletionIndex(&pod); err == nil && pix == ix {
			result = append(result, &pod)
		}
	}
	return result, nil
}

func getCompletionIndex(p *v1.Pod) (int, error) {
	if p.Annotations == nil {
		return 0, errors.New("no annotations found")
	}
	v, ok := p.Annotations[batchv1.JobCompletionIndexAnnotation]
	if !ok {
		return 0, fmt.Errorf("annotation %s not found", batchv1.JobCompletionIndexAnnotation)
	}
	return strconv.Atoi(v)
}

func createJobWithDefaults(ctx context.Context, clientSet clientset.Interface, ns string, jobObj *batchv1.Job) (*batchv1.Job, error) {
	if jobObj.Name == "" {
		jobObj.Name = "test-job"
	}
	if len(jobObj.Spec.Template.Spec.Containers) == 0 {
		jobObj.Spec.Template.Spec.Containers = []v1.Container{
			{Name: "foo", Image: "bar"},
		}
	}
	if jobObj.Spec.Template.Spec.RestartPolicy == "" {
		jobObj.Spec.Template.Spec.RestartPolicy = v1.RestartPolicyNever
	}
	return clientSet.BatchV1().Jobs(ns).Create(ctx, jobObj, metav1.CreateOptions{})
}

func setup(t testing.TB, nsBaseName string) (framework.TearDownFunc, *restclient.Config, clientset.Interface, *v1.Namespace) {
	// Disable ServiceAccount admission plugin as we don't have serviceaccount controller running.
	server := kubeapiservertesting.StartTestServerOrDie(t, nil, []string{"--disable-admission-plugins=ServiceAccount"}, framework.SharedEtcd())

	config := restclient.CopyConfig(server.ClientConfig)
	config.QPS = 200
	config.Burst = 200
	config.Timeout = 0
	clientSet, err := clientset.NewForConfig(config)
	if err != nil {
		t.Fatalf("Error creating clientset: %v", err)
	}

	ns := framework.CreateNamespaceOrDie(clientSet, nsBaseName, t)
	closeFn := func() {
		framework.DeleteNamespaceOrDie(clientSet, ns, t)
		server.TearDownFn()
	}
	return closeFn, config, clientSet, ns
}

func startJobControllerAndWaitForCaches(restConfig *restclient.Config) (context.Context, context.CancelFunc) {
	informerSet := informers.NewSharedInformerFactory(clientset.NewForConfigOrDie(restclient.AddUserAgent(restConfig, "job-informers")), 0)
	jc, ctx, cancel := createJobControllerWithSharedInformers(restConfig, informerSet)
	informerSet.Start(ctx.Done())
	go jc.Run(ctx, 1)

	// since this method starts the controller in a separate goroutine
	// and the tests don't check /readyz there is no way
	// the tests can tell it is safe to call the server and requests won't be rejected
	// thus we wait until caches have synced
	informerSet.WaitForCacheSync(ctx.Done())
	return ctx, cancel
}

func resetMetrics() {
	metrics.TerminatedPodsTrackingFinalizerTotal.Reset()
	metrics.JobFinishedNum.Reset()
	metrics.JobPodsFinished.Reset()
	metrics.PodFailuresHandledByFailurePolicy.Reset()
}

func createJobControllerWithSharedInformers(restConfig *restclient.Config, informerSet informers.SharedInformerFactory) (*jobcontroller.Controller, context.Context, context.CancelFunc) {
	clientSet := clientset.NewForConfigOrDie(restclient.AddUserAgent(restConfig, "job-controller"))
	ctx, cancel := context.WithCancel(context.Background())
	jc := jobcontroller.NewController(ctx, informerSet.Core().V1().Pods(), informerSet.Batch().V1().Jobs(), clientSet)
	return jc, ctx, cancel
}

func hasJobTrackingFinalizer(obj metav1.Object) bool {
	for _, fin := range obj.GetFinalizers() {
		if fin == batchv1.JobTrackingFinalizer {
			return true
		}
	}
	return false
}

func setDuringTest(val *int, newVal int) func() {
	origVal := *val
	*val = newVal
	return func() {
		*val = origVal
	}
}

func setDurationDuringTest(val *time.Duration, newVal time.Duration) func() {
	origVal := *val
	*val = newVal
	return func() {
		*val = origVal
	}
}

func updateJob(ctx context.Context, jobClient typedv1.JobInterface, jobName string, updateFunc func(*batchv1.Job)) (*batchv1.Job, error) {
	var job *batchv1.Job
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		newJob, err := jobClient.Get(ctx, jobName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		updateFunc(newJob)
		job, err = jobClient.Update(ctx, newJob, metav1.UpdateOptions{})
		return err
	})
	return job, err
}
