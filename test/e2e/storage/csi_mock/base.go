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

package csi_mock

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	csipbv1 "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/onsi/ginkgo/v2"
	"google.golang.org/grpc/codes"
	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2epv "k8s.io/kubernetes/test/e2e/framework/pv"
	"k8s.io/kubernetes/test/e2e/storage/drivers"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	"k8s.io/kubernetes/test/e2e/storage/testsuites"
	"k8s.io/kubernetes/test/e2e/storage/utils"
	imageutils "k8s.io/kubernetes/test/utils/image"
)

const (
	csiNodeLimitUpdateTimeout  = 5 * time.Minute
	csiPodUnschedulableTimeout = 5 * time.Minute
	csiResizeWaitPeriod        = 5 * time.Minute
	csiVolumeAttachmentTimeout = 7 * time.Minute
	// how long to wait for Resizing Condition on PVC to appear
	csiResizingConditionWait = 2 * time.Minute

	// Time for starting a pod with a volume.
	csiPodRunningTimeout = 5 * time.Minute

	// How log to wait for kubelet to unstage a volume after a pod is deleted
	csiUnstageWaitTimeout = 1 * time.Minute
)

// csiCall represents an expected call from Kubernetes to CSI mock driver and
// expected return value.
// When matching expected csiCall with a real CSI mock driver output, one csiCall
// matches *one or more* calls with the same method and error code.
// This is due to exponential backoff in Kubernetes, where the test cannot expect
// exact number of call repetitions.
type csiCall struct {
	expectedMethod string
	expectedError  codes.Code
	expectedSecret map[string]string
	// This is a mark for the test itself to delete the tested pod *after*
	// this csiCall is received.
	deletePod bool
}

type testParameters struct {
	disableAttach       bool
	attachLimit         int
	registerDriver      bool
	lateBinding         bool
	enableTopology      bool
	podInfo             *bool
	storageCapacity     *bool
	scName              string // pre-selected storage class name; must be unique in the cluster
	enableResizing      bool   // enable resizing for both CSI mock driver and storageClass.
	enableNodeExpansion bool   // enable node expansion for CSI mock driver
	// just disable resizing on driver it overrides enableResizing flag for CSI mock driver
	disableResizingOnDriver       bool
	enableSnapshot                bool
	enableVolumeMountGroup        bool // enable the VOLUME_MOUNT_GROUP node capability in the CSI mock driver.
	hooks                         *drivers.Hooks
	tokenRequests                 []storagev1.TokenRequest
	requiresRepublish             *bool
	fsGroupPolicy                 *storagev1.FSGroupPolicy
	enableSELinuxMount            *bool
	enableRecoverExpansionFailure bool
	enableCSINodeExpandSecret     bool
}

type mockDriverSetup struct {
	cs          clientset.Interface
	config      *storageframework.PerTestConfig
	pods        []*v1.Pod
	pvcs        []*v1.PersistentVolumeClaim
	sc          map[string]*storagev1.StorageClass
	vsc         map[string]*unstructured.Unstructured
	driver      drivers.MockCSITestDriver
	provisioner string
	tp          testParameters
	f           *framework.Framework
}

type volumeType string

var (
	csiEphemeral     = volumeType("CSI")
	genericEphemeral = volumeType("Ephemeral")
	pvcReference     = volumeType("PVC")
)

const (
	poll                           = 2 * time.Second
	pvcAsSourceProtectionFinalizer = "snapshot.storage.kubernetes.io/pvc-as-source-protection"
	volumeSnapshotContentFinalizer = "snapshot.storage.kubernetes.io/volumesnapshotcontent-bound-protection"
	volumeSnapshotBoundFinalizer   = "snapshot.storage.kubernetes.io/volumesnapshot-bound-protection"
	errReasonNotEnoughSpace        = "node(s) did not have enough free storage"

	csiNodeExpandSecretKey          = "csi.storage.k8s.io/node-expand-secret-name"
	csiNodeExpandSecretNamespaceKey = "csi.storage.k8s.io/node-expand-secret-namespace"
)

var (
	errPodCompleted   = fmt.Errorf("pod ran to completion")
	errNotEnoughSpace = errors.New(errReasonNotEnoughSpace)
)

func newMockDriverSetup(f *framework.Framework) *mockDriverSetup {
	return &mockDriverSetup{
		cs:  f.ClientSet,
		sc:  make(map[string]*storagev1.StorageClass),
		vsc: make(map[string]*unstructured.Unstructured),
		f:   f,
	}
}

