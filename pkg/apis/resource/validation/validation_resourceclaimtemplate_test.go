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

package validation

import (
	"testing"

	"github.com/stretchr/testify/assert"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/kubernetes/pkg/apis/resource"
	"k8s.io/utils/pointer"
)

func testClaimTemplate(name, namespace string, spec resource.ResourceClaimSpec) *resource.ResourceClaimTemplate {
	return &resource.ResourceClaimTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: resource.ResourceClaimTemplateSpec{
			Spec: spec,
		},
	}
}

func TestValidateClaimTemplate(t *testing.T) {
	validMode := resource.AllocationModeImmediate
	invalidMode := resource.AllocationMode("invalid")
	goodName := "foo"
	badName := "!@#$%^"
	goodNS := "ns"
	goodClaimSpec := resource.ResourceClaimSpec{
		ResourceClassName: goodName,
		AllocationMode:    validMode,
	}
	now := metav1.Now()
	badValue := "spaces not allowed"

	scenarios := map[string]struct {
		template     *resource.ResourceClaimTemplate
		wantFailures field.ErrorList
	}{
		"good-claim": {
			template: testClaimTemplate(goodName, goodNS, goodClaimSpec),
		},
		"missing-name": {
			wantFailures: field.ErrorList{field.Required(field.NewPath("metadata", "name"), "name or generateName is required")},
			template:     testClaimTemplate("", goodNS, goodClaimSpec),
		},
		"bad-name": {
			wantFailures: field.ErrorList{field.Invalid(field.NewPath("metadata", "name"), badName, "a lowercase RFC 1123 subdomain must consist of lower case alphanumeric characters, '-' or '.', and must start and end with an alphanumeric character (e.g. 'example.com', regex used for validation is '[a-z0-9]([-a-z0-9]*[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*')")},
			template:     testClaimTemplate(badName, goodNS, goodClaimSpec),
		},
		"missing-namespace": {
			wantFailures: field.ErrorList{field.Required(field.NewPath("metadata", "namespace"), "")},
			template:     testClaimTemplate(goodName, "", goodClaimSpec),
		},
		"generate-name": {
			template: func() *resource.ResourceClaimTemplate {
				template := testClaimTemplate(goodName, goodNS, goodClaimSpec)
				template.GenerateName = "pvc-"
				return template
			}(),
		},
		"uid": {
			template: func() *resource.ResourceClaimTemplate {
				template := testClaimTemplate(goodName, goodNS, goodClaimSpec)
				template.UID = "ac051fac-2ead-46d9-b8b4-4e0fbeb7455d"
				return template
			}(),
		},
		"resource-version": {
			template: func() *resource.ResourceClaimTemplate {
				template := testClaimTemplate(goodName, goodNS, goodClaimSpec)
				template.ResourceVersion = "1"
				return template
			}(),
		},
		"generation": {
			template: func() *resource.ResourceClaimTemplate {
				template := testClaimTemplate(goodName, goodNS, goodClaimSpec)
				template.Generation = 100
				return template
			}(),
		},
		"creation-timestamp": {
			template: func() *resource.ResourceClaimTemplate {
				template := testClaimTemplate(goodName, goodNS, goodClaimSpec)
				template.CreationTimestamp = now
				return template
			}(),
		},
		"deletion-grace-period-seconds": {
			template: func() *resource.ResourceClaimTemplate {
				template := testClaimTemplate(goodName, goodNS, goodClaimSpec)
				template.DeletionGracePeriodSeconds = pointer.Int64(10)
				return template
			}(),
		},
		"owner-references": {
			template: func() *resource.ResourceClaimTemplate {
				template := testClaimTemplate(goodName, goodNS, goodClaimSpec)
				template.OwnerReferences = []metav1.OwnerReference{
					{
						APIVersion: "v1",
						Kind:       "pod",
						Name:       "foo",
						UID:        "ac051fac-2ead-46d9-b8b4-4e0fbeb7455d",
					},
				}
				return template
			}(),
		},
		"finalizers": {
			template: func() *resource.ResourceClaimTemplate {
				template := testClaimTemplate(goodName, goodNS, goodClaimSpec)
				template.Finalizers = []string{
					"example.com/foo",
				}
				return template
			}(),
		},
		"managed-fields": {
			template: func() *resource.ResourceClaimTemplate {
				template := testClaimTemplate(goodName, goodNS, goodClaimSpec)
				template.ManagedFields = []metav1.ManagedFieldsEntry{
					{
						FieldsType: "FieldsV1",
						Operation:  "Apply",
						APIVersion: "apps/v1",
						Manager:    "foo",
					},
				}
				return template
			}(),
		},
		"good-labels": {
			template: func() *resource.ResourceClaimTemplate {
				template := testClaimTemplate(goodName, goodNS, goodClaimSpec)
				template.Labels = map[string]string{
					"apps.kubernetes.io/name": "test",
				}
				return template
			}(),
		},
		"bad-labels": {
			wantFailures: field.ErrorList{field.Invalid(field.NewPath("metadata", "labels"), badValue, "a valid label must be an empty string or consist of alphanumeric characters, '-', '_' or '.', and must start and end with an alphanumeric character (e.g. 'MyValue',  or 'my_value',  or '12345', regex used for validation is '(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?')")},
			template: func() *resource.ResourceClaimTemplate {
				template := testClaimTemplate(goodName, goodNS, goodClaimSpec)
				template.Labels = map[string]string{
					"hello-world": badValue,
				}
				return template
			}(),
		},
		"good-annotations": {
			template: func() *resource.ResourceClaimTemplate {
				template := testClaimTemplate(goodName, goodNS, goodClaimSpec)
				template.Annotations = map[string]string{
					"foo": "bar",
				}
				return template
			}(),
		},
		"bad-annotations": {
			wantFailures: field.ErrorList{field.Invalid(field.NewPath("metadata", "annotations"), badName, "name part must consist of alphanumeric characters, '-', '_' or '.', and must start and end with an alphanumeric character (e.g. 'MyName',  or 'my.name',  or '123-abc', regex used for validation is '([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]')")},
			template: func() *resource.ResourceClaimTemplate {
				template := testClaimTemplate(goodName, goodNS, goodClaimSpec)
				template.Annotations = map[string]string{
					badName: "hello world",
				}
				return template
			}(),
		},
		"bad-classname": {
			wantFailures: field.ErrorList{field.Invalid(field.NewPath("spec", "spec", "resourceClassName"), badName, "a lowercase RFC 1123 subdomain must consist of lower case alphanumeric characters, '-' or '.', and must start and end with an alphanumeric character (e.g. 'example.com', regex used for validation is '[a-z0-9]([-a-z0-9]*[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*')")},
			template: func() *resource.ResourceClaimTemplate {
				template := testClaimTemplate(goodName, goodNS, goodClaimSpec)
				template.Spec.Spec.ResourceClassName = badName
				return template
			}(),
		},
		"bad-mode": {
			wantFailures: field.ErrorList{field.NotSupported(field.NewPath("spec", "spec", "allocationMode"), invalidMode, supportedAllocationModes.List())},
			template: func() *resource.ResourceClaimTemplate {
				template := testClaimTemplate(goodName, goodNS, goodClaimSpec)
				template.Spec.Spec.AllocationMode = invalidMode
				return template
			}(),
		},
		"good-parameters": {
			template: func() *resource.ResourceClaimTemplate {
				template := testClaimTemplate(goodName, goodNS, goodClaimSpec)
				template.Spec.Spec.ParametersRef = &resource.ResourceClaimParametersReference{
					Kind: "foo",
					Name: "bar",
				}
				return template
			}(),
		},
		"missing-parameters-kind": {
			wantFailures: field.ErrorList{field.Required(field.NewPath("spec", "spec", "parametersRef", "kind"), "")},
			template: func() *resource.ResourceClaimTemplate {
				template := testClaimTemplate(goodName, goodNS, goodClaimSpec)
				template.Spec.Spec.ParametersRef = &resource.ResourceClaimParametersReference{
					Name: "bar",
				}
				return template
			}(),
		},
		"missing-parameters-name": {
			wantFailures: field.ErrorList{field.Required(field.NewPath("spec", "spec", "parametersRef", "name"), "")},
			template: func() *resource.ResourceClaimTemplate {
				template := testClaimTemplate(goodName, goodNS, goodClaimSpec)
				template.Spec.Spec.ParametersRef = &resource.ResourceClaimParametersReference{
					Kind: "foo",
				}
				return template
			}(),
		},
	}

	for name, scenario := range scenarios {
		t.Run(name, func(t *testing.T) {
			errs := ValidateClaimTemplate(scenario.template)
			assert.Equal(t, scenario.wantFailures, errs)
		})
	}
}

