/*
Copyright 2017 The Kubernetes Authors.

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
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation/field"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	api "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/utils/pointer"
)

func TestVisitContainers(t *testing.T) {
	setAllFeatureEnabledContainersDuringTest := ContainerType(0)
	testCases := []struct {
		desc           string
		spec           *api.PodSpec
		wantContainers []string
		mask           ContainerType
	}{
		{
			desc:           "empty podspec",
			spec:           &api.PodSpec{},
			wantContainers: []string{},
			mask:           AllContainers,
		},
		{
			desc: "regular containers",
			spec: &api.PodSpec{
				Containers: []api.Container{
					{Name: "c1"},
					{Name: "c2"},
				},
				InitContainers: []api.Container{
					{Name: "i1"},
					{Name: "i2"},
				},
				EphemeralContainers: []api.EphemeralContainer{
					{EphemeralContainerCommon: api.EphemeralContainerCommon{Name: "e1"}},
					{EphemeralContainerCommon: api.EphemeralContainerCommon{Name: "e2"}},
				},
			},
			wantContainers: []string{"c1", "c2"},
			mask:           Containers,
		},
		{
			desc: "init containers",
			spec: &api.PodSpec{
				Containers: []api.Container{
					{Name: "c1"},
					{Name: "c2"},
				},
				InitContainers: []api.Container{
					{Name: "i1"},
					{Name: "i2"},
				},
				EphemeralContainers: []api.EphemeralContainer{
					{EphemeralContainerCommon: api.EphemeralContainerCommon{Name: "e1"}},
					{EphemeralContainerCommon: api.EphemeralContainerCommon{Name: "e2"}},
				},
			},
			wantContainers: []string{"i1", "i2"},
			mask:           InitContainers,
		},
		{
			desc: "ephemeral containers",
			spec: &api.PodSpec{
				Containers: []api.Container{
					{Name: "c1"},
					{Name: "c2"},
				},
				InitContainers: []api.Container{
					{Name: "i1"},
					{Name: "i2"},
				},
				EphemeralContainers: []api.EphemeralContainer{
					{EphemeralContainerCommon: api.EphemeralContainerCommon{Name: "e1"}},
					{EphemeralContainerCommon: api.EphemeralContainerCommon{Name: "e2"}},
				},
			},
			wantContainers: []string{"e1", "e2"},
			mask:           EphemeralContainers,
		},
		{
			desc: "all container types",
			spec: &api.PodSpec{
				Containers: []api.Container{
					{Name: "c1"},
					{Name: "c2"},
				},
				InitContainers: []api.Container{
					{Name: "i1"},
					{Name: "i2"},
				},
				EphemeralContainers: []api.EphemeralContainer{
					{EphemeralContainerCommon: api.EphemeralContainerCommon{Name: "e1"}},
					{EphemeralContainerCommon: api.EphemeralContainerCommon{Name: "e2"}},
				},
			},
			wantContainers: []string{"i1", "i2", "c1", "c2", "e1", "e2"},
			mask:           AllContainers,
		},
		{
			desc: "all feature enabled container types with ephemeral containers enabled",
			spec: &api.PodSpec{
				Containers: []api.Container{
					{Name: "c1"},
					{Name: "c2", SecurityContext: &api.SecurityContext{}},
				},
				InitContainers: []api.Container{
					{Name: "i1"},
					{Name: "i2", SecurityContext: &api.SecurityContext{}},
				},
				EphemeralContainers: []api.EphemeralContainer{
					{EphemeralContainerCommon: api.EphemeralContainerCommon{Name: "e1"}},
					{EphemeralContainerCommon: api.EphemeralContainerCommon{Name: "e2"}},
				},
			},
			wantContainers: []string{"i1", "i2", "c1", "c2", "e1", "e2"},
			mask:           setAllFeatureEnabledContainersDuringTest,
		},
		{
			desc: "dropping fields",
			spec: &api.PodSpec{
				Containers: []api.Container{
					{Name: "c1"},
					{Name: "c2", SecurityContext: &api.SecurityContext{}},
				},
				InitContainers: []api.Container{
					{Name: "i1"},
					{Name: "i2", SecurityContext: &api.SecurityContext{}},
				},
				EphemeralContainers: []api.EphemeralContainer{
					{EphemeralContainerCommon: api.EphemeralContainerCommon{Name: "e1"}},
					{EphemeralContainerCommon: api.EphemeralContainerCommon{Name: "e2", SecurityContext: &api.SecurityContext{}}},
				},
			},
			wantContainers: []string{"i1", "i2", "c1", "c2", "e1", "e2"},
			mask:           AllContainers,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			if tc.mask == setAllFeatureEnabledContainersDuringTest {
				tc.mask = AllFeatureEnabledContainers()
			}

			gotContainers := []string{}
			VisitContainers(tc.spec, tc.mask, func(c *api.Container, containerType ContainerType) bool {
				gotContainers = append(gotContainers, c.Name)
				if c.SecurityContext != nil {
					c.SecurityContext = nil
				}
				return true
			})
			if !cmp.Equal(gotContainers, tc.wantContainers) {
				t.Errorf("VisitContainers() = %+v, want %+v", gotContainers, tc.wantContainers)
			}
			for _, c := range tc.spec.Containers {
				if c.SecurityContext != nil {
					t.Errorf("VisitContainers() did not drop SecurityContext for container %q", c.Name)
				}
			}
			for _, c := range tc.spec.InitContainers {
				if c.SecurityContext != nil {
					t.Errorf("VisitContainers() did not drop SecurityContext for init container %q", c.Name)
				}
			}
			for _, c := range tc.spec.EphemeralContainers {
				if c.SecurityContext != nil {
					t.Errorf("VisitContainers() did not drop SecurityContext for ephemeral container %q", c.Name)
				}
			}
		})
	}
}

func TestPodSecrets(t *testing.T) {
	// Stub containing all possible secret references in a pod.
	// The names of the referenced secrets match struct paths detected by reflection.
	pod := &api.Pod{
		Spec: api.PodSpec{
			Containers: []api.Container{{
				EnvFrom: []api.EnvFromSource{{
					SecretRef: &api.SecretEnvSource{
						LocalObjectReference: api.LocalObjectReference{
							Name: "Spec.Containers[*].EnvFrom[*].SecretRef"}}}},
				Env: []api.EnvVar{{
					ValueFrom: &api.EnvVarSource{
						SecretKeyRef: &api.SecretKeySelector{
							LocalObjectReference: api.LocalObjectReference{
								Name: "Spec.Containers[*].Env[*].ValueFrom.SecretKeyRef"}}}}}}},
			ImagePullSecrets: []api.LocalObjectReference{{
				Name: "Spec.ImagePullSecrets"}},
			InitContainers: []api.Container{{
				EnvFrom: []api.EnvFromSource{{
					SecretRef: &api.SecretEnvSource{
						LocalObjectReference: api.LocalObjectReference{
							Name: "Spec.InitContainers[*].EnvFrom[*].SecretRef"}}}},
				Env: []api.EnvVar{{
					ValueFrom: &api.EnvVarSource{
						SecretKeyRef: &api.SecretKeySelector{
							LocalObjectReference: api.LocalObjectReference{
								Name: "Spec.InitContainers[*].Env[*].ValueFrom.SecretKeyRef"}}}}}}},
			Volumes: []api.Volume{{
				VolumeSource: api.VolumeSource{
					AzureFile: &api.AzureFileVolumeSource{
						SecretName: "Spec.Volumes[*].VolumeSource.AzureFile.SecretName"}}}, {
				VolumeSource: api.VolumeSource{
					CephFS: &api.CephFSVolumeSource{
						SecretRef: &api.LocalObjectReference{
							Name: "Spec.Volumes[*].VolumeSource.CephFS.SecretRef"}}}}, {
				VolumeSource: api.VolumeSource{
					Cinder: &api.CinderVolumeSource{
						SecretRef: &api.LocalObjectReference{
							Name: "Spec.Volumes[*].VolumeSource.Cinder.SecretRef"}}}}, {
				VolumeSource: api.VolumeSource{
					FlexVolume: &api.FlexVolumeSource{
						SecretRef: &api.LocalObjectReference{
							Name: "Spec.Volumes[*].VolumeSource.FlexVolume.SecretRef"}}}}, {
				VolumeSource: api.VolumeSource{
					Projected: &api.ProjectedVolumeSource{
						Sources: []api.VolumeProjection{{
							Secret: &api.SecretProjection{
								LocalObjectReference: api.LocalObjectReference{
									Name: "Spec.Volumes[*].VolumeSource.Projected.Sources[*].Secret"}}}}}}}, {
				VolumeSource: api.VolumeSource{
					RBD: &api.RBDVolumeSource{
						SecretRef: &api.LocalObjectReference{
							Name: "Spec.Volumes[*].VolumeSource.RBD.SecretRef"}}}}, {
				VolumeSource: api.VolumeSource{
					Secret: &api.SecretVolumeSource{
						SecretName: "Spec.Volumes[*].VolumeSource.Secret.SecretName"}}}, {
				VolumeSource: api.VolumeSource{
					Secret: &api.SecretVolumeSource{
						SecretName: "Spec.Volumes[*].VolumeSource.Secret"}}}, {
				VolumeSource: api.VolumeSource{
					ScaleIO: &api.ScaleIOVolumeSource{
						SecretRef: &api.LocalObjectReference{
							Name: "Spec.Volumes[*].VolumeSource.ScaleIO.SecretRef"}}}}, {
				VolumeSource: api.VolumeSource{
					ISCSI: &api.ISCSIVolumeSource{
						SecretRef: &api.LocalObjectReference{
							Name: "Spec.Volumes[*].VolumeSource.ISCSI.SecretRef"}}}}, {
				VolumeSource: api.VolumeSource{
					StorageOS: &api.StorageOSVolumeSource{
						SecretRef: &api.LocalObjectReference{
							Name: "Spec.Volumes[*].VolumeSource.StorageOS.SecretRef"}}}}, {
				VolumeSource: api.VolumeSource{
					CSI: &api.CSIVolumeSource{
						NodePublishSecretRef: &api.LocalObjectReference{
							Name: "Spec.Volumes[*].VolumeSource.CSI.NodePublishSecretRef"}}}}},
			EphemeralContainers: []api.EphemeralContainer{{
				EphemeralContainerCommon: api.EphemeralContainerCommon{
					EnvFrom: []api.EnvFromSource{{
						SecretRef: &api.SecretEnvSource{
							LocalObjectReference: api.LocalObjectReference{
								Name: "Spec.EphemeralContainers[*].EphemeralContainerCommon.EnvFrom[*].SecretRef"}}}},
					Env: []api.EnvVar{{
						ValueFrom: &api.EnvVarSource{
							SecretKeyRef: &api.SecretKeySelector{
								LocalObjectReference: api.LocalObjectReference{
									Name: "Spec.EphemeralContainers[*].EphemeralContainerCommon.Env[*].ValueFrom.SecretKeyRef"}}}}}}}},
		},
	}
	extractedNames := sets.NewString()
	VisitPodSecretNames(pod, func(name string) bool {
		extractedNames.Insert(name)
		return true
	}, AllContainers)

	// excludedSecretPaths holds struct paths to fields with "secret" in the name that are not actually references to secret API objects
	excludedSecretPaths := sets.NewString(
		"Spec.Volumes[*].VolumeSource.CephFS.SecretFile",
	)
	// expectedSecretPaths holds struct paths to fields with "secret" in the name that are references to secret API objects.
	// every path here should be represented as an example in the Pod stub above, with the secret name set to the path.
	expectedSecretPaths := sets.NewString(
		"Spec.Containers[*].EnvFrom[*].SecretRef",
		"Spec.Containers[*].Env[*].ValueFrom.SecretKeyRef",
		"Spec.EphemeralContainers[*].EphemeralContainerCommon.EnvFrom[*].SecretRef",
		"Spec.EphemeralContainers[*].EphemeralContainerCommon.Env[*].ValueFrom.SecretKeyRef",
		"Spec.ImagePullSecrets",
		"Spec.InitContainers[*].EnvFrom[*].SecretRef",
		"Spec.InitContainers[*].Env[*].ValueFrom.SecretKeyRef",
		"Spec.Volumes[*].VolumeSource.AzureFile.SecretName",
		"Spec.Volumes[*].VolumeSource.CephFS.SecretRef",
		"Spec.Volumes[*].VolumeSource.Cinder.SecretRef",
		"Spec.Volumes[*].VolumeSource.FlexVolume.SecretRef",
		"Spec.Volumes[*].VolumeSource.Projected.Sources[*].Secret",
		"Spec.Volumes[*].VolumeSource.RBD.SecretRef",
		"Spec.Volumes[*].VolumeSource.Secret",
		"Spec.Volumes[*].VolumeSource.Secret.SecretName",
		"Spec.Volumes[*].VolumeSource.ScaleIO.SecretRef",
		"Spec.Volumes[*].VolumeSource.ISCSI.SecretRef",
		"Spec.Volumes[*].VolumeSource.StorageOS.SecretRef",
		"Spec.Volumes[*].VolumeSource.CSI.NodePublishSecretRef",
	)
	secretPaths := collectResourcePaths(t, "secret", nil, "", reflect.TypeOf(&api.Pod{}))
	secretPaths = secretPaths.Difference(excludedSecretPaths)
	if missingPaths := expectedSecretPaths.Difference(secretPaths); len(missingPaths) > 0 {
		t.Logf("Missing expected secret paths:\n%s", strings.Join(missingPaths.List(), "\n"))
		t.Error("Missing expected secret paths. Verify VisitPodSecretNames() is correctly finding the missing paths, then correct expectedSecretPaths")
	}
	if extraPaths := secretPaths.Difference(expectedSecretPaths); len(extraPaths) > 0 {
		t.Logf("Extra secret paths:\n%s", strings.Join(extraPaths.List(), "\n"))
		t.Error("Extra fields with 'secret' in the name found. Verify VisitPodSecretNames() is including these fields if appropriate, then correct expectedSecretPaths")
	}

	if missingNames := expectedSecretPaths.Difference(extractedNames); len(missingNames) > 0 {
		t.Logf("Missing expected secret names:\n%s", strings.Join(missingNames.List(), "\n"))
		t.Error("Missing expected secret names. Verify the pod stub above includes these references, then verify VisitPodSecretNames() is correctly finding the missing names")
	}
	if extraNames := extractedNames.Difference(expectedSecretPaths); len(extraNames) > 0 {
		t.Logf("Extra secret names:\n%s", strings.Join(extraNames.List(), "\n"))
		t.Error("Extra secret names extracted. Verify VisitPodSecretNames() is correctly extracting secret names")
	}

	// emptyPod is a stub containing empty object names
	emptyPod := &api.Pod{
		Spec: api.PodSpec{
			Containers: []api.Container{{
				EnvFrom: []api.EnvFromSource{{
					SecretRef: &api.SecretEnvSource{
						LocalObjectReference: api.LocalObjectReference{
							Name: ""}}}}}},
		},
	}
	VisitPodSecretNames(emptyPod, func(name string) bool {
		t.Fatalf("expected no empty names collected, got %q", name)
		return false
	}, AllContainers)
}

// collectResourcePaths traverses the object, computing all the struct paths that lead to fields with resourcename in the name.
func collectResourcePaths(t *testing.T, resourcename string, path *field.Path, name string, tp reflect.Type) sets.String {
	resourcename = strings.ToLower(resourcename)
	resourcePaths := sets.NewString()

	if tp.Kind() == reflect.Pointer {
		resourcePaths.Insert(collectResourcePaths(t, resourcename, path, name, tp.Elem()).List()...)
		return resourcePaths
	}

	if strings.Contains(strings.ToLower(name), resourcename) {
		resourcePaths.Insert(path.String())
	}

	switch tp.Kind() {
	case reflect.Pointer:
		resourcePaths.Insert(collectResourcePaths(t, resourcename, path, name, tp.Elem()).List()...)
	case reflect.Struct:
		// ObjectMeta is generic and therefore should never have a field with a specific resource's name;
		// it contains cycles so it's easiest to just skip it.
		if name == "ObjectMeta" {
			break
		}
		for i := 0; i < tp.NumField(); i++ {
			field := tp.Field(i)
			resourcePaths.Insert(collectResourcePaths(t, resourcename, path.Child(field.Name), field.Name, field.Type).List()...)
		}
	case reflect.Interface:
		t.Errorf("cannot find %s fields in interface{} field %s", resourcename, path.String())
	case reflect.Map:
		resourcePaths.Insert(collectResourcePaths(t, resourcename, path.Key("*"), "", tp.Elem()).List()...)
	case reflect.Slice:
		resourcePaths.Insert(collectResourcePaths(t, resourcename, path.Key("*"), "", tp.Elem()).List()...)
	default:
		// all primitive types
	}

	return resourcePaths
}

func TestPodConfigmaps(t *testing.T) {
	// Stub containing all possible ConfigMap references in a pod.
	// The names of the referenced ConfigMaps match struct paths detected by reflection.
	pod := &api.Pod{
		Spec: api.PodSpec{
			Containers: []api.Container{{
				EnvFrom: []api.EnvFromSource{{
					ConfigMapRef: &api.ConfigMapEnvSource{
						LocalObjectReference: api.LocalObjectReference{
							Name: "Spec.Containers[*].EnvFrom[*].ConfigMapRef"}}}},
				Env: []api.EnvVar{{
					ValueFrom: &api.EnvVarSource{
						ConfigMapKeyRef: &api.ConfigMapKeySelector{
							LocalObjectReference: api.LocalObjectReference{
								Name: "Spec.Containers[*].Env[*].ValueFrom.ConfigMapKeyRef"}}}}}}},
			EphemeralContainers: []api.EphemeralContainer{{
				EphemeralContainerCommon: api.EphemeralContainerCommon{
					EnvFrom: []api.EnvFromSource{{
						ConfigMapRef: &api.ConfigMapEnvSource{
							LocalObjectReference: api.LocalObjectReference{
								Name: "Spec.EphemeralContainers[*].EphemeralContainerCommon.EnvFrom[*].ConfigMapRef"}}}},
					Env: []api.EnvVar{{
						ValueFrom: &api.EnvVarSource{
							ConfigMapKeyRef: &api.ConfigMapKeySelector{
								LocalObjectReference: api.LocalObjectReference{
									Name: "Spec.EphemeralContainers[*].EphemeralContainerCommon.Env[*].ValueFrom.ConfigMapKeyRef"}}}}}}}},
			InitContainers: []api.Container{{
				EnvFrom: []api.EnvFromSource{{
					ConfigMapRef: &api.ConfigMapEnvSource{
						LocalObjectReference: api.LocalObjectReference{
							Name: "Spec.InitContainers[*].EnvFrom[*].ConfigMapRef"}}}},
				Env: []api.EnvVar{{
					ValueFrom: &api.EnvVarSource{
						ConfigMapKeyRef: &api.ConfigMapKeySelector{
							LocalObjectReference: api.LocalObjectReference{
								Name: "Spec.InitContainers[*].Env[*].ValueFrom.ConfigMapKeyRef"}}}}}}},
			Volumes: []api.Volume{{
				VolumeSource: api.VolumeSource{
					Projected: &api.ProjectedVolumeSource{
						Sources: []api.VolumeProjection{{
							ConfigMap: &api.ConfigMapProjection{
								LocalObjectReference: api.LocalObjectReference{
									Name: "Spec.Volumes[*].VolumeSource.Projected.Sources[*].ConfigMap"}}}}}}}, {
				VolumeSource: api.VolumeSource{
					ConfigMap: &api.ConfigMapVolumeSource{
						LocalObjectReference: api.LocalObjectReference{
							Name: "Spec.Volumes[*].VolumeSource.ConfigMap"}}}}},
		},
	}
	extractedNames := sets.NewString()
	VisitPodConfigmapNames(pod, func(name string) bool {
		extractedNames.Insert(name)
		return true
	}, AllContainers)

	// expectedPaths holds struct paths to fields with "ConfigMap" in the name that are references to ConfigMap API objects.
	// every path here should be represented as an example in the Pod stub above, with the ConfigMap name set to the path.
	expectedPaths := sets.NewString(
		"Spec.Containers[*].EnvFrom[*].ConfigMapRef",
		"Spec.Containers[*].Env[*].ValueFrom.ConfigMapKeyRef",
		"Spec.EphemeralContainers[*].EphemeralContainerCommon.EnvFrom[*].ConfigMapRef",
		"Spec.EphemeralContainers[*].EphemeralContainerCommon.Env[*].ValueFrom.ConfigMapKeyRef",
		"Spec.InitContainers[*].EnvFrom[*].ConfigMapRef",
		"Spec.InitContainers[*].Env[*].ValueFrom.ConfigMapKeyRef",
		"Spec.Volumes[*].VolumeSource.Projected.Sources[*].ConfigMap",
		"Spec.Volumes[*].VolumeSource.ConfigMap",
	)
	collectPaths := collectResourcePaths(t, "ConfigMap", nil, "", reflect.TypeOf(&api.Pod{}))
	if missingPaths := expectedPaths.Difference(collectPaths); len(missingPaths) > 0 {
		t.Logf("Missing expected paths:\n%s", strings.Join(missingPaths.List(), "\n"))
		t.Error("Missing expected paths. Verify VisitPodConfigmapNames() is correctly finding the missing paths, then correct expectedPaths")
	}
	if extraPaths := collectPaths.Difference(expectedPaths); len(extraPaths) > 0 {
		t.Logf("Extra paths:\n%s", strings.Join(extraPaths.List(), "\n"))
		t.Error("Extra fields with resource in the name found. Verify VisitPodConfigmapNames() is including these fields if appropriate, then correct expectedPaths")
	}

	if missingNames := expectedPaths.Difference(extractedNames); len(missingNames) > 0 {
		t.Logf("Missing expected names:\n%s", strings.Join(missingNames.List(), "\n"))
		t.Error("Missing expected names. Verify the pod stub above includes these references, then verify VisitPodConfigmapNames() is correctly finding the missing names")
	}
	if extraNames := extractedNames.Difference(expectedPaths); len(extraNames) > 0 {
		t.Logf("Extra names:\n%s", strings.Join(extraNames.List(), "\n"))
		t.Error("Extra names extracted. Verify VisitPodConfigmapNames() is correctly extracting resource names")
	}

	// emptyPod is a stub containing empty object names
	emptyPod := &api.Pod{
		Spec: api.PodSpec{
			Containers: []api.Container{{
				EnvFrom: []api.EnvFromSource{{
					ConfigMapRef: &api.ConfigMapEnvSource{
						LocalObjectReference: api.LocalObjectReference{
							Name: ""}}}}}},
		},
	}
	VisitPodConfigmapNames(emptyPod, func(name string) bool {
		t.Fatalf("expected no empty names collected, got %q", name)
		return false
	}, AllContainers)
}

func TestDropFSGroupFields(t *testing.T) {
	nofsGroupPod := func() *api.Pod {
		return &api.Pod{
			Spec: api.PodSpec{
				Containers: []api.Container{
					{
						Name:  "container1",
						Image: "testimage",
					},
				},
			},
		}
	}

	var podFSGroup int64 = 100
	changePolicy := api.FSGroupChangeAlways

	fsGroupPod := func() *api.Pod {
		return &api.Pod{
			Spec: api.PodSpec{
				Containers: []api.Container{
					{
						Name:  "container1",
						Image: "testimage",
					},
				},
				SecurityContext: &api.PodSecurityContext{
					FSGroup:             &podFSGroup,
					FSGroupChangePolicy: &changePolicy,
				},
			},
		}
	}
	podInfos := []struct {
		description                  string
		newPodHasFSGroupChangePolicy bool
		pod                          func() *api.Pod
		expectPolicyInPod            bool
	}{
		{
			description:                  "oldPod.FSGroupChangePolicy=nil, feature=true, newPod.FSGroupChangePolicy=true",
			pod:                          nofsGroupPod,
			newPodHasFSGroupChangePolicy: true,
			expectPolicyInPod:            true,
		},
		{
			description:                  "oldPod=nil, feature=true, newPod.FSGroupChangePolicy=true",
			pod:                          func() *api.Pod { return nil },
			newPodHasFSGroupChangePolicy: true,
			expectPolicyInPod:            true,
		},
		{
			description:                  "oldPod.FSGroupChangePolicy=true, feature=true, newPod.FSGroupChangePolicy=false",
			pod:                          fsGroupPod,
			newPodHasFSGroupChangePolicy: false,
			expectPolicyInPod:            false,
		},
	}
	for _, podInfo := range podInfos {
		t.Run(podInfo.description, func(t *testing.T) {
			oldPod := podInfo.pod()
			newPod := oldPod.DeepCopy()
			if oldPod == nil && podInfo.newPodHasFSGroupChangePolicy {
				newPod = fsGroupPod()
			}

			if oldPod != nil {
				if podInfo.newPodHasFSGroupChangePolicy {
					newPod.Spec.SecurityContext = &api.PodSecurityContext{
						FSGroup:             &podFSGroup,
						FSGroupChangePolicy: &changePolicy,
					}
				} else {
					newPod.Spec.SecurityContext = &api.PodSecurityContext{}
				}
			}
			DropDisabledPodFields(newPod, oldPod)

			if podInfo.expectPolicyInPod {
				secContext := newPod.Spec.SecurityContext
				if secContext == nil || secContext.FSGroupChangePolicy == nil {
					t.Errorf("for %s, expected fsGroupChangepolicy found none", podInfo.description)
				}
			} else {
				secConext := newPod.Spec.SecurityContext
				if secConext != nil && secConext.FSGroupChangePolicy != nil {
					t.Errorf("for %s, unexpected fsGroupChangepolicy set", podInfo.description)
				}
			}
		})
	}

}

func TestDropProcMount(t *testing.T) {
	procMount := api.UnmaskedProcMount
	defaultProcMount := api.DefaultProcMount
	podWithProcMount := func() *api.Pod {
		return &api.Pod{
			Spec: api.PodSpec{
				RestartPolicy:  api.RestartPolicyNever,
				Containers:     []api.Container{{Name: "container1", Image: "testimage", SecurityContext: &api.SecurityContext{ProcMount: &procMount}}},
				InitContainers: []api.Container{{Name: "container1", Image: "testimage", SecurityContext: &api.SecurityContext{ProcMount: &procMount}}},
			},
		}
	}
	podWithDefaultProcMount := func() *api.Pod {
		return &api.Pod{
			Spec: api.PodSpec{
				RestartPolicy:  api.RestartPolicyNever,
				Containers:     []api.Container{{Name: "container1", Image: "testimage", SecurityContext: &api.SecurityContext{ProcMount: &defaultProcMount}}},
				InitContainers: []api.Container{{Name: "container1", Image: "testimage", SecurityContext: &api.SecurityContext{ProcMount: &defaultProcMount}}},
			},
		}
	}
	podWithoutProcMount := func() *api.Pod {
		return &api.Pod{
			Spec: api.PodSpec{
				RestartPolicy:  api.RestartPolicyNever,
				Containers:     []api.Container{{Name: "container1", Image: "testimage", SecurityContext: &api.SecurityContext{ProcMount: nil}}},
				InitContainers: []api.Container{{Name: "container1", Image: "testimage", SecurityContext: &api.SecurityContext{ProcMount: nil}}},
			},
		}
	}

	podInfo := []struct {
		description  string
		hasProcMount bool
		pod          func() *api.Pod
	}{
		{
			description:  "has ProcMount",
			hasProcMount: true,
			pod:          podWithProcMount,
		},
		{
			description:  "has default ProcMount",
			hasProcMount: false,
			pod:          podWithDefaultProcMount,
		},
		{
			description:  "does not have ProcMount",
			hasProcMount: false,
			pod:          podWithoutProcMount,
		},
		{
			description:  "is nil",
			hasProcMount: false,
			pod:          func() *api.Pod { return nil },
		},
	}

	for _, enabled := range []bool{true, false} {
		for _, oldPodInfo := range podInfo {
			for _, newPodInfo := range podInfo {
				oldPodHasProcMount, oldPod := oldPodInfo.hasProcMount, oldPodInfo.pod()
				newPodHasProcMount, newPod := newPodInfo.hasProcMount, newPodInfo.pod()
				if newPod == nil {
					continue
				}

				t.Run(fmt.Sprintf("feature enabled=%v, old pod %v, new pod %v", enabled, oldPodInfo.description, newPodInfo.description), func(t *testing.T) {
					defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.ProcMountType, enabled)()

					var oldPodSpec *api.PodSpec
					if oldPod != nil {
						oldPodSpec = &oldPod.Spec
					}
					dropDisabledFields(&newPod.Spec, nil, oldPodSpec, nil)

					// old pod should never be changed
					if !reflect.DeepEqual(oldPod, oldPodInfo.pod()) {
						t.Errorf("old pod changed: %v", cmp.Diff(oldPod, oldPodInfo.pod()))
					}

					switch {
					case enabled || oldPodHasProcMount:
						// new pod should not be changed if the feature is enabled, or if the old pod had ProcMount
						if !reflect.DeepEqual(newPod, newPodInfo.pod()) {
							t.Errorf("new pod changed: %v", cmp.Diff(newPod, newPodInfo.pod()))
						}
					case newPodHasProcMount:
						// new pod should be changed
						if reflect.DeepEqual(newPod, newPodInfo.pod()) {
							t.Errorf("new pod was not changed")
						}
						// new pod should not have ProcMount
						if procMountInUse(&newPod.Spec) {
							t.Errorf("new pod had ProcMount: %#v", &newPod.Spec)
						}
					default:
						// new pod should not need to be changed
						if !reflect.DeepEqual(newPod, newPodInfo.pod()) {
							t.Errorf("new pod changed: %v", cmp.Diff(newPod, newPodInfo.pod()))
						}
					}
				})
			}
		}
	}
}

func TestDropAppArmor(t *testing.T) {
	podWithAppArmor := func() *api.Pod {
		return &api.Pod{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"a": "1", v1.AppArmorBetaContainerAnnotationKeyPrefix + "foo": "default"}},
			Spec:       api.PodSpec{},
		}
	}
	podWithoutAppArmor := func() *api.Pod {
		return &api.Pod{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"a": "1"}},
			Spec:       api.PodSpec{},
		}
	}

	podInfo := []struct {
		description string
		hasAppArmor bool
		pod         func() *api.Pod
	}{
		{
			description: "has AppArmor",
			hasAppArmor: true,
			pod:         podWithAppArmor,
		},
		{
			description: "does not have AppArmor",
			hasAppArmor: false,
			pod:         podWithoutAppArmor,
		},
		{
			description: "is nil",
			hasAppArmor: false,
			pod:         func() *api.Pod { return nil },
		},
	}

	for _, enabled := range []bool{true, false} {
		for _, oldPodInfo := range podInfo {
			for _, newPodInfo := range podInfo {
				oldPodHasAppArmor, oldPod := oldPodInfo.hasAppArmor, oldPodInfo.pod()
				newPodHasAppArmor, newPod := newPodInfo.hasAppArmor, newPodInfo.pod()
				if newPod == nil {
					continue
				}

				t.Run(fmt.Sprintf("feature enabled=%v, old pod %v, new pod %v", enabled, oldPodInfo.description, newPodInfo.description), func(t *testing.T) {
					defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.AppArmor, enabled)()

					DropDisabledPodFields(newPod, oldPod)

					// old pod should never be changed
					if !reflect.DeepEqual(oldPod, oldPodInfo.pod()) {
						t.Errorf("old pod changed: %v", cmp.Diff(oldPod, oldPodInfo.pod()))
					}

					switch {
					case enabled || oldPodHasAppArmor:
						// new pod should not be changed if the feature is enabled, or if the old pod had AppArmor
						if !reflect.DeepEqual(newPod, newPodInfo.pod()) {
							t.Errorf("new pod changed: %v", cmp.Diff(newPod, newPodInfo.pod()))
						}
					case newPodHasAppArmor:
						// new pod should be changed
						if reflect.DeepEqual(newPod, newPodInfo.pod()) {
							t.Errorf("new pod was not changed")
						}
						// new pod should not have AppArmor
						if !reflect.DeepEqual(newPod, podWithoutAppArmor()) {
							t.Errorf("new pod had EmptyDir SizeLimit: %v", cmp.Diff(newPod, podWithoutAppArmor()))
						}
					default:
						// new pod should not need to be changed
						if !reflect.DeepEqual(newPod, newPodInfo.pod()) {
							t.Errorf("new pod changed: %v", cmp.Diff(newPod, newPodInfo.pod()))
						}
					}
				})
			}
		}
	}
}

func TestDropDynamicResourceAllocation(t *testing.T) {
	resourceClaimName := "external-claim"

	podWithClaims := &api.Pod{
		Spec: api.PodSpec{
			Containers: []api.Container{
				{
					Resources: api.ResourceRequirements{
						Claims: []api.ResourceClaim{{Name: "my-claim"}},
					},
				},
			},
			InitContainers: []api.Container{
				{
					Resources: api.ResourceRequirements{
						Claims: []api.ResourceClaim{{Name: "my-claim"}},
					},
				},
			},
			EphemeralContainers: []api.EphemeralContainer{
				{
					EphemeralContainerCommon: api.EphemeralContainerCommon{
						Resources: api.ResourceRequirements{
							Claims: []api.ResourceClaim{{Name: "my-claim"}},
						},
					},
				},
			},
			ResourceClaims: []api.PodResourceClaim{
				{
					Name: "my-claim",
					Source: api.ClaimSource{
						ResourceClaimName: &resourceClaimName,
					},
				},
			},
		},
		Status: api.PodStatus{
			ResourceClaimStatuses: []api.PodResourceClaimStatus{
				{Name: "my-claim", ResourceClaimName: pointer.String("pod-my-claim")},
			},
		},
	}
	podWithoutClaims := &api.Pod{
		Spec: api.PodSpec{
			Containers:          []api.Container{{}},
			InitContainers:      []api.Container{{}},
			EphemeralContainers: []api.EphemeralContainer{{}},
		},
	}

	var noPod *api.Pod

	testcases := []struct {
		description string
		enabled     bool
		oldPod      *api.Pod
		newPod      *api.Pod
		wantPod     *api.Pod
	}{
		{
			description: "old with claims / new with claims / disabled",
			oldPod:      podWithClaims,
			newPod:      podWithClaims,
			wantPod:     podWithClaims,
		},
		{
			description: "old without claims / new with claims / disabled",
			oldPod:      podWithoutClaims,
			newPod:      podWithClaims,
			wantPod:     podWithoutClaims,
		},
		{
			description: "no old pod/ new with claims / disabled",
			oldPod:      noPod,
			newPod:      podWithClaims,
			wantPod:     podWithoutClaims,
		},

		{
			description: "old with claims / new without claims / disabled",
			oldPod:      podWithClaims,
			newPod:      podWithoutClaims,
			wantPod:     podWithoutClaims,
		},
		{
			description: "old without claims / new without claims / disabled",
			oldPod:      podWithoutClaims,
			newPod:      podWithoutClaims,
			wantPod:     podWithoutClaims,
		},
		{
			description: "no old pod/ new without claims / disabled",
			oldPod:      noPod,
			newPod:      podWithoutClaims,
			wantPod:     podWithoutClaims,
		},

		{
			description: "old with claims / new with claims / enabled",
			enabled:     true,
			oldPod:      podWithClaims,
			newPod:      podWithClaims,
			wantPod:     podWithClaims,
		},
		{
			description: "old without claims / new with claims / enabled",
			enabled:     true,
			oldPod:      podWithoutClaims,
			newPod:      podWithClaims,
			wantPod:     podWithClaims,
		},
		{
			description: "no old pod/ new with claims / enabled",
			enabled:     true,
			oldPod:      noPod,
			newPod:      podWithClaims,
			wantPod:     podWithClaims,
		},

		{
			description: "old with claims / new without claims / enabled",
			enabled:     true,
			oldPod:      podWithClaims,
			newPod:      podWithoutClaims,
			wantPod:     podWithoutClaims,
		},
		{
			description: "old without claims / new without claims / enabled",
			enabled:     true,
			oldPod:      podWithoutClaims,
			newPod:      podWithoutClaims,
			wantPod:     podWithoutClaims,
		},
		{
			description: "no old pod/ new without claims / enabled",
			enabled:     true,
			oldPod:      noPod,
			newPod:      podWithoutClaims,
			wantPod:     podWithoutClaims,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.description, func(t *testing.T) {
			defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.DynamicResourceAllocation, tc.enabled)()

			oldPod := tc.oldPod.DeepCopy()
			newPod := tc.newPod.DeepCopy()
			wantPod := tc.wantPod
			DropDisabledPodFields(newPod, oldPod)

			// old pod should never be changed
			if diff := cmp.Diff(oldPod, tc.oldPod); diff != "" {
				t.Errorf("old pod changed: %s", diff)
			}

			if diff := cmp.Diff(wantPod, newPod); diff != "" {
				t.Errorf("new pod changed (- want, + got): %s", diff)
			}
		})
	}
}

func TestValidatePodDeletionCostOption(t *testing.T) {
	testCases := []struct {
		name                            string
		oldPodMeta                      *metav1.ObjectMeta
		featureEnabled                  bool
		wantAllowInvalidPodDeletionCost bool
	}{
		{
			name:                            "CreateFeatureEnabled",
			featureEnabled:                  true,
			wantAllowInvalidPodDeletionCost: false,
		},
		{
			name:                            "CreateFeatureDisabled",
			featureEnabled:                  false,
			wantAllowInvalidPodDeletionCost: true,
		},
		{
			name:                            "UpdateFeatureDisabled",
			oldPodMeta:                      &metav1.ObjectMeta{Annotations: map[string]string{api.PodDeletionCost: "100"}},
			featureEnabled:                  false,
			wantAllowInvalidPodDeletionCost: true,
		},
		{
			name:                            "UpdateFeatureEnabledValidOldValue",
			oldPodMeta:                      &metav1.ObjectMeta{Annotations: map[string]string{api.PodDeletionCost: "100"}},
			featureEnabled:                  true,
			wantAllowInvalidPodDeletionCost: false,
		},
		{
			name:                            "UpdateFeatureEnabledValidOldValue",
			oldPodMeta:                      &metav1.ObjectMeta{Annotations: map[string]string{api.PodDeletionCost: "invalid-value"}},
			featureEnabled:                  true,
			wantAllowInvalidPodDeletionCost: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.PodDeletionCost, tc.featureEnabled)()
			// The new pod doesn't impact the outcome.
			gotOptions := GetValidationOptionsFromPodSpecAndMeta(nil, nil, nil, tc.oldPodMeta)
			if tc.wantAllowInvalidPodDeletionCost != gotOptions.AllowInvalidPodDeletionCost {
				t.Errorf("unexpected diff, want: %v, got: %v", tc.wantAllowInvalidPodDeletionCost, gotOptions.AllowInvalidPodDeletionCost)
			}
		})
	}
}

func TestDropDisabledTopologySpreadConstraintsFields(t *testing.T) {
	testCases := []struct {
		name        string
		enabled     bool
		podSpec     *api.PodSpec
		oldPodSpec  *api.PodSpec
		wantPodSpec *api.PodSpec
	}{
		{
			name:        "TopologySpreadConstraints is nil",
			podSpec:     &api.PodSpec{},
			oldPodSpec:  &api.PodSpec{},
			wantPodSpec: &api.PodSpec{},
		},
		{
			name:        "TopologySpreadConstraints is empty",
			podSpec:     &api.PodSpec{TopologySpreadConstraints: []api.TopologySpreadConstraint{}},
			oldPodSpec:  &api.PodSpec{TopologySpreadConstraints: []api.TopologySpreadConstraint{}},
			wantPodSpec: &api.PodSpec{TopologySpreadConstraints: []api.TopologySpreadConstraint{}},
		},
		{
			name: "TopologySpreadConstraints is not empty, but all constraints don't have minDomains",
			podSpec: &api.PodSpec{TopologySpreadConstraints: []api.TopologySpreadConstraint{
				{
					MinDomains: nil,
				},
				{
					MinDomains: nil,
				},
			}},
			oldPodSpec: &api.PodSpec{TopologySpreadConstraints: []api.TopologySpreadConstraint{
				{
					MinDomains: nil,
				},
				{
					MinDomains: nil,
				},
			}},
			wantPodSpec: &api.PodSpec{TopologySpreadConstraints: []api.TopologySpreadConstraint{
				{
					MinDomains: nil,
				},
				{
					MinDomains: nil,
				},
			}},
		},
		{
			name: "one constraint in podSpec has non-empty minDomains, feature gate is disabled " +
				"and all constraint in oldPodSpec doesn't have minDomains",
			enabled: false,
			podSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{
						MinDomains: pointer.Int32(2),
					},
					{
						MinDomains: nil,
					},
				},
			},
			oldPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{
						MinDomains: nil,
					},
					{
						MinDomains: nil,
					},
				},
			},
			wantPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{
						// cleared.
						MinDomains: nil,
					},
					{
						MinDomains: nil,
					},
				},
			},
		},
		{
			name: "one constraint in podSpec has non-empty minDomains, feature gate is disabled " +
				"and one constraint in oldPodSpec has minDomains",
			enabled: false,
			podSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{
						MinDomains: pointer.Int32(2),
					},
					{
						MinDomains: nil,
					},
				},
			},
			oldPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{
						MinDomains: pointer.Int32(2),
					},
					{
						MinDomains: nil,
					},
				},
			},
			wantPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{
						// not cleared.
						MinDomains: pointer.Int32(2),
					},
					{
						MinDomains: nil,
					},
				},
			},
		},
		{
			name: "one constraint in podSpec has non-empty minDomains, feature gate is enabled" +
				"and all constraint in oldPodSpec doesn't have minDomains",
			enabled: true,
			podSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{
						MinDomains: pointer.Int32(2),
					},
					{
						MinDomains: nil,
					},
				},
			},
			oldPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{
						MinDomains: nil,
					},
					{
						MinDomains: nil,
					},
				},
			},
			wantPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{
						// not cleared.
						MinDomains: pointer.Int32(2),
					},
					{
						MinDomains: nil,
					},
				},
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.MinDomainsInPodTopologySpread, tc.enabled)()
			dropDisabledFields(tc.podSpec, nil, tc.oldPodSpec, nil)
			if diff := cmp.Diff(tc.wantPodSpec, tc.podSpec); diff != "" {
				t.Errorf("unexpected pod spec (-want, +got):\n%s", diff)
			}
		})
	}
}

func TestDropDisabledPodStatusFields(t *testing.T) {
	podWithHostIPs := func() *api.PodStatus {
		return &api.PodStatus{
			HostIPs: makeHostIPs("10.0.0.1", "fd00:10::1"),
		}
	}

	podWithoutHostIPs := func() *api.PodStatus {
		return &api.PodStatus{
			HostIPs: nil,
		}
	}

	tests := []struct {
		name           string
		podStatus      *api.PodStatus
		oldPodStatus   *api.PodStatus
		wantPodStatus  *api.PodStatus
		featureEnabled bool
	}{
		{
			name:           "gate off, old=without, new=without",
			oldPodStatus:   podWithoutHostIPs(),
			podStatus:      podWithoutHostIPs(),
			featureEnabled: false,

			wantPodStatus: podWithoutHostIPs(),
		},
		{
			name:           "gate off, old=without, new=with",
			oldPodStatus:   podWithoutHostIPs(),
			podStatus:      podWithHostIPs(),
			featureEnabled: false,

			wantPodStatus: podWithoutHostIPs(),
		},
		{
			name:           "gate off, old=with, new=without",
			oldPodStatus:   podWithHostIPs(),
			podStatus:      podWithoutHostIPs(),
			featureEnabled: false,

			wantPodStatus: podWithoutHostIPs(),
		},
		{
			name:           "gate off, old=with, new=with",
			oldPodStatus:   podWithHostIPs(),
			podStatus:      podWithHostIPs(),
			featureEnabled: false,

			wantPodStatus: podWithHostIPs(),
		},
		{
			name:           "gate on, old=without, new=without",
			oldPodStatus:   podWithoutHostIPs(),
			podStatus:      podWithoutHostIPs(),
			featureEnabled: true,

			wantPodStatus: podWithoutHostIPs(),
		},
		{
			name:           "gate on, old=without, new=with",
			oldPodStatus:   podWithoutHostIPs(),
			podStatus:      podWithHostIPs(),
			featureEnabled: true,

			wantPodStatus: podWithHostIPs(),
		},
		{
			name:           "gate on, old=with, new=without",
			oldPodStatus:   podWithHostIPs(),
			podStatus:      podWithoutHostIPs(),
			featureEnabled: true,

			wantPodStatus: podWithoutHostIPs(),
		},
		{
			name:           "gate on, old=with, new=with",
			oldPodStatus:   podWithHostIPs(),
			podStatus:      podWithHostIPs(),
			featureEnabled: true,

			wantPodStatus: podWithHostIPs(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.PodHostIPs, tt.featureEnabled)()

			dropDisabledPodStatusFields(tt.podStatus, tt.oldPodStatus, &api.PodSpec{}, &api.PodSpec{})

			if !reflect.DeepEqual(tt.podStatus, tt.wantPodStatus) {
				t.Errorf("dropDisabledStatusFields() = %v, want %v", tt.podStatus, tt.wantPodStatus)
			}
		})
	}
}

func makeHostIPs(ips ...string) []api.HostIP {
	ret := []api.HostIP{}
	for _, ip := range ips {
		ret = append(ret, api.HostIP{IP: ip})
	}
	return ret
}

func TestDropNodeInclusionPolicyFields(t *testing.T) {
	ignore := api.NodeInclusionPolicyIgnore
	honor := api.NodeInclusionPolicyHonor

	tests := []struct {
		name        string
		enabled     bool
		podSpec     *api.PodSpec
		oldPodSpec  *api.PodSpec
		wantPodSpec *api.PodSpec
	}{
		{
			name:    "feature disabled, both pods don't use the fields",
			enabled: false,
			oldPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{},
			},
			podSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{},
			},
			wantPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{},
			},
		},
		{
			name:    "feature disabled, only old pod use NodeAffinityPolicy field",
			enabled: false,
			oldPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{NodeAffinityPolicy: &honor},
				},
			},
			podSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{},
			},
			wantPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{},
			},
		},
		{
			name:    "feature disabled, only old pod use NodeTaintsPolicy field",
			enabled: false,
			oldPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{NodeTaintsPolicy: &ignore},
				},
			},
			podSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{},
			},
			wantPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{},
			},
		},
		{
			name:    "feature disabled, only current pod use NodeAffinityPolicy field",
			enabled: false,
			oldPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{},
			},
			podSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{NodeAffinityPolicy: &honor},
				},
			},
			wantPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{{
					NodeAffinityPolicy: nil,
				}},
			},
		},
		{
			name:    "feature disabled, only current pod use NodeTaintsPolicy field",
			enabled: false,
			oldPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{},
			},
			podSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{NodeTaintsPolicy: &ignore},
				},
			},
			wantPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{NodeTaintsPolicy: nil},
				},
			},
		},
		{
			name:    "feature disabled, both pods use NodeAffinityPolicy fields",
			enabled: false,
			oldPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{NodeAffinityPolicy: &honor},
				},
			},
			podSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{NodeAffinityPolicy: &ignore},
				},
			},
			wantPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{NodeAffinityPolicy: &ignore},
				},
			},
		},
		{
			name:    "feature disabled, both pods use NodeTaintsPolicy fields",
			enabled: false,
			oldPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{NodeTaintsPolicy: &ignore},
				},
			},
			podSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{NodeTaintsPolicy: &honor},
				},
			},
			wantPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{NodeTaintsPolicy: &honor},
				},
			},
		},
		{
			name:    "feature enabled, both pods use the fields",
			enabled: true,
			oldPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{
						NodeAffinityPolicy: &ignore,
						NodeTaintsPolicy:   &honor,
					},
				},
			},
			podSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{
						NodeAffinityPolicy: &honor,
						NodeTaintsPolicy:   &ignore,
					},
				},
			},
			wantPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{
						NodeAffinityPolicy: &honor,
						NodeTaintsPolicy:   &ignore,
					},
				},
			},
		},
		{
			name:    "feature enabled, only old pod use NodeAffinityPolicy field",
			enabled: true,
			oldPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{
						NodeAffinityPolicy: &honor,
					},
				},
			},
			podSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{},
			},
			wantPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{},
			},
		},
		{
			name:    "feature enabled, only old pod use NodeTaintsPolicy field",
			enabled: true,
			oldPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{
						NodeTaintsPolicy: &ignore,
					},
				},
			},
			podSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{},
			},
			wantPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{},
			},
		},
		{
			name:    "feature enabled, only current pod use NodeAffinityPolicy field",
			enabled: true,
			oldPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{},
			},
			podSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{
						NodeAffinityPolicy: &ignore,
					},
				},
			},
			wantPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{
						NodeAffinityPolicy: &ignore,
					},
				},
			},
		},
		{
			name:    "feature enabled, only current pod use NodeTaintsPolicy field",
			enabled: true,
			oldPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{},
			},
			podSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{
						NodeTaintsPolicy: &honor,
					},
				},
			},
			wantPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{
						NodeTaintsPolicy: &honor,
					},
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.NodeInclusionPolicyInPodTopologySpread, test.enabled)()

			dropDisabledFields(test.podSpec, nil, test.oldPodSpec, nil)
			if diff := cmp.Diff(test.wantPodSpec, test.podSpec); diff != "" {
				t.Errorf("unexpected pod spec (-want, +got):\n%s", diff)
			}
		})
	}
}

func TestDropDisabledMatchLabelKeysField(t *testing.T) {
	tests := []struct {
		name        string
		enabled     bool
		podSpec     *api.PodSpec
		oldPodSpec  *api.PodSpec
		wantPodSpec *api.PodSpec
	}{
		{
			name:    "feature disabled, both pods don't use MatchLabelKeys fields",
			enabled: false,
			oldPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{},
			},
			podSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{},
			},
			wantPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{},
			},
		},
		{
			name:    "feature disabled, only old pod uses MatchLabelKeys field",
			enabled: false,
			oldPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{MatchLabelKeys: []string{"foo"}},
				},
			},
			podSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{},
			},
			wantPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{},
			},
		},
		{
			name:    "feature disabled, only current pod uses MatchLabelKeys field",
			enabled: false,
			oldPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{},
			},
			podSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{MatchLabelKeys: []string{"foo"}},
				},
			},
			wantPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{{}},
			},
		},
		{
			name:    "feature disabled, both pods use MatchLabelKeys fields",
			enabled: false,
			oldPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{MatchLabelKeys: []string{"foo"}},
				},
			},
			podSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{MatchLabelKeys: []string{"foo"}},
				},
			},
			wantPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{MatchLabelKeys: []string{"foo"}},
				},
			},
		},
		{
			name:    "feature enabled, only old pod uses MatchLabelKeys field",
			enabled: true,
			oldPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{MatchLabelKeys: []string{"foo"}},
				},
			},
			podSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{},
			},
			wantPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{},
			},
		},
		{
			name:    "feature enabled, only current pod uses MatchLabelKeys field",
			enabled: true,
			oldPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{},
			},
			podSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{MatchLabelKeys: []string{"foo"}},
				},
			},
			wantPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{MatchLabelKeys: []string{"foo"}},
				},
			},
		},
		{
			name:    "feature enabled, both pods use MatchLabelKeys fields",
			enabled: false,
			oldPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{MatchLabelKeys: []string{"foo"}},
				},
			},
			podSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{MatchLabelKeys: []string{"foo"}},
				},
			},
			wantPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{MatchLabelKeys: []string{"foo"}},
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.MatchLabelKeysInPodTopologySpread, test.enabled)()

			dropDisabledFields(test.podSpec, nil, test.oldPodSpec, nil)
			if diff := cmp.Diff(test.wantPodSpec, test.podSpec); diff != "" {
				t.Errorf("unexpected pod spec (-want, +got):\n%s", diff)
			}
		})
	}
}

func TestDropHostUsers(t *testing.T) {
	falseVar := false
	trueVar := true

	podWithoutHostUsers := func() *api.Pod {
		return &api.Pod{
			Spec: api.PodSpec{
				SecurityContext: &api.PodSecurityContext{}},
		}
	}
	podWithHostUsersFalse := func() *api.Pod {
		return &api.Pod{
			Spec: api.PodSpec{
				SecurityContext: &api.PodSecurityContext{
					HostUsers: &falseVar,
				},
			},
		}
	}
	podWithHostUsersTrue := func() *api.Pod {
		return &api.Pod{
			Spec: api.PodSpec{
				SecurityContext: &api.PodSecurityContext{
					HostUsers: &trueVar,
				},
			},
		}
	}

	podInfo := []struct {
		description  string
		hasHostUsers bool
		pod          func() *api.Pod
	}{
		{
			description:  "with hostUsers=true",
			hasHostUsers: true,
			pod:          podWithHostUsersTrue,
		},
		{
			description:  "with hostUsers=false",
			hasHostUsers: true,
			pod:          podWithHostUsersFalse,
		},
		{
			description: "with hostUsers=nil",
			pod:         func() *api.Pod { return nil },
		},
	}

	for _, enabled := range []bool{true, false} {
		for _, oldPodInfo := range podInfo {
			for _, newPodInfo := range podInfo {
				oldPodHasHostUsers, oldPod := oldPodInfo.hasHostUsers, oldPodInfo.pod()
				newPodHasHostUsers, newPod := newPodInfo.hasHostUsers, newPodInfo.pod()
				if newPod == nil {
					continue
				}

				t.Run(fmt.Sprintf("feature enabled=%v, old pod %v, new pod %v", enabled, oldPodInfo.description, newPodInfo.description), func(t *testing.T) {
					defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.UserNamespacesSupport, enabled)()

					DropDisabledPodFields(newPod, oldPod)

					// old pod should never be changed
					if !reflect.DeepEqual(oldPod, oldPodInfo.pod()) {
						t.Errorf("old pod changed: %v", cmp.Diff(oldPod, oldPodInfo.pod()))
					}

					switch {
					case enabled || oldPodHasHostUsers:
						// new pod should not be changed if the feature is enabled, or if the old pod had hostUsers
						if !reflect.DeepEqual(newPod, newPodInfo.pod()) {
							t.Errorf("new pod changed: %v", cmp.Diff(newPod, newPodInfo.pod()))
						}
					case newPodHasHostUsers:
						// new pod should be changed
						if reflect.DeepEqual(newPod, newPodInfo.pod()) {
							t.Errorf("new pod was not changed")
						}
						// new pod should not have hostUsers
						if exp := podWithoutHostUsers(); !reflect.DeepEqual(newPod, exp) {
							t.Errorf("new pod had hostUsers: %v", cmp.Diff(newPod, exp))
						}
					default:
						// new pod should not need to be changed
						if !reflect.DeepEqual(newPod, newPodInfo.pod()) {
							t.Errorf("new pod changed: %v", cmp.Diff(newPod, newPodInfo.pod()))
						}
					}
				})
			}
		}
	}

}

func TestDropSchedulingGates(t *testing.T) {
	podWithSchedulingGates := func() *api.Pod {
		return &api.Pod{
			Spec: api.PodSpec{
				SchedulingGates: []api.PodSchedulingGate{
					{Name: "foo"},
					{Name: "bar"},
				},
			},
		}
	}
	podWithoutSchedulingGates := func() *api.Pod { return &api.Pod{} }

	podInfo := []struct {
		description             string
		hasSchedulingGatesField bool
		pod                     func() *api.Pod
	}{
		{
			description:             "has SchedulingGates field",
			hasSchedulingGatesField: true,
			pod:                     podWithSchedulingGates,
		},
		{
			description:             "does not have SchedulingGates field",
			hasSchedulingGatesField: false,
			pod:                     podWithoutSchedulingGates,
		},
		{
			description:             "is nil",
			hasSchedulingGatesField: false,
			pod:                     func() *api.Pod { return nil },
		},
	}

	for _, enabled := range []bool{true, false} {
		for _, oldPodInfo := range podInfo {
			for _, newPodInfo := range podInfo {
				oldPodHasSchedulingGates, oldPod := oldPodInfo.hasSchedulingGatesField, oldPodInfo.pod()
				newPodHasSchedulingGates, newPod := newPodInfo.hasSchedulingGatesField, newPodInfo.pod()
				if newPod == nil {
					continue
				}

				t.Run(fmt.Sprintf("feature enabled=%v, old pod %v, new pod %v", enabled, oldPodInfo.description, newPodInfo.description), func(t *testing.T) {
					defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.PodSchedulingReadiness, enabled)()
					var oldPodSpec *api.PodSpec
					if oldPod != nil {
						oldPodSpec = &oldPod.Spec
					}
					dropDisabledFields(&newPod.Spec, nil, oldPodSpec, nil)
					// Old Pod should never be changed.
					if diff := cmp.Diff(oldPod, oldPodInfo.pod()); diff != "" {
						t.Errorf("old pod changed: %v", diff)
					}
					switch {
					case enabled || oldPodHasSchedulingGates:
						// New Pod should not be changed if the feature is enabled, or if the old Pod had schedulingGates.
						if diff := cmp.Diff(newPod, newPodInfo.pod()); diff != "" {
							t.Errorf("new pod changed: %v", diff)
						}
					case newPodHasSchedulingGates:
						// New Pod should be changed.
						if reflect.DeepEqual(newPod, newPodInfo.pod()) {
							t.Errorf("new pod was not changed")
						}
						// New Pod should not have SchedulingGates field.
						if diff := cmp.Diff(newPod, podWithoutSchedulingGates()); diff != "" {
							t.Errorf("new pod has SchedulingGates field: %v", diff)
						}
					default:
						// New pod should not need to be changed.
						if diff := cmp.Diff(newPod, newPodInfo.pod()); diff != "" {
							t.Errorf("new pod changed: %v", diff)
						}
					}
				})
			}
		}
	}
}

func TestValidateTopologySpreadConstraintLabelSelectorOption(t *testing.T) {
	testCases := []struct {
		name       string
		oldPodSpec *api.PodSpec
		wantOption bool
	}{
		{
			name:       "Create",
			wantOption: false,
		},
		{
			name: "UpdateInvalidLabelSelector",
			oldPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{
						LabelSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"NoUppercaseOrSpecialCharsLike=Equals": "foo"},
						},
					},
				},
			},
			wantOption: true,
		},
		{
			name: "UpdateValidLabelSelector",
			oldPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{
						LabelSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"foo": "foo"},
						},
					},
				},
			},
			wantOption: false,
		},
		{
			name: "UpdateEmptyLabelSelector",
			oldPodSpec: &api.PodSpec{
				TopologySpreadConstraints: []api.TopologySpreadConstraint{
					{
						LabelSelector: nil,
					},
				},
			},
			wantOption: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Pod meta doesn't impact the outcome.
			gotOptions := GetValidationOptionsFromPodSpecAndMeta(&api.PodSpec{}, tc.oldPodSpec, nil, nil)
			if tc.wantOption != gotOptions.AllowInvalidTopologySpreadConstraintLabelSelector {
				t.Errorf("Got AllowInvalidLabelValueInSelector=%t, want %t", gotOptions.AllowInvalidTopologySpreadConstraintLabelSelector, tc.wantOption)
			}
		})
	}
}

func TestDropVolumesClaimField(t *testing.T) {
	pod := &api.Pod{
		Spec: api.PodSpec{
			Volumes: []api.Volume{
				{},
				{
					VolumeSource: api.VolumeSource{
						Ephemeral: &api.EphemeralVolumeSource{},
					},
				},
				{
					VolumeSource: api.VolumeSource{
						Ephemeral: &api.EphemeralVolumeSource{
							VolumeClaimTemplate: &api.PersistentVolumeClaimTemplate{
								Spec: api.PersistentVolumeClaimSpec{
									Resources: api.ResourceRequirements{
										Claims: []api.ResourceClaim{
											{Name: "dra"},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	DropDisabledPodFields(pod, nil)

	for i, volume := range pod.Spec.Volumes {
		if volume.Ephemeral != nil && volume.Ephemeral.VolumeClaimTemplate != nil && volume.Ephemeral.VolumeClaimTemplate.Spec.Resources.Claims != nil {
			t.Errorf("volume #%d: Resources.Claim should be nil", i)
		}
	}
}

func TestDropInPlacePodVerticalScaling(t *testing.T) {
	podWithInPlaceVerticalScaling := func() *api.Pod {
		return &api.Pod{
			Spec: api.PodSpec{
				Containers: []api.Container{
					{
						Name:  "c1",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("100m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
						},
						ResizePolicy: []api.ContainerResizePolicy{
							{ResourceName: api.ResourceCPU, RestartPolicy: api.NotRequired},
							{ResourceName: api.ResourceMemory, RestartPolicy: api.RestartContainer},
						},
					},
				},
			},
			Status: api.PodStatus{
				Resize: api.PodResizeStatusInProgress,
				ContainerStatuses: []api.ContainerStatus{
					{
						Name:               "c1",
						Image:              "image",
						AllocatedResources: api.ResourceList{api.ResourceCPU: resource.MustParse("100m")},
						Resources: &api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("300m")},
						},
					},
				},
			},
		}
	}
	podWithoutInPlaceVerticalScaling := func() *api.Pod {
		return &api.Pod{
			Spec: api.PodSpec{
				Containers: []api.Container{
					{
						Name:  "c1",
						Image: "image",
						Resources: api.ResourceRequirements{
							Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("100m")},
							Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
						},
					},
				},
			},
			Status: api.PodStatus{
				ContainerStatuses: []api.ContainerStatus{
					{
						Name:  "c1",
						Image: "image",
					},
				},
			},
		}
	}

	podInfo := []struct {
		description               string
		hasInPlaceVerticalScaling bool
		pod                       func() *api.Pod
	}{
		{
			description:               "has in-place vertical scaling enabled with resources",
			hasInPlaceVerticalScaling: true,
			pod:                       podWithInPlaceVerticalScaling,
		},
		{
			description:               "has in-place vertical scaling disabled",
			hasInPlaceVerticalScaling: false,
			pod:                       podWithoutInPlaceVerticalScaling,
		},
		{
			description:               "is nil",
			hasInPlaceVerticalScaling: false,
			pod:                       func() *api.Pod { return nil },
		},
	}

	for _, enabled := range []bool{true, false} {
		for _, oldPodInfo := range podInfo {
			for _, newPodInfo := range podInfo {
				oldPodHasInPlaceVerticalScaling, oldPod := oldPodInfo.hasInPlaceVerticalScaling, oldPodInfo.pod()
				newPodHasInPlaceVerticalScaling, newPod := newPodInfo.hasInPlaceVerticalScaling, newPodInfo.pod()
				if newPod == nil {
					continue
				}

				t.Run(fmt.Sprintf("feature enabled=%v, old pod %v, new pod %v", enabled, oldPodInfo.description, newPodInfo.description), func(t *testing.T) {
					defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.InPlacePodVerticalScaling, enabled)()

					var oldPodSpec *api.PodSpec
					var oldPodStatus *api.PodStatus
					if oldPod != nil {
						oldPodSpec = &oldPod.Spec
						oldPodStatus = &oldPod.Status
					}
					dropDisabledFields(&newPod.Spec, nil, oldPodSpec, nil)
					dropDisabledPodStatusFields(&newPod.Status, oldPodStatus, &newPod.Spec, oldPodSpec)

					// old pod should never be changed
					if !reflect.DeepEqual(oldPod, oldPodInfo.pod()) {
						t.Errorf("old pod changed: %v", cmp.Diff(oldPod, oldPodInfo.pod()))
					}

					switch {
					case enabled || oldPodHasInPlaceVerticalScaling:
						// new pod shouldn't change if feature enabled or if old pod has ResizePolicy set
						if !reflect.DeepEqual(newPod, newPodInfo.pod()) {
							t.Errorf("new pod changed: %v", cmp.Diff(newPod, newPodInfo.pod()))
						}
					case newPodHasInPlaceVerticalScaling:
						// new pod should be changed
						if reflect.DeepEqual(newPod, newPodInfo.pod()) {
							t.Errorf("new pod was not changed")
						}
						// new pod should not have ResizePolicy
						if !reflect.DeepEqual(newPod, podWithoutInPlaceVerticalScaling()) {
							t.Errorf("new pod has ResizePolicy: %v", cmp.Diff(newPod, podWithoutInPlaceVerticalScaling()))
						}
					default:
						// new pod should not need to be changed
						if !reflect.DeepEqual(newPod, newPodInfo.pod()) {
							t.Errorf("new pod changed: %v", cmp.Diff(newPod, newPodInfo.pod()))
						}
					}
				})
			}
		}
	}
}

func TestDropSidecarContainers(t *testing.T) {
	containerRestartPolicyAlways := api.ContainerRestartPolicyAlways

	podWithSidecarContainers := func() *api.Pod {
		return &api.Pod{
			Spec: api.PodSpec{
				InitContainers: []api.Container{
					{
						Name:          "c1",
						Image:         "image",
						RestartPolicy: &containerRestartPolicyAlways,
					},
				},
			},
		}
	}

	podWithoutSidecarContainers := func() *api.Pod {
		return &api.Pod{
			Spec: api.PodSpec{
				InitContainers: []api.Container{
					{
						Name:  "c1",
						Image: "image",
					},
				},
			},
		}
	}

	podInfo := []struct {
		description         string
		hasSidecarContainer bool
		pod                 func() *api.Pod
	}{
		{
			description:         "has a sidecar container",
			hasSidecarContainer: true,
			pod:                 podWithSidecarContainers,
		},
		{
			description:         "does not have a sidecar container",
			hasSidecarContainer: false,
			pod:                 podWithoutSidecarContainers,
		},
		{
			description:         "is nil",
			hasSidecarContainer: false,
			pod:                 func() *api.Pod { return nil },
		},
	}

	for _, enabled := range []bool{true, false} {
		for _, oldPodInfo := range podInfo {
			for _, newPodInfo := range podInfo {
				oldPodHasSidecarContainer, oldPod := oldPodInfo.hasSidecarContainer, oldPodInfo.pod()
				newPodHasSidecarContainer, newPod := newPodInfo.hasSidecarContainer, newPodInfo.pod()
				if newPod == nil {
					continue
				}

				t.Run(fmt.Sprintf("feature enabled=%v, old pod %v, new pod %v", enabled, oldPodInfo.description, newPodInfo.description), func(t *testing.T) {
					defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.SidecarContainers, enabled)()

					var oldPodSpec *api.PodSpec
					if oldPod != nil {
						oldPodSpec = &oldPod.Spec
					}
					dropDisabledFields(&newPod.Spec, nil, oldPodSpec, nil)

					// old pod should never be changed
					if !reflect.DeepEqual(oldPod, oldPodInfo.pod()) {
						t.Errorf("old pod changed: %v", cmp.Diff(oldPod, oldPodInfo.pod()))
					}

					switch {
					case enabled || oldPodHasSidecarContainer:
						// new pod shouldn't change if feature enabled or if old pod has
						// any sidecar container
						if !reflect.DeepEqual(newPod, newPodInfo.pod()) {
							t.Errorf("new pod changed: %v", cmp.Diff(newPod, newPodInfo.pod()))
						}
					case newPodHasSidecarContainer:
						// new pod should be changed
						if reflect.DeepEqual(newPod, newPodInfo.pod()) {
							t.Errorf("new pod was not changed")
						}
						// new pod should not have any sidecar container
						if !reflect.DeepEqual(newPod, podWithoutSidecarContainers()) {
							t.Errorf("new pod has a sidecar container: %v", cmp.Diff(newPod, podWithoutSidecarContainers()))
						}
					default:
						// new pod should not need to be changed
						if !reflect.DeepEqual(newPod, newPodInfo.pod()) {
							t.Errorf("new pod changed: %v", cmp.Diff(newPod, newPodInfo.pod()))
						}
					}
				})
			}
		}
	}
}

func TestMarkPodProposedForResize(t *testing.T) {
	testCases := []struct {
		desc        string
		newPod      *api.Pod
		oldPod      *api.Pod
		expectedPod *api.Pod
	}{
		{
			desc: "nil requests",
			newPod: &api.Pod{
				Spec: api.PodSpec{
					Containers: []api.Container{
						{
							Name:  "c1",
							Image: "image",
						},
					},
				},
				Status: api.PodStatus{
					ContainerStatuses: []api.ContainerStatus{
						{
							Name:  "c1",
							Image: "image",
						},
					},
				},
			},
			oldPod: &api.Pod{
				Spec: api.PodSpec{
					Containers: []api.Container{
						{
							Name:  "c1",
							Image: "image",
						},
					},
				},
				Status: api.PodStatus{
					ContainerStatuses: []api.ContainerStatus{
						{
							Name:  "c1",
							Image: "image",
						},
					},
				},
			},
			expectedPod: &api.Pod{
				Spec: api.PodSpec{
					Containers: []api.Container{
						{
							Name:  "c1",
							Image: "image",
						},
					},
				},
				Status: api.PodStatus{
					ContainerStatuses: []api.ContainerStatus{
						{
							Name:  "c1",
							Image: "image",
						},
					},
				},
			},
		},
		{
			desc: "resources unchanged",
			newPod: &api.Pod{
				Spec: api.PodSpec{
					Containers: []api.Container{
						{
							Name:  "c1",
							Image: "image",
							Resources: api.ResourceRequirements{
								Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("100m")},
								Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
							},
						},
					},
				},
				Status: api.PodStatus{
					ContainerStatuses: []api.ContainerStatus{
						{
							Name:  "c1",
							Image: "image",
						},
					},
				},
			},
			oldPod: &api.Pod{
				Spec: api.PodSpec{
					Containers: []api.Container{
						{
							Name:  "c1",
							Image: "image",
							Resources: api.ResourceRequirements{
								Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("100m")},
								Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
							},
						},
					},
				},
				Status: api.PodStatus{
					ContainerStatuses: []api.ContainerStatus{
						{
							Name:  "c1",
							Image: "image",
						},
					},
				},
			},
			expectedPod: &api.Pod{
				Spec: api.PodSpec{
					Containers: []api.Container{
						{
							Name:  "c1",
							Image: "image",
							Resources: api.ResourceRequirements{
								Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("100m")},
								Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
							},
						},
					},
				},
				Status: api.PodStatus{
					ContainerStatuses: []api.ContainerStatus{
						{
							Name:  "c1",
							Image: "image",
						},
					},
				},
			},
		},
		{
			desc: "resize desired",
			newPod: &api.Pod{
				Spec: api.PodSpec{
					Containers: []api.Container{
						{
							Name:  "c1",
							Image: "image",
							Resources: api.ResourceRequirements{
								Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("100m")},
								Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
							},
						},
						{
							Name:  "c2",
							Image: "image",
							Resources: api.ResourceRequirements{
								Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("300m")},
								Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("400m")},
							},
						},
					},
				},
				Status: api.PodStatus{
					ContainerStatuses: []api.ContainerStatus{
						{
							Name:               "c1",
							Image:              "image",
							AllocatedResources: api.ResourceList{api.ResourceCPU: resource.MustParse("100m")},
						},
						{
							Name:               "c2",
							Image:              "image",
							AllocatedResources: api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
						},
					},
				},
			},
			oldPod: &api.Pod{
				Spec: api.PodSpec{
					Containers: []api.Container{
						{
							Name:  "c1",
							Image: "image",
							Resources: api.ResourceRequirements{
								Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("100m")},
								Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
							},
						},
						{
							Name:  "c2",
							Image: "image",
							Resources: api.ResourceRequirements{
								Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
								Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("300m")},
							},
						},
					},
				},
				Status: api.PodStatus{
					ContainerStatuses: []api.ContainerStatus{
						{
							Name:               "c1",
							Image:              "image",
							AllocatedResources: api.ResourceList{api.ResourceCPU: resource.MustParse("100m")},
						},
						{
							Name:               "c2",
							Image:              "image",
							AllocatedResources: api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
						},
					},
				},
			},
			expectedPod: &api.Pod{
				Spec: api.PodSpec{
					Containers: []api.Container{
						{
							Name:  "c1",
							Image: "image",
							Resources: api.ResourceRequirements{
								Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("100m")},
								Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
							},
						},
						{
							Name:  "c2",
							Image: "image",
							Resources: api.ResourceRequirements{
								Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("300m")},
								Limits:   api.ResourceList{api.ResourceCPU: resource.MustParse("400m")},
							},
						},
					},
				},
				Status: api.PodStatus{
					Resize: api.PodResizeStatusProposed,
					ContainerStatuses: []api.ContainerStatus{
						{
							Name:               "c1",
							Image:              "image",
							AllocatedResources: api.ResourceList{api.ResourceCPU: resource.MustParse("100m")},
						},
						{
							Name:               "c2",
							Image:              "image",
							AllocatedResources: api.ResourceList{api.ResourceCPU: resource.MustParse("200m")},
						},
					},
				},
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			MarkPodProposedForResize(tc.oldPod, tc.newPod)
			if diff := cmp.Diff(tc.expectedPod, tc.newPod); diff != "" {
				t.Errorf("unexpected pod spec (-want, +got):\n%s", diff)
			}
		})
	}
}