func (m *mockDriverSetup) init(ctx context.Context, tp testParameters) {
	m.cs = m.f.ClientSet
	m.tp = tp

	var err error
	driverOpts := drivers.CSIMockDriverOpts{
		RegisterDriver:                tp.registerDriver,
		PodInfo:                       tp.podInfo,
		StorageCapacity:               tp.storageCapacity,
		EnableTopology:                tp.enableTopology,
		AttachLimit:                   tp.attachLimit,
		DisableAttach:                 tp.disableAttach,
		EnableResizing:                tp.enableResizing,
		EnableNodeExpansion:           tp.enableNodeExpansion,
		EnableSnapshot:                tp.enableSnapshot,
		EnableVolumeMountGroup:        tp.enableVolumeMountGroup,
		TokenRequests:                 tp.tokenRequests,
		RequiresRepublish:             tp.requiresRepublish,
		FSGroupPolicy:                 tp.fsGroupPolicy,
		EnableSELinuxMount:            tp.enableSELinuxMount,
		EnableRecoverExpansionFailure: tp.enableRecoverExpansionFailure,
	}

	// At the moment, only tests which need hooks are
	// using the embedded CSI mock driver. The rest run
	// the driver inside the cluster although they could
	// changed to use embedding merely by setting
	// driverOpts.embedded to true.
	//
	// Not enabling it for all tests minimizes
	// the risk that the introduction of embedded breaks
	// some existings tests and avoids a dependency
	// on port forwarding, which is important if some of
	// these tests are supposed to become part of
	// conformance testing (port forwarding isn't
	// currently required).
	if tp.hooks != nil {
		driverOpts.Embedded = true
		driverOpts.Hooks = *tp.hooks
	}

	// this just disable resizing on driver, keeping resizing on SC enabled.
	if tp.disableResizingOnDriver {
		driverOpts.EnableResizing = false
	}

	m.driver = drivers.InitMockCSIDriver(driverOpts)
	config := m.driver.PrepareTest(ctx, m.f)
	m.config = config
	m.provisioner = config.GetUniqueDriverName()

	if tp.registerDriver {
		err = waitForCSIDriver(m.cs, m.config.GetUniqueDriverName())
		framework.ExpectNoError(err, "Failed to get CSIDriver %v", m.config.GetUniqueDriverName())
		ginkgo.DeferCleanup(destroyCSIDriver, m.cs, m.config.GetUniqueDriverName())
	}

	// Wait for the CSIDriver actually get deployed and CSINode object to be generated.
	// This indicates the mock CSI driver pod is up and running healthy.
	err = drivers.WaitForCSIDriverRegistrationOnNode(ctx, m.config.ClientNodeSelection.Name, m.config.GetUniqueDriverName(), m.cs)
	framework.ExpectNoError(err, "Failed to register CSIDriver %v", m.config.GetUniqueDriverName())
}

func (m *mockDriverSetup) cleanup(ctx context.Context) {
	cs := m.f.ClientSet
	var errs []error

	for _, pod := range m.pods {
		ginkgo.By(fmt.Sprintf("Deleting pod %s", pod.Name))
		errs = append(errs, e2epod.DeletePodWithWait(ctx, cs, pod))
	}

	for _, claim := range m.pvcs {
		ginkgo.By(fmt.Sprintf("Deleting claim %s", claim.Name))
		claim, err := cs.CoreV1().PersistentVolumeClaims(claim.Namespace).Get(context.TODO(), claim.Name, metav1.GetOptions{})
		if err == nil {
			if err := cs.CoreV1().PersistentVolumeClaims(claim.Namespace).Delete(context.TODO(), claim.Name, metav1.DeleteOptions{}); err != nil {
				errs = append(errs, err)
			}
			if claim.Spec.VolumeName != "" {
				errs = append(errs, e2epv.WaitForPersistentVolumeDeleted(ctx, cs, claim.Spec.VolumeName, framework.Poll, 2*time.Minute))
			}
		}
	}

	for _, sc := range m.sc {
		ginkgo.By(fmt.Sprintf("Deleting storageclass %s", sc.Name))
		cs.StorageV1().StorageClasses().Delete(context.TODO(), sc.Name, metav1.DeleteOptions{})
	}

	for _, vsc := range m.vsc {
		ginkgo.By(fmt.Sprintf("Deleting volumesnapshotclass %s", vsc.GetName()))
		m.config.Framework.DynamicClient.Resource(utils.SnapshotClassGVR).Delete(context.TODO(), vsc.GetName(), metav1.DeleteOptions{})
	}

	err := utilerrors.NewAggregate(errs)
	framework.ExpectNoError(err, "while cleaning up after test")
}

