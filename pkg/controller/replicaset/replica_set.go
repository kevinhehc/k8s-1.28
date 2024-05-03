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

// ### ATTENTION ###
//
// This code implements both ReplicaSet and ReplicationController.
//
// For RC, the objects are converted on the way in and out (see ../replication/),
// as if ReplicationController were just an older API version of ReplicaSet.
// However, RC and RS still have separate storage and separate instantiations
// of the ReplicaSetController object.
//
// Use rsc.Kind in log messages rather than hard-coding "ReplicaSet".

package replicaset

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	apps "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	appsinformers "k8s.io/client-go/informers/apps/v1"
	coreinformers "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	appslisters "k8s.io/client-go/listers/apps/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/component-base/metrics/legacyregistry"
	"k8s.io/klog/v2"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	"k8s.io/kubernetes/pkg/controller"
	"k8s.io/kubernetes/pkg/controller/replicaset/metrics"
	"k8s.io/utils/integer"
)

const (
	// Realistic value of the burstReplica field for the replica set manager based off
	// performance requirements for kubernetes 1.0.
	BurstReplicas = 500

	// The number of times we retry updating a ReplicaSet's status.
	statusUpdateRetries = 1

	// controllerUIDIndex is the name for the ReplicaSet store's index function,
	// which is to index by ReplicaSet's controllerUID.
	controllerUIDIndex = "controllerUID"
)

// ReplicaSetController is responsible for synchronizing ReplicaSet objects stored
// in the system with actual running pods.
type ReplicaSetController struct {
	// GroupVersionKind indicates the controller type.
	// Different instances of this struct may handle different GVKs.
	// For example, this struct can be used (with adapters) to handle ReplicationController.
	schema.GroupVersionKind

	kubeClient clientset.Interface
	podControl controller.PodControlInterface

	eventBroadcaster record.EventBroadcaster

	// A ReplicaSet is temporarily suspended after creating/deleting these many replicas.
	// It resumes normal action after observing the watch events for them.
	burstReplicas int
	// To allow injection of syncReplicaSet for testing.
	syncHandler func(ctx context.Context, rsKey string) error

	// A TTLCache of pod creates/deletes each rc expects to see.
	expectations *controller.UIDTrackingControllerExpectations

	// A store of ReplicaSets, populated by the shared informer passed to NewReplicaSetController
	rsLister appslisters.ReplicaSetLister
	// rsListerSynced returns true if the pod store has been synced at least once.
	// Added as a member to the struct to allow injection for testing.
	rsListerSynced cache.InformerSynced
	rsIndexer      cache.Indexer

	// A store of pods, populated by the shared informer passed to NewReplicaSetController
	podLister corelisters.PodLister
	// podListerSynced returns true if the pod store has been synced at least once.
	// Added as a member to the struct to allow injection for testing.
	podListerSynced cache.InformerSynced

	// Controllers that need to be synced
	queue workqueue.RateLimitingInterface
}

// NewReplicaSetController configures a replica set controller with the specified event recorder
func NewReplicaSetController(logger klog.Logger, rsInformer appsinformers.ReplicaSetInformer, podInformer coreinformers.PodInformer, kubeClient clientset.Interface, burstReplicas int) *ReplicaSetController {
	// 1、此处调用 NewBaseController
	eventBroadcaster := record.NewBroadcaster()
	if err := metrics.Register(legacyregistry.Register); err != nil {
		logger.Error(err, "unable to register metrics")
	}
	return NewBaseController(logger, rsInformer, podInformer, kubeClient, burstReplicas,
		apps.SchemeGroupVersion.WithKind("ReplicaSet"),
		"replicaset_controller",
		"replicaset",
		controller.RealPodControl{
			KubeClient: kubeClient,
			Recorder:   eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "replicaset-controller"}),
		},
		eventBroadcaster,
	)
}

// NewBaseController is the implementation of NewReplicaSetController with additional injected
// parameters so that it can also serve as the implementation of NewReplicationController.
func NewBaseController(logger klog.Logger, rsInformer appsinformers.ReplicaSetInformer, podInformer coreinformers.PodInformer, kubeClient clientset.Interface, burstReplicas int,
	gvk schema.GroupVersionKind, metricOwnerName, queueName string, podControl controller.PodControlInterface, eventBroadcaster record.EventBroadcaster) *ReplicaSetController {

	// 2、ReplicaSetController 初始化
	rsc := &ReplicaSetController{
		GroupVersionKind: gvk,
		kubeClient:       kubeClient,
		podControl:       podControl,
		eventBroadcaster: eventBroadcaster,
		burstReplicas:    burstReplicas,
		// 3、expectations 的初始化
		expectations: controller.NewUIDTrackingControllerExpectations(controller.NewControllerExpectations()),
		queue:        workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), queueName),
	}

	// 4、rsInformer 中注册的 EventHandler
	rsInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			rsc.addRS(logger, obj)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			rsc.updateRS(logger, oldObj, newObj)
		},
		DeleteFunc: func(obj interface{}) {
			rsc.deleteRS(logger, obj)
		},
	})
	rsInformer.Informer().AddIndexers(cache.Indexers{
		controllerUIDIndex: func(obj interface{}) ([]string, error) {
			rs, ok := obj.(*apps.ReplicaSet)
			if !ok {
				return []string{}, nil
			}
			controllerRef := metav1.GetControllerOf(rs)
			if controllerRef == nil {
				return []string{}, nil
			}
			return []string{string(controllerRef.UID)}, nil
		},
	})
	rsc.rsIndexer = rsInformer.Informer().GetIndexer()
	rsc.rsLister = rsInformer.Lister()
	rsc.rsListerSynced = rsInformer.Informer().HasSynced

	// 5、podInformer 中注册的 EventHandler
	podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			rsc.addPod(logger, obj)
		},
		// This invokes the ReplicaSet for every pod change, eg: host assignment. Though this might seem like
		// overkill the most frequent pod update is status, and the associated ReplicaSet will only list from
		// local storage, so it should be ok.
		UpdateFunc: func(oldObj, newObj interface{}) {
			rsc.updatePod(logger, oldObj, newObj)
		},
		DeleteFunc: func(obj interface{}) {
			rsc.deletePod(logger, obj)
		},
	})
	rsc.podLister = podInformer.Lister()
	rsc.podListerSynced = podInformer.Informer().HasSynced

	rsc.syncHandler = rsc.syncReplicaSet

	return rsc
}

