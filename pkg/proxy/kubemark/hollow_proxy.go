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

package kubemark

import (
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientset "k8s.io/client-go/kubernetes"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/events"
	utilsysctl "k8s.io/component-helpers/node/util/sysctl"
	proxyapp "k8s.io/kubernetes/cmd/kube-proxy/app"
	"k8s.io/kubernetes/pkg/proxy"
	proxyconfigapi "k8s.io/kubernetes/pkg/proxy/apis/config"
	proxyconfig "k8s.io/kubernetes/pkg/proxy/config"
	"k8s.io/kubernetes/pkg/proxy/iptables"
	proxyutiliptables "k8s.io/kubernetes/pkg/proxy/util/iptables"
	utiliptables "k8s.io/kubernetes/pkg/util/iptables"
	utilnode "k8s.io/kubernetes/pkg/util/node"
	utilexec "k8s.io/utils/exec"
	netutils "k8s.io/utils/net"
	utilpointer "k8s.io/utils/pointer"

	"k8s.io/klog/v2"
)

type HollowProxy struct {
	ProxyServer *proxyapp.ProxyServer
}

type FakeProxier struct {
	proxyconfig.NoopNodeHandler
}

func (*FakeProxier) Sync() {}
func (*FakeProxier) SyncLoop() {
	select {}
}
func (*FakeProxier) OnServiceAdd(service *v1.Service)                                 {}
func (*FakeProxier) OnServiceUpdate(oldService, service *v1.Service)                  {}
func (*FakeProxier) OnServiceDelete(service *v1.Service)                              {}
func (*FakeProxier) OnServiceSynced()                                                 {}
func (*FakeProxier) OnEndpointSliceAdd(slice *discoveryv1.EndpointSlice)              {}
func (*FakeProxier) OnEndpointSliceUpdate(oldSlice, slice *discoveryv1.EndpointSlice) {}
func (*FakeProxier) OnEndpointSliceDelete(slice *discoveryv1.EndpointSlice)           {}
func (*FakeProxier) OnEndpointSlicesSynced()                                          {}

func NewHollowProxyOrDie(
	nodeName string,
	client clientset.Interface,
	eventClient v1core.EventsGetter,
	iptInterface utiliptables.Interface,
	sysctl utilsysctl.Interface,
	execer utilexec.Interface,
	broadcaster events.EventBroadcaster,
	recorder events.EventRecorder,
	useRealProxier bool,
	proxierSyncPeriod time.Duration,
	proxierMinSyncPeriod time.Duration,
) (*HollowProxy, error) {
	// Create proxier and service/endpoint handlers.
	var proxier proxy.Provider
	var err error

	if useRealProxier {
		nodeIP := utilnode.GetNodeIP(client, nodeName)
		if nodeIP == nil {
			klog.InfoS("Can't determine this node's IP, assuming 127.0.0.1")
			nodeIP = netutils.ParseIPSloppy("127.0.0.1")
		}
		family := v1.IPv4Protocol
		if iptInterface.IsIPv6() {
			family = v1.IPv6Protocol
		}
		// Real proxier with fake iptables, sysctl, etc underneath it.
		//var err error
		proxier, err = iptables.NewProxier(
			family,
			iptInterface,
			sysctl,
			execer,
			proxierSyncPeriod,
			proxierMinSyncPeriod,
			false,
			false,
			0,
			proxyutiliptables.NewNoOpLocalDetector(),
			nodeName,
			nodeIP,
			recorder,
			nil,
			[]string{},
		)
		if err != nil {
			return nil, fmt.Errorf("unable to create proxier: %v", err)
		}
	} else {
		proxier = &FakeProxier{}
	}

	// Create a Hollow Proxy instance.
	nodeRef := &v1.ObjectReference{
		Kind:      "Node",
		Name:      nodeName,
		UID:       types.UID(nodeName),
		Namespace: "",
	}
	return &HollowProxy{
		ProxyServer: &proxyapp.ProxyServer{
			Config: &proxyconfigapi.KubeProxyConfiguration{
				Mode:             proxyconfigapi.ProxyMode("fake"),
				ConfigSyncPeriod: metav1.Duration{Duration: 30 * time.Second},
				OOMScoreAdj:      utilpointer.Int32Ptr(0),
			},

			Client:      client,
			Proxier:     proxier,
			Broadcaster: broadcaster,
			Recorder:    recorder,
			NodeRef:     nodeRef,
		},
	}, nil
}

func (hp *HollowProxy) Run() error {
	if err := hp.ProxyServer.Run(); err != nil {
		return fmt.Errorf("Error while running proxy: %w", err)
	}
	return nil
}