func (m *mockDriverSetup) createPod(ctx context.Context, withVolume volumeType) (class *storagev1.StorageClass, claim *v1.PersistentVolumeClaim, pod *v1.Pod) {
	ginkgo.By("Creating pod")
	f := m.f

	sc := m.driver.GetDynamicProvisionStorageClass(ctx, m.config, "")
	if m.tp.enableCSINodeExpandSecret {
		if sc.Parameters == nil {
			parameters := map[string]string{
				csiNodeExpandSecretKey:          "test-secret",
				csiNodeExpandSecretNamespaceKey: f.Namespace.Name,
			}
			sc.Parameters = parameters
		} else {
			sc.Parameters[csiNodeExpandSecretKey] = "test-secret"
			sc.Parameters[csiNodeExpandSecretNamespaceKey] = f.Namespace.Name
		}
	}
	scTest := testsuites.StorageClassTest{
		Name:                 m.driver.GetDriverInfo().Name,
		Timeouts:             f.Timeouts,
		Provisioner:          sc.Provisioner,
		Parameters:           sc.Parameters,
		ClaimSize:            "1Gi",
		ExpectedSize:         "1Gi",
		DelayBinding:         m.tp.lateBinding,
		AllowVolumeExpansion: m.tp.enableResizing,
	}

	// The mock driver only works when everything runs on a single node.
	nodeSelection := m.config.ClientNodeSelection
	switch withVolume {
	case csiEphemeral:
		pod = startPausePodInline(f.ClientSet, scTest, nodeSelection, f.Namespace.Name)
	case genericEphemeral:
		class, pod = startPausePodGenericEphemeral(f.ClientSet, scTest, nodeSelection, m.tp.scName, f.Namespace.Name)
		if class != nil {
			m.sc[class.Name] = class
		}
		claim = &v1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pod.Name + "-" + pod.Spec.Volumes[0].Name,
				Namespace: f.Namespace.Name,
			},
		}
	case pvcReference:
		class, claim, pod = startPausePod(ctx, f.ClientSet, scTest, nodeSelection, m.tp.scName, f.Namespace.Name)
		if class != nil {
			m.sc[class.Name] = class
		}
		if claim != nil {
			m.pvcs = append(m.pvcs, claim)
		}
	}
	if pod != nil {
		m.pods = append(m.pods, pod)
	}
	return // result variables set above
}

func (m *mockDriverSetup) createPodWithPVC(pvc *v1.PersistentVolumeClaim) (*v1.Pod, error) {
	f := m.f

	nodeSelection := m.config.ClientNodeSelection
	pod, err := startPausePodWithClaim(m.cs, pvc, nodeSelection, f.Namespace.Name)
	if pod != nil {
		m.pods = append(m.pods, pod)
	}
	return pod, err
}

func (m *mockDriverSetup) createPodWithFSGroup(ctx context.Context, fsGroup *int64) (*storagev1.StorageClass, *v1.PersistentVolumeClaim, *v1.Pod) {
	f := m.f

	ginkgo.By("Creating pod with fsGroup")
	nodeSelection := m.config.ClientNodeSelection
	sc := m.driver.GetDynamicProvisionStorageClass(ctx, m.config, "")
	scTest := testsuites.StorageClassTest{
		Name:                 m.driver.GetDriverInfo().Name,
		Provisioner:          sc.Provisioner,
		Parameters:           sc.Parameters,
		ClaimSize:            "1Gi",
		ExpectedSize:         "1Gi",
		DelayBinding:         m.tp.lateBinding,
		AllowVolumeExpansion: m.tp.enableResizing,
	}
	class, claim, pod := startBusyBoxPod(ctx, f.ClientSet, scTest, nodeSelection, m.tp.scName, f.Namespace.Name, fsGroup)

	if class != nil {
		m.sc[class.Name] = class
	}
	if claim != nil {
		m.pvcs = append(m.pvcs, claim)
	}

	if pod != nil {
		m.pods = append(m.pods, pod)
	}

	return class, claim, pod
}

func (m *mockDriverSetup) createPodWithSELinux(ctx context.Context, accessModes []v1.PersistentVolumeAccessMode, mountOptions []string, seLinuxOpts *v1.SELinuxOptions) (*storagev1.StorageClass, *v1.PersistentVolumeClaim, *v1.Pod) {
	ginkgo.By("Creating pod with SELinux context")
	f := m.f
	nodeSelection := m.config.ClientNodeSelection
	sc := m.driver.GetDynamicProvisionStorageClass(ctx, m.config, "")
	scTest := testsuites.StorageClassTest{
		Name:                 m.driver.GetDriverInfo().Name,
		Provisioner:          sc.Provisioner,
		Parameters:           sc.Parameters,
		ClaimSize:            "1Gi",
		ExpectedSize:         "1Gi",
		DelayBinding:         m.tp.lateBinding,
		AllowVolumeExpansion: m.tp.enableResizing,
		MountOptions:         mountOptions,
	}
	class, claim := createClaim(ctx, f.ClientSet, scTest, nodeSelection, m.tp.scName, f.Namespace.Name, accessModes)
	pod, err := startPausePodWithSELinuxOptions(f.ClientSet, claim, nodeSelection, f.Namespace.Name, seLinuxOpts)
	framework.ExpectNoError(err, "Failed to create pause pod with SELinux context %s: %v", seLinuxOpts, err)

	if class != nil {
		m.sc[class.Name] = class
	}
	if claim != nil {
		m.pvcs = append(m.pvcs, claim)
	}

	if pod != nil {
		m.pods = append(m.pods, pod)
	}

	return class, claim, pod
}