func TestValidateClaimTemplateUpdate(t *testing.T) {
	name := "valid"
	parameters := &resource.ResourceClaimParametersReference{
		Kind: "foo",
		Name: "bar",
	}
	validClaimTemplate := testClaimTemplate("foo", "ns", resource.ResourceClaimSpec{
		ResourceClassName: name,
		AllocationMode:    resource.AllocationModeImmediate,
		ParametersRef:     parameters,
	})

	scenarios := map[string]struct {
		oldClaimTemplate *resource.ResourceClaimTemplate
		update           func(claim *resource.ResourceClaimTemplate) *resource.ResourceClaimTemplate
		wantFailures     field.ErrorList
	}{
		"valid-no-op-update": {
			oldClaimTemplate: validClaimTemplate,
			update:           func(claim *resource.ResourceClaimTemplate) *resource.ResourceClaimTemplate { return claim },
		},
		"invalid-update-class": {
			wantFailures: field.ErrorList{field.Invalid(field.NewPath("spec"), func() resource.ResourceClaimTemplateSpec {
				spec := validClaimTemplate.Spec.DeepCopy()
				spec.Spec.ResourceClassName += "2"
				return *spec
			}(), "field is immutable")},
			oldClaimTemplate: validClaimTemplate,
			update: func(template *resource.ResourceClaimTemplate) *resource.ResourceClaimTemplate {
				template.Spec.Spec.ResourceClassName += "2"
				return template
			},
		},
		"invalid-update-remove-parameters": {
			wantFailures: field.ErrorList{field.Invalid(field.NewPath("spec"), func() resource.ResourceClaimTemplateSpec {
				spec := validClaimTemplate.Spec.DeepCopy()
				spec.Spec.ParametersRef = nil
				return *spec
			}(), "field is immutable")},
			oldClaimTemplate: validClaimTemplate,
			update: func(template *resource.ResourceClaimTemplate) *resource.ResourceClaimTemplate {
				template.Spec.Spec.ParametersRef = nil
				return template
			},
		},
		"invalid-update-mode": {
			wantFailures: field.ErrorList{field.Invalid(field.NewPath("spec"), func() resource.ResourceClaimTemplateSpec {
				spec := validClaimTemplate.Spec.DeepCopy()
				spec.Spec.AllocationMode = resource.AllocationModeWaitForFirstConsumer
				return *spec
			}(), "field is immutable")},
			oldClaimTemplate: validClaimTemplate,
			update: func(template *resource.ResourceClaimTemplate) *resource.ResourceClaimTemplate {
				template.Spec.Spec.AllocationMode = resource.AllocationModeWaitForFirstConsumer
				return template
			},
		},
	}

	for name, scenario := range scenarios {
		t.Run(name, func(t *testing.T) {
			scenario.oldClaimTemplate.ResourceVersion = "1"
			errs := ValidateClaimTemplateUpdate(scenario.update(scenario.oldClaimTemplate.DeepCopy()), scenario.oldClaimTemplate)
			assert.Equal(t, scenario.wantFailures, errs)
		})
	}
}
