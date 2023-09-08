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

package policy

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestSysctls(t *testing.T) {
	tests := []struct {
		name         string
		pod          *corev1.Pod
		allowed      bool
		expectReason string
		expectDetail string
	}{
		{
			name: "forbidden sysctls",
			pod: &corev1.Pod{Spec: corev1.PodSpec{
				SecurityContext: &corev1.PodSecurityContext{
					Sysctls: []corev1.Sysctl{{Name: "a"}, {Name: "b"}},
				},
			}},
			allowed:      false,
			expectReason: `forbidden sysctls`,
			expectDetail: `a, b`,
		},
		{
			name: "new supported sysctls not supported",
			pod: &corev1.Pod{Spec: corev1.PodSpec{
				SecurityContext: &corev1.PodSecurityContext{
					Sysctls: []corev1.Sysctl{{Name: "net.ipv4.ip_local_reserved_ports", Value: "1024-4999"}},
				},
			}},
			allowed:      false,
			expectReason: `forbidden sysctls`,
			expectDetail: `net.ipv4.ip_local_reserved_ports`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := sysctls_1_0(&tc.pod.ObjectMeta, &tc.pod.Spec)
			if !tc.allowed {
				if result.Allowed {
					t.Fatal("expected disallowed")
				}
				if e, a := tc.expectReason, result.ForbiddenReason; e != a {
					t.Errorf("expected\n%s\ngot\n%s", e, a)
				}
				if e, a := tc.expectDetail, result.ForbiddenDetail; e != a {
					t.Errorf("expected\n%s\ngot\n%s", e, a)
				}
			} else {
				if !result.Allowed {
					t.Fatal("expected allowed")
				}
			}
		})
	}
}

func TestSysctls_1_27(t *testing.T) {
	tests := []struct {
		name         string
		pod          *corev1.Pod
		allowed      bool
		expectReason string
		expectDetail string
	}{
		{
			name: "forbidden sysctls",
			pod: &corev1.Pod{Spec: corev1.PodSpec{
				SecurityContext: &corev1.PodSecurityContext{
					Sysctls: []corev1.Sysctl{{Name: "a"}, {Name: "b"}},
				},
			}},
			allowed:      false,
			expectReason: `forbidden sysctls`,
			expectDetail: `a, b`,
		},
		{
			name: "new supported sysctls",
			pod: &corev1.Pod{Spec: corev1.PodSpec{
				SecurityContext: &corev1.PodSecurityContext{
					Sysctls: []corev1.Sysctl{{Name: "net.ipv4.ip_local_reserved_ports", Value: "1024-4999"}},
				},
			}},
			allowed: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := sysctls_1_27(&tc.pod.ObjectMeta, &tc.pod.Spec)
			if !tc.allowed {
				if result.Allowed {
					t.Fatal("expected disallowed")
				}
				if e, a := tc.expectReason, result.ForbiddenReason; e != a {
					t.Errorf("expected\n%s\ngot\n%s", e, a)
				}
				if e, a := tc.expectDetail, result.ForbiddenDetail; e != a {
					t.Errorf("expected\n%s\ngot\n%s", e, a)
				}
			} else {
				if !result.Allowed {
					t.Fatal("expected allowed")
				}
			}
		})
	}
}