// Run begins watching and syncing.
func (rsc *ReplicaSetController) Run(ctx context.Context, workers int) {
	defer utilruntime.HandleCrash()

	// Start events processing pipeline.
	rsc.eventBroadcaster.StartStructuredLogging(0)
	rsc.eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: rsc.kubeClient.CoreV1().Events("")})
	defer rsc.eventBroadcaster.Shutdown()

	defer rsc.queue.ShutDown()

	controllerName := strings.ToLower(rsc.Kind)
	logger := klog.FromContext(ctx)
	logger.Info("Starting controller", "name", controllerName)
	defer logger.Info("Shutting down controller", "name", controllerName)

	// 1、等待 informer 同步缓存
	if !cache.WaitForNamedCacheSync(rsc.Kind, ctx.Done(), rsc.podListerSynced, rsc.rsListerSynced) {
		return
	}

	// 2、启动 5 个 goroutine 执行 worker 方法
	for i := 0; i < workers; i++ {
		go wait.UntilWithContext(ctx, rsc.worker, time.Second)
	}

	<-ctx.Done()
}

// getReplicaSetsWithSameController returns a list of ReplicaSets with the same
// owner as the given ReplicaSet.
func (rsc *ReplicaSetController) getReplicaSetsWithSameController(logger klog.Logger, rs *apps.ReplicaSet) []*apps.ReplicaSet {
	controllerRef := metav1.GetControllerOf(rs)
	if controllerRef == nil {
		utilruntime.HandleError(fmt.Errorf("ReplicaSet has no controller: %v", rs))
		return nil
	}

	objects, err := rsc.rsIndexer.ByIndex(controllerUIDIndex, string(controllerRef.UID))
	if err != nil {
		utilruntime.HandleError(err)
		return nil
	}
	relatedRSs := make([]*apps.ReplicaSet, 0, len(objects))
	for _, obj := range objects {
		relatedRSs = append(relatedRSs, obj.(*apps.ReplicaSet))
	}

	if klogV := logger.V(2); klogV.Enabled() {
		klogV.Info("Found related ReplicaSets", "replicaSet", klog.KObj(rs), "relatedReplicaSets", klog.KObjSlice(relatedRSs))
	}

	return relatedRSs
}

// getPodReplicaSets returns a list of ReplicaSets matching the given pod.
func (rsc *ReplicaSetController) getPodReplicaSets(pod *v1.Pod) []*apps.ReplicaSet {
	rss, err := rsc.rsLister.GetPodReplicaSets(pod)
	if err != nil {
		return nil
	}
	if len(rss) > 1 {
		// ControllerRef will ensure we don't do anything crazy, but more than one
		// item in this list nevertheless constitutes user error.
		utilruntime.HandleError(fmt.Errorf("user error! more than one %v is selecting pods with labels: %+v", rsc.Kind, pod.Labels))
	}
	return rss
}

// resolveControllerRef returns the controller referenced by a ControllerRef,
// or nil if the ControllerRef could not be resolved to a matching controller
// of the correct Kind.
func (rsc *ReplicaSetController) resolveControllerRef(namespace string, controllerRef *metav1.OwnerReference) *apps.ReplicaSet {
	// We can't look up by UID, so look up by Name and then verify UID.
	// Don't even try to look up by Name if it's the wrong Kind.
	if controllerRef.Kind != rsc.Kind {
		return nil
	}
	rs, err := rsc.rsLister.ReplicaSets(namespace).Get(controllerRef.Name)
	if err != nil {
		return nil
	}
	if rs.UID != controllerRef.UID {
		// The controller we found with this Name is not the same one that the
		// ControllerRef points to.
		return nil
	}
	return rs
}

func (rsc *ReplicaSetController) enqueueRS(rs *apps.ReplicaSet) {
	key, err := controller.KeyFunc(rs)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %#v: %v", rs, err))
		return
	}

	rsc.queue.Add(key)
}

