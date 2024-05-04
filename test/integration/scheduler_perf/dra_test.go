/*
Copyright 2019 The Kubernetes Authors.

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

package benchmark

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	resourcev1alpha2 "k8s.io/api/resource/v1alpha2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	draapp "k8s.io/kubernetes/test/e2e/dra/test-driver/app"
)

// createResourceClaimsOp defines an op where resource claims are created.
type createResourceClaimsOp struct {
	// Must be createResourceClaimsOpcode.
	Opcode operationCode
	// Number of claims to create. Parameterizable through CountParam.
	Count int
	// Template parameter for Count.
	CountParam string
	// Namespace the claims should be created in.
	Namespace string
	// Path to spec file describing the claims to create.
	TemplatePath string
}

var _ realOp = &createResourceClaimsOp{}
var _ runnableOp = &createResourceClaimsOp{}

func (op *createResourceClaimsOp) isValid(allowParameterization bool) error {
	if op.Opcode != createResourceClaimsOpcode {
		return fmt.Errorf("invalid opcode %q; expected %q", op.Opcode, createResourceClaimsOpcode)
	}
	if !isValidCount(allowParameterization, op.Count, op.CountParam) {
		return fmt.Errorf("invalid Count=%d / CountParam=%q", op.Count, op.CountParam)
	}
	if op.Namespace == "" {
		return fmt.Errorf("Namespace must be set")
	}
	if op.TemplatePath == "" {
		return fmt.Errorf("TemplatePath must be set")
	}
	return nil
}

func (op *createResourceClaimsOp) collectsMetrics() bool {
	return false
}
func (op *createResourceClaimsOp) patchParams(w *workload) (realOp, error) {
	if op.CountParam != "" {
		var err error
		op.Count, err = w.Params.get(op.CountParam[1:])
		if err != nil {
			return nil, err
		}
	}
	return op, op.isValid(false)
}

func (op *createResourceClaimsOp) requiredNamespaces() []string {
	return []string{op.Namespace}
}

func (op *createResourceClaimsOp) run(ctx context.Context, tb testing.TB, clientset clientset.Interface) {
	tb.Logf("creating %d claims in namespace %q", op.Count, op.Namespace)

	var claimTemplate *resourcev1alpha2.ResourceClaim
	if err := getSpecFromFile(&op.TemplatePath, &claimTemplate); err != nil {
		tb.Fatalf("parsing ResourceClaim %q: %v", op.TemplatePath, err)
	}
	var createErr error
	var mutex sync.Mutex
	create := func(i int) {
		err := func() error {
			if _, err := clientset.ResourceV1alpha2().ResourceClaims(op.Namespace).Create(ctx, claimTemplate.DeepCopy(), metav1.CreateOptions{}); err != nil {
				return fmt.Errorf("create claim: %v", err)
			}
			return nil
		}()
		if err != nil {
			mutex.Lock()
			defer mutex.Unlock()
			createErr = err
		}
	}

	workers := op.Count
	if workers > 30 {
		workers = 30
	}
	workqueue.ParallelizeUntil(ctx, workers, op.Count, create)
	if createErr != nil {
		tb.Fatal(createErr.Error())
	}
}

// createResourceClassOpType customizes createOp for creating a ResourceClass.
type createResourceClassOpType struct{}

func (c createResourceClassOpType) Opcode() operationCode { return createResourceClassOpcode }
func (c createResourceClassOpType) Name() string          { return "ResourceClass" }
func (c createResourceClassOpType) Namespaced() bool      { return false }
func (c createResourceClassOpType) CreateCall(client clientset.Interface, namespace string) func(context.Context, *resourcev1alpha2.ResourceClass, metav1.CreateOptions) (*resourcev1alpha2.ResourceClass, error) {
	return client.ResourceV1alpha2().ResourceClasses().Create
}

// createResourceClassOpType customizes createOp for creating a ResourceClaim.
type createResourceClaimTemplateOpType struct{}

func (c createResourceClaimTemplateOpType) Opcode() operationCode {
	return createResourceClaimTemplateOpcode
}
func (c createResourceClaimTemplateOpType) Name() string     { return "ResourceClaimTemplate" }
func (c createResourceClaimTemplateOpType) Namespaced() bool { return true }
func (c createResourceClaimTemplateOpType) CreateCall(client clientset.Interface, namespace string) func(context.Context, *resourcev1alpha2.ResourceClaimTemplate, metav1.CreateOptions) (*resourcev1alpha2.ResourceClaimTemplate, error) {
	return client.ResourceV1alpha2().ResourceClaimTemplates(namespace).Create
}

// createResourceDriverOp defines an op where resource claims are created.
type createResourceDriverOp struct {
	// Must be createResourceDriverOpcode.
	Opcode operationCode
	// Name of the driver, used to reference it in a resource class.
	DriverName string
	// Number of claims to allow per node. Parameterizable through MaxClaimsPerNodeParam.
	MaxClaimsPerNode int
	// Template parameter for MaxClaimsPerNode.
	MaxClaimsPerNodeParam string
	// Nodes matching this glob pattern have resources managed by the driver.
	Nodes string
}

var _ realOp = &createResourceDriverOp{}
var _ runnableOp = &createResourceDriverOp{}

func (op *createResourceDriverOp) isValid(allowParameterization bool) error {
	if op.Opcode != createResourceDriverOpcode {
		return fmt.Errorf("invalid opcode %q; expected %q", op.Opcode, createResourceDriverOpcode)
	}
	if !isValidCount(allowParameterization, op.MaxClaimsPerNode, op.MaxClaimsPerNodeParam) {
		return fmt.Errorf("invalid MaxClaimsPerNode=%d / MaxClaimsPerNodeParam=%q", op.MaxClaimsPerNode, op.MaxClaimsPerNodeParam)
	}
	if op.DriverName == "" {
		return fmt.Errorf("DriverName must be set")
	}
	if op.Nodes == "" {
		return fmt.Errorf("Nodes must be set")
	}
	return nil
}

func (op *createResourceDriverOp) collectsMetrics() bool {
	return false
}
func (op *createResourceDriverOp) patchParams(w *workload) (realOp, error) {
	if op.MaxClaimsPerNodeParam != "" {
		var err error
		op.MaxClaimsPerNode, err = w.Params.get(op.MaxClaimsPerNodeParam[1:])
		if err != nil {
			return nil, err
		}
	}
	return op, op.isValid(false)
}

func (op *createResourceDriverOp) requiredNamespaces() []string { return nil }

func (op *createResourceDriverOp) run(ctx context.Context, tb testing.TB, clientset clientset.Interface) {
	tb.Logf("creating resource driver %q for nodes matching %q", op.DriverName, op.Nodes)

	// Start the controller side of the DRA test driver such that it simulates
	// per-node resources.
	resources := draapp.Resources{
		NodeLocal:      true,
		MaxAllocations: op.MaxClaimsPerNode,
	}

	nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		tb.Fatalf("list nodes: %v", err)
	}
	for _, node := range nodes.Items {
		match, err := filepath.Match(op.Nodes, node.Name)
		if err != nil {
			tb.Fatalf("matching glob pattern %q against node name %q: %v", op.Nodes, node.Name, err)
		}
		if match {
			resources.Nodes = append(resources.Nodes, node.Name)
		}
	}

	controller := draapp.NewController(clientset, "test-driver.cdi.k8s.io", resources)
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ctx := klog.NewContext(ctx, klog.LoggerWithName(klog.FromContext(ctx), "DRA test driver"))
		controller.Run(ctx, 5 /* workers */)
	}()
	tb.Cleanup(func() {
		tb.Logf("stopping resource driver %q", op.DriverName)
		// We must cancel before waiting.
		cancel()
		wg.Wait()
		tb.Logf("stopped resource driver %q", op.DriverName)
	})
}
