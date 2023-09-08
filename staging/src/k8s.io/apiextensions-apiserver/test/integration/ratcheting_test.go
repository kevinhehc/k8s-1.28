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

package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	jsonpatch "github.com/evanphx/json-patch"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apiextensions-apiserver/pkg/features"
	"k8s.io/apiextensions-apiserver/test/integration/fixtures"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/dynamic"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
)

var stringSchema *apiextensionsv1.JSONSchemaProps = &apiextensionsv1.JSONSchemaProps{
	Type: "string",
}

var stringMapSchema *apiextensionsv1.JSONSchemaProps = &apiextensionsv1.JSONSchemaProps{
	Type: "object",
	AdditionalProperties: &apiextensionsv1.JSONSchemaPropsOrBool{
		Schema: stringSchema,
	},
}

var numberSchema *apiextensionsv1.JSONSchemaProps = &apiextensionsv1.JSONSchemaProps{
	Type: "integer",
}

var numbersMapSchema *apiextensionsv1.JSONSchemaProps = &apiextensionsv1.JSONSchemaProps{
	Type: "object",
	AdditionalProperties: &apiextensionsv1.JSONSchemaPropsOrBool{
		Schema: numberSchema,
	},
}

type ratchetingTestContext struct {
	*testing.T
	DynamicClient       dynamic.Interface
	APIExtensionsClient clientset.Interface
}

type ratchetingTestOperation interface {
	Do(ctx *ratchetingTestContext) error
	Description() string
}

type expectError struct {
	op ratchetingTestOperation
}

func (e expectError) Do(ctx *ratchetingTestContext) error {
	err := e.op.Do(ctx)
	if err != nil {
		return nil
	}
	return errors.New("expected error")
}

func (e expectError) Description() string {
	return fmt.Sprintf("Expect Error: %v", e.op.Description())
}

// apiextensions-apiserver has discovery disabled, so hardcode this mapping
var fakeRESTMapper map[schema.GroupVersionResource]string = map[schema.GroupVersionResource]string{
	myCRDV1Beta1: "MyCoolCRD",
}

type applyPatchOperation struct {
	description string
	gvr         schema.GroupVersionResource
	name        string
	patch       map[string]interface{}
}

func (a applyPatchOperation) Do(ctx *ratchetingTestContext) error {
	// Lookup GVK from discovery
	kind, ok := fakeRESTMapper[a.gvr]
	if !ok {
		return fmt.Errorf("no mapping found for Gvr %v, add entry to fakeRESTMapper", a.gvr)
	}

	a.patch["kind"] = kind
	a.patch["apiVersion"] = a.gvr.GroupVersion().String()

	if meta, ok := a.patch["metadata"]; ok {
		mObj := meta.(map[string]interface{})
		mObj["name"] = a.name
		mObj["namespace"] = "default"
	} else {
		a.patch["metadata"] = map[string]interface{}{
			"name":      a.name,
			"namespace": "default",
		}
	}

	_, err := ctx.DynamicClient.Resource(a.gvr).Namespace("default").Apply(context.TODO(), a.name, &unstructured.Unstructured{
		Object: a.patch,
	}, metav1.ApplyOptions{
		FieldManager: "manager",
	})

	return err

}

func (a applyPatchOperation) Description() string {
	return a.description
}

// Replaces schema used for v1beta1 of crd
type updateMyCRDV1Beta1Schema struct {
	newSchema *apiextensionsv1.JSONSchemaProps
}

func (u updateMyCRDV1Beta1Schema) Do(ctx *ratchetingTestContext) error {
	var myCRD *apiextensionsv1.CustomResourceDefinition
	var err error = apierrors.NewConflict(schema.GroupResource{}, "", nil)
	for apierrors.IsConflict(err) {
		myCRD, err = ctx.APIExtensionsClient.ApiextensionsV1().CustomResourceDefinitions().Get(context.TODO(), myCRDV1Beta1.Resource+"."+myCRDV1Beta1.Group, metav1.GetOptions{})
		if err != nil {
			return err
		}

		// Insert a sentinel property that we can probe to detect when the
		// schema takes effect
		sch := u.newSchema.DeepCopy()
		if sch.Properties == nil {
			sch.Properties = map[string]apiextensionsv1.JSONSchemaProps{}
		}

		uuidString := string(uuid.NewUUID())
		// UUID string is just hex separated by dashes, which is safe to
		// throw into regex like this
		pattern := "^" + uuidString + "$"
		sentinelName := "__ratcheting_sentinel_field__"
		sch.Properties[sentinelName] = apiextensionsv1.JSONSchemaProps{
			Type:    "string",
			Pattern: pattern,

			// Put MaxLength condition inside AllOf since the string_validator
			// in kube-openapi short circuits upon seeing MaxLength, and we
			// want both pattern and MaxLength errors
			AllOf: []apiextensionsv1.JSONSchemaProps{
				{
					MinLength: ptr((int64(1))), // 1 MinLength to prevent empty value from ever being admitted
					MaxLength: ptr((int64(0))), // 0 MaxLength to prevent non-empty value from ever being admitted
				},
			},
		}

		for _, v := range myCRD.Spec.Versions {
			if v.Name != myCRDV1Beta1.Version {
				continue
			}
			v.Schema.OpenAPIV3Schema = sch
		}

		_, err = ctx.APIExtensionsClient.ApiextensionsV1().CustomResourceDefinitions().Update(context.TODO(), myCRD, metav1.UpdateOptions{
			FieldManager: "manager",
		})
		if err != nil {
			return err
		}

		// Keep trying to create an invalid instance of the CRD until we
		// get an error containing the ResourceVersion we are looking for
		//
		counter := 0
		return wait.PollUntilContextCancel(context.TODO(), 100*time.Millisecond, true, func(_ context.Context) (done bool, err error) {
			counter += 1
			err = applyPatchOperation{
				gvr:  myCRDV1Beta1,
				name: "sentinel-resource",
				patch: map[string]interface{}{
					// Just keep using different values
					sentinelName: fmt.Sprintf("invalid %v %v", uuidString, counter),
				}}.Do(ctx)

			if err == nil {
				return false, errors.New("expected error when creating sentinel resource")
			}

			// Check to see if the returned error message contains our
			// unique string. UUID should be unique enough to just check
			// simple existence in the error.
			if strings.Contains(err.Error(), uuidString) {
				return true, nil
			}

			return false, nil
		})
	}
	return err
}