func waitForCSIDriver(cs clientset.Interface, driverName string) error {
	timeout := 4 * time.Minute

	framework.Logf("waiting up to %v for CSIDriver %q", timeout, driverName)
	for start := time.Now(); time.Since(start) < timeout; time.Sleep(framework.Poll) {
		_, err := cs.StorageV1().CSIDrivers().Get(context.TODO(), driverName, metav1.GetOptions{})
		if !apierrors.IsNotFound(err) {
			return err
		}
	}
	return fmt.Errorf("gave up after waiting %v for CSIDriver %q", timeout, driverName)
}

func destroyCSIDriver(cs clientset.Interface, driverName string) {
	driverGet, err := cs.StorageV1().CSIDrivers().Get(context.TODO(), driverName, metav1.GetOptions{})
	if err == nil {
		framework.Logf("deleting %s.%s: %s", driverGet.TypeMeta.APIVersion, driverGet.TypeMeta.Kind, driverGet.ObjectMeta.Name)
		// Uncomment the following line to get full dump of CSIDriver object
		// framework.Logf("%s", framework.PrettyPrint(driverGet))
		cs.StorageV1().CSIDrivers().Delete(context.TODO(), driverName, metav1.DeleteOptions{})
	}
}

func newStorageClass(t testsuites.StorageClassTest, ns string, prefix string) *storagev1.StorageClass {
	pluginName := t.Provisioner
	if pluginName == "" {
		pluginName = getDefaultPluginName()
	}
	if prefix == "" {
		prefix = "sc"
	}
	bindingMode := storagev1.VolumeBindingImmediate
	if t.DelayBinding {
		bindingMode = storagev1.VolumeBindingWaitForFirstConsumer
	}
	if t.Parameters == nil {
		t.Parameters = make(map[string]string)
	}

	if framework.NodeOSDistroIs("windows") {
		// fstype might be forced from outside, in that case skip setting a default
		if _, exists := t.Parameters["fstype"]; !exists {
			t.Parameters["fstype"] = e2epv.GetDefaultFSType()
			framework.Logf("settings a default fsType=%s in the storage class", t.Parameters["fstype"])
		}
	}

	sc := getStorageClass(pluginName, t.Parameters, &bindingMode, t.MountOptions, ns, prefix)
	if t.AllowVolumeExpansion {
		sc.AllowVolumeExpansion = &t.AllowVolumeExpansion
	}
	return sc
}

func getStorageClass(
	provisioner string,
	parameters map[string]string,
	bindingMode *storagev1.VolumeBindingMode,
	mountOptions []string,
	ns string,
	prefix string,
) *storagev1.StorageClass {
	if bindingMode == nil {
		defaultBindingMode := storagev1.VolumeBindingImmediate
		bindingMode = &defaultBindingMode
	}
	return &storagev1.StorageClass{
		TypeMeta: metav1.TypeMeta{
			Kind: "StorageClass",
		},
		ObjectMeta: metav1.ObjectMeta{
			// Name must be unique, so let's base it on namespace name and the prefix (the prefix is test specific)
			GenerateName: ns + "-" + prefix,
		},
		Provisioner:       provisioner,
		Parameters:        parameters,
		VolumeBindingMode: bindingMode,
		MountOptions:      mountOptions,
	}
}

func getDefaultPluginName() string {
	switch {
	case framework.ProviderIs("gke"), framework.ProviderIs("gce"):
		return "kubernetes.io/gce-pd"
	case framework.ProviderIs("aws"):
		return "kubernetes.io/aws-ebs"
	case framework.ProviderIs("vsphere"):
		return "kubernetes.io/vsphere-volume"
	case framework.ProviderIs("azure"):
		return "kubernetes.io/azure-disk"
	}
	return ""
}

func createSC(cs clientset.Interface, t testsuites.StorageClassTest, scName, ns string) *storagev1.StorageClass {
	class := newStorageClass(t, ns, "")
	if scName != "" {
		class.Name = scName
	}
	var err error
	_, err = cs.StorageV1().StorageClasses().Get(context.TODO(), class.Name, metav1.GetOptions{})
	if err != nil {
		class, err = cs.StorageV1().StorageClasses().Create(context.TODO(), class, metav1.CreateOptions{})
		framework.ExpectNoError(err, "Failed to create class: %v", err)
	}

	return class
}