func (rsc *ReplicaSetController) enqueueRSAfter(rs *apps.ReplicaSet, duration time.Duration) {
	key, err := controller.KeyFunc(rs)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %#v: %v", rs, err))
		return
	}

	rsc.queue.AddAfter(key, duration)
}

func (rsc *ReplicaSetController) addRS(logger klog.Logger, obj interface{}) {
	rs := obj.(*apps.ReplicaSet)
	logger.V(4).Info("Adding", "replicaSet", klog.KObj(rs))
	rsc.enqueueRS(rs)
}

// callback when RS is updated
// updateRS 也仅仅是将对应的 rs 进行入队，不过多了一个打印日志的操作
func (rsc *ReplicaSetController) updateRS(logger klog.Logger, old, cur interface{}) {
	oldRS := old.(*apps.ReplicaSet)
	curRS := cur.(*apps.ReplicaSet)

	// TODO: make a KEP and fix informers to always call the delete event handler on re-create
	if curRS.UID != oldRS.UID {
		key, err := controller.KeyFunc(oldRS)
		if err != nil {
			utilruntime.HandleError(fmt.Errorf("couldn't get key for object %#v: %v", oldRS, err))
			return
		}
		rsc.deleteRS(logger, cache.DeletedFinalStateUnknown{
			Key: key,
			Obj: oldRS,
		})
	}

	// You might imagine that we only really need to enqueue the
	// replica set when Spec changes, but it is safer to sync any
	// time this function is triggered. That way a full informer
	// resync can requeue any replica set that don't yet have pods
	// but whose last attempts at creating a pod have failed (since
	// we don't block on creation of pods) instead of those
	// replica sets stalling indefinitely. Enqueueing every time
	// does result in some spurious syncs (like when Status.Replica
	// is updated and the watch notification from it retriggers
	// this function), but in general extra resyncs shouldn't be
	// that bad as ReplicaSets that haven't met expectations yet won't
	// sync, and all the listing is done using local stores.
	if *(oldRS.Spec.Replicas) != *(curRS.Spec.Replicas) {
		logger.V(4).Info("replicaSet updated. Desired pod count change.", "replicaSet", klog.KObj(oldRS), "oldReplicas", *(oldRS.Spec.Replicas), "newReplicas", *(curRS.Spec.Replicas))
	}
	rsc.enqueueRS(curRS)
}

func (rsc *ReplicaSetController) deleteRS(logger klog.Logger, obj interface{}) {
	rs, ok := obj.(*apps.ReplicaSet)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("couldn't get object from tombstone %#v", obj))
			return
		}
		rs, ok = tombstone.Obj.(*apps.ReplicaSet)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not a ReplicaSet %#v", obj))
			return
		}
	}

	key, err := controller.KeyFunc(rs)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %#v: %v", rs, err))
		return
	}

	logger.V(4).Info("Deleting", "replicaSet", klog.KObj(rs))

	// Delete expectations for the ReplicaSet so if we create a new one with the same name it starts clean
	rsc.expectations.DeleteExpectations(logger, key)

	rsc.queue.Add(key)
}

// When a pod is created, enqueue the replica set that manages it and update its expectations.
/*
1、判断 pod 是否处于删除状态；
2、获取该 pod 关联的 rs 以及 rsKey，入队 rs 并更新 rsKey 的 expectations；
3、若 pod 对象没体现出关联的 rs 则为孤儿 pod，遍历 rsList 查找匹配的 rs，
	若该 rs.Namespace == pod.Namespace 并且 rs.Spec.Selector 匹配 pod.Labels，
	则说明该 pod 应该与此 rs 关联，将匹配的 rs 入队；

*/
func (rsc *ReplicaSetController) addPod(logger klog.Logger, obj interface{}) {
	pod := obj.(*v1.Pod)

	if pod.DeletionTimestamp != nil {
		// on a restart of the controller manager, it's possible a new pod shows up in a state that
		// is already pending deletion. Prevent the pod from being a creation observation.
		rsc.deletePod(logger, pod)
		return
	}

	// If it has a ControllerRef, that's all that matters.
	// 1、获取 pod 所关联的 rs
	if controllerRef := metav1.GetControllerOf(pod); controllerRef != nil {
		rs := rsc.resolveControllerRef(pod.Namespace, controllerRef)
		if rs == nil {
			return
		}
		rsKey, err := controller.KeyFunc(rs)
		if err != nil {
			return
		}
		logger.V(4).Info("Pod created", "pod", klog.KObj(pod), "detail", pod)
		// 2、更新 expectations，rsKey 的 add - 1
		rsc.expectations.CreationObserved(logger, rsKey)
		rsc.queue.Add(rsKey)
		return
	}

	// Otherwise, it's an orphan. Get a list of all matching ReplicaSets and sync
	// them to see if anyone wants to adopt it.
	// DO NOT observe creation because no controller should be waiting for an
	// orphan.
	rss := rsc.getPodReplicaSets(pod)
	if len(rss) == 0 {
		return
	}
	logger.V(4).Info("Orphan Pod created", "pod", klog.KObj(pod), "detail", pod)
	for _, rs := range rss {
		rsc.enqueueRS(rs)
	}
}