func (u updateMyCRDV1Beta1Schema) Description() string {
	return "Update CRD schema"
}

type patchMyCRDV1Beta1Schema struct {
	description string
	patch       map[string]interface{}
}

func (p patchMyCRDV1Beta1Schema) Do(ctx *ratchetingTestContext) error {
	var err error
	patchJSON, err := json.Marshal(p.patch)
	if err != nil {
		return err
	}

	myCRD, err := ctx.APIExtensionsClient.ApiextensionsV1().CustomResourceDefinitions().Get(context.TODO(), myCRDV1Beta1.Resource+"."+myCRDV1Beta1.Group, metav1.GetOptions{})
	if err != nil {
		return err
	}

	for _, v := range myCRD.Spec.Versions {
		if v.Name != myCRDV1Beta1.Version {
			continue
		}

		jsonSchema, err := json.Marshal(v.Schema.OpenAPIV3Schema)
		if err != nil {
			return err
		}

		merged, err := jsonpatch.MergePatch(jsonSchema, patchJSON)
		if err != nil {
			return err
		}

		var parsed apiextensionsv1.JSONSchemaProps
		if err := json.Unmarshal(merged, &parsed); err != nil {
			return err
		}

		return updateMyCRDV1Beta1Schema{
			newSchema: &parsed,
		}.Do(ctx)
	}

	return fmt.Errorf("could not find version %v in CRD %v", myCRDV1Beta1.Version, myCRD.Name)
}

func (p patchMyCRDV1Beta1Schema) Description() string {
	return p.description
}

type ratchetingTestCase struct {
	Name       string
	Disabled   bool
	Operations []ratchetingTestOperation
}