func createClaim(ctx context.Context, cs clientset.Interface, t testsuites.StorageClassTest, node e2epod.NodeSelection, scName, ns string, accessModes []v1.PersistentVolumeAccessMode) (*storagev1.StorageClass, *v1.PersistentVolumeClaim) {
	class := createSC(cs, t, scName, ns)
	claim := e2epv.MakePersistentVolumeClaim(e2epv.PersistentVolumeClaimConfig{
		ClaimSize:        t.ClaimSize,
		StorageClassName: &(class.Name),
		VolumeMode:       &t.VolumeMode,
		AccessModes:      accessModes,
	}, ns)
	claim, err := cs.CoreV1().PersistentVolumeClaims(ns).Create(context.TODO(), claim, metav1.CreateOptions{})
	framework.ExpectNoError(err, "Failed to create claim: %v", err)

	if !t.DelayBinding {
		pvcClaims := []*v1.PersistentVolumeClaim{claim}
		_, err = e2epv.WaitForPVClaimBoundPhase(ctx, cs, pvcClaims, framework.ClaimProvisionTimeout)
		framework.ExpectNoError(err, "Failed waiting for PVC to be bound: %v", err)
	}
	return class, claim
}

func startPausePod(ctx context.Context, cs clientset.Interface, t testsuites.StorageClassTest, node e2epod.NodeSelection, scName, ns string) (*storagev1.StorageClass, *v1.PersistentVolumeClaim, *v1.Pod) {
	class, claim := createClaim(ctx, cs, t, node, scName, ns, nil)

	pod, err := startPausePodWithClaim(cs, claim, node, ns)
	framework.ExpectNoError(err, "Failed to create pause pod: %v", err)
	return class, claim, pod
}

func startBusyBoxPod(ctx context.Context, cs clientset.Interface, t testsuites.StorageClassTest, node e2epod.NodeSelection, scName, ns string, fsGroup *int64) (*storagev1.StorageClass, *v1.PersistentVolumeClaim, *v1.Pod) {
	class, claim := createClaim(ctx, cs, t, node, scName, ns, nil)
	pod, err := startBusyBoxPodWithClaim(cs, claim, node, ns, fsGroup)
	framework.ExpectNoError(err, "Failed to create busybox pod: %v", err)
	return class, claim, pod
}

func startPausePodInline(cs clientset.Interface, t testsuites.StorageClassTest, node e2epod.NodeSelection, ns string) *v1.Pod {
	pod, err := startPausePodWithInlineVolume(cs,
		&v1.CSIVolumeSource{
			Driver: t.Provisioner,
		},
		node, ns)
	framework.ExpectNoError(err, "Failed to create pod: %v", err)
	return pod
}

func startPausePodGenericEphemeral(cs clientset.Interface, t testsuites.StorageClassTest, node e2epod.NodeSelection, scName, ns string) (*storagev1.StorageClass, *v1.Pod) {
	class := createSC(cs, t, scName, ns)
	claim := e2epv.MakePersistentVolumeClaim(e2epv.PersistentVolumeClaimConfig{
		ClaimSize:        t.ClaimSize,
		StorageClassName: &(class.Name),
		VolumeMode:       &t.VolumeMode,
	}, ns)
	pod, err := startPausePodWithVolumeSource(cs, v1.VolumeSource{
		Ephemeral: &v1.EphemeralVolumeSource{
			VolumeClaimTemplate: &v1.PersistentVolumeClaimTemplate{Spec: claim.Spec}},
	}, node, ns)
	framework.ExpectNoError(err, "Failed to create pod: %v", err)
	return class, pod
}

func startPausePodWithClaim(cs clientset.Interface, pvc *v1.PersistentVolumeClaim, node e2epod.NodeSelection, ns string) (*v1.Pod, error) {
	return startPausePodWithVolumeSource(cs,
		v1.VolumeSource{
			PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
				ClaimName: pvc.Name,
				ReadOnly:  false,
			},
		},
		node, ns)
}

func startBusyBoxPodWithClaim(cs clientset.Interface, pvc *v1.PersistentVolumeClaim, node e2epod.NodeSelection, ns string, fsGroup *int64) (*v1.Pod, error) {
	return startBusyBoxPodWithVolumeSource(cs,
		v1.VolumeSource{
			PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
				ClaimName: pvc.Name,
				ReadOnly:  false,
			},
		},
		node, ns, fsGroup)
}

func startPausePodWithInlineVolume(cs clientset.Interface, inlineVolume *v1.CSIVolumeSource, node e2epod.NodeSelection, ns string) (*v1.Pod, error) {
	return startPausePodWithVolumeSource(cs,
		v1.VolumeSource{
			CSI: inlineVolume,
		},
		node, ns)
}