// When a pod is updated, figure out what replica set/s manage it and wake them
// up. If the labels of the pod have changed we need to awaken both the old
// and new replica set. old and cur must be *v1.Pod types.
/*
1、如果 pod label 改变或者处于删除状态，则直接删除；
2、如果 pod 的 OwnerReference 发生改变，此时 oldRS 需要创建 pod，将 oldRS 入队；
3、获取 pod 关联的 rs，入队 rs，若 pod 当前处于 ready 并非 available 状态，则会再次将该 rs 加入到延迟队列中，
	因为 pod 从 ready 到 available 状态需要触发一次 status 的更新；
4、否则为孤儿 pod，遍历 rsList 查找匹配的 rs，若找到则将 rs 入队；
*/
func (rsc *ReplicaSetController) updatePod(logger klog.Logger, old, cur interface{}) {
	curPod := cur.(*v1.Pod)
	oldPod := old.(*v1.Pod)
	if curPod.ResourceVersion == oldPod.ResourceVersion {
		// Periodic resync will send update events for all known pods.
		// Two different versions of the same pod will always have different RVs.
		return
	}

	// 1、如果 pod label 改变或者处于删除状态，则直接删除
	labelChanged := !reflect.DeepEqual(curPod.Labels, oldPod.Labels)
	if curPod.DeletionTimestamp != nil {
		// when a pod is deleted gracefully it's deletion timestamp is first modified to reflect a grace period,
		// and after such time has passed, the kubelet actually deletes it from the store. We receive an update
		// for modification of the deletion timestamp and expect an rs to create more replicas asap, not wait
		// until the kubelet actually deletes the pod. This is different from the Phase of a pod changing, because
		// an rs never initiates a phase change, and so is never asleep waiting for the same.
		rsc.deletePod(logger, curPod)
		if labelChanged {
			// we don't need to check the oldPod.DeletionTimestamp because DeletionTimestamp cannot be unset.
			rsc.deletePod(logger, oldPod)
		}
		return
	}

	// 2、如果 pod 的 OwnerReference 发生改变，将 oldRS 入队
	curControllerRef := metav1.GetControllerOf(curPod)
	oldControllerRef := metav1.GetControllerOf(oldPod)
	controllerRefChanged := !reflect.DeepEqual(curControllerRef, oldControllerRef)
	if controllerRefChanged && oldControllerRef != nil {
		// The ControllerRef was changed. Sync the old controller, if any.
		if rs := rsc.resolveControllerRef(oldPod.Namespace, oldControllerRef); rs != nil {
			rsc.enqueueRS(rs)
		}
	}

	// If it has a ControllerRef, that's all that matters.
	// 3、获取 pod 关联的 rs，入队 rs
	if curControllerRef != nil {
		rs := rsc.resolveControllerRef(curPod.Namespace, curControllerRef)
		if rs == nil {
			return
		}
		logger.V(4).Info("Pod objectMeta updated.", "pod", klog.KObj(oldPod), "oldObjectMeta", oldPod.ObjectMeta, "curObjectMeta", curPod.ObjectMeta)
		rsc.enqueueRS(rs)
		// TODO: MinReadySeconds in the Pod will generate an Available condition to be added in
		// the Pod status which in turn will trigger a requeue of the owning replica set thus
		// having its status updated with the newly available replica. For now, we can fake the
		// update by resyncing the controller MinReadySeconds after the it is requeued because
		// a Pod transitioned to Ready.
		// Note that this still suffers from #29229, we are just moving the problem one level
		// "closer" to kubelet (from the deployment to the replica set controller).
		if !podutil.IsPodReady(oldPod) && podutil.IsPodReady(curPod) && rs.Spec.MinReadySeconds > 0 {
			logger.V(2).Info("pod will be enqueued after a while for availability check", "duration", rs.Spec.MinReadySeconds, "kind", rsc.Kind, "pod", klog.KObj(oldPod))
			// Add a second to avoid milliseconds skew in AddAfter.
			// See https://github.com/kubernetes/kubernetes/issues/39785#issuecomment-279959133 for more info.
			rsc.enqueueRSAfter(rs, (time.Duration(rs.Spec.MinReadySeconds)*time.Second)+time.Second)
		}
		return
	}

	// Otherwise, it's an orphan. If anything changed, sync matching controllers
	// to see if anyone wants to adopt it now.
	// 4、查找匹配的 rs
	if labelChanged || controllerRefChanged {
		rss := rsc.getPodReplicaSets(curPod)
		if len(rss) == 0 {
			return
		}
		logger.V(4).Info("Orphan Pod objectMeta updated.", "pod", klog.KObj(oldPod), "oldObjectMeta", oldPod.ObjectMeta, "curObjectMeta", curPod.ObjectMeta)
		for _, rs := range rss {
			rsc.enqueueRS(rs)
		}
	}
}

