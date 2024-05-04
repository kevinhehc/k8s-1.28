/*
Copyright 2015 The Kubernetes Authors.

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

package cache

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache/synctrack"
	"k8s.io/utils/buffer"
	"k8s.io/utils/clock"

	"k8s.io/klog/v2"
)

// SharedInformer provides eventually consistent linkage of its
// clients to the authoritative state of a given collection of
// objects.  An object is identified by its API group, kind/resource,
// namespace (if any), and name; the `ObjectMeta.UID` is not part of
// an object's ID as far as this contract is concerned.  One
// SharedInformer provides linkage to objects of a particular API
// group and kind/resource.  The linked object collection of a
// SharedInformer may be further restricted to one namespace (if
// applicable) and/or by label selector and/or field selector.
//
// The authoritative state of an object is what apiservers provide
// access to, and an object goes through a strict sequence of states.
// An object state is either (1) present with a ResourceVersion and
// other appropriate content or (2) "absent".
//
// A SharedInformer maintains a local cache --- exposed by GetStore(),
// by GetIndexer() in the case of an indexed informer, and possibly by
// machinery involved in creating and/or accessing the informer --- of
// the state of each relevant object.  This cache is eventually
// consistent with the authoritative state.  This means that, unless
// prevented by persistent communication problems, if ever a
// particular object ID X is authoritatively associated with a state S
// then for every SharedInformer I whose collection includes (X, S)
// eventually either (1) I's cache associates X with S or a later
// state of X, (2) I is stopped, or (3) the authoritative state
// service for X terminates.  To be formally complete, we say that the
// absent state meets any restriction by label selector or field
// selector.
//
// For a given informer and relevant object ID X, the sequence of
// states that appears in the informer's cache is a subsequence of the
// states authoritatively associated with X.  That is, some states
// might never appear in the cache but ordering among the appearing
// states is correct.  Note, however, that there is no promise about
// ordering between states seen for different objects.
//
// The local cache starts out empty, and gets populated and updated
// during `Run()`.
//
// As a simple example, if a collection of objects is henceforth
// unchanging, a SharedInformer is created that links to that
// collection, and that SharedInformer is `Run()` then that
// SharedInformer's cache eventually holds an exact copy of that
// collection (unless it is stopped too soon, the authoritative state
// service ends, or communication problems between the two
// persistently thwart achievement).
//
// As another simple example, if the local cache ever holds a
// non-absent state for some object ID and the object is eventually
// removed from the authoritative state then eventually the object is
// removed from the local cache (unless the SharedInformer is stopped
// too soon, the authoritative state service ends, or communication
// problems persistently thwart the desired result).
//
// The keys in the Store are of the form namespace/name for namespaced
// objects, and are simply the name for non-namespaced objects.
// Clients can use `MetaNamespaceKeyFunc(obj)` to extract the key for
// a given object, and `SplitMetaNamespaceKey(key)` to split a key
// into its constituent parts.
//
// Every query against the local cache is answered entirely from one
// snapshot of the cache's state.  Thus, the result of a `List` call
// will not contain two entries with the same namespace and name.
//
// A client is identified here by a ResourceEventHandler.  For every
// update to the SharedInformer's local cache and for every client
// added before `Run()`, eventually either the SharedInformer is
// stopped or the client is notified of the update.  A client added
// after `Run()` starts gets a startup batch of notifications of
// additions of the objects existing in the cache at the time that
// client was added; also, for every update to the SharedInformer's
// local cache after that client was added, eventually either the
// SharedInformer is stopped or that client is notified of that
// update.  Client notifications happen after the corresponding cache
// update and, in the case of a SharedIndexInformer, after the
// corresponding index updates.  It is possible that additional cache
// and index updates happen before such a prescribed notification.
// For a given SharedInformer and client, the notifications are
// delivered sequentially.  For a given SharedInformer, client, and
// object ID, the notifications are delivered in order.  Because
// `ObjectMeta.UID` has no role in identifying objects, it is possible
// that when (1) object O1 with ID (e.g. namespace and name) X and
// `ObjectMeta.UID` U1 in the SharedInformer's local cache is deleted
// and later (2) another object O2 with ID X and ObjectMeta.UID U2 is
// created the informer's clients are not notified of (1) and (2) but
// rather are notified only of an update from O1 to O2. Clients that
// need to detect such cases might do so by comparing the `ObjectMeta.UID`
// field of the old and the new object in the code that handles update
// notifications (i.e. `OnUpdate` method of ResourceEventHandler).
//
// A client must process each notification promptly; a SharedInformer
// is not engineered to deal well with a large backlog of
// notifications to deliver.  Lengthy processing should be passed off
// to something else, for example through a
// `client-go/util/workqueue`.
//
// A delete notification exposes the last locally known non-absent
// state, except that its ResourceVersion is replaced with a
// ResourceVersion in which the object is actually absent.
type SharedInformer interface {
	// AddEventHandler adds an event handler to the shared informer using
	// the shared informer's resync period.  Events to a single handler are
	// delivered sequentially, but there is no coordination between
	// different handlers.
	// It returns a registration handle for the handler that can be used to
	// remove the handler again, or to tell if the handler is synced (has
	// seen every item in the initial list).
	// 增加用户自己的自定义处理逻辑
	AddEventHandler(handler ResourceEventHandler) (ResourceEventHandlerRegistration, error)
	// AddEventHandlerWithResyncPeriod adds an event handler to the
	// shared informer with the requested resync period; zero means
	// this handler does not care about resyncs.  The resync operation
	// consists of delivering to the handler an update notification
	// for every object in the informer's local cache; it does not add
	// any interactions with the authoritative storage.  Some
	// informers do no resyncs at all, not even for handlers added
	// with a non-zero resyncPeriod.  For an informer that does
	// resyncs, and for each handler that requests resyncs, that
	// informer develops a nominal resync period that is no shorter
	// than the requested period but may be longer.  The actual time
	// between any two resyncs may be longer than the nominal period
	// because the implementation takes time to do work and there may
	// be competing load and scheduling noise.
	// It returns a registration handle for the handler that can be used to remove
	// the handler again and an error if the handler cannot be added.
	//  增加用户自己的自定义处理逻辑 带有resyncPeriod时间
	AddEventHandlerWithResyncPeriod(handler ResourceEventHandler, resyncPeriod time.Duration) (ResourceEventHandlerRegistration, error)
	// RemoveEventHandler removes a formerly added event handler given by
	// its registration handle.
	// This function is guaranteed to be idempotent, and thread-safe.
	RemoveEventHandler(handle ResourceEventHandlerRegistration) error
	// GetStore returns the informer's local cache as a Store.
	//  获得Store 也就是DeltaFIFO
	GetStore() Store
	// GetController is deprecated, it does nothing useful
	//  获得Controller 也就是controller
	GetController() Controller
	// Run starts and runs the shared informer, returning after it stops.
	// The informer will be stopped when stopCh is closed.
	Run(stopCh <-chan struct{})
	// HasSynced returns true if the shared informer's store has been
	// informed by at least one full LIST of the authoritative state
	// of the informer's object collection.  This is unrelated to "resync".
	//
	// Note that this doesn't tell you if an individual handler is synced!!
	// For that, please call HasSynced on the handle returned by
	// AddEventHandler.
	HasSynced() bool
	// LastSyncResourceVersion is the resource version observed when last synced with the underlying
	// store. The value returned is not synchronized with access to the underlying store and is not
	// thread-safe.
	//  该SharedInformer对应的类型的上一次处理的ResourceVersion
	LastSyncResourceVersion() string

	// The WatchErrorHandler is called whenever ListAndWatch drops the
	// connection with an error. After calling this handler, the informer
	// will backoff and retry.
	//
	// The default implementation looks at the error type and tries to log
	// the error message at an appropriate level.
	//
	// There's only one handler, so if you call this multiple times, last one
	// wins; calling after the informer has been started returns an error.
	//
	// The handler is intended for visibility, not to e.g. pause the consumers.
	// The handler should return quickly - any expensive processing should be
	// offloaded.
	SetWatchErrorHandler(handler WatchErrorHandler) error

	// The TransformFunc is called for each object which is about to be stored.
	//
	// This function is intended for you to take the opportunity to
	// remove, transform, or normalize fields. One use case is to strip unused
	// metadata fields out of objects to save on RAM cost.
	//
	// Must be set before starting the informer.
	//
	// Please see the comment on TransformFunc for more details.
	SetTransform(handler TransformFunc) error

	// IsStopped reports whether the informer has already been stopped.
	// Adding event handlers to already stopped informers is not possible.
	// An informer already stopped will never be started again.
	IsStopped() bool
}

// Opaque interface representing the registration of ResourceEventHandler for
// a SharedInformer. Must be supplied back to the same SharedInformer's
// `RemoveEventHandler` to unregister the handlers.
//
// Also used to tell if the handler is synced (has had all items in the initial
// list delivered).
type ResourceEventHandlerRegistration interface {
	// HasSynced reports if both the parent has synced and all pre-sync
	// events have been delivered.
	HasSynced() bool
}

// SharedIndexInformer provides add and get Indexers ability based on SharedInformer.
type SharedIndexInformer interface {
	SharedInformer
	// AddIndexers add indexers to the informer before it starts.
	AddIndexers(indexers Indexers) error
	GetIndexer() Indexer
}

// NewSharedInformer creates a new instance for the ListerWatcher. See NewSharedIndexInformerWithOptions for full details.
func NewSharedInformer(lw ListerWatcher, exampleObject runtime.Object, defaultEventHandlerResyncPeriod time.Duration) SharedInformer {
	return NewSharedIndexInformer(lw, exampleObject, defaultEventHandlerResyncPeriod, Indexers{})
}

// NewSharedIndexInformer creates a new instance for the ListerWatcher and specified Indexers. See
// NewSharedIndexInformerWithOptions for full details.
func NewSharedIndexInformer(lw ListerWatcher, exampleObject runtime.Object, defaultEventHandlerResyncPeriod time.Duration, indexers Indexers) SharedIndexInformer {
	return NewSharedIndexInformerWithOptions(
		lw,
		exampleObject,
		SharedIndexInformerOptions{
			ResyncPeriod: defaultEventHandlerResyncPeriod,
			Indexers:     indexers,
		},
	)
}

// NewSharedIndexInformerWithOptions creates a new instance for the ListerWatcher.
// The created informer will not do resyncs if options.ResyncPeriod is zero.  Otherwise: for each
// handler that with a non-zero requested resync period, whether added
// before or after the informer starts, the nominal resync period is
// the requested resync period rounded up to a multiple of the
// informer's resync checking period.  Such an informer's resync
// checking period is established when the informer starts running,
// and is the maximum of (a) the minimum of the resync periods
// requested before the informer starts and the
// options.ResyncPeriod given here and (b) the constant
// `minimumResyncPeriod` defined in this file.
func NewSharedIndexInformerWithOptions(lw ListerWatcher, exampleObject runtime.Object, options SharedIndexInformerOptions) SharedIndexInformer {
	realClock := &clock.RealClock{}

	return &sharedIndexInformer{
		indexer:                         NewIndexer(DeletionHandlingMetaNamespaceKeyFunc, options.Indexers),
		processor:                       &sharedProcessor{clock: realClock},
		listerWatcher:                   lw,
		objectType:                      exampleObject,
		objectDescription:               options.ObjectDescription,
		resyncCheckPeriod:               options.ResyncPeriod,
		defaultEventHandlerResyncPeriod: options.ResyncPeriod,
		clock:                           realClock,
		cacheMutationDetector:           NewCacheMutationDetector(fmt.Sprintf("%T", exampleObject)),
	}
}

// SharedIndexInformerOptions configures a sharedIndexInformer.
type SharedIndexInformerOptions struct {
	// ResyncPeriod is the default event handler resync period and resync check
	// period. If unset/unspecified, these are defaulted to 0 (do not resync).
	ResyncPeriod time.Duration

	// Indexers is the sharedIndexInformer's indexers. If unset/unspecified, no indexers are configured.
	Indexers Indexers

	// ObjectDescription is the sharedIndexInformer's object description. This is passed through to the
	// underlying Reflector's type description.
	ObjectDescription string
}

// InformerSynced is a function that can be used to determine if an informer has synced.  This is useful for determining if caches have synced.
type InformerSynced func() bool

const (
	// syncedPollPeriod controls how often you look at the status of your sync funcs
	syncedPollPeriod = 100 * time.Millisecond

	// initialBufferSize is the initial number of event notifications that can be buffered.
	initialBufferSize = 1024
)

// WaitForNamedCacheSync is a wrapper around WaitForCacheSync that generates log messages
// indicating that the caller identified by name is waiting for syncs, followed by
// either a successful or failed sync.
func WaitForNamedCacheSync(controllerName string, stopCh <-chan struct{}, cacheSyncs ...InformerSynced) bool {
	klog.Infof("Waiting for caches to sync for %s", controllerName)

	if !WaitForCacheSync(stopCh, cacheSyncs...) {
		utilruntime.HandleError(fmt.Errorf("unable to sync caches for %s", controllerName))
		return false
	}

	klog.Infof("Caches are synced for %s", controllerName)
	return true
}

// WaitForCacheSync waits for caches to populate.  It returns true if it was successful, false
// if the controller should shutdown
// callers should prefer WaitForNamedCacheSync()
func WaitForCacheSync(stopCh <-chan struct{}, cacheSyncs ...InformerSynced) bool {
	err := wait.PollImmediateUntil(syncedPollPeriod,
		func() (bool, error) {
			for _, syncFunc := range cacheSyncs {
				if !syncFunc() {
					return false, nil
				}
			}
			return true, nil
		},
		stopCh)
	if err != nil {
		return false
	}

	return true
}

// `*sharedIndexInformer` implements SharedIndexInformer and has three
// main components.  One is an indexed local cache, `indexer Indexer`.
// The second main component is a Controller that pulls
// objects/notifications using the ListerWatcher and pushes them into
// a DeltaFIFO --- whose knownObjects is the informer's local cache
// --- while concurrently Popping Deltas values from that fifo and
// processing them with `sharedIndexInformer::HandleDeltas`.  Each
// invocation of HandleDeltas, which is done with the fifo's lock
// held, processes each Delta in turn.  For each Delta this both
// updates the local cache and stuffs the relevant notification into
// the sharedProcessor.  The third main component is that
// sharedProcessor, which is responsible for relaying those
// notifications to each of the informer's clients.
type sharedIndexInformer struct {
	//  本地缓存
	indexer    Indexer
	controller Controller

	processor             *sharedProcessor
	cacheMutationDetector MutationDetector

	listerWatcher ListerWatcher

	// objectType is an example object of the type this informer is expected to handle. If set, an event
	// with an object with a mismatching type is dropped instead of being delivered to listeners.
	// 该sharedIndexInformers监控的类型
	objectType runtime.Object

	// objectDescription is the description of this informer's objects. This typically defaults to
	objectDescription string

	// resyncCheckPeriod is how often we want the reflector's resync timer to fire so it can call
	// shouldResync to check if any of our listeners need a resync.
	//  在reflector中每隔resyncCheckPeriod时间会调用shouldResync方法来判断是否有任何一个listener需要resync操作
	resyncCheckPeriod time.Duration
	// defaultEventHandlerResyncPeriod is the default resync period for any handlers added via
	// AddEventHandler (i.e. they don't specify one and just want to use the shared informer's default
	// value).
	defaultEventHandlerResyncPeriod time.Duration
	// clock allows for testability
	clock clock.Clock

	started, stopped bool
	startedLock      sync.Mutex

	// blockDeltas gives a way to stop all event distribution so that a late event handler
	// can safely join the shared informer.
	//  可以停止分发obj给各个listeners
	//  因为HandleDeltas方法需要得到该锁, 如果失去了该锁, 就只能等到再次获得锁之后再分发
	blockDeltas sync.Mutex

	// Called whenever the ListAndWatch drops the connection with an error.
	watchErrorHandler WatchErrorHandler

	transform TransformFunc
}

// dummyController hides the fact that a SharedInformer is different from a dedicated one
// where a caller can `Run`.  The run method is disconnected in this case, because higher
// level logic will decide when to start the SharedInformer and related controller.
// Because returning information back is always asynchronous, the legacy callers shouldn't
// notice any change in behavior.
type dummyController struct {
	informer *sharedIndexInformer
}

func (v *dummyController) Run(stopCh <-chan struct{}) {
}

func (v *dummyController) HasSynced() bool {
	return v.informer.HasSynced()
}

func (v *dummyController) LastSyncResourceVersion() string {
	return ""
}

type updateNotification struct {
	oldObj interface{}
	newObj interface{}
}

type addNotification struct {
	newObj          interface{}
	isInInitialList bool
}

type deleteNotification struct {
	oldObj interface{}
}

func (s *sharedIndexInformer) SetWatchErrorHandler(handler WatchErrorHandler) error {
	s.startedLock.Lock()
	defer s.startedLock.Unlock()

	if s.started {
		return fmt.Errorf("informer has already started")
	}

	s.watchErrorHandler = handler
	return nil
}

func (s *sharedIndexInformer) SetTransform(handler TransformFunc) error {
	s.startedLock.Lock()
	defer s.startedLock.Unlock()

	if s.started {
		return fmt.Errorf("informer has already started")
	}

	s.transform = handler
	return nil
}

func (s *sharedIndexInformer) Run(stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()

	if s.HasStarted() {
		klog.Warningf("The sharedIndexInformer has started, run more than once is not allowed")
		return
	}

	func() {
		s.startedLock.Lock()
		defer s.startedLock.Unlock()

		//  初始化DeltaFIFO
		//  生成一个DeltaFIFO 并且knowObjects是s.indexer 也就是本地缓存
		fifo := NewDeltaFIFOWithOptions(DeltaFIFOOptions{
			KnownObjects:          s.indexer,
			EmitDeltaTypeReplaced: true,
			Transformer:           s.transform,
		})

		//  config初始化
		//  这里重点关注ListerWatcher对象和Process对象，Process关联的是HandleDeltas函数
		//  HandleDeltas是消费增量信息（Delta对象）的核心方法
		cfg := &Config{
			Queue: fifo,
			// ListAndWatch对象
			ListerWatcher:     s.listerWatcher,
			ObjectType:        s.objectType,
			ObjectDescription: s.objectDescription,
			FullResyncPeriod:  s.resyncCheckPeriod,
			RetryOnError:      false,

			//  对应的是sharedProcessor的shouldResync 会去计算所有的listeners是否有谁到了resync的时间
			ShouldResync: s.processor.shouldResync,

			//  注册回调函数 HandleDeltas,资源变更时，存到到本地Indexer
			Process:           s.HandleDeltas,
			WatchErrorHandler: s.watchErrorHandler,
		}

		//  这里主要是controller的初始化
		s.controller = New(cfg)
		s.controller.(*controller).clock = s.clock
		s.started = true
	}()

	// Separate stop channel because Processor should be stopped strictly after controller
	processorStopCh := make(chan struct{})
	var wg wait.Group
	defer wg.Wait()              // Wait for Processor to stop
	defer close(processorStopCh) // Tell Processor to stop

	//  s.cacheMutationDetector.Run检查缓存对象是否存在
	// hhc1
	wg.StartWithChannel(processorStopCh, s.cacheMutationDetector.Run)
	//  执行sharedProcessor.run方法
	//  这个方法非常重要
	//  hhc1
	wg.StartWithChannel(processorStopCh, s.processor.run)

	defer func() {
		s.startedLock.Lock()
		defer s.startedLock.Unlock()
		s.stopped = true // Don't want any new listeners
	}()
	//  调用Controller的Run方法
	s.controller.Run(stopCh)
}

func (s *sharedIndexInformer) HasStarted() bool {
	s.startedLock.Lock()
	defer s.startedLock.Unlock()
	return s.started
}

func (s *sharedIndexInformer) HasSynced() bool {
	s.startedLock.Lock()
	defer s.startedLock.Unlock()

	if s.controller == nil {
		return false
	}
	return s.controller.HasSynced()
}

func (s *sharedIndexInformer) LastSyncResourceVersion() string {
	s.startedLock.Lock()
	defer s.startedLock.Unlock()

	if s.controller == nil {
		return ""
	}
	return s.controller.LastSyncResourceVersion()
}

func (s *sharedIndexInformer) GetStore() Store {
	return s.indexer
}

func (s *sharedIndexInformer) GetIndexer() Indexer {
	return s.indexer
}

func (s *sharedIndexInformer) AddIndexers(indexers Indexers) error {
	s.startedLock.Lock()
	defer s.startedLock.Unlock()

	if s.started {
		return fmt.Errorf("informer has already started")
	}

	return s.indexer.AddIndexers(indexers)
}

func (s *sharedIndexInformer) GetController() Controller {
	return &dummyController{informer: s}
}

// AddEventHandler 开始注册事件处理函数
func (s *sharedIndexInformer) AddEventHandler(handler ResourceEventHandler) (ResourceEventHandlerRegistration, error) {
	return s.AddEventHandlerWithResyncPeriod(handler, s.defaultEventHandlerResyncPeriod)
}

// 1. 如果desired或check其中一个是0 则返回0
// 2. 返回max(desired, check)
func determineResyncPeriod(desired, check time.Duration) time.Duration {
	if desired == 0 {
		return desired
	}
	if check == 0 {
		klog.Warningf("The specified resyncPeriod %v is invalid because this shared informer doesn't support resyncing", desired)
		return 0
	}
	if desired < check {
		klog.Warningf("The specified resyncPeriod %v is being increased to the minimum resyncCheckPeriod %v", desired, check)
		return check
	}
	return desired
}

const minimumResyncPeriod = 1 * time.Second

func (s *sharedIndexInformer) AddEventHandlerWithResyncPeriod(handler ResourceEventHandler, resyncPeriod time.Duration) (ResourceEventHandlerRegistration, error) {
	s.startedLock.Lock()
	defer s.startedLock.Unlock()

	if s.stopped {
		// 该sharedIndexInformer已经结束
		return nil, fmt.Errorf("handler %v was not added to shared informer because it has stopped already", handler)
	}

	if resyncPeriod > 0 {
		//  如果比最小的resync时间minimumResyncPeriod还要小 就取最小的minimumResyncPeriod
		if resyncPeriod < minimumResyncPeriod {
			klog.Warningf("resyncPeriod %v is too small. Changing it to the minimum allowed value of %v", resyncPeriod, minimumResyncPeriod)
			resyncPeriod = minimumResyncPeriod
		}

		if resyncPeriod < s.resyncCheckPeriod {
			//  如果比该sharedIndexInformer的resyncCheckPeriod小
			//  1. 如果该sharedIndexInformer已经启动 那把resyncPeriod变为resyncCheckPeriod时间
			//  2. 如果该sharedIndexInformer没有启动 那就尽量让resyncCheckPeriod变小点 改成resyncPeriod时间 再重新计算各个listeners的resync时间
			if s.started {
				klog.Warningf("resyncPeriod %v is smaller than resyncCheckPeriod %v and the informer has already started. Changing it to %v", resyncPeriod, s.resyncCheckPeriod, s.resyncCheckPeriod)
				resyncPeriod = s.resyncCheckPeriod
			} else {
				// if the event handler's resyncPeriod is smaller than the current resyncCheckPeriod, update
				// resyncCheckPeriod to match resyncPeriod and adjust the resync periods of all the listeners
				// accordingly
				s.resyncCheckPeriod = resyncPeriod
				s.processor.resyncCheckPeriodChanged(resyncPeriod)
			}
		}
	}

	//  每一个监听者，都会注册为一个listner实例
	//  每个listener中持有一个handler对象，后面事件发生时，框架会调用handler方法，也就走到用户注册的代码逻辑了
	listener := newProcessListener(handler, resyncPeriod, determineResyncPeriod(resyncPeriod, s.resyncCheckPeriod), s.clock.Now(), initialBufferSize, s.HasSynced)

	if !s.started {
		return s.processor.addListener(listener), nil
	}

	// in order to safely join, we have to
	// 1. stop sending add/update/delete notifications
	// 2. do a list against the store
	// 3. send synthetic "Add" events to the new handler
	// 4. unblock
	//  1. 如果获得锁 意味着HandleDeltas方法会失去该锁 无法分发消息了(stop sending add/update/delete notifications)
	//  2. 从本地缓存中取出所有对象(do a list against the store)
	//  3. 将这些对象发送一个Add事件给这个listener并且交给新的handler处理(send synthetic "Add" events to the new handler)
	//  4. 解锁(unblock)
	s.blockDeltas.Lock()
	defer s.blockDeltas.Unlock()

	//  将listner添加到sharedProcessor中
	handle := s.processor.addListener(listener)
	for _, item := range s.indexer.List() {
		// Note that we enqueue these notifications with the lock held
		// and before returning the handle. That means there is never a
		// chance for anyone to call the handle's HasSynced method in a
		// state when it would falsely return true (i.e., when the
		// shared informer is synced but it has not observed an Add
		// with isInitialList being true, nor when the thread
		// processing notifications somehow goes faster than this
		// thread adding them and the counter is temporarily zero).
		//  把本地缓存中的数据往该listener中加入一遍
		listener.add(addNotification{newObj: item, isInInitialList: true})
	}
	return handle, nil
}

func (s *sharedIndexInformer) HandleDeltas(obj interface{}, isInInitialList bool) error {
	s.blockDeltas.Lock()
	defer s.blockDeltas.Unlock()

	if deltas, ok := obj.(Deltas); ok {
		return processDeltas(s, s.indexer, deltas, isInInitialList)
	}
	return errors.New("object given as Process argument is not Deltas")
}

// Conforms to ResourceEventHandler
func (s *sharedIndexInformer) OnAdd(obj interface{}, isInInitialList bool) {
	// Invocation of this function is locked under s.blockDeltas, so it is
	// save to distribute the notification
	s.cacheMutationDetector.AddObject(obj)
	s.processor.distribute(addNotification{newObj: obj, isInInitialList: isInInitialList}, false)
}

// Conforms to ResourceEventHandler
func (s *sharedIndexInformer) OnUpdate(old, new interface{}) {
	isSync := false

	// If is a Sync event, isSync should be true
	// If is a Replaced event, isSync is true if resource version is unchanged.
	// If RV is unchanged: this is a Sync/Replaced event, so isSync is true

	if accessor, err := meta.Accessor(new); err == nil {
		if oldAccessor, err := meta.Accessor(old); err == nil {
			// Events that didn't change resourceVersion are treated as resync events
			// and only propagated to listeners that requested resync
			isSync = accessor.GetResourceVersion() == oldAccessor.GetResourceVersion()
		}
	}

	// Invocation of this function is locked under s.blockDeltas, so it is
	// save to distribute the notification
	s.cacheMutationDetector.AddObject(new)
	s.processor.distribute(updateNotification{oldObj: old, newObj: new}, isSync)
}

// Conforms to ResourceEventHandler
func (s *sharedIndexInformer) OnDelete(old interface{}) {
	// Invocation of this function is locked under s.blockDeltas, so it is
	// save to distribute the notification
	s.processor.distribute(deleteNotification{oldObj: old}, false)
}

// IsStopped reports whether the informer has already been stopped
func (s *sharedIndexInformer) IsStopped() bool {
	s.startedLock.Lock()
	defer s.startedLock.Unlock()
	return s.stopped
}

func (s *sharedIndexInformer) RemoveEventHandler(handle ResourceEventHandlerRegistration) error {
	s.startedLock.Lock()
	defer s.startedLock.Unlock()

	// in order to safely remove, we have to
	// 1. stop sending add/update/delete notifications
	// 2. remove and stop listener
	// 3. unblock
	s.blockDeltas.Lock()
	defer s.blockDeltas.Unlock()
	return s.processor.removeListener(handle)
}

// sharedProcessor has a collection of processorListener and can
// distribute a notification object to its listeners.  There are two
// kinds of distribute operations.  The sync distributions go to a
// subset of the listeners that (a) is recomputed in the occasional
// calls to shouldResync and (b) every listener is initially put in.
// The non-sync distributions go to every listener.
type sharedProcessor struct {
	listenersStarted bool
	listenersLock    sync.RWMutex
	// Map from listeners to whether or not they are currently syncing
	listeners map[*processorListener]bool
	clock     clock.Clock
	wg        wait.Group
}

func (p *sharedProcessor) getListener(registration ResourceEventHandlerRegistration) *processorListener {
	p.listenersLock.RLock()
	defer p.listenersLock.RUnlock()

	if p.listeners == nil {
		return nil
	}

	if result, ok := registration.(*processorListener); ok {
		if _, exists := p.listeners[result]; exists {
			return result
		}
	}

	return nil
}

// 将listener添加到sharedProcessor中
func (p *sharedProcessor) addListener(listener *processorListener) ResourceEventHandlerRegistration {
	p.listenersLock.Lock()
	defer p.listenersLock.Unlock()

	if p.listeners == nil {
		p.listeners = make(map[*processorListener]bool)
	}

	p.listeners[listener] = true

	// listener后台启动了两个协程，这两个协程很关键，后面会介绍
	if p.listenersStarted {
		p.wg.Start(listener.run)
		p.wg.Start(listener.pop)
	}

	return listener
}

func (p *sharedProcessor) removeListener(handle ResourceEventHandlerRegistration) error {
	p.listenersLock.Lock()
	defer p.listenersLock.Unlock()

	listener, ok := handle.(*processorListener)
	if !ok {
		return fmt.Errorf("invalid key type %t", handle)
	} else if p.listeners == nil {
		// No listeners are registered, do nothing
		return nil
	} else if _, exists := p.listeners[listener]; !exists {
		// Listener is not registered, just do nothing
		return nil
	}

	delete(p.listeners, listener)

	if p.listenersStarted {
		close(listener.addCh)
	}

	return nil
}

// distribute函数
func (p *sharedProcessor) distribute(obj interface{}, sync bool) {
	p.listenersLock.RLock()
	defer p.listenersLock.RUnlock()

	for listener, isSyncing := range p.listeners {
		switch {
		case !sync:
			// non-sync messages are delivered to every listener
			listener.add(obj)
		case isSyncing:
			// sync messages are delivered to every syncing listener
			listener.add(obj)
		default:
			// skipping a sync obj for a non-syncing listener
		}
	}
}

func (p *sharedProcessor) run(stopCh <-chan struct{}) {
	func() {
		p.listenersLock.RLock()
		defer p.listenersLock.RUnlock()
		//  sharedProcessor的所有Listener，每个后台启动两个协程
		//  分别指向run和pop方法
		//  以goroutine的方式启动所有的listeners监听
		for listener := range p.listeners {
			p.wg.Start(listener.run)
			p.wg.Start(listener.pop)
		}
		p.listenersStarted = true
	}()
	// 等待有信号告知退出
	<-stopCh

	p.listenersLock.Lock()
	defer p.listenersLock.Unlock()
	// 关闭所有listener的addCh channel
	for listener := range p.listeners {
		// 通知pop()停止 pop()会告诉run()停止
		close(listener.addCh) // Tell .pop() to stop. .pop() will tell .run() to stop
	}

	// Wipe out list of listeners since they are now closed
	// (processorListener cannot be re-used)
	p.listeners = nil

	// Reset to false since no listeners are running
	p.listenersStarted = false
	// 等待所有的pop()和run()方法退出
	p.wg.Wait() // Wait for all .pop() and .run() to stop
}

// shouldResync queries every listener to determine if any of them need a resync, based on each
// listener's resyncPeriod.
// 可以看到该方法会重新生成syncingListeners, 遍历所有的listeners, 判断哪个已经到了resync时间,
// 如果到了就加入到syncingListeners中, 并且它的下一次resync的时间.
func (p *sharedProcessor) shouldResync() bool {
	p.listenersLock.Lock()
	defer p.listenersLock.Unlock()

	resyncNeeded := false
	now := p.clock.Now()
	for listener := range p.listeners {
		// need to loop through all the listeners to see if they need to resync so we can prepare any
		// listeners that are going to be resyncing.
		shouldResync := listener.shouldResync(now)
		p.listeners[listener] = shouldResync

		if shouldResync {
			resyncNeeded = true
			listener.determineNextResync(now)
		}
	}
	return resyncNeeded
}

func (p *sharedProcessor) resyncCheckPeriodChanged(resyncCheckPeriod time.Duration) {
	p.listenersLock.RLock()
	defer p.listenersLock.RUnlock()

	for listener := range p.listeners {
		// 根据listener自己要求的requestedResyncPeriod和resyncCheckPeriod来决定该listener真正的resyncPeriod
		resyncPeriod := determineResyncPeriod(
			listener.requestedResyncPeriod, resyncCheckPeriod)
		listener.setResyncPeriod(resyncPeriod)
	}
}

// processorListener relays notifications from a sharedProcessor to
// one ResourceEventHandler --- using two goroutines, two unbuffered
// channels, and an unbounded ring buffer.  The `add(notification)`
// function sends the given notification to `addCh`.  One goroutine
// runs `pop()`, which pumps notifications from `addCh` to `nextCh`
// using storage in the ring buffer while `nextCh` is not keeping up.
// Another goroutine runs `run()`, which receives notifications from
// `nextCh` and synchronously invokes the appropriate handler method.
//
// processorListener also keeps track of the adjusted requested resync
// period of the listener.
type processorListener struct {
	nextCh chan interface{}
	addCh  chan interface{}

	//  一个自定义处理数据的handler
	handler ResourceEventHandler

	syncTracker *synctrack.SingleFileTracker

	// pendingNotifications is an unbounded ring buffer that holds all notifications not yet distributed.
	// There is one per listener, but a failing/stalled listener will have infinite pendingNotifications
	// added until we OOM.
	// TODO: This is no worse than before, since reflectors were backed by unbounded DeltaFIFOs, but
	// we should try to do something better.
	//  一个环形的buffer 存着那些还没有被分发的notifications
	pendingNotifications buffer.RingGrowing

	// requestedResyncPeriod is how frequently the listener wants a
	// full resync from the shared informer, but modified by two
	// adjustments.  One is imposing a lower bound,
	// `minimumResyncPeriod`.  The other is another lower bound, the
	// sharedIndexInformer's `resyncCheckPeriod`, that is imposed (a) only
	// in AddEventHandlerWithResyncPeriod invocations made after the
	// sharedIndexInformer starts and (b) only if the informer does
	// resyncs at all.
	requestedResyncPeriod time.Duration
	// resyncPeriod is the threshold that will be used in the logic
	// for this listener.  This value differs from
	// requestedResyncPeriod only when the sharedIndexInformer does
	// not do resyncs, in which case the value here is zero.  The
	// actual time between resyncs depends on when the
	// sharedProcessor's `shouldResync` function is invoked and when
	// the sharedIndexInformer processes `Sync` type Delta objects.

	resyncPeriod time.Duration
	// nextResync is the earliest time the listener should get a full resync
	// 下次要resync的时候
	nextResync time.Time
	// resyncLock guards access to resyncPeriod and nextResync
	resyncLock sync.Mutex
}

// HasSynced returns true if the source informer has synced, and all
// corresponding events have been delivered.
func (p *processorListener) HasSynced() bool {
	return p.syncTracker.HasSynced()
}

func newProcessListener(handler ResourceEventHandler, requestedResyncPeriod, resyncPeriod time.Duration, now time.Time, bufferSize int, hasSynced func() bool) *processorListener {
	ret := &processorListener{
		nextCh:                make(chan interface{}),
		addCh:                 make(chan interface{}),
		handler:               handler,
		syncTracker:           &synctrack.SingleFileTracker{UpstreamHasSynced: hasSynced},
		pendingNotifications:  *buffer.NewRingGrowing(bufferSize),
		requestedResyncPeriod: requestedResyncPeriod,
		resyncPeriod:          resyncPeriod,
	}

	ret.determineNextResync(now)

	return ret
}

// listener.add方法，这里将事件添加到listener的addCh通道中
// 至此，也回答的前面的问题-- 谁往p.addCh中生产数据
func (p *processorListener) add(notification interface{}) {
	if a, ok := notification.(addNotification); ok && a.isInInitialList {
		p.syncTracker.Start()
	}
	// 不同更新类型的对象加入到channel中
	// 供给processorListener的Run方法使用
	p.addCh <- notification
}

func (p *processorListener) pop() {
	defer utilruntime.HandleCrash()
	defer close(p.nextCh) // Tell .run() to stop

	var nextCh chan<- interface{}
	var notification interface{}
	for {
		select {
		//  函数第一次进来，这个notification一定是空的，这个case会被阻塞住
		//  当第二个case调用完成后，notification被赋值为notificationToAdd，才会进入到这里
		case nextCh <- notification:
			// Notification dispatched
			var ok bool
			notification, ok = p.pendingNotifications.ReadOne()
			if !ok { // Nothing to pop
				nextCh = nil // Disable this select case
			}

			// 第一次调用时，会进入这里，首先从addCh中获取数据
		case notificationToAdd, ok := <-p.addCh:
			if !ok {
				return
			}
			if notification == nil {
				// No notification to pop (and pendingNotifications is empty)
				// Optimize the case - skip adding to pendingNotifications
				// channel是引用类型，将p.nextCh指向nextCh，对nextCh的操作就是操作p.nextCh
				//  这里也解答了前面的run方法里面提到的疑问，谁是nextCh的生产者，往nextCh放入数据
				notification = notificationToAdd
				nextCh = p.nextCh
			} else { // There is already a notification waiting to be dispatched
				p.pendingNotifications.WriteOne(notificationToAdd)
			}
		}
	}
}

func (p *processorListener) run() {
	// this call blocks until the channel is closed.  When a panic happens during the notification
	// we will catch it, **the offending item will be skipped!**, and after a short delay (one second)
	// the next notification will be attempted.  This is usually better than the alternative of never
	// delivering again.
	stopCh := make(chan struct{})
	// hhc1
	wait.Until(func() {
		//  消费者方法，不断从通道中获取事件
		for next := range p.nextCh {
			switch notification := next.(type) {
			case updateNotification:
				// 调用handler的方法，
				p.handler.OnUpdate(notification.oldObj, notification.newObj)
			case addNotification:
				p.handler.OnAdd(notification.newObj, notification.isInInitialList)
				if notification.isInInitialList {
					p.syncTracker.Finished()
				}
			case deleteNotification:
				p.handler.OnDelete(notification.oldObj)
			default:
				utilruntime.HandleError(fmt.Errorf("unrecognized notification: %T", next))
			}
		}
		// the only way to get here is if the p.nextCh is empty and closed
		close(stopCh)
	}, 1*time.Second, stopCh)
}

// shouldResync deterimines if the listener needs a resync. If the listener's resyncPeriod is 0,
// this always returns false.
func (p *processorListener) shouldResync(now time.Time) bool {
	p.resyncLock.Lock()
	defer p.resyncLock.Unlock()

	if p.resyncPeriod == 0 {
		return false
	}

	return now.After(p.nextResync) || now.Equal(p.nextResync)
}

func (p *processorListener) determineNextResync(now time.Time) {
	p.resyncLock.Lock()
	defer p.resyncLock.Unlock()

	p.nextResync = now.Add(p.resyncPeriod)
}

func (p *processorListener) setResyncPeriod(resyncPeriod time.Duration) {
	p.resyncLock.Lock()
	defer p.resyncLock.Unlock()

	p.resyncPeriod = resyncPeriod
}