func startPausePodWithVolumeSource(cs clientset.Interface, volumeSource v1.VolumeSource, node e2epod.NodeSelection, ns string) (*v1.Pod, error) {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "pvc-volume-tester-",
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:  "volume-tester",
					Image: imageutils.GetE2EImage(imageutils.Pause),
					VolumeMounts: []v1.VolumeMount{
						{
							Name:      "my-volume",
							MountPath: "/mnt/test",
						},
					},
				},
			},
			RestartPolicy: v1.RestartPolicyNever,
			Volumes: []v1.Volume{
				{
					Name:         "my-volume",
					VolumeSource: volumeSource,
				},
			},
		},
	}
	e2epod.SetNodeSelection(&pod.Spec, node)
	return cs.CoreV1().Pods(ns).Create(context.TODO(), pod, metav1.CreateOptions{})
}

func startBusyBoxPodWithVolumeSource(cs clientset.Interface, volumeSource v1.VolumeSource, node e2epod.NodeSelection, ns string, fsGroup *int64) (*v1.Pod, error) {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "pvc-volume-tester-",
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:  "volume-tester",
					Image: framework.BusyBoxImage,
					VolumeMounts: []v1.VolumeMount{
						{
							Name:      "my-volume",
							MountPath: "/mnt/test",
						},
					},
					Command: e2epod.GenerateScriptCmd("while true ; do sleep 2; done"),
				},
			},
			SecurityContext: &v1.PodSecurityContext{
				FSGroup: fsGroup,
			},
			RestartPolicy: v1.RestartPolicyNever,
			Volumes: []v1.Volume{
				{
					Name:         "my-volume",
					VolumeSource: volumeSource,
				},
			},
		},
	}
	e2epod.SetNodeSelection(&pod.Spec, node)
	return cs.CoreV1().Pods(ns).Create(context.TODO(), pod, metav1.CreateOptions{})
}

func startPausePodWithSELinuxOptions(cs clientset.Interface, pvc *v1.PersistentVolumeClaim, node e2epod.NodeSelection, ns string, seLinuxOpts *v1.SELinuxOptions) (*v1.Pod, error) {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "pvc-volume-tester-",
		},
		Spec: v1.PodSpec{
			SecurityContext: &v1.PodSecurityContext{
				SELinuxOptions: seLinuxOpts,
			},
			Containers: []v1.Container{
				{
					Name:  "volume-tester",
					Image: imageutils.GetE2EImage(imageutils.Pause),
					VolumeMounts: []v1.VolumeMount{
						{
							Name:      "my-volume",
							MountPath: "/mnt/test",
						},
					},
				},
			},
			RestartPolicy: v1.RestartPolicyNever,
			Volumes: []v1.Volume{
				{
					Name: "my-volume",
					VolumeSource: v1.VolumeSource{
						PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
							ClaimName: pvc.Name,
							ReadOnly:  false,
						},
					},
				},
			},
		},
	}
	if node.Name != "" {
		// Force schedule the pod to skip scheduler RWOP checks
		framework.Logf("Forcing node name %s", node.Name)
		pod.Spec.NodeName = node.Name
	}
	e2epod.SetNodeSelection(&pod.Spec, node)
	return cs.CoreV1().Pods(ns).Create(context.TODO(), pod, metav1.CreateOptions{})
}

func checkPodLogs(ctx context.Context, getCalls func(ctx context.Context) ([]drivers.MockCSICall, error), pod *v1.Pod, expectPodInfo, ephemeralVolume, csiInlineVolumesEnabled, csiServiceAccountTokenEnabled bool, expectedNumNodePublish int) error {
	expectedAttributes := map[string]string{}
	if expectPodInfo {
		expectedAttributes["csi.storage.k8s.io/pod.name"] = pod.Name
		expectedAttributes["csi.storage.k8s.io/pod.namespace"] = pod.Namespace
		expectedAttributes["csi.storage.k8s.io/pod.uid"] = string(pod.UID)
		expectedAttributes["csi.storage.k8s.io/serviceAccount.name"] = "default"

	}
	if csiInlineVolumesEnabled {
		// This is only passed in 1.15 when the CSIInlineVolume feature gate is set.
		expectedAttributes["csi.storage.k8s.io/ephemeral"] = strconv.FormatBool(ephemeralVolume)
	}

	if csiServiceAccountTokenEnabled {
		expectedAttributes["csi.storage.k8s.io/serviceAccount.tokens"] = "<nonempty>"
	}

	// Find NodePublish in the GRPC calls.
	foundAttributes := sets.NewString()
	numNodePublishVolume := 0
	numNodeUnpublishVolume := 0
	calls, err := getCalls(ctx)
	if err != nil {
		return err
	}

	for _, call := range calls {
		switch call.Method {
		case "NodePublishVolume":
			numNodePublishVolume++
			if numNodePublishVolume == 1 {
				// Check that NodePublish had expected attributes for first volume
				for k, v := range expectedAttributes {
					vv, found := call.Request.VolumeContext[k]
					if found && (v == vv || (v == "<nonempty>" && len(vv) != 0)) {
						foundAttributes.Insert(k)
						framework.Logf("Found volume attribute %s: %s", k, vv)
					}
				}
			}
		case "NodeUnpublishVolume":
			framework.Logf("Found NodeUnpublishVolume: %+v", call)
			numNodeUnpublishVolume++
		}
	}
	if numNodePublishVolume < expectedNumNodePublish {
		return fmt.Errorf("NodePublish should be called at least %d", expectedNumNodePublish)
	}

	if numNodeUnpublishVolume == 0 {
		return fmt.Errorf("NodeUnpublish was never called")
	}
	if foundAttributes.Len() != len(expectedAttributes) {
		return fmt.Errorf("number of found volume attributes does not match, expected %d, got %d", len(expectedAttributes), foundAttributes.Len())
	}
	return nil
}