// When a pod is deleted, enqueue the replica set that manages the pod and update its expectations.
// obj could be an *v1.Pod, or a DeletionFinalStateUnknown marker item.
/*
1、确认该对象是否为 pod；
2、判断是否为孤儿 pod；
3、获取其对应的 rs 以及 rsKey；
4、更新 expectations 中 rsKey 的 del 值；
5、将 rs 入队
*/
func (rsc *ReplicaSetController) deletePod(logger klog.Logger, obj interface{}) {
	pod, ok := obj.(*v1.Pod)

	// When a delete is dropped, the relist will notice a pod in the store not
	// in the list, leading to the insertion of a tombstone object which contains
	// the deleted key/value. Note that this value might be stale. If the pod
	// changed labels the new ReplicaSet will not be woken up till the periodic resync.
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("couldn't get object from tombstone %+v", obj))
			return
		}
		pod, ok = tombstone.Obj.(*v1.Pod)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not a pod %#v", obj))
			return
		}
	}

	controllerRef := metav1.GetControllerOf(pod)
	if controllerRef == nil {
		// No controller should care about orphans being deleted.
		return
	}
	rs := rsc.resolveControllerRef(pod.Namespace, controllerRef)
	if rs == nil {
		return
	}
	rsKey, err := controller.KeyFunc(rs)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %#v: %v", rs, err))
		return
	}
	logger.V(4).Info("Pod deleted", "delete_by", utilruntime.GetCaller(), "deletion_timestamp", pod.DeletionTimestamp, "pod", klog.KObj(pod))
	// 更新 expectations，该 rsKey 的 del - 1
	rsc.expectations.DeletionObserved(logger, rsKey, controller.PodKey(pod))
	rsc.queue.Add(rsKey)
}

// worker runs a worker thread that just dequeues items, processes them, and marks them done.
// It enforces that the syncHandler is never invoked concurrently with the same key.
// 3、worker 方法中调用 rocessNextWorkItem
func (rsc *ReplicaSetController) worker(ctx context.Context) {
	for rsc.processNextWorkItem(ctx) {
	}
}

func (rsc *ReplicaSetController) processNextWorkItem(ctx context.Context) bool {
	// 4、从队列中取出对象
	key, quit := rsc.queue.Get()
	if quit {
		return false
	}
	defer rsc.queue.Done(key)

	// 5、执行 sync 操作
	err := rsc.syncHandler(ctx, key.(string))
	if err == nil {
		rsc.queue.Forget(key)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("sync %q failed with %v", key, err))
	rsc.queue.AddRateLimited(key)

	return true
}

