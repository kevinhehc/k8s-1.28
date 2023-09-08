/*
Copyright 2023 The Kubernetes Authors.

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

package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lithammer/dedent"
)

func TestLoadResetConfigurationFromFile(t *testing.T) {
	// Create temp folder for the test case
	tmpdir, err := os.MkdirTemp("", "")
	if err != nil {
		t.Fatalf("Couldn't create tmpdir: %v", err)
	}
	defer os.RemoveAll(tmpdir)

	var tests = []struct {
		name         string
		fileContents string
		expectErr    bool
	}{
		{
			name:      "empty file causes error",
			expectErr: true,
		},
		{
			name: "Invalid v1beta4 causes error",
			fileContents: dedent.Dedent(`
				apiVersion: kubeadm.k8s.io/unknownVersion
				kind: ResetConfiguration
				criSocket: unix:///var/run/containerd/containerd.sock
			`),
			expectErr: true,
		},
		{
			name: "valid v1beta4 is loaded",
			fileContents: dedent.Dedent(`
				apiVersion: kubeadm.k8s.io/v1beta4
				kind: ResetConfiguration
				force: true
				cleanupTmpDir: true
				criSocket: unix:///var/run/containerd/containerd.sock
				certificatesDir: /etc/kubernetes/pki
				ignorePreflightErrors:
				- a
				- b
			`),
		},
	}

	for _, rt := range tests {
		t.Run(rt.name, func(t2 *testing.T) {
			cfgPath := filepath.Join(tmpdir, rt.name)
			err := os.WriteFile(cfgPath, []byte(rt.fileContents), 0644)
			if err != nil {
				t.Errorf("Couldn't create file: %v", err)
				return
			}

			obj, err := LoadResetConfigurationFromFile(cfgPath, true)
			if rt.expectErr {
				if err == nil {
					t.Error("Unexpected success")
				}
			} else {
				if err != nil {
					t.Errorf("Error reading file: %v", err)
					return
				}

				if obj == nil {
					t.Error("Unexpected nil return value")
				}
			}
		})
	}
}