// createFSGroupRequestPreHook creates a hook that records the fsGroup passed in
// through NodeStageVolume and NodePublishVolume calls.
func createFSGroupRequestPreHook(nodeStageFsGroup, nodePublishFsGroup *string) *drivers.Hooks {
	return &drivers.Hooks{
		Pre: func(ctx context.Context, fullMethod string, request interface{}) (reply interface{}, err error) {
			nodeStageRequest, ok := request.(*csipbv1.NodeStageVolumeRequest)
			if ok {
				mountVolume := nodeStageRequest.GetVolumeCapability().GetMount()
				if mountVolume != nil {
					*nodeStageFsGroup = mountVolume.VolumeMountGroup
				}
			}
			nodePublishRequest, ok := request.(*csipbv1.NodePublishVolumeRequest)
			if ok {
				mountVolume := nodePublishRequest.GetVolumeCapability().GetMount()
				if mountVolume != nil {
					*nodePublishFsGroup = mountVolume.VolumeMountGroup
				}
			}
			return nil, nil
		},
	}
}

// createPreHook counts invocations of a certain method (identified by a substring in the full gRPC method name).
func createPreHook(method string, callback func(counter int64) error) *drivers.Hooks {
	var counter int64

	return &drivers.Hooks{
		Pre: func() func(ctx context.Context, fullMethod string, request interface{}) (reply interface{}, err error) {
			return func(ctx context.Context, fullMethod string, request interface{}) (reply interface{}, err error) {
				if strings.Contains(fullMethod, method) {
					counter := atomic.AddInt64(&counter, 1)
					return nil, callback(counter)
				}
				return nil, nil
			}
		}(),
	}
}

// compareCSICalls compares expectedCalls with logs of the mock driver.
// It returns index of the first expectedCall that was *not* received
// yet or error when calls do not match.
// All repeated calls to the CSI mock driver (e.g. due to exponential backoff)
// are squashed and checked against single expectedCallSequence item.
//
// Only permanent errors are returned. Other errors are logged and no
// calls are returned. The caller is expected to retry.
func compareCSICalls(ctx context.Context, trackedCalls []string, expectedCallSequence []csiCall, getCalls func(ctx context.Context) ([]drivers.MockCSICall, error)) ([]drivers.MockCSICall, int, error) {
	allCalls, err := getCalls(ctx)
	if err != nil {
		framework.Logf("intermittent (?) log retrieval error, proceeding without output: %v", err)
		return nil, 0, nil
	}

	// Remove all repeated and ignored calls
	tracked := sets.NewString(trackedCalls...)
	var calls []drivers.MockCSICall
	var last drivers.MockCSICall
	for _, c := range allCalls {
		if !tracked.Has(c.Method) {
			continue
		}
		if c.Method != last.Method || c.FullError.Code != last.FullError.Code {
			last = c
			calls = append(calls, c)
		}
		// This call is the same as the last one, ignore it.
	}

	for i, c := range calls {
		if i >= len(expectedCallSequence) {
			// Log all unexpected calls first, return error below outside the loop.
			framework.Logf("Unexpected CSI driver call: %s (%d)", c.Method, c.FullError)
			continue
		}

		// Compare current call with expected call
		expectedCall := expectedCallSequence[i]
		if c.Method != expectedCall.expectedMethod || c.FullError.Code != expectedCall.expectedError {
			return allCalls, i, fmt.Errorf("Unexpected CSI call %d: expected %s (%d), got %s (%d)", i, expectedCall.expectedMethod, expectedCall.expectedError, c.Method, c.FullError.Code)
		}

		// if the secret is not nil, compare it
		if expectedCall.expectedSecret != nil {
			if !reflect.DeepEqual(expectedCall.expectedSecret, c.Request.Secrets) {
				return allCalls, i, fmt.Errorf("Unexpected secret: expected %v, got %v", expectedCall.expectedSecret, c.Request.Secrets)
			}
		}

	}
	if len(calls) > len(expectedCallSequence) {
		return allCalls, len(expectedCallSequence), fmt.Errorf("Received %d unexpected CSI driver calls", len(calls)-len(expectedCallSequence))
	}
	// All calls were correct
	return allCalls, len(calls), nil

}