// manageReplicas checks and updates replicas for the given ReplicaSet.
// Does NOT modify <filteredPods>.
// It will requeue the replica set in case of an error while creating/deleting pods.
/*
manageReplicas 是最核心的方法，它会计算 replicaSet 需要创建或者删除多少个 pod 并调用 apiserver 的接口进行操作，
在此阶段仅仅是调用 apiserver 的接口进行创建，并不保证 pod 成功运行，如果在某一轮，未能成功创建的所有 Pod 对象，则不再创建剩余的 pod。
一个周期内最多只能创建或删除 500 个 pod，若超过上限值未创建完成的 pod 数会在下一个 syncLoop 继续进行处理。

该方法主要逻辑如下所示：

1、计算已存在 pod 数与期望数的差异；
2、如果 diff < 0 说明 rs 实际的 pod 数未达到期望值需要继续创建 pod，首先会将需要创建的 pod 数在 expectations 中进行记录，
	然后调用 slowStartBatch 创建所需要的 pod，slowStartBatch 以指数级增长的方式批量创建 pod，
	创建 pod 过程中若出现 timeout err 则忽略，若为其他 err 则终止创建操作并更新 expectations；
3、如果 diff > 0 说明可能是一次缩容操作需要删除多余的 pod，如果需要删除全部的 pod 则直接进行删除，
	否则会通过 getPodsToDelete 方法筛选出需要删除的 pod，具体的筛选策略在下文会将到，然后并发删除这些 pod，
	对于删除失败操作也会记录在 expectations 中；

在 slowStartBatch 中会调用 rsc.podControl.CreatePodsWithControllerRef 方法创建 pod，
若创建 pod 失败会判断是否为创建超时错误，或者可能是超时后失败，但此时认为超时并不影响后续的批量创建动作，
大家知道，创建 pod 操作提交到 apiserver 后会经过认证、鉴权、以及动态访问控制三个步骤，此过程有可能会超时，
即使真的创建失败了，等到 expectations 过期后在下一个 syncLoop 时会重新创建。
*/
func (rsc *ReplicaSetController) manageReplicas(ctx context.Context, filteredPods []*v1.Pod, rs *apps.ReplicaSet) error {
	// 1、计算已存在 pod 数与期望数的差异
	diff := len(filteredPods) - int(*(rs.Spec.Replicas))
	rsKey, err := controller.KeyFunc(rs)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for %v %#v: %v", rsc.Kind, rs, err))
		return nil
	}
	logger := klog.FromContext(ctx)
	// 2、如果 <0，则需要创建 pod
	if diff < 0 {
		diff *= -1
		// 3、判断需要创建的 pod 数是否超过单次 sync 上限值 500
		if diff > rsc.burstReplicas {
			diff = rsc.burstReplicas
		}
		// TODO: Track UIDs of creates just like deletes. The problem currently
		// is we'd need to wait on the result of a create to record the pod's
		// UID, which would require locking *across* the create, which will turn
		// into a performance bottleneck. We should generate a UID for the pod
		// beforehand and store it via ExpectCreations.
		//  4、在 expectations 中进行记录，若该 key 已经存在会进行覆盖
		rsc.expectations.ExpectCreations(logger, rsKey, diff)
		logger.V(2).Info("Too few replicas", "replicaSet", klog.KObj(rs), "need", *(rs.Spec.Replicas), "creating", diff)
		// Batch the pod creates. Batch sizes start at SlowStartInitialBatchSize
		// and double with each successful iteration in a kind of "slow start".
		// This handles attempts to start large numbers of pods that would
		// likely all fail with the same error. For example a project with a
		// low quota that attempts to create a large number of pods will be
		// prevented from spamming the API service with the pod create requests
		// after one of its pods fails.  Conveniently, this also prevents the
		// event spam that those failures would generate.
		//  5、调用 slowStartBatch 创建所需要的 pod
		successfulCreations, err := slowStartBatch(diff, controller.SlowStartInitialBatchSize, func() error {
			err := rsc.podControl.CreatePods(ctx, rs.Namespace, &rs.Spec.Template, rs, metav1.NewControllerRef(rs, rsc.GroupVersionKind))
			if err != nil {
				if apierrors.HasStatusCause(err, v1.NamespaceTerminatingCause) {
					// if the namespace is being terminated, we don't have to do
					// anything because any creation will fail
					return nil
				}
			}
			return err
		})

		// Any skipped pods that we never attempted to start shouldn't be expected.
		// The skipped pods will be retried later. The next controller resync will
		// retry the slow start process.
		// 7、计算未创建的 pod 数，并记录在 expectations 中
		// 若 pod 创建成功，informer watch 到事件后会在 addPod handler 中更新 expectations
		if skippedPods := diff - successfulCreations; skippedPods > 0 {
			logger.V(2).Info("Slow-start failure. Skipping creation of pods, decrementing expectations", "podsSkipped", skippedPods, "kind", rsc.Kind, "replicaSet", klog.KObj(rs))
			for i := 0; i < skippedPods; i++ {
				// Decrement the expected number of creates because the informer won't observe this pod
				rsc.expectations.CreationObserved(logger, rsKey)
			}
		}
		return err
	} else if diff > 0 {
		// 8、若 diff >0 说明需要删除多创建的 pod
		if diff > rsc.burstReplicas {
			diff = rsc.burstReplicas
		}
		logger.V(2).Info("Too many replicas", "replicaSet", klog.KObj(rs), "need", *(rs.Spec.Replicas), "deleting", diff)

		relatedPods, err := rsc.getIndirectlyRelatedPods(logger, rs)
		utilruntime.HandleError(err)

		// Choose which Pods to delete, preferring those in earlier phases of startup.
		// 9、getPodsToDelete 会按照一定的策略找出需要删除的 pod 列表
		podsToDelete := getPodsToDelete(filteredPods, relatedPods, diff)

		// Snapshot the UIDs (ns/name) of the pods we're expecting to see
		// deleted, so we know to record their expectations exactly once either
		// when we see it as an update of the deletion timestamp, or as a delete.
		// Note that if the labels on a pod/rs change in a way that the pod gets
		// orphaned, the rs will only wake up after the expectations have
		// expired even if other pods are deleted.
		// 10、在 expectations 中进行记录，若该 key 已经存在会进行覆盖
		rsc.expectations.ExpectDeletions(logger, rsKey, getPodKeys(podsToDelete))

		// 11、进行并发删除的操作
		errCh := make(chan error, diff)
		var wg sync.WaitGroup
		wg.Add(diff)
		for _, pod := range podsToDelete {
			go func(targetPod *v1.Pod) {
				defer wg.Done()
				if err := rsc.podControl.DeletePod(ctx, rs.Namespace, targetPod.Name, rs); err != nil {
					// Decrement the expected number of deletes because the informer won't observe this deletion
					podKey := controller.PodKey(targetPod)
					// 12、某次删除操作若失败会记录在 expectations 中
					rsc.expectations.DeletionObserved(logger, rsKey, podKey)
					if !apierrors.IsNotFound(err) {
						logger.V(2).Info("Failed to delete pod, decremented expectations", "pod", podKey, "kind", rsc.Kind, "replicaSet", klog.KObj(rs))
						errCh <- err
					}
				}
			}(pod)
		}
		wg.Wait()

		// 13、返回其中一条 err
		select {
		case err := <-errCh:
			// all errors have been reported before and they're likely to be the same, so we'll only return the first one we hit.
			if err != nil {
				return err
			}
		default:
		}
	}

	return nil
}