func runTests(t *testing.T, cases []ratchetingTestCase) {
	defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.CRDValidationRatcheting, true)()
	tearDown, apiExtensionClient, dynamicClient, err := fixtures.StartDefaultServerWithClients(t)
	if err != nil {
		t.Fatal(err)
	}
	defer tearDown()

	group := myCRDV1Beta1.Group
	version := myCRDV1Beta1.Version
	resource := myCRDV1Beta1.Resource
	kind := fakeRESTMapper[myCRDV1Beta1]

	myCRD := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: resource + "." + group},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: group,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name:    version,
				Served:  true,
				Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
						Type: "object",
						Properties: map[string]apiextensionsv1.JSONSchemaProps{
							"content": {
								Type: "object",
								AdditionalProperties: &apiextensionsv1.JSONSchemaPropsOrBool{
									Schema: &apiextensionsv1.JSONSchemaProps{
										Type: "string",
									},
								},
							},
							"num": {
								Type: "object",
								AdditionalProperties: &apiextensionsv1.JSONSchemaPropsOrBool{
									Schema: &apiextensionsv1.JSONSchemaProps{
										Type: "integer",
									},
								},
							},
						},
					},
				},
			}},
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural:   resource,
				Kind:     kind,
				ListKind: kind + "List",
			},
			Scope: apiextensionsv1.NamespaceScoped,
		},
	}

	_, err = fixtures.CreateNewV1CustomResourceDefinition(myCRD, apiExtensionClient, dynamicClient)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		if c.Disabled {
			continue
		}

		t.Run(c.Name, func(t *testing.T) {
			ctx := &ratchetingTestContext{
				T:                   t,
				DynamicClient:       dynamicClient,
				APIExtensionsClient: apiExtensionClient,
			}

			for i, op := range c.Operations {
				t.Logf("Performing Operation: %v", op.Description())
				if err := op.Do(ctx); err != nil {
					t.Fatalf("failed %T operation %v: %v\n%v", op, i, err, op)
				}
			}

			// Reset resources
			err := ctx.DynamicClient.Resource(myCRDV1Beta1).Namespace("default").DeleteCollection(context.TODO(), metav1.DeleteOptions{}, metav1.ListOptions{})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

var myCRDV1Beta1 schema.GroupVersionResource = schema.GroupVersionResource{
	Group:    "mygroup.example.com",
	Version:  "v1beta1",
	Resource: "mycrds",
}

var myCRDInstanceName string = "mycrdinstance"

func TestRatchetingFunctionality(t *testing.T) {
	cases := []ratchetingTestCase{
		{
			Name: "Minimum Maximum",
			Operations: []ratchetingTestOperation{
				updateMyCRDV1Beta1Schema{&apiextensionsv1.JSONSchemaProps{
					Type: "object",
					Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"hasMinimum":           *numberSchema,
						"hasMaximum":           *numberSchema,
						"hasMinimumAndMaximum": *numberSchema,
					},
				}},
				applyPatchOperation{
					"Create an object that complies with the schema",
					myCRDV1Beta1,
					myCRDInstanceName,
					map[string]interface{}{
						"hasMinimum":           0,
						"hasMaximum":           1000,
						"hasMinimumAndMaximum": 50,
					}},
				patchMyCRDV1Beta1Schema{
					"Add stricter minimums and maximums that violate the previous object",
					map[string]interface{}{
						"properties": map[string]interface{}{
							"hasMinimum": map[string]interface{}{
								"minimum": 10,
							},
							"hasMaximum": map[string]interface{}{
								"maximum": 20,
							},
							"hasMinimumAndMaximum": map[string]interface{}{
								"minimum": 10,
								"maximum": 20,
							},
							"noRestrictions": map[string]interface{}{
								"type": "integer",
							},
						},
					}},
				applyPatchOperation{
					"Add new fields that validates successfully without changing old ones",
					myCRDV1Beta1,
					myCRDInstanceName,
					map[string]interface{}{
						"noRestrictions": 50,
					}},
				expectError{
					applyPatchOperation{
						"Change a single old field to be invalid",
						myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
							"hasMinimum": 5,
						}},
				},
				expectError{
					applyPatchOperation{
						"Change multiple old fields to be invalid",
						myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
							"hasMinimum": 5,
							"hasMaximum": 21,
						}},
				},
				applyPatchOperation{
					"Change single old field to be valid",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"hasMinimum": 11,
					}},
				applyPatchOperation{
					"Change multiple old fields to be valid",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"hasMaximum":           19,
						"hasMinimumAndMaximum": 15,
					}},
			},
		},
		{
			Name: "Enum",
			Operations: []ratchetingTestOperation{
				// Create schema with some enum element
				updateMyCRDV1Beta1Schema{&apiextensionsv1.JSONSchemaProps{
					Type: "object",
					Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"enumField": *stringSchema,
					},
				}},
				applyPatchOperation{
					"Create an instance with a soon-to-be-invalid value",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"enumField": "okValueNowBadValueLater",
					}},
				patchMyCRDV1Beta1Schema{
					"restrict `enumField` to an enum of A, B, or C",
					map[string]interface{}{
						"properties": map[string]interface{}{
							"enumField": map[string]interface{}{
								"enum": []interface{}{
									"A", "B", "C",
								},
							},
							"otherField": map[string]interface{}{
								"type": "string",
							},
						},
					}},
				applyPatchOperation{
					"An invalid patch with no changes is a noop",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"enumField": "okValueNowBadValueLater",
					}},
				applyPatchOperation{
					"Add a new field, and include old value in our patch",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"enumField":  "okValueNowBadValueLater",
						"otherField": "anythingGoes",
					}},
				expectError{
					applyPatchOperation{
						"Set enumField to invalid value D",
						myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
							"enumField": "D",
						}},
				},
				applyPatchOperation{
					"Set to a valid value",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"enumField": "A",
					}},
				expectError{
					applyPatchOperation{
						"After setting a valid value, return to the old, accepted value",
						myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
							"enumField": "okValueNowBadValueLater",
						}},
				},
			},
		},
		{
			Name: "AdditionalProperties",
			Operations: []ratchetingTestOperation{
				updateMyCRDV1Beta1Schema{&apiextensionsv1.JSONSchemaProps{
					Type: "object",
					Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"nums":    *numbersMapSchema,
						"content": *stringMapSchema,
					},
				}},
				applyPatchOperation{
					"Create an instance",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"nums": map[string]interface{}{
							"num1": 1,
							"num2": 1000000,
						},
						"content": map[string]interface{}{
							"k1": "some content",
							"k2": "other content",
						},
					}},
				patchMyCRDV1Beta1Schema{
					"set minimum value for fields with additionalProperties",
					map[string]interface{}{
						"properties": map[string]interface{}{
							"nums": map[string]interface{}{
								"additionalProperties": map[string]interface{}{
									"minimum": 1000,
								},
							},
						},
					}},
				applyPatchOperation{
					"updating validating field num2 to another validating value, but rachet invalid field num1",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"nums": map[string]interface{}{
							"num1": 1,
							"num2": 2000,
						},
					}},
				expectError{applyPatchOperation{
					"update field num1 to different invalid value",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"nums": map[string]interface{}{
							"num1": 2,
							"num2": 2000,
						},
					}}},
			},
		},
		{
			Name: "MinProperties MaxProperties",
			Operations: []ratchetingTestOperation{
				updateMyCRDV1Beta1Schema{&apiextensionsv1.JSONSchemaProps{
					Type: "object",
					Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"restricted": {
							Type: "object",
							AdditionalProperties: &apiextensionsv1.JSONSchemaPropsOrBool{
								Schema: stringSchema,
							},
						},
						"unrestricted": {
							Type: "object",
							AdditionalProperties: &apiextensionsv1.JSONSchemaPropsOrBool{
								Schema: stringSchema,
							},
						},
					},
				}},
				applyPatchOperation{
					"Create instance",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"restricted": map[string]interface{}{
							"key1": "hi",
							"key2": "there",
						},
					}},
				patchMyCRDV1Beta1Schema{
					"set both minProperties and maxProperties to 1 to violate the previous object",
					map[string]interface{}{
						"properties": map[string]interface{}{
							"restricted": map[string]interface{}{
								"minProperties": 1,
								"maxProperties": 1,
							},
						},
					}},
				applyPatchOperation{
					"ratchet violating object 'restricted' around changes to unrelated field",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"restricted": map[string]interface{}{
							"key1": "hi",
							"key2": "there",
						},
						"unrestricted": map[string]interface{}{
							"key1": "yo",
						},
					}},
				expectError{applyPatchOperation{
					"make invalid changes to previously ratcheted invalid field",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"restricted": map[string]interface{}{
							"key1": "changed",
							"key2": "there",
						},
						"unrestricted": map[string]interface{}{
							"key1": "yo",
						},
					}}},

				patchMyCRDV1Beta1Schema{
					"remove maxProeprties, set minProperties to 2",
					map[string]interface{}{
						"properties": map[string]interface{}{
							"restricted": map[string]interface{}{
								"minProperties": 2,
								"maxProperties": nil,
							},
						},
					}},
				applyPatchOperation{
					"a new value",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"restricted": map[string]interface{}{
							"key1": "hi",
							"key2": "there",
							"key3": "buddy",
						},
					}},

				expectError{applyPatchOperation{
					"violate new validation by removing keys",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"restricted": map[string]interface{}{
							"key1": "hi",
							"key2": nil,
							"key3": nil,
						},
					}}},
				patchMyCRDV1Beta1Schema{
					"remove minProperties, set maxProperties to 1",
					map[string]interface{}{
						"properties": map[string]interface{}{
							"restricted": map[string]interface{}{
								"minProperties": nil,
								"maxProperties": 1,
							},
						},
					}},
				applyPatchOperation{
					"modify only the other key, ratcheting maxProperties for field `restricted`",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"restricted": map[string]interface{}{
							"key1": "hi",
							"key2": "there",
							"key3": "buddy",
						},
						"unrestricted": map[string]interface{}{
							"key1": "value",
							"key2": "value",
						},
					}},
				expectError{
					applyPatchOperation{
						"modifying one value in the object with maxProperties restriction, but keeping old fields",
						myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
							"restricted": map[string]interface{}{
								"key1": "hi",
								"key2": "theres",
								"key3": "buddy",
							},
						}}},
			},
		},
		{
			Name: "MinItems",
			Operations: []ratchetingTestOperation{
				updateMyCRDV1Beta1Schema{&apiextensionsv1.JSONSchemaProps{
					Type: "object",
					Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"field": *stringSchema,
						"array": {
							Type: "array",
							Items: &apiextensionsv1.JSONSchemaPropsOrArray{
								Schema: stringSchema,
							},
						},
					},
				}},
				applyPatchOperation{
					"Create instance",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"array": []interface{}{"value1", "value2", "value3"},
					}},
				patchMyCRDV1Beta1Schema{
					"change minItems on array to 10, invalidates previous object",
					map[string]interface{}{
						"properties": map[string]interface{}{
							"array": map[string]interface{}{
								"minItems": 10,
							},
						},
					}},
				applyPatchOperation{
					"keep invalid field `array` unchanged, add new field with ratcheting",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"array": []interface{}{"value1", "value2", "value3"},
						"field": "value",
					}},
				expectError{
					applyPatchOperation{
						"modify array element without satisfying property",
						myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
							"array": []interface{}{"value2", "value2", "value3"},
						}}},

				expectError{
					applyPatchOperation{
						"add array element without satisfying proeprty",
						myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
							"array": []interface{}{"value1", "value2", "value3", "value4"},
						}}},

				applyPatchOperation{
					"make array valid",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"array": []interface{}{"value1", "value2", "value3", "4", "5", "6", "7", "8", "9", "10"},
					}},
				expectError{
					applyPatchOperation{
						"revert to original value",
						myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
							"array": []interface{}{"value1", "value2", "value3"},
						}}},
			},
		},
		{
			Name: "MaxItems",
			Operations: []ratchetingTestOperation{
				updateMyCRDV1Beta1Schema{&apiextensionsv1.JSONSchemaProps{
					Type: "object",
					Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"field": *stringSchema,
						"array": {
							Type: "array",
							Items: &apiextensionsv1.JSONSchemaPropsOrArray{
								Schema: stringSchema,
							},
						},
					},
				}},
				applyPatchOperation{
					"create instance",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"array": []interface{}{"value1", "value2", "value3"},
					}},
				patchMyCRDV1Beta1Schema{
					"change maxItems on array to 1, invalidates previous object",
					map[string]interface{}{
						"properties": map[string]interface{}{
							"array": map[string]interface{}{
								"maxItems": 1,
							},
						},
					}},
				applyPatchOperation{
					"ratchet old value of array through an update to another field",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"array": []interface{}{"value1", "value2", "value3"},
						"field": "value",
					}},
				expectError{
					applyPatchOperation{
						"modify array element without satisfying property",
						myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
							"array": []interface{}{"value2", "value2", "value3"},
						}}},

				expectError{
					applyPatchOperation{
						"remove array element without satisfying proeprty",
						myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
							"array": []interface{}{"value1", "value2"},
						}}},

				applyPatchOperation{
					"change array to valid value that satisfies maxItems",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"array": []interface{}{"value1"},
					}},
				expectError{
					applyPatchOperation{
						"revert to previous invalid ratcheted value",
						myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
							"array": []interface{}{"value1", "value2", "value3"},
						}}},
			},
		},
		{
			Name: "MinLength MaxLength",
			Operations: []ratchetingTestOperation{
				updateMyCRDV1Beta1Schema{&apiextensionsv1.JSONSchemaProps{
					Type: "object",
					Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"minField":   *stringSchema,
						"maxField":   *stringSchema,
						"otherField": *stringSchema,
					},
				}},
				applyPatchOperation{
					"create instance",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"minField": "value",
						"maxField": "valueThatsVeryLongSee",
					}},
				patchMyCRDV1Beta1Schema{
					"set minField maxLength to 10, and maxField's minLength to 15",
					map[string]interface{}{
						"properties": map[string]interface{}{
							"minField": map[string]interface{}{
								"minLength": 10,
							},
							"maxField": map[string]interface{}{
								"maxLength": 15,
							},
						},
					}},
				applyPatchOperation{
					"add new field `otherField`, ratcheting `minField` and `maxField`",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"minField":   "value",
						"maxField":   "valueThatsVeryLongSee",
						"otherField": "otherValue",
					}},
				applyPatchOperation{
					"make minField valid, ratcheting old value for maxField",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"minField":   "valuelength13",
						"maxField":   "valueThatsVeryLongSee",
						"otherField": "otherValue",
					}},
				applyPatchOperation{
					"make maxField shorter",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"maxField": "l2",
					}},
				expectError{
					applyPatchOperation{
						"make maxField too long",
						myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
							"maxField": "valuewithlength17",
						}}},
				expectError{
					applyPatchOperation{
						"revert minFIeld to previously ratcheted value",
						myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
							"minField": "value",
						}}},
				expectError{
					applyPatchOperation{
						"revert maxField to previously ratcheted value",
						myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
							"maxField": "valueThatsVeryLongSee",
						}}},
			},
		},
		{
			Name: "Pattern",
			Operations: []ratchetingTestOperation{
				updateMyCRDV1Beta1Schema{&apiextensionsv1.JSONSchemaProps{
					Type: "object",
					Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"field": *stringSchema,
					},
				}},
				applyPatchOperation{
					"create instance",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"field": "doesnt abide pattern",
					}},
				patchMyCRDV1Beta1Schema{
					"add pattern validation on `field`",
					map[string]interface{}{
						"properties": map[string]interface{}{
							"field": map[string]interface{}{
								"pattern": "^[1-9]+$",
							},
							"otherField": map[string]interface{}{
								"type": "string",
							},
						},
					}},
				applyPatchOperation{
					"add unrelated field, ratcheting old invalid field",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"field":      "doesnt abide pattern",
						"otherField": "added",
					}},
				expectError{applyPatchOperation{
					"change field to invalid value",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"field":      "w123",
						"otherField": "added",
					}}},
				applyPatchOperation{
					"change field to a valid value",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"field":      "123",
						"otherField": "added",
					}},
			},
		},
		{
			Name: "Format Addition and Change",
			Operations: []ratchetingTestOperation{
				updateMyCRDV1Beta1Schema{&apiextensionsv1.JSONSchemaProps{
					Type: "object",
					Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"field": *stringSchema,
					},
				}},
				applyPatchOperation{
					"create instance",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"field": "doesnt abide any format",
					}},
				patchMyCRDV1Beta1Schema{
					"change `field`'s format to `byte",
					map[string]interface{}{
						"properties": map[string]interface{}{
							"field": map[string]interface{}{
								"format": "byte",
							},
							"otherField": map[string]interface{}{
								"type": "string",
							},
						},
					}},
				applyPatchOperation{
					"add unrelated otherField, ratchet invalid old field format",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"field":      "doesnt abide any format",
						"otherField": "value",
					}},
				expectError{applyPatchOperation{
					"change field to an invalid string",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"field": "asd",
					}}},
				applyPatchOperation{
					"change field to a valid byte string",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"field": "dGhpcyBpcyBwYXNzd29yZA==",
					}},
				patchMyCRDV1Beta1Schema{
					"change `field`'s format to date-time",
					map[string]interface{}{
						"properties": map[string]interface{}{
							"field": map[string]interface{}{
								"format": "date-time",
							},
						},
					}},
				applyPatchOperation{
					"change otherField, ratchet `field`'s invalid byte format",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"field":      "dGhpcyBpcyBwYXNzd29yZA==",
						"otherField": "value2",
					}},
				applyPatchOperation{
					"change `field` to a valid value",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"field":      "2018-11-13T20:20:39+00:00",
						"otherField": "value2",
					}},
				expectError{
					applyPatchOperation{
						"revert `field` to previously ratcheted value",
						myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
							"field":      "dGhpcyBpcyBwYXNzd29yZA==",
							"otherField": "value2",
						}}},
				expectError{
					applyPatchOperation{
						"revert `field` to its initial value from creation",
						myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
							"field": "doesnt abide any format",
						}}},
			},
		},
		{
			Name: "Map Type List Reordering Grandfathers Invalid Key",
			Operations: []ratchetingTestOperation{
				updateMyCRDV1Beta1Schema{&apiextensionsv1.JSONSchemaProps{
					Type: "object",
					Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"field": {
							Type:         "array",
							XListType:    ptr("map"),
							XListMapKeys: []string{"name", "port"},
							Items: &apiextensionsv1.JSONSchemaPropsOrArray{
								Schema: &apiextensionsv1.JSONSchemaProps{
									Type:     "object",
									Required: []string{"name", "port"},
									Properties: map[string]apiextensionsv1.JSONSchemaProps{
										"name":  *stringSchema,
										"port":  *numberSchema,
										"field": *stringSchema,
									},
								},
							},
						},
					},
				}},
				applyPatchOperation{
					"create instance with three soon-to-be-invalid keys",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"field": []interface{}{
							map[string]interface{}{
								"name":  "nginx",
								"port":  443,
								"field": "value",
							},
							map[string]interface{}{
								"name":  "etcd",
								"port":  2379,
								"field": "value",
							},
							map[string]interface{}{
								"name":  "kube-apiserver",
								"port":  6443,
								"field": "value",
							},
						},
					}},
				patchMyCRDV1Beta1Schema{
					"set `field`'s maxItems to 2, which is exceeded by all of previous object's elements",
					map[string]interface{}{
						"properties": map[string]interface{}{
							"field": map[string]interface{}{
								"maxItems": 2,
							},
						},
					}},
				applyPatchOperation{
					"reorder invalid objects which have too many properties, but do not modify them or change keys",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"field": []interface{}{
							map[string]interface{}{
								"name":  "kube-apiserver",
								"port":  6443,
								"field": "value",
							},
							map[string]interface{}{
								"name":  "nginx",
								"port":  443,
								"field": "value",
							},
							map[string]interface{}{
								"name":  "etcd",
								"port":  2379,
								"field": "value",
							},
						},
					}},
				expectError{
					applyPatchOperation{
						"attempt to change one of the fields of the items which exceed maxItems",
						myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
							"field": []interface{}{
								map[string]interface{}{
									"name":  "kube-apiserver",
									"port":  6443,
									"field": "value",
								},
								map[string]interface{}{
									"name":  "nginx",
									"port":  443,
									"field": "value",
								},
								map[string]interface{}{
									"name":  "etcd",
									"port":  2379,
									"field": "value",
								},
								map[string]interface{}{
									"name":  "dev",
									"port":  8080,
									"field": "value",
								},
							},
						}}},
				patchMyCRDV1Beta1Schema{
					"Require even numbered port in key, remove maxItems requirement",
					map[string]interface{}{
						"properties": map[string]interface{}{
							"field": map[string]interface{}{
								"maxItems": nil,
								"items": map[string]interface{}{
									"properties": map[string]interface{}{
										"port": map[string]interface{}{
											"multipleOf": 2,
										},
									},
								},
							},
						},
					}},

				applyPatchOperation{
					"reorder fields without changing anything",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"field": []interface{}{
							map[string]interface{}{
								"name":  "nginx",
								"port":  443,
								"field": "value",
							},
							map[string]interface{}{
								"name":  "etcd",
								"port":  2379,
								"field": "value",
							},
							map[string]interface{}{
								"name":  "kube-apiserver",
								"port":  6443,
								"field": "value",
							},
						},
					}},

				applyPatchOperation{
					`use "invalid" keys despite changing order and changing sibling fields to the key`,
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"field": []interface{}{
							map[string]interface{}{
								"name":  "nginx",
								"port":  443,
								"field": "value",
							},
							map[string]interface{}{
								"name":  "etcd",
								"port":  2379,
								"field": "value",
							},
							map[string]interface{}{
								"name":  "kube-apiserver",
								"port":  6443,
								"field": "this is a changed value for an an invalid but grandfathered key",
							},
							map[string]interface{}{
								"name":  "dev",
								"port":  8080,
								"field": "value",
							},
						},
					}},
			},
		},
		{
			Name: "ArrayItems correlate by index",
			Operations: []ratchetingTestOperation{
				updateMyCRDV1Beta1Schema{&apiextensionsv1.JSONSchemaProps{
					Type: "object",
					Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"values": {
							Type: "array",
							Items: &apiextensionsv1.JSONSchemaPropsOrArray{
								Schema: stringMapSchema,
							},
						},
						"otherField": *stringSchema,
					},
				}},
				applyPatchOperation{
					"create instance with length 5 values",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"values": []interface{}{
							map[string]interface{}{
								"name": "1",
								"key":  "value",
							},
							map[string]interface{}{
								"name": "2",
								"key":  "value",
							},
						},
					}},
				patchMyCRDV1Beta1Schema{
					"Set minimum length of 6 for values of elements in the items array",
					map[string]interface{}{
						"properties": map[string]interface{}{
							"values": map[string]interface{}{
								"items": map[string]interface{}{
									"additionalProperties": map[string]interface{}{
										"minLength": 6,
									},
								},
							},
						},
					}},
				expectError{
					applyPatchOperation{
						"change value to one that exceeds minLength",
						myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
							"values": []interface{}{
								map[string]interface{}{
									"name": "1",
									"key":  "value",
								},
								map[string]interface{}{
									"name": "2",
									"key":  "bad",
								},
							},
						}}},
				applyPatchOperation{
					"add new fields without touching the map",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"values": []interface{}{
							map[string]interface{}{
								"name": "1",
								"key":  "value",
							},
							map[string]interface{}{
								"name": "2",
								"key":  "value",
							},
						},
						"otherField": "hello world",
					}},
				// (This test shows an array can be correlated by index with its old value)
				applyPatchOperation{
					"add new, valid fields to elements of the array, ratcheting unchanged old fields within the array elements by correlating by index",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"values": []interface{}{
							map[string]interface{}{
								"name": "1",
								"key":  "value",
							},
							map[string]interface{}{
								"name": "2",
								"key":  "value",
								"key2": "valid value",
							},
						},
					}},
				expectError{
					applyPatchOperation{
						"reorder the array, preventing index correlation",
						myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
							"values": []interface{}{
								map[string]interface{}{
									"name": "2",
									"key":  "value",
									"key2": "valid value",
								},
								map[string]interface{}{
									"name": "1",
									"key":  "value",
								},
							},
						}}},
			},
		},
		// Features that should not ratchet
		{
			Name: "AllOf_should_not_ratchet",
		},
		{
			Name: "OneOf_should_not_ratchet",
		},
		{
			Name: "AnyOf_should_not_ratchet",
		},
		{
			Name: "Not_should_not_ratchet",
		},
		{
			Name: "CEL_transition_rules_should_not_ratchet",
		},
		// Future Functionality, disabled tests
		{
			Name: "CEL Add Change Rule",
			// Planned future test. CEL Rules are not yet ratcheted in alpha
			// implementation of CRD Validation Ratcheting
			Disabled: true,
			Operations: []ratchetingTestOperation{
				updateMyCRDV1Beta1Schema{&apiextensionsv1.JSONSchemaProps{
					Type: "object",
					Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"field": {
							Type: "object",
							AdditionalProperties: &apiextensionsv1.JSONSchemaPropsOrBool{
								Schema: &apiextensionsv1.JSONSchemaProps{
									Type: "object",
									Properties: map[string]apiextensionsv1.JSONSchemaProps{
										"stringField":   *stringSchema,
										"intField":      *numberSchema,
										"otherIntField": *numberSchema,
									},
								},
							},
						},
					},
				}},
				applyPatchOperation{
					"create instance with strings that do not start with k8s",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"field": map[string]interface{}{
							"object1": map[string]interface{}{
								"stringField": "a string",
								"intField":    5,
							},
							"object2": map[string]interface{}{
								"stringField": "another string",
								"intField":    15,
							},
							"object3": map[string]interface{}{
								"stringField": "a third string",
								"intField":    7,
							},
						},
					}},
				patchMyCRDV1Beta1Schema{
					"require that stringField value start with `k8s`",
					map[string]interface{}{
						"properties": map[string]interface{}{
							"field": map[string]interface{}{
								"additionalProperties": map[string]interface{}{
									"properties": map[string]interface{}{
										"stringField": map[string]interface{}{
											"x-kubernetes-validations": []interface{}{
												map[string]interface{}{
													"rule":    "self.startsWith('k8s')",
													"message": "strings must have k8s prefix",
												},
											},
										},
									},
								},
							},
						},
					}},
				applyPatchOperation{
					"add a new entry that follows the new rule, ratchet old values",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"field": map[string]interface{}{
							"object1": map[string]interface{}{
								"stringField": "a string",
								"intField":    5,
							},
							"object2": map[string]interface{}{
								"stringField": "another string",
								"intField":    15,
							},
							"object3": map[string]interface{}{
								"stringField": "a third string",
								"intField":    7,
							},
							"object4": map[string]interface{}{
								"stringField": "k8s third string",
								"intField":    7,
							},
						},
					}},
				applyPatchOperation{
					"modify a sibling to an invalid value, ratcheting the unchanged invalid value",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"field": map[string]interface{}{
							"object1": map[string]interface{}{
								"stringField": "a string",
								"intField":    15,
							},
							"object2": map[string]interface{}{
								"stringField":   "another string",
								"intField":      10,
								"otherIntField": 20,
							},
						},
					}},
				expectError{
					applyPatchOperation{
						"change a previously ratcheted field to an invalid value",
						myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
							"field": map[string]interface{}{
								"object2": map[string]interface{}{
									"stringField": "a changed string",
								},
								"object3": map[string]interface{}{
									"stringField": "a changed third string",
								},
							},
						}}},
				patchMyCRDV1Beta1Schema{
					"require that stringField values are also odd length",
					map[string]interface{}{
						"properties": map[string]interface{}{
							"field": map[string]interface{}{
								"additionalProperties": map[string]interface{}{
									"stringField": map[string]interface{}{
										"x-kubernetes-validations": []interface{}{
											map[string]interface{}{
												"rule":    "self.startsWith('k8s')",
												"message": "strings must have k8s prefix",
											},
											map[string]interface{}{
												"rule":    "len(self) % 2 == 1",
												"message": "strings must have odd length",
											},
										},
									},
								},
							},
						},
					}},
				applyPatchOperation{
					"have mixed ratcheting of one or two CEL rules, object4 is ratcheted by one rule, object1 is ratcheting 2 rules",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"field": map[string]interface{}{
							"object1": map[string]interface{}{
								"stringField": "a string", // invalid. even number length, no k8s prefix
								"intField":    1000,
							},
							"object4": map[string]interface{}{
								"stringField": "k8s third string", // invalid. even number length. ratcheted
								"intField":    7000,
							},
						},
					}},
				expectError{
					applyPatchOperation{
						"swap keys between valuesin the map",
						myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
							"field": map[string]interface{}{
								"object1": map[string]interface{}{
									"stringField": "k8s third string",
									"intField":    1000,
								},
								"object4": map[string]interface{}{
									"stringField": "a string",
									"intField":    7000,
								},
							},
						}}},
				applyPatchOperation{
					"fix keys",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"field": map[string]interface{}{
							"object1": map[string]interface{}{
								"stringField": "k8s a stringy",
								"intField":    1000,
							},
							"object4": map[string]interface{}{
								"stringField": "k8s third stringy",
								"intField":    7000,
							},
						},
					}},
			},
		},
		{
			// Changing a list to a set should allow you to keep the items the
			// same, but if you modify any one item the set must be uniqued
			//
			// Possibly a future area of improvement. As it stands now,
			// SSA implementation is incompatible with ratcheting this field:
			// https://github.com/kubernetes/kubernetes/blob/ec9a8ffb237e391ce9ccc58de93ba4ecc2fabf42/staging/src/k8s.io/apimachinery/pkg/util/managedfields/internal/structuredmerge.go#L146-L149
			//
			// Throws error trying to interpret an invalid existing `liveObj`
			// as a set.
			Name:     "Change list to set",
			Disabled: true,
			Operations: []ratchetingTestOperation{
				updateMyCRDV1Beta1Schema{&apiextensionsv1.JSONSchemaProps{
					Type: "object",
					Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"values": {
							Type: "object",
							AdditionalProperties: &apiextensionsv1.JSONSchemaPropsOrBool{
								Schema: &apiextensionsv1.JSONSchemaProps{
									Type: "array",
									Items: &apiextensionsv1.JSONSchemaPropsOrArray{
										Schema: numberSchema,
									},
								},
							},
						},
					},
				}},
				applyPatchOperation{
					"reate a list of numbers with duplicates using the old simple schema",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"values": map[string]interface{}{
							"dups": []interface{}{1, 2, 2, 3, 1000, 2000},
						},
					}},
				patchMyCRDV1Beta1Schema{
					"change list type to set",
					map[string]interface{}{
						"properties": map[string]interface{}{
							"values": map[string]interface{}{
								"additionalProperties": map[string]interface{}{
									"x-kubernetes-list-type": "set",
								},
							},
						},
					}},
				expectError{
					applyPatchOperation{
						"change original without removing duplicates",
						myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
							"values": map[string]interface{}{
								"dups": []interface{}{1, 2, 2, 3, 1000, 2000, 3},
							},
						}}},
				expectError{applyPatchOperation{
					"add another list with duplicates",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"values": map[string]interface{}{
							"dups":  []interface{}{1, 2, 2, 3, 1000, 2000},
							"dups2": []interface{}{1, 2, 2, 3, 1000, 2000},
						},
					}}},
				// Can add a valid sibling field
				//! Remove this ExpectError if/when we add support for ratcheting
				// the type of a list
				applyPatchOperation{
					"add a valid sibling field",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"values": map[string]interface{}{
							"dups":       []interface{}{1, 2, 2, 3, 1000, 2000},
							"otherField": []interface{}{1, 2, 3},
						},
					}},
				// Can remove dups to make valid
				//! Normally this woud be valid, but SSA is unable to interpret
				// the `liveObj` in the new schema, so fails. Changing
				// x-kubernetes-list-type from anything to a set is unsupported by SSA.
				applyPatchOperation{
					"remove dups to make list valid",
					myCRDV1Beta1,
					myCRDInstanceName,
					map[string]interface{}{
						"values": map[string]interface{}{
							"dups":       []interface{}{1, 3, 1000, 2000},
							"otherField": []interface{}{1, 2, 3},
						},
					}},
			},
		},
	}

	runTests(t, cases)
}

func ptr[T any](v T) *T {
	return &v
}
