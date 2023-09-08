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

package resourceclaimtemplate

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/kubernetes/pkg/apis/resource"
)

var resourceClaimTemplate = &resource.ResourceClaimTemplate{
	ObjectMeta: metav1.ObjectMeta{
		Name:      "valid-claim-template",
		Namespace: "default",
	},
	Spec: resource.ResourceClaimTemplateSpec{
		Spec: resource.ResourceClaimSpec{
			ResourceClassName: "valid-class",
			AllocationMode:    resource.AllocationModeImmediate,
		},
	},
}

func TestClaimTemplateStrategy(t *testing.T) {
	if !Strategy.NamespaceScoped() {
		t.Errorf("ResourceClaimTemplate must be namespace scoped")
	}
	if Strategy.AllowCreateOnUpdate() {
		t.Errorf("ResourceClaimTemplate should not allow create on update")
	}
}

func TestClaimTemplateStrategyCreate(t *testing.T) {
	ctx := genericapirequest.NewDefaultContext()
	resourceClaimTemplate := resourceClaimTemplate.DeepCopy()

	Strategy.PrepareForCreate(ctx, resourceClaimTemplate)
	errs := Strategy.Validate(ctx, resourceClaimTemplate)
	if len(errs) != 0 {
		t.Errorf("unexpected error validating for create %v", errs)
	}
}

func TestClaimTemplateStrategyUpdate(t *testing.T) {
	t.Run("no-changes-okay", func(t *testing.T) {
		ctx := genericapirequest.NewDefaultContext()
		resourceClaimTemplate := resourceClaimTemplate.DeepCopy()
		newClaimTemplate := resourceClaimTemplate.DeepCopy()
		newClaimTemplate.ResourceVersion = "4"

		Strategy.PrepareForUpdate(ctx, newClaimTemplate, resourceClaimTemplate)
		errs := Strategy.ValidateUpdate(ctx, newClaimTemplate, resourceClaimTemplate)
		if len(errs) != 0 {
			t.Errorf("unexpected validation errors: %v", errs)
		}
	})

	t.Run("name-change-not-allowed", func(t *testing.T) {
		ctx := genericapirequest.NewDefaultContext()
		resourceClaimTemplate := resourceClaimTemplate.DeepCopy()
		newClaimTemplate := resourceClaimTemplate.DeepCopy()
		newClaimTemplate.Name = "valid-class-2"
		newClaimTemplate.ResourceVersion = "4"

		Strategy.PrepareForUpdate(ctx, newClaimTemplate, resourceClaimTemplate)
		errs := Strategy.ValidateUpdate(ctx, newClaimTemplate, resourceClaimTemplate)
		if len(errs) == 0 {
			t.Errorf("expected a validation error")
		}
	})
}