// syncReplicaSet will sync the ReplicaSet with the given key if it has had its expectations fulfilled,
// meaning it did not expect to see any more of its pods created or deleted. This function is not meant to be
// invoked concurrently with the same key.
/*
1、根据 ns/name 获取 rs 对象；
2、调用 expectations.SatisfiedExpectations 判断是否需要执行真正的 sync 操作；
3、获取所有 pod list；
4、根据 pod label 进行过滤获取与该 rs 关联的 pod 列表，
	对于其中的孤儿 pod 若与该 rs label 匹配则进行关联，若已关联的 pod 与 rs label 不匹配则解除关联关系；
5、调用 manageReplicas 进行同步 pod 操作，add/del pod；
6、计算 rs 当前的 status 并进行更新；
7、若 rs 设置了 MinReadySeconds 字段则将该 rs 加入到延迟队列中；
*/
func (rsc *ReplicaSetController) syncReplicaSet(ctx context.Context, key string) error {
	logger := klog.FromContext(ctx)
	startTime := time.Now()
	defer func() {
		logger.Info("Finished syncing", "kind", rsc.Kind, "key", key, "duration", time.Since(startTime))
	}()

	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	// 1、根据 ns/name 从 informer cache 中获取 rs 对象，
	// 若 rs 已经被删除则直接删除 expectations 中的对象
	rs, err := rsc.rsLister.ReplicaSets(namespace).Get(name)
	if apierrors.IsNotFound(err) {
		logger.V(4).Info("deleted", "kind", rsc.Kind, "key", key)
		rsc.expectations.DeleteExpectations(logger, key)
		return nil
	}
	if err != nil {
		return err
	}

	// 2、判断该 rs 是否需要执行 sync 操作
	rsNeedsSync := rsc.expectations.SatisfiedExpectations(logger, key)
	selector, err := metav1.LabelSelectorAsSelector(rs.Spec.Selector)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("error converting pod selector to selector for rs %v/%v: %v", namespace, name, err))
		return nil
	}

	// list all pods to include the pods that don't match the rs`s selector
	// anymore but has the stale controller ref.
	// TODO: Do the List and Filter in a single pass, or use an index.
	// 3、获取所有 pod list
	allPods, err := rsc.podLister.Pods(rs.Namespace).List(labels.Everything())
	if err != nil {
		return err
	}
	// Ignore inactive pods.
	// 4、过滤掉异常 pod，处于删除状态或者 failed 状态的 pod 都为非 active 状态
	filteredPods := controller.FilterActivePods(logger, allPods)

	// NOTE: filteredPods are pointing to objects from cache - if you need to
	// modify them, you need to copy it first.
	// 5、检查所有 pod，根据 pod 并进行 adopt 与 release 操作，最后获取与该 rs 关联的 pod list
	filteredPods, err = rsc.claimPods(ctx, rs, selector, filteredPods)
	if err != nil {
		return err
	}

	// 6、若需要 sync 则执行 manageReplicas 创建/删除 pod
	var manageReplicasErr error
	if rsNeedsSync && rs.DeletionTimestamp == nil {
		manageReplicasErr = rsc.manageReplicas(ctx, filteredPods, rs)
	}
	rs = rs.DeepCopy()
	// 7、计算 rs 当前的 status
	newStatus := calculateStatus(rs, filteredPods, manageReplicasErr)

	// Always updates status as pods come up or die.
	// 8、更新 rs status
	updatedRS, err := updateReplicaSetStatus(logger, rsc.kubeClient.AppsV1().ReplicaSets(rs.Namespace), rs, newStatus)
	if err != nil {
		// Multiple things could lead to this update failing. Requeuing the replica set ensures
		// Returning an error causes a requeue without forcing a hotloop
		return err
	}
	// Resync the ReplicaSet after MinReadySeconds as a last line of defense to guard against clock-skew.
	// 9、判断是否需要将 rs 加入到延迟队列中
	if manageReplicasErr == nil && updatedRS.Spec.MinReadySeconds > 0 &&
		updatedRS.Status.ReadyReplicas == *(updatedRS.Spec.Replicas) &&
		updatedRS.Status.AvailableReplicas != *(updatedRS.Spec.Replicas) {
		rsc.queue.AddAfter(key, time.Duration(updatedRS.Spec.MinReadySeconds)*time.Second)
	}
	return manageReplicasErr
}