// createSELinuxMountPreHook creates a hook that records the mountOptions passed in
// through NodeStageVolume and NodePublishVolume calls.
func createSELinuxMountPreHook(nodeStageMountOpts, nodePublishMountOpts *[]string, stageCalls, unstageCalls, publishCalls, unpublishCalls *atomic.Int32) *drivers.Hooks {
	return &drivers.Hooks{
		Pre: func(ctx context.Context, fullMethod string, request interface{}) (reply interface{}, err error) {
			switch req := request.(type) {
			case *csipbv1.NodeStageVolumeRequest:
				stageCalls.Add(1)
				mountVolume := req.GetVolumeCapability().GetMount()
				if mountVolume != nil {
					*nodeStageMountOpts = mountVolume.MountFlags
				}
			case *csipbv1.NodePublishVolumeRequest:
				publishCalls.Add(1)
				mountVolume := req.GetVolumeCapability().GetMount()
				if mountVolume != nil {
					*nodePublishMountOpts = mountVolume.MountFlags
				}
			case *csipbv1.NodeUnstageVolumeRequest:
				unstageCalls.Add(1)
			case *csipbv1.NodeUnpublishVolumeRequest:
				unpublishCalls.Add(1)
			}
			return nil, nil
		},
	}
}

// A lot of this code was copied from e2e/framework. It would be nicer
// if it could be reused - see https://github.com/kubernetes/kubernetes/issues/92754
func podRunning(ctx context.Context, c clientset.Interface, podName, namespace string) wait.ConditionFunc {
	return func() (bool, error) {
		pod, err := c.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		switch pod.Status.Phase {
		case v1.PodRunning:
			return true, nil
		case v1.PodFailed, v1.PodSucceeded:
			return false, errPodCompleted
		}
		return false, nil
	}
}

func podHasStorage(ctx context.Context, c clientset.Interface, podName, namespace string, when time.Time) wait.ConditionFunc {
	// Check for events of this pod. Copied from test/e2e/common/container_probe.go.
	expectedEvent := fields.Set{
		"involvedObject.kind":      "Pod",
		"involvedObject.name":      podName,
		"involvedObject.namespace": namespace,
		"reason":                   "FailedScheduling",
	}.AsSelector().String()
	options := metav1.ListOptions{
		FieldSelector: expectedEvent,
	}
	// copied from test/e2e/framework/events/events.go
	return func() (bool, error) {
		// We cannot be sure here whether it has enough storage, only when
		// it hasn't. In that case we abort waiting with a special error.
		events, err := c.CoreV1().Events(namespace).List(ctx, options)
		if err != nil {
			return false, fmt.Errorf("got error while getting events: %w", err)
		}
		for _, event := range events.Items {
			if /* event.CreationTimestamp.After(when) &&
			 */strings.Contains(event.Message, errReasonNotEnoughSpace) {
				return false, errNotEnoughSpace
			}
		}
		return false, nil
	}
}

func anyOf(conditions ...wait.ConditionFunc) wait.ConditionFunc {
	return func() (bool, error) {
		for _, condition := range conditions {
			done, err := condition()
			if err != nil {
				return false, err
			}
			if done {
				return true, nil
			}
		}
		return false, nil
	}
}

func waitForMaxVolumeCondition(pod *v1.Pod, cs clientset.Interface) error {
	waitErr := wait.PollImmediate(10*time.Second, csiPodUnschedulableTimeout, func() (bool, error) {
		pod, err := cs.CoreV1().Pods(pod.Namespace).Get(context.TODO(), pod.Name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		for _, c := range pod.Status.Conditions {
			// Conformance tests cannot rely on specific output of optional fields (e.g., Reason
			// and Message) because these fields are not suject to the deprecation policy.
			if c.Type == v1.PodScheduled && c.Status == v1.ConditionFalse && c.Reason != "" && c.Message != "" {
				return true, nil
			}
		}
		return false, nil
	})
	if waitErr != nil {
		return fmt.Errorf("error waiting for pod %s/%s to have max volume condition: %v", pod.Namespace, pod.Name, waitErr)
	}
	return nil
}
