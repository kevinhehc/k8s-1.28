/*
Copyright 2022 The Kubernetes Authors.

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

package schedulinggates

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"

	v1 "k8s.io/api/core/v1"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/feature"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
)

func TestPreEnqueue(t *testing.T) {
	tests := []struct {
		name                         string
		pod                          *v1.Pod
		enablePodSchedulingReadiness bool
		want                         *framework.Status
	}{
		{
			name:                         "pod does not carry scheduling gates, feature disabled",
			pod:                          st.MakePod().Name("p").Obj(),
			enablePodSchedulingReadiness: false,
			want:                         nil,
		},
		{
			name:                         "pod does not carry scheduling gates, feature enabled",
			pod:                          st.MakePod().Name("p").Obj(),
			enablePodSchedulingReadiness: true,
			want:                         nil,
		},
		{
			name:                         "pod carries scheduling gates, feature disabled",
			pod:                          st.MakePod().Name("p").SchedulingGates([]string{"foo", "bar"}).Obj(),
			enablePodSchedulingReadiness: false,
			want:                         nil,
		},
		{
			name:                         "pod carries scheduling gates, feature enabled",
			pod:                          st.MakePod().Name("p").SchedulingGates([]string{"foo", "bar"}).Obj(),
			enablePodSchedulingReadiness: true,
			want:                         framework.NewStatus(framework.UnschedulableAndUnresolvable, "waiting for scheduling gates: [foo bar]"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := New(nil, nil, feature.Features{EnablePodSchedulingReadiness: tt.enablePodSchedulingReadiness})
			if err != nil {
				t.Fatalf("Creating plugin: %v", err)
			}

			got := p.(framework.PreEnqueuePlugin).PreEnqueue(context.Background(), tt.pod)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("unexpected status (-want, +got):\n%s", diff)
			}
		})
	}
}