func (rsc *ReplicaSetController) claimPods(ctx context.Context, rs *apps.ReplicaSet, selector labels.Selector, filteredPods []*v1.Pod) ([]*v1.Pod, error) {
	// If any adoptions are attempted, we should first recheck for deletion with
	// an uncached quorum read sometime after listing Pods (see #42639).
	canAdoptFunc := controller.RecheckDeletionTimestamp(func(ctx context.Context) (metav1.Object, error) {
		fresh, err := rsc.kubeClient.AppsV1().ReplicaSets(rs.Namespace).Get(ctx, rs.Name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		if fresh.UID != rs.UID {
			return nil, fmt.Errorf("original %v %v/%v is gone: got uid %v, wanted %v", rsc.Kind, rs.Namespace, rs.Name, fresh.UID, rs.UID)
		}
		return fresh, nil
	})
	cm := controller.NewPodControllerRefManager(rsc.podControl, rs, selector, rsc.GroupVersionKind, canAdoptFunc)
	return cm.ClaimPods(ctx, filteredPods)
}

// slowStartBatch tries to call the provided function a total of 'count' times,
// starting slow to check for errors, then speeding up if calls succeed.
//
// It groups the calls into batches, starting with a group of initialBatchSize.
// Within each batch, it may call the function multiple times concurrently.
//
// If a whole batch succeeds, the next batch may get exponentially larger.
// If there are any failures in a batch, all remaining batches are skipped
// after waiting for the current batch to complete.
//
// It returns the number of successful calls to the function.
// slowStartBatch 会批量创建出已计算出的 diff pod 数，创建的 pod 数依次为 1、2、4、8......，呈指数级增长
func slowStartBatch(count int, initialBatchSize int, fn func() error) (int, error) {
	remaining := count
	successes := 0
	for batchSize := integer.IntMin(remaining, initialBatchSize); batchSize > 0; batchSize = integer.IntMin(2*batchSize, remaining) {
		errCh := make(chan error, batchSize)
		var wg sync.WaitGroup
		wg.Add(batchSize)
		for i := 0; i < batchSize; i++ {
			go func() {
				defer wg.Done()
				if err := fn(); err != nil {
					errCh <- err
				}
			}()
		}
		wg.Wait()
		curSuccesses := batchSize - len(errCh)
		successes += curSuccesses
		if len(errCh) > 0 {
			return successes, <-errCh
		}
		remaining -= batchSize
	}
	return successes, nil
}

// getIndirectlyRelatedPods returns all pods that are owned by any ReplicaSet
// that is owned by the given ReplicaSet's owner.
func (rsc *ReplicaSetController) getIndirectlyRelatedPods(logger klog.Logger, rs *apps.ReplicaSet) ([]*v1.Pod, error) {
	var relatedPods []*v1.Pod
	seen := make(map[types.UID]*apps.ReplicaSet)
	for _, relatedRS := range rsc.getReplicaSetsWithSameController(logger, rs) {
		selector, err := metav1.LabelSelectorAsSelector(relatedRS.Spec.Selector)
		if err != nil {
			// This object has an invalid selector, it does not match any pods
			continue
		}
		pods, err := rsc.podLister.Pods(relatedRS.Namespace).List(selector)
		if err != nil {
			return nil, err
		}
		for _, pod := range pods {
			if otherRS, found := seen[pod.UID]; found {
				logger.V(5).Info("Pod is owned by both", "pod", klog.KObj(pod), "kind", rsc.Kind, "replicaSets", klog.KObjSlice([]klog.KMetadata{otherRS, relatedRS}))
				continue
			}
			seen[pod.UID] = relatedRS
			relatedPods = append(relatedPods, pod)
		}
	}
	logger.V(4).Info("Found related pods", "kind", rsc.Kind, "replicaSet", klog.KObj(rs), "pods", klog.KObjSlice(relatedPods))
	return relatedPods, nil
}

/*
1、判断是否绑定了 node：Unassigned < assigned；
2、判断 pod phase：PodPending < PodUnknown < PodRunning；
3、判断 pod 状态：Not ready < ready；
4、若 pod 都为 ready，则按运行时间排序，运行时间最短会被删除：empty time < less time < more time；
5、根据 pod 重启次数排序：higher restart counts < lower restart counts；
6、按 pod 创建时间进行排序：Empty creation time pods < newer pods < older pods；
*/
func getPodsToDelete(filteredPods, relatedPods []*v1.Pod, diff int) []*v1.Pod {
	// No need to sort pods if we are about to delete all of them.
	// diff will always be <= len(filteredPods), so not need to handle > case.
	if diff < len(filteredPods) {
		podsWithRanks := getPodsRankedByRelatedPodsOnSameNode(filteredPods, relatedPods)
		sort.Sort(podsWithRanks)
		reportSortingDeletionAgeRatioMetric(filteredPods, diff)
	}
	return filteredPods[:diff]
}

func reportSortingDeletionAgeRatioMetric(filteredPods []*v1.Pod, diff int) {
	now := time.Now()
	youngestTime := time.Time{}
	// first we need to check all of the ready pods to get the youngest, as they may not necessarily be sorted by timestamp alone
	for _, pod := range filteredPods {
		if pod.CreationTimestamp.Time.After(youngestTime) && podutil.IsPodReady(pod) {
			youngestTime = pod.CreationTimestamp.Time
		}
	}

	// for each pod chosen for deletion, report the ratio of its age to the youngest pod's age
	for _, pod := range filteredPods[:diff] {
		if !podutil.IsPodReady(pod) {
			continue
		}
		ratio := float64(now.Sub(pod.CreationTimestamp.Time).Milliseconds() / now.Sub(youngestTime).Milliseconds())
		metrics.SortingDeletionAgeRatio.Observe(ratio)
	}
}

// getPodsRankedByRelatedPodsOnSameNode returns an ActivePodsWithRanks value
// that wraps podsToRank and assigns each pod a rank equal to the number of
// active pods in relatedPods that are colocated on the same node with the pod.
// relatedPods generally should be a superset of podsToRank.
func getPodsRankedByRelatedPodsOnSameNode(podsToRank, relatedPods []*v1.Pod) controller.ActivePodsWithRanks {
	podsOnNode := make(map[string]int)
	for _, pod := range relatedPods {
		if controller.IsPodActive(pod) {
			podsOnNode[pod.Spec.NodeName]++
		}
	}
	ranks := make([]int, len(podsToRank))
	for i, pod := range podsToRank {
		ranks[i] = podsOnNode[pod.Spec.NodeName]
	}
	return controller.ActivePodsWithRanks{Pods: podsToRank, Rank: ranks, Now: metav1.Now()}
}

func getPodKeys(pods []*v1.Pod) []string {
	podKeys := make([]string, 0, len(pods))
	for _, pod := range pods {
		podKeys = append(podKeys, controller.PodKey(pod))
	}
	return podKeys
}
