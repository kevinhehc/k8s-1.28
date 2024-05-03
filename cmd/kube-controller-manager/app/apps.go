/*
Copyright 2016 The Kubernetes Authors.

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

// Package app implements a server that runs a set of active
// components.  This includes replication controllers, service endpoints and
// nodes.
package app

import (
	"context"
	"fmt"
	"time"

	"k8s.io/client-go/util/flowcontrol"
	"k8s.io/controller-manager/controller"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/controller/daemon"
	"k8s.io/kubernetes/pkg/controller/deployment"
	"k8s.io/kubernetes/pkg/controller/replicaset"
	"k8s.io/kubernetes/pkg/controller/statefulset"
)

/*
初始化 DaemonSetsController 对象并调用 Run方法启动 daemonset controller，
从该方法中可以看出 daemonset controller 会监听 daemonsets、controllerRevision、pod 和 node 四种对象资源的变动。
其中 ConcurrentDaemonSetSyncs的默认值为 2
*/
func startDaemonSetController(ctx context.Context, controllerContext ControllerContext) (controller.Interface, bool, error) {
	dsc, err := daemon.NewDaemonSetsController(
		ctx,
		controllerContext.InformerFactory.Apps().V1().DaemonSets(),
		controllerContext.InformerFactory.Apps().V1().ControllerRevisions(),
		controllerContext.InformerFactory.Core().V1().Pods(),
		controllerContext.InformerFactory.Core().V1().Nodes(),
		controllerContext.ClientBuilder.ClientOrDie("daemon-set-controller"),
		flowcontrol.NewBackOff(1*time.Second, 15*time.Minute),
	)
	if err != nil {
		return nil, true, fmt.Errorf("error creating DaemonSets controller: %v", err)
	}
	go dsc.Run(ctx, int(controllerContext.ComponentConfig.DaemonSetController.ConcurrentDaemonSetSyncs))
	return nil, true, nil
}

func startStatefulSetController(ctx context.Context, controllerContext ControllerContext) (controller.Interface, bool, error) {
	go statefulset.NewStatefulSetController(
		ctx,
		controllerContext.InformerFactory.Core().V1().Pods(),
		controllerContext.InformerFactory.Apps().V1().StatefulSets(),
		controllerContext.InformerFactory.Core().V1().PersistentVolumeClaims(),
		controllerContext.InformerFactory.Apps().V1().ControllerRevisions(),
		controllerContext.ClientBuilder.ClientOrDie("statefulset-controller"),
		// ConcurrentStatefulSetSyncs 默认值为 5
	).Run(ctx, int(controllerContext.ComponentConfig.StatefulSetController.ConcurrentStatefulSetSyncs))
	return nil, true, nil
}

/*
BurstReplicas：用来控制在一个 syncLoop 过程中 rs 最多能创建的 pod 数量，设置上限值是为了避免单个 rs 影响整个系统，默认值为 500；
ConcurrentRSSyncs：指的是需要启动多少个 goroutine 处理 informer 队列中的对象，默认值为 5
*/
func startReplicaSetController(ctx context.Context, controllerContext ControllerContext) (controller.Interface, bool, error) {
	go replicaset.NewReplicaSetController(
		klog.FromContext(ctx),
		controllerContext.InformerFactory.Apps().V1().ReplicaSets(),
		controllerContext.InformerFactory.Core().V1().Pods(),
		controllerContext.ClientBuilder.ClientOrDie("replicaset-controller"),
		replicaset.BurstReplicas,
	).Run(ctx, int(controllerContext.ComponentConfig.ReplicaSetController.ConcurrentRSSyncs))
	return nil, true, nil
}

func startDeploymentController(ctx context.Context, controllerContext ControllerContext) (controller.Interface, bool, error) {
	// 初始化 controller
	dc, err := deployment.NewDeploymentController(
		ctx,
		controllerContext.InformerFactory.Apps().V1().Deployments(),
		controllerContext.InformerFactory.Apps().V1().ReplicaSets(),
		controllerContext.InformerFactory.Core().V1().Pods(),
		controllerContext.ClientBuilder.ClientOrDie("deployment-controller"),
	)
	if err != nil {
		return nil, true, fmt.Errorf("error creating Deployment controller: %v", err)
	}
	// 启动 controller
	// ConcurrentDeploymentSyncs 指定了 deployment controller 中工作的 goroutine 数量，默认值为 5，
	// 即会启动五个 goroutine 从 workqueue 中取出 object 并进行 sync 操作
	go dc.Run(ctx, int(controllerContext.ComponentConfig.DeploymentController.ConcurrentDeploymentSyncs))
	return nil, true, nil
}
