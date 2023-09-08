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

package proxy

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/pointer"
)

func (proxier *FakeProxier) addEndpointSlice(slice *discovery.EndpointSlice) {
	proxier.endpointsChanges.EndpointSliceUpdate(slice, false)
}

func (proxier *FakeProxier) updateEndpointSlice(oldSlice, slice *discovery.EndpointSlice) {
	proxier.endpointsChanges.EndpointSliceUpdate(slice, false)
}

func (proxier *FakeProxier) deleteEndpointSlice(slice *discovery.EndpointSlice) {
	proxier.endpointsChanges.EndpointSliceUpdate(slice, true)
}

func TestGetLocalEndpointIPs(t *testing.T) {
	testCases := []struct {
		endpointsMap EndpointsMap
		expected     map[types.NamespacedName]sets.Set[string]
	}{{
		// Case[0]: nothing
		endpointsMap: EndpointsMap{},
		expected:     map[types.NamespacedName]sets.Set[string]{},
	}, {
		// Case[1]: unnamed port
		endpointsMap: EndpointsMap{
			makeServicePortName("ns1", "ep1", "", v1.ProtocolTCP): []Endpoint{
				&BaseEndpointInfo{Endpoint: "1.1.1.1:11", IsLocal: false, Ready: true, Serving: true, Terminating: false},
			},
		},
		expected: map[types.NamespacedName]sets.Set[string]{},
	}, {
		// Case[2]: unnamed port local
		endpointsMap: EndpointsMap{
			makeServicePortName("ns1", "ep1", "", v1.ProtocolTCP): []Endpoint{
				&BaseEndpointInfo{Endpoint: "1.1.1.1:11", IsLocal: true, Ready: true, Serving: true, Terminating: false},
			},
		},
		expected: map[types.NamespacedName]sets.Set[string]{
			{Namespace: "ns1", Name: "ep1"}: sets.New[string]("1.1.1.1"),
		},
	}, {
		// Case[3]: named local and non-local ports for the same IP.
		endpointsMap: EndpointsMap{
			makeServicePortName("ns1", "ep1", "p11", v1.ProtocolTCP): []Endpoint{
				&BaseEndpointInfo{Endpoint: "1.1.1.1:11", IsLocal: false, Ready: true, Serving: true, Terminating: false},
				&BaseEndpointInfo{Endpoint: "1.1.1.2:11", IsLocal: true, Ready: true, Serving: true, Terminating: false},
			},
			makeServicePortName("ns1", "ep1", "p12", v1.ProtocolTCP): []Endpoint{
				&BaseEndpointInfo{Endpoint: "1.1.1.1:12", IsLocal: false, Ready: true, Serving: true, Terminating: false},
				&BaseEndpointInfo{Endpoint: "1.1.1.2:12", IsLocal: true, Ready: true, Serving: true, Terminating: false},
			},
		},
		expected: map[types.NamespacedName]sets.Set[string]{
			{Namespace: "ns1", Name: "ep1"}: sets.New[string]("1.1.1.2"),
		},
	}, {
		// Case[4]: named local and non-local ports for different IPs.
		endpointsMap: EndpointsMap{
			makeServicePortName("ns1", "ep1", "p11", v1.ProtocolTCP): []Endpoint{
				&BaseEndpointInfo{Endpoint: "1.1.1.1:11", IsLocal: false, Ready: true, Serving: true, Terminating: false},
			},
			makeServicePortName("ns2", "ep2", "p22", v1.ProtocolTCP): []Endpoint{
				&BaseEndpointInfo{Endpoint: "2.2.2.2:22", IsLocal: true, Ready: true, Serving: true, Terminating: false},
				&BaseEndpointInfo{Endpoint: "2.2.2.22:22", IsLocal: true, Ready: true, Serving: true, Terminating: false},
			},
			makeServicePortName("ns2", "ep2", "p23", v1.ProtocolTCP): []Endpoint{
				&BaseEndpointInfo{Endpoint: "2.2.2.3:23", IsLocal: true, Ready: true, Serving: true, Terminating: false},
			},
			makeServicePortName("ns4", "ep4", "p44", v1.ProtocolTCP): []Endpoint{
				&BaseEndpointInfo{Endpoint: "4.4.4.4:44", IsLocal: true, Ready: true, Serving: true, Terminating: false},
				&BaseEndpointInfo{Endpoint: "4.4.4.5:44", IsLocal: false, Ready: true, Serving: true, Terminating: false},
			},
			makeServicePortName("ns4", "ep4", "p45", v1.ProtocolTCP): []Endpoint{
				&BaseEndpointInfo{Endpoint: "4.4.4.6:45", IsLocal: true, Ready: true, Serving: true, Terminating: false},
			},
		},
		expected: map[types.NamespacedName]sets.Set[string]{
			{Namespace: "ns2", Name: "ep2"}: sets.New[string]("2.2.2.2", "2.2.2.22", "2.2.2.3"),
			{Namespace: "ns4", Name: "ep4"}: sets.New[string]("4.4.4.4", "4.4.4.6"),
		},
	}, {
		// Case[5]: named local and non-local ports for different IPs, some not ready.
		endpointsMap: EndpointsMap{
			makeServicePortName("ns1", "ep1", "p11", v1.ProtocolTCP): []Endpoint{
				&BaseEndpointInfo{Endpoint: "1.1.1.1:11", IsLocal: false, Ready: true, Serving: true, Terminating: false},
			},
			makeServicePortName("ns2", "ep2", "p22", v1.ProtocolTCP): []Endpoint{
				&BaseEndpointInfo{Endpoint: "2.2.2.2:22", IsLocal: true, Ready: true, Serving: true, Terminating: false},
				&BaseEndpointInfo{Endpoint: "2.2.2.22:22", IsLocal: true, Ready: true, Serving: true, Terminating: false},
			},
			makeServicePortName("ns2", "ep2", "p23", v1.ProtocolTCP): []Endpoint{
				&BaseEndpointInfo{Endpoint: "2.2.2.3:23", IsLocal: true, Ready: false, Serving: true, Terminating: true},
			},
			makeServicePortName("ns4", "ep4", "p44", v1.ProtocolTCP): []Endpoint{
				&BaseEndpointInfo{Endpoint: "4.4.4.4:44", IsLocal: true, Ready: true, Serving: true, Terminating: false},
				&BaseEndpointInfo{Endpoint: "4.4.4.5:44", IsLocal: false, Ready: true, Serving: true, Terminating: false},
			},
			makeServicePortName("ns4", "ep4", "p45", v1.ProtocolTCP): []Endpoint{
				&BaseEndpointInfo{Endpoint: "4.4.4.6:45", IsLocal: true, Ready: true, Serving: true, Terminating: false},
			},
		},
		expected: map[types.NamespacedName]sets.Set[string]{
			{Namespace: "ns2", Name: "ep2"}: sets.New[string]("2.2.2.2", "2.2.2.22"),
			{Namespace: "ns4", Name: "ep4"}: sets.New[string]("4.4.4.4", "4.4.4.6"),
		},
	}, {
		// Case[6]: all endpoints are terminating,, so getLocalReadyEndpointIPs should return 0 ready endpoints
		endpointsMap: EndpointsMap{
			makeServicePortName("ns1", "ep1", "p11", v1.ProtocolTCP): []Endpoint{
				&BaseEndpointInfo{Endpoint: "1.1.1.1:11", IsLocal: false, Ready: false, Serving: true, Terminating: true},
			},
			makeServicePortName("ns2", "ep2", "p22", v1.ProtocolTCP): []Endpoint{
				&BaseEndpointInfo{Endpoint: "2.2.2.2:22", IsLocal: true, Ready: false, Serving: true, Terminating: true},
				&BaseEndpointInfo{Endpoint: "2.2.2.22:22", IsLocal: true, Ready: false, Serving: true, Terminating: true},
			},
			makeServicePortName("ns2", "ep2", "p23", v1.ProtocolTCP): []Endpoint{
				&BaseEndpointInfo{Endpoint: "2.2.2.3:23", IsLocal: true, Ready: false, Serving: true, Terminating: true},
			},
			makeServicePortName("ns4", "ep4", "p44", v1.ProtocolTCP): []Endpoint{
				&BaseEndpointInfo{Endpoint: "4.4.4.4:44", IsLocal: true, Ready: false, Serving: true, Terminating: true},
				&BaseEndpointInfo{Endpoint: "4.4.4.5:44", IsLocal: false, Ready: false, Serving: true, Terminating: true},
			},
			makeServicePortName("ns4", "ep4", "p45", v1.ProtocolTCP): []Endpoint{
				&BaseEndpointInfo{Endpoint: "4.4.4.6:45", IsLocal: true, Ready: false, Serving: true, Terminating: true},
			},
		},
		expected: make(map[types.NamespacedName]sets.Set[string], 0),
	}}

	for tci, tc := range testCases {
		// outputs
		localIPs := tc.endpointsMap.getLocalReadyEndpointIPs()

		if !reflect.DeepEqual(localIPs, tc.expected) {
			t.Errorf("[%d] expected %#v, got %#v", tci, tc.expected, localIPs)
		}
	}
}

func makeTestEndpointSlice(namespace, name string, slice int, epsFunc func(*discovery.EndpointSlice)) *discovery.EndpointSlice {
	eps := &discovery.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:        fmt.Sprintf("%s-%d", name, slice),
			Namespace:   namespace,
			Annotations: map[string]string{},
			Labels: map[string]string{
				discovery.LabelServiceName: name,
			},
		},
		AddressType: discovery.AddressTypeIPv4,
	}
	epsFunc(eps)
	return eps
}

func TestUpdateEndpointsMap(t *testing.T) {
	var nodeName = testHostname
	udp := v1.ProtocolUDP

	emptyEndpoint := func(eps *discovery.EndpointSlice) {
		eps.Endpoints = []discovery.Endpoint{}
	}
	unnamedPort := func(eps *discovery.EndpointSlice) {
		eps.Endpoints = []discovery.Endpoint{{
			Addresses: []string{"1.1.1.1"},
		}}
		eps.Ports = []discovery.EndpointPort{{
			Name:     pointer.String(""),
			Port:     pointer.Int32(11),
			Protocol: &udp,
		}}
	}
	unnamedPortLocal := func(eps *discovery.EndpointSlice) {
		eps.Endpoints = []discovery.Endpoint{{
			Addresses: []string{"1.1.1.1"},
			NodeName:  &nodeName,
		}}
		eps.Ports = []discovery.EndpointPort{{
			Name:     pointer.String(""),
			Port:     pointer.Int32(11),
			Protocol: &udp,
		}}
	}
	namedPortLocal := func(eps *discovery.EndpointSlice) {
		eps.Endpoints = []discovery.Endpoint{{
			Addresses: []string{"1.1.1.1"},
			NodeName:  &nodeName,
		}}
		eps.Ports = []discovery.EndpointPort{{
			Name:     pointer.String("p11"),
			Port:     pointer.Int32(11),
			Protocol: &udp,
		}}
	}
	namedPort := func(eps *discovery.EndpointSlice) {
		eps.Endpoints = []discovery.Endpoint{{
			Addresses: []string{"1.1.1.1"},
		}}
		eps.Ports = []discovery.EndpointPort{{
			Name:     pointer.String("p11"),
			Port:     pointer.Int32(11),
			Protocol: &udp,
		}}
	}
	namedPortRenamed := func(eps *discovery.EndpointSlice) {
		eps.Endpoints = []discovery.Endpoint{{
			Addresses: []string{"1.1.1.1"},
		}}
		eps.Ports = []discovery.EndpointPort{{
			Name:     pointer.String("p11-2"),
			Port:     pointer.Int32(11),
			Protocol: &udp,
		}}
	}
	namedPortRenumbered := func(eps *discovery.EndpointSlice) {
		eps.Endpoints = []discovery.Endpoint{{
			Addresses: []string{"1.1.1.1"},
		}}
		eps.Ports = []discovery.EndpointPort{{
			Name:     pointer.String("p11"),
			Port:     pointer.Int32(22),
			Protocol: &udp,
		}}
	}
	namedPortsLocalNoLocal := func(eps *discovery.EndpointSlice) {
		eps.Endpoints = []discovery.Endpoint{{
			Addresses: []string{"1.1.1.1"},
		}, {
			Addresses: []string{"1.1.1.2"},
			NodeName:  &nodeName,
		}}
		eps.Ports = []discovery.EndpointPort{{
			Name:     pointer.String("p11"),
			Port:     pointer.Int32(11),
			Protocol: &udp,
		}, {
			Name:     pointer.String("p12"),
			Port:     pointer.Int32(12),
			Protocol: &udp,
		}}
	}
	multipleSubsets_s1 := func(eps *discovery.EndpointSlice) {
		eps.Endpoints = []discovery.Endpoint{{
			Addresses: []string{"1.1.1.1"},
		}}
		eps.Ports = []discovery.EndpointPort{{
			Name:     pointer.String("p11"),
			Port:     pointer.Int32(11),
			Protocol: &udp,
		}}
	}
	multipleSubsets_s2 := func(eps *discovery.EndpointSlice) {
		eps.Endpoints = []discovery.Endpoint{{
			Addresses: []string{"1.1.1.2"},
		}}
		eps.Ports = []discovery.EndpointPort{{
			Name:     pointer.String("p12"),
			Port:     pointer.Int32(12),
			Protocol: &udp,
		}}
	}
	multipleSubsetsWithLocal_s1 := func(eps *discovery.EndpointSlice) {
		eps.Endpoints = []discovery.Endpoint{{
			Addresses: []string{"1.1.1.1"},
		}}
		eps.Ports = []discovery.EndpointPort{{
			Name:     pointer.String("p11"),
			Port:     pointer.Int32(11),
			Protocol: &udp,
		}}
	}
	multipleSubsetsWithLocal_s2 := func(eps *discovery.EndpointSlice) {
		eps.Endpoints = []discovery.Endpoint{{
			Addresses: []string{"1.1.1.2"},
			NodeName:  &nodeName,
		}}
		eps.Ports = []discovery.EndpointPort{{
			Name:     pointer.String("p12"),
			Port:     pointer.Int32(12),
			Protocol: &udp,
		}}
	}
	multipleSubsetsMultiplePortsLocal_s1 := func(eps *discovery.EndpointSlice) {
		eps.Endpoints = []discovery.Endpoint{{
			Addresses: []string{"1.1.1.1"},
			NodeName:  &nodeName,
		}}
		eps.Ports = []discovery.EndpointPort{{
			Name:     pointer.String("p11"),
			Port:     pointer.Int32(11),
			Protocol: &udp,
		}, {
			Name:     pointer.String("p12"),
			Port:     pointer.Int32(12),
			Protocol: &udp,
		}}
	}
	multipleSubsetsMultiplePortsLocal_s2 := func(eps *discovery.EndpointSlice) {
		eps.Endpoints = []discovery.Endpoint{{
			Addresses: []string{"1.1.1.3"},
		}}
		eps.Ports = []discovery.EndpointPort{{
			Name:     pointer.String("p13"),
			Port:     pointer.Int32(13),
			Protocol: &udp,
		}}
	}
	multipleSubsetsIPsPorts1_s1 := func(eps *discovery.EndpointSlice) {
		eps.Endpoints = []discovery.Endpoint{{
			Addresses: []string{"1.1.1.1"},
		}, {
			Addresses: []string{"1.1.1.2"},
			NodeName:  &nodeName,
		}}
		eps.Ports = []discovery.EndpointPort{{
			Name:     pointer.String("p11"),
			Port:     pointer.Int32(11),
			Protocol: &udp,
		}, {
			Name:     pointer.String("p12"),
			Port:     pointer.Int32(12),
			Protocol: &udp,
		}}
	}
	multipleSubsetsIPsPorts1_s2 := func(eps *discovery.EndpointSlice) {
		eps.Endpoints = []discovery.Endpoint{{
			Addresses: []string{"1.1.1.3"},
		}, {
			Addresses: []string{"1.1.1.4"},
			NodeName:  &nodeName,
		}}
		eps.Ports = []discovery.EndpointPort{{
			Name:     pointer.String("p13"),
			Port:     pointer.Int32(13),
			Protocol: &udp,
		}, {
			Name:     pointer.String("p14"),
			Port:     pointer.Int32(14),
			Protocol: &udp,
		}}
	}
	multipleSubsetsIPsPorts2 := func(eps *discovery.EndpointSlice) {
		eps.Endpoints = []discovery.Endpoint{{
			Addresses: []string{"2.2.2.1"},
		}, {
			Addresses: []string{"2.2.2.2"},
			NodeName:  &nodeName,
		}}
		eps.Ports = []discovery.EndpointPort{{
			Name:     pointer.String("p21"),
			Port:     pointer.Int32(21),
			Protocol: &udp,
		}, {
			Name:     pointer.String("p22"),
			Port:     pointer.Int32(22),
			Protocol: &udp,
		}}
	}
	complexBefore1 := func(eps *discovery.EndpointSlice) {
		eps.Endpoints = []discovery.Endpoint{{
			Addresses: []string{"1.1.1.1"},
		}}
		eps.Ports = []discovery.EndpointPort{{
			Name:     pointer.String("p11"),
			Port:     pointer.Int32(11),
			Protocol: &udp,
		}}
	}
	complexBefore2_s1 := func(eps *discovery.EndpointSlice) {
		eps.Endpoints = []discovery.Endpoint{{
			Addresses: []string{"2.2.2.2"},
			NodeName:  &nodeName,
		}, {
			Addresses: []string{"2.2.2.22"},
			NodeName:  &nodeName,
		}}
		eps.Ports = []discovery.EndpointPort{{
			Name:     pointer.String("p22"),
			Port:     pointer.Int32(22),
			Protocol: &udp,
		}}
	}
	complexBefore2_s2 := func(eps *discovery.EndpointSlice) {
		eps.Endpoints = []discovery.Endpoint{{
			Addresses: []string{"2.2.2.3"},
			NodeName:  &nodeName,
		}}
		eps.Ports = []discovery.EndpointPort{{
			Name:     pointer.String("p23"),
			Port:     pointer.Int32(23),
			Protocol: &udp,
		}}
	}
	complexBefore4_s1 := func(eps *discovery.EndpointSlice) {
		eps.Endpoints = []discovery.Endpoint{{
			Addresses: []string{"4.4.4.4"},
			NodeName:  &nodeName,
		}, {
			Addresses: []string{"4.4.4.5"},
			NodeName:  &nodeName,
		}}
		eps.Ports = []discovery.EndpointPort{{
			Name:     pointer.String("p44"),
			Port:     pointer.Int32(44),
			Protocol: &udp,
		}}
	}
	complexBefore4_s2 := func(eps *discovery.EndpointSlice) {
		eps.Endpoints = []discovery.Endpoint{{
			Addresses: []string{"4.4.4.6"},
			NodeName:  &nodeName,
		}}
		eps.Ports = []discovery.EndpointPort{{
			Name:     pointer.String("p45"),
			Port:     pointer.Int32(45),
			Protocol: &udp,
		}}
	}
	complexAfter1_s1 := func(eps *discovery.EndpointSlice) {
		eps.Endpoints = []discovery.Endpoint{{
			Addresses: []string{"1.1.1.1"},
		}, {
			Addresses: []string{"1.1.1.11"},
		}}
		eps.Ports = []discovery.EndpointPort{{
			Name:     pointer.String("p11"),
			Port:     pointer.Int32(11),
			Protocol: &udp,
		}}
	}
	complexAfter1_s2 := func(eps *discovery.EndpointSlice) {
		eps.Endpoints = []discovery.Endpoint{{
			Addresses: []string{"1.1.1.2"},
		}}
		eps.Ports = []discovery.EndpointPort{{
			Name:     pointer.String("p12"),
			Port:     pointer.Int32(12),
			Protocol: &udp,
		}, {
			Name:     pointer.String("p122"),
			Port:     pointer.Int32(122),
			Protocol: &udp,
		}}
	}
	complexAfter3 := func(eps *discovery.EndpointSlice) {
		eps.Endpoints = []discovery.Endpoint{{
			Addresses: []string{"3.3.3.3"},
		}}
		eps.Ports = []discovery.EndpointPort{{
			Name:     pointer.String("p33"),
			Port:     pointer.Int32(33),
			Protocol: &udp,
		}}
	}
	complexAfter4 := func(eps *discovery.EndpointSlice) {
		eps.Endpoints = []discovery.Endpoint{{
			Addresses: []string{"4.4.4.4"},
			NodeName:  &nodeName,
		}}
		eps.Ports = []discovery.EndpointPort{{
			Name:     pointer.String("p44"),
			Port:     pointer.Int32(44),
			Protocol: &udp,
		}}
	}

	testCases := []struct {
		// previousEndpoints and currentEndpoints are used to call appropriate
		// handlers OnEndpointSlice* (based on whether corresponding values are nil
		// or non-nil) and must be of equal length.
		name                           string
		previousEndpoints              []*discovery.EndpointSlice
		currentEndpoints               []*discovery.EndpointSlice
		oldEndpoints                   map[ServicePortName][]*BaseEndpointInfo
		expectedResult                 map[ServicePortName][]*BaseEndpointInfo
		expectedDeletedUDPEndpoints    []ServiceEndpoint
		expectedNewlyActiveUDPServices map[ServicePortName]bool
		expectedLocalEndpoints         map[types.NamespacedName]int
		expectedChangedEndpoints       sets.Set[string]
	}{{
		name:                           "empty",
		oldEndpoints:                   map[ServicePortName][]*BaseEndpointInfo{},
		expectedResult:                 map[ServicePortName][]*BaseEndpointInfo{},
		expectedDeletedUDPEndpoints:    []ServiceEndpoint{},
		expectedNewlyActiveUDPServices: map[ServicePortName]bool{},
		expectedLocalEndpoints:         map[types.NamespacedName]int{},
		expectedChangedEndpoints:       sets.New[string](),
	}, {
		name: "no change, unnamed port",
		previousEndpoints: []*discovery.EndpointSlice{
			makeTestEndpointSlice("ns1", "ep1", 1, unnamedPort),
		},
		currentEndpoints: []*discovery.EndpointSlice{
			makeTestEndpointSlice("ns1", "ep1", 1, unnamedPort),
		},
		oldEndpoints: map[ServicePortName][]*BaseEndpointInfo{
			makeServicePortName("ns1", "ep1", "", v1.ProtocolUDP): {
				{Endpoint: "1.1.1.1:11", IsLocal: false, Ready: true, Serving: true, Terminating: false},
			},
		},
		expectedResult: map[ServicePortName][]*BaseEndpointInfo{
			makeServicePortName("ns1", "ep1", "", v1.ProtocolUDP): {
				{Endpoint: "1.1.1.1:11", IsLocal: false, Ready: true, Serving: true, Terminating: false},
			},
		},
		expectedDeletedUDPEndpoints:    []ServiceEndpoint{},
		expectedNewlyActiveUDPServices: map[ServicePortName]bool{},
		expectedLocalEndpoints:         map[types.NamespacedName]int{},
		expectedChangedEndpoints:       sets.New[string](),
	}, {
		name: "no change, named port, local",
		previousEndpoints: []*discovery.EndpointSlice{
			makeTestEndpointSlice("ns1", "ep1", 1, namedPortLocal),
		},
		currentEndpoints: []*discovery.EndpointSlice{
			makeTestEndpointSlice("ns1", "ep1", 1, namedPortLocal),
		},
		oldEndpoints: map[ServicePortName][]*BaseEndpointInfo{
			makeServicePortName("ns1", "ep1", "p11", v1.ProtocolUDP): {
				{Endpoint: "1.1.1.1:11", IsLocal: true, Ready: true, Serving: true, Terminating: false},
			},
		},
		expectedResult: map[ServicePortName][]*BaseEndpointInfo{
			makeServicePortName("ns1", "ep1", "p11", v1.ProtocolUDP): {
				{Endpoint: "1.1.1.1:11", IsLocal: true, Ready: true, Serving: true, Terminating: false},
			},
		},
		expectedDeletedUDPEndpoints:    []ServiceEndpoint{},
		expectedNewlyActiveUDPServices: map[ServicePortName]bool{},
		expectedLocalEndpoints: map[types.NamespacedName]int{
			makeNSN("ns1", "ep1"): 1,
		},
		expectedChangedEndpoints: sets.New[string](),
	}, {
		name: "no change, multiple slices",
		previousEndpoints: []*discovery.EndpointSlice{
			makeTestEndpointSlice("ns1", "ep1", 1, multipleSubsets_s1),
			makeTestEndpointSlice("ns1", "ep1", 2, multipleSubsets_s2),
		},
		currentEndpoints: []*discovery.EndpointSlice{
			makeTestEndpointSlice("ns1", "ep1", 1, multipleSubsets_s1),
			makeTestEndpointSlice("ns1", "ep1", 2, multipleSubsets_s2),
		},
		oldEndpoints: map[ServicePortName][]*BaseEndpointInfo{
			makeServicePortName("ns1", "ep1", "p11", v1.ProtocolUDP): {
				{Endpoint: "1.1.1.1:11", IsLocal: false, Ready: true, Serving: true, Terminating: false},
			},
			makeServicePortName("ns1", "ep1", "p12", v1.ProtocolUDP): {
				{Endpoint: "1.1.1.2:12", IsLocal: false, Ready: true, Serving: true, Terminating: false},
			},
		},
		expectedResult: map[ServicePortName][]*BaseEndpointInfo{
			makeServicePortName("ns1", "ep1", "p11", v1.ProtocolUDP): {
				{Endpoint: "1.1.1.1:11", IsLocal: false, Ready: true, Serving: true, Terminating: false},
			},
			makeServicePortName("ns1", "ep1", "p12", v1.ProtocolUDP): {
				{Endpoint: "1.1.1.2:12", IsLocal: false, Ready: true, Serving: true, Terminating: false},
			},
		},
		expectedDeletedUDPEndpoints:    []ServiceEndpoint{},
		expectedNewlyActiveUDPServices: map[ServicePortName]bool{},
		expectedLocalEndpoints:         map[types.NamespacedName]int{},
		expectedChangedEndpoints:       sets.New[string](),
	}, {
		name: "no change, multiple slices, multiple ports, local",
		previousEndpoints: []*discovery.EndpointSlice{
			makeTestEndpointSlice("ns1", "ep1", 1, multipleSubsetsMultiplePortsLocal_s1),
			makeTestEndpointSlice("ns1", "ep1", 2, multipleSubsetsMultiplePortsLocal_s2),
		},
		currentEndpoints: []*discovery.EndpointSlice{
			makeTestEndpointSlice("ns1", "ep1", 1, multipleSubsetsMultiplePortsLocal_s1),
			makeTestEndpointSlice("ns1", "ep1", 2, multipleSubsetsMultiplePortsLocal_s2),
		},
		oldEndpoints: map[ServicePortName][]*BaseEndpointInfo{
			makeServicePortName("ns1", "ep1", "p11", v1.ProtocolUDP): {
				{Endpoint: "1.1.1.1:11", IsLocal: true, Ready: true, Serving: true, Terminating: false},
			},
			makeServicePortName("ns1", "ep1", "p12", v1.ProtocolUDP): {
				{Endpoint: "1.1.1.1:12", IsLocal: true, Ready: true, Serving: true, Terminating: false},
			},
			makeServicePortName("ns1", "ep1", "p13", v1.ProtocolUDP): {
				{Endpoint: "1.1.1.3:13", IsLocal: false, Ready: true, Serving: true, Terminating: false},
			},
		},
		expectedResult: map[ServicePortName][]*BaseEndpointInfo{
			makeServicePortName("ns1", "ep1", "p11", v1.ProtocolUDP): {
				{Endpoint: "1.1.1.1:11", IsLocal: true, Ready: true, Serving: true, Terminating: false},
			},
			makeServicePortName("ns1", "ep1", "p12", v1.ProtocolUDP): {
				{Endpoint: "1.1.1.1:12", IsLocal: true, Ready: true, Serving: true, Terminating: false},
			},
			makeServicePortName("ns1", "ep1", "p13", v1.ProtocolUDP): {
				{Endpoint: "1.1.1.3:13", IsLocal: false, Ready: true, Serving: true, Terminating: false},
			},
		},
		expectedDeletedUDPEndpoints:    []ServiceEndpoint{},
		expectedNewlyActiveUDPServices: map[ServicePortName]bool{},
		expectedLocalEndpoints: map[types.NamespacedName]int{
			makeNSN("ns1", "ep1"): 1,
		},
		expectedChangedEndpoints: sets.New[string](),
	}, {
		name: "no change, multiple services, slices, IPs, and ports",
		previousEndpoints: []*discovery.EndpointSlice{
			makeTestEndpointSlice("ns1", "ep1", 1, multipleSubsetsIPsPorts1_s1),
			makeTestEndpointSlice("ns1", "ep1", 2, multipleSubsetsIPsPorts1_s2),
			makeTestEndpointSlice("ns2", "ep2", 1, multipleSubsetsIPsPorts2),
		},
		currentEndpoints: []*discovery.EndpointSlice{
			makeTestEndpointSlice("ns1", "ep1", 1, multipleSubsetsIPsPorts1_s1),
			makeTestEndpointSlice("ns1", "ep1", 2, multipleSubsetsIPsPorts1_s2),
			makeTestEndpointSlice("ns2", "ep2", 1, multipleSubsetsIPsPorts2),
		},
		oldEndpoints: map[ServicePortName][]*BaseEndpointInfo{
			makeServicePortName("ns1", "ep1", "p11", v1.ProtocolUDP): {
				{Endpoint: "1.1.1.1:11", IsLocal: false, Ready: true, Serving: true, Terminating: false},
				{Endpoint: "1.1.1.2:11", IsLocal: true, Ready: true, Serving: true, Terminating: false},
			},
			makeServicePortName("ns1", "ep1", "p12", v1.ProtocolUDP): {
				{Endpoint: "1.1.1.1:12", IsLocal: false, Ready: true, Serving: true, Terminating: false},
				{Endpoint: "1.1.1.2:12", IsLocal: true, Ready: true, Serving: true, Terminating: false},
			},
			makeServicePortName("ns1", "ep1", "p13", v1.ProtocolUDP): {
				{Endpoint: "1.1.1.3:13", IsLocal: false, Ready: true, Serving: true, Terminating: false},
				{Endpoint: "1.1.1.4:13", IsLocal: true, Ready: true, Serving: true, Terminating: false},
			},
			makeServicePortName("ns1", "ep1", "p14", v1.ProtocolUDP): {
				{Endpoint: "1.1.1.3:14", IsLocal: false, Ready: true, Serving: true, Terminating: false},
				{Endpoint: "1.1.1.4:14", IsLocal: true, Ready: true, Serving: true, Terminating: false},
			},
			makeServicePortName("ns2", "ep2", "p21", v1.ProtocolUDP): {
				{Endpoint: "2.2.2.1:21", IsLocal: false, Ready: true, Serving: true, Terminating: false},
				{Endpoint: "2.2.2.2:21", IsLocal: true, Ready: true, Serving: true, Terminating: false},
			},
			makeServicePortName("ns2", "ep2", "p22", v1.ProtocolUDP): {
				{Endpoint: "2.2.2.1:22", IsLocal: false, Ready: true, Serving: true, Terminating: false},
				{Endpoint: "2.2.2.2:22", IsLocal: true, Ready: true, Serving: true, Terminating: false},
			},
		},
		expectedResult: map[ServicePortName][]*BaseEndpointInfo{
			makeServicePortName("ns1", "ep1", "p11", v1.ProtocolUDP): {
				{Endpoint: "1.1.1.1:11", IsLocal: false, Ready: true, Serving: true, Terminating: false},
				{Endpoint: "1.1.1.2:11", IsLocal: true, Ready: true, Serving: true, Terminating: false},
			},
			makeServicePortName("ns1", "ep1", "p12", v1.ProtocolUDP): {
				{Endpoint: "1.1.1.1:12", IsLocal: false, Ready: true, Serving: true, Terminating: false},
				{Endpoint: "1.1.1.2:12", IsLocal: true, Ready: true, Serving: true, Terminating: false},
			},
			makeServicePortName("ns1", "ep1", "p13", v1.ProtocolUDP): {
				{Endpoint: "1.1.1.3:13", IsLocal: false, Ready: true, Serving: true, Terminating: false},
				{Endpoint: "1.1.1.4:13", IsLocal: true, Ready: true, Serving: true, Terminating: false},
			},
			makeServicePortName("ns1", "ep1", "p14", v1.ProtocolUDP): {
				{Endpoint: "1.1.1.3:14", IsLocal: false, Ready: true, Serving: true, Terminating: false},
				{Endpoint: "1.1.1.4:14", IsLocal: true, Ready: true, Serving: true, Terminating: false},
			},
			makeServicePortName("ns2", "ep2", "p21", v1.ProtocolUDP): {
				{Endpoint: "2.2.2.1:21", IsLocal: false, Ready: true, Serving: true, Terminating: false},
				{Endpoint: "2.2.2.2:21", IsLocal: true, Ready: true, Serving: true, Terminating: false},
			},
			makeServicePortName("ns2", "ep2", "p22", v1.ProtocolUDP): {
				{Endpoint: "2.2.2.1:22", IsLocal: false, Ready: true, Serving: true, Terminating: false},
				{Endpoint: "2.2.2.2:22", IsLocal: true, Ready: true, Serving: true, Terminating: false},
			},
		},
		expectedDeletedUDPEndpoints:    []ServiceEndpoint{},
		expectedNewlyActiveUDPServices: map[ServicePortName]bool{},
		expectedLocalEndpoints: map[types.NamespacedName]int{
			makeNSN("ns1", "ep1"): 2,
			makeNSN("ns2", "ep2"): 1,
		},
		expectedChangedEndpoints: sets.New[string](),
	}, {
		name: "add an EndpointSlice",
		previousEndpoints: []*discovery.EndpointSlice{
			nil,
		},
		currentEndpoints: []*discovery.EndpointSlice{
			makeTestEndpointSlice("ns1", "ep1", 1, unnamedPortLocal),
		},
		oldEndpoints: map[ServicePortName][]*BaseEndpointInfo{},
		expectedResult: map[ServicePortName][]*BaseEndpointInfo{
			makeServicePortName("ns1", "ep1", "", v1.ProtocolUDP): {
				{Endpoint: "1.1.1.1:11", IsLocal: true, Ready: true, Serving: true, Terminating: false},
			},
		},
		expectedDeletedUDPEndpoints: []ServiceEndpoint{},
		expectedNewlyActiveUDPServices: map[ServicePortName]bool{
			makeServicePortName("ns1", "ep1", "", v1.ProtocolUDP): true,
		},
		expectedLocalEndpoints: map[types.NamespacedName]int{
			makeNSN("ns1", "ep1"): 1,
		},
		expectedChangedEndpoints: sets.New[string]("ns1/ep1"),
	}, {
		name: "remove an EndpointSlice",
		previousEndpoints: []*discovery.EndpointSlice{
			makeTestEndpointSlice("ns1", "ep1", 1, unnamedPortLocal),
		},
		currentEndpoints: []*discovery.EndpointSlice{
			nil,
		},
		oldEndpoints: map[ServicePortName][]*BaseEndpointInfo{
			makeServicePortName("ns1", "ep1", "", v1.ProtocolUDP): {
				{Endpoint: "1.1.1.1:11", IsLocal: true, Ready: true, Serving: true, Terminating: false},
			},
		},
		expectedResult: map[ServicePortName][]*BaseEndpointInfo{},
		expectedDeletedUDPEndpoints: []ServiceEndpoint{{
			Endpoint:        "1.1.1.1:11",
			ServicePortName: makeServicePortName("ns1", "ep1", "", v1.ProtocolUDP),
		}},
		expectedNewlyActiveUDPServices: map[ServicePortName]bool{},
		expectedLocalEndpoints:         map[types.NamespacedName]int{},
		expectedChangedEndpoints:       sets.New[string]("ns1/ep1"),
	}, {
		name: "add an IP and port",
		previousEndpoints: []*discovery.EndpointSlice{
			makeTestEndpointSlice("ns1", "ep1", 1, namedPort),
		},
		currentEndpoints: []*discovery.EndpointSlice{
			makeTestEndpointSlice("ns1", "ep1", 1, namedPortsLocalNoLocal),
		},
		oldEndpoints: map[ServicePortName][]*BaseEndpointInfo{
			makeServicePortName("ns1", "ep1", "p11", v1.ProtocolUDP): {
				{Endpoint: "1.1.1.1:11", IsLocal: false, Ready: true, Serving: true, Terminating: false},
			},
		},
		expectedResult: map[ServicePortName][]*BaseEndpointInfo{
			makeServicePortName("ns1", "ep1", "p11", v1.ProtocolUDP): {
				{Endpoint: "1.1.1.1:11", IsLocal: false, Ready: true, Serving: true, Terminating: false},
				{Endpoint: "1.1.1.2:11", IsLocal: true, Ready: true, Serving: true, Terminating: false},
			},
			makeServicePortName("ns1", "ep1", "p12", v1.ProtocolUDP): {
				{Endpoint: "1.1.1.1:12", IsLocal: false, Ready: true, Serving: true, Terminating: false},
				{Endpoint: "1.1.1.2:12", IsLocal: true, Ready: true, Serving: true, Terminating: false},
			},
		},
		expectedDeletedUDPEndpoints: []ServiceEndpoint{},
		expectedNewlyActiveUDPServices: map[ServicePortName]bool{
			makeServicePortName("ns1", "ep1", "p12", v1.ProtocolUDP): true,
		},
		expectedLocalEndpoints: map[types.NamespacedName]int{
			makeNSN("ns1", "ep1"): 1,
		},
		expectedChangedEndpoints: sets.New[string]("ns1/ep1"),
	}, {
		name: "remove an IP and port",
		previousEndpoints: []*discovery.EndpointSlice{
			makeTestEndpointSlice("ns1", "ep1", 1, namedPortsLocalNoLocal),
		},
		currentEndpoints: []*discovery.EndpointSlice{
			makeTestEndpointSlice("ns1", "ep1", 1, namedPort),
		},
		oldEndpoints: map[ServicePortName][]*BaseEndpointInfo{
			makeServicePortName("ns1", "ep1", "p11", v1.ProtocolUDP): {
				{Endpoint: "1.1.1.1:11", IsLocal: false, Ready: true, Serving: true, Terminating: false},
				{Endpoint: "1.1.1.2:11", IsLocal: true, Ready: true, Serving: true, Terminating: false},
			},
			makeServicePortName("ns1", "ep1", "p12", v1.ProtocolUDP): {
				{Endpoint: "1.1.1.1:12", IsLocal: false, Ready: true, Serving: true, Terminating: false},
				{Endpoint: "1.1.1.2:12", IsLocal: true, Ready: true, Serving: true, Terminating: false},
			},
		},
		expectedResult: map[ServicePortName][]*BaseEndpointInfo{
			makeServicePortName("ns1", "ep1", "p11", v1.ProtocolUDP): {
				{Endpoint: "1.1.1.1:11", IsLocal: false, Ready: true, Serving: true, Terminating: false},
			},
		},
		expectedDeletedUDPEndpoints: []ServiceEndpoint{{
			Endpoint:        "1.1.1.2:11",
			ServicePortName: makeServicePortName("ns1", "ep1", "p11", v1.ProtocolUDP),
		}, {
			Endpoint:        "1.1.1.1:12",
			ServicePortName: makeServicePortName("ns1", "ep1", "p12", v1.ProtocolUDP),
		}, {
			Endpoint:        "1.1.1.2:12",
			ServicePortName: makeServicePortName("ns1", "ep1", "p12", v1.ProtocolUDP),
		}},
		expectedNewlyActiveUDPServices: map[ServicePortName]bool{},
		expectedLocalEndpoints:         map[types.NamespacedName]int{},
		expectedChangedEndpoints:       sets.New[string]("ns1/ep1"),
	}, {
		name: "add a slice to an endpoint",
		previousEndpoints: []*discovery.EndpointSlice{
			makeTestEndpointSlice("ns1", "ep1", 1, namedPort),
			nil,
		},
		currentEndpoints: []*discovery.EndpointSlice{
			makeTestEndpointSlice("ns1", "ep1", 1, multipleSubsetsWithLocal_s1),
			makeTestEndpointSlice("ns1", "ep1", 2, multipleSubsetsWithLocal_s2),
		},
		oldEndpoints: map[ServicePortName][]*BaseEndpointInfo{
			makeServicePortName("ns1", "ep1", "p11", v1.ProtocolUDP): {
				{Endpoint: "1.1.1.1:11", IsLocal: false, Ready: true, Serving: true, Terminating: false},
			},
		},
		expectedResult: map[ServicePortName][]*BaseEndpointInfo{
			makeServicePortName("ns1", "ep1", "p11", v1.ProtocolUDP): {
				{Endpoint: "1.1.1.1:11", IsLocal: false, Ready: true, Serving: true, Terminating: false},
			},
			makeServicePortName("ns1", "ep1", "p12", v1.ProtocolUDP): {
				{Endpoint: "1.1.1.2:12", IsLocal: true, Ready: true, Serving: true, Terminating: false},
			},
		},
		expectedDeletedUDPEndpoints: []ServiceEndpoint{},
		expectedNewlyActiveUDPServices: map[ServicePortName]bool{
			makeServicePortName("ns1", "ep1", "p12", v1.ProtocolUDP): true,
		},
		expectedLocalEndpoints: map[types.NamespacedName]int{
			makeNSN("ns1", "ep1"): 1,
		},
		expectedChangedEndpoints: sets.New[string]("ns1/ep1"),
	}, {
		name: "remove a slice from an endpoint",
		previousEndpoints: []*discovery.EndpointSlice{
			makeTestEndpointSlice("ns1", "ep1", 1, multipleSubsets_s1),
			makeTestEndpointSlice("ns1", "ep1", 2, multipleSubsets_s2),
		},
		currentEndpoints: []*discovery.EndpointSlice{
			makeTestEndpointSlice("ns1", "ep1", 1, namedPort),
			nil,
		},
		oldEndpoints: map[ServicePortName][]*BaseEndpointInfo{
			makeServicePortName("ns1", "ep1", "p11", v1.ProtocolUDP): {
				{Endpoint: "1.1.1.1:11", IsLocal: false, Ready: true, Serving: true, Terminating: false},
			},
			makeServicePortName("ns1", "ep1", "p12", v1.ProtocolUDP): {
				{Endpoint: "1.1.1.2:12", IsLocal: false, Ready: true, Serving: true, Terminating: false},
			},
		},
		expectedResult: map[ServicePortName][]*BaseEndpointInfo{
			makeServicePortName("ns1", "ep1", "p11", v1.ProtocolUDP): {
				{Endpoint: "1.1.1.1:11", IsLocal: false, Ready: true, Serving: true, Terminating: false},
			},
		},
		expectedDeletedUDPEndpoints: []ServiceEndpoint{{
			Endpoint:        "1.1.1.2:12",
			ServicePortName: makeServicePortName("ns1", "ep1", "p12", v1.ProtocolUDP),
		}},
		expectedNewlyActiveUDPServices: map[ServicePortName]bool{},
		expectedLocalEndpoints:         map[types.NamespacedName]int{},
		expectedChangedEndpoints:       sets.New[string]("ns1/ep1"),
	}, {
		name: "rename a port",
		previousEndpoints: []*discovery.EndpointSlice{
			makeTestEndpointSlice("ns1", "ep1", 1, namedPort),
		},
		currentEndpoints: []*discovery.EndpointSlice{
			makeTestEndpointSlice("ns1", "ep1", 1, namedPortRenamed),
		},
		oldEndpoints: map[ServicePortName][]*BaseEndpointInfo{
			makeServicePortName("ns1", "ep1", "p11", v1.ProtocolUDP): {
				{Endpoint: "1.1.1.1:11", IsLocal: false, Ready: true, Serving: true, Terminating: false},
			},
		},
		expectedResult: map[ServicePortName][]*BaseEndpointInfo{
			makeServicePortName("ns1", "ep1", "p11-2", v1.ProtocolUDP): {
				{Endpoint: "1.1.1.1:11", IsLocal: false, Ready: true, Serving: true, Terminating: false},
			},
		},
		expectedDeletedUDPEndpoints: []ServiceEndpoint{{
			Endpoint:        "1.1.1.1:11",
			ServicePortName: makeServicePortName("ns1", "ep1", "p11", v1.ProtocolUDP),
		}},
		expectedNewlyActiveUDPServices: map[ServicePortName]bool{
			makeServicePortName("ns1", "ep1", "p11-2", v1.ProtocolUDP): true,
		},
		expectedLocalEndpoints:   map[types.NamespacedName]int{},
		expectedChangedEndpoints: sets.New[string]("ns1/ep1"),
	}, {
		name: "renumber a port",
		previousEndpoints: []*discovery.EndpointSlice{
			makeTestEndpointSlice("ns1", "ep1", 1, namedPort),
		},
		currentEndpoints: []*discovery.EndpointSlice{
			makeTestEndpointSlice("ns1", "ep1", 1, namedPortRenumbered),
		},
		oldEndpoints: map[ServicePortName][]*BaseEndpointInfo{
			makeServicePortName("ns1", "ep1", "p11", v1.ProtocolUDP): {
				{Endpoint: "1.1.1.1:11", IsLocal: false, Ready: true, Serving: true, Terminating: false},
			},
		},
		expectedResult: map[ServicePortName][]*BaseEndpointInfo{
			makeServicePortName("ns1", "ep1", "p11", v1.ProtocolUDP): {
				{Endpoint: "1.1.1.1:22", IsLocal: false, Ready: true, Serving: true, Terminating: false},
			},
		},
		expectedDeletedUDPEndpoints: []ServiceEndpoint{{
			Endpoint:        "1.1.1.1:11",
			ServicePortName: makeServicePortName("ns1", "ep1", "p11", v1.ProtocolUDP),
		}},
		expectedNewlyActiveUDPServices: map[ServicePortName]bool{},
		expectedLocalEndpoints:         map[types.NamespacedName]int{},
		expectedChangedEndpoints:       sets.New[string]("ns1/ep1"),
	}, {
		name: "complex add and remove",
		previousEndpoints: []*discovery.EndpointSlice{
			makeTestEndpointSlice("ns1", "ep1", 1, complexBefore1),
			nil,

			makeTestEndpointSlice("ns2", "ep2", 1, complexBefore2_s1),
			makeTestEndpointSlice("ns2", "ep2", 2, complexBefore2_s2),

			nil,
			nil,

			makeTestEndpointSlice("ns4", "ep4", 1, complexBefore4_s1),
			makeTestEndpointSlice("ns4", "ep4", 2, complexBefore4_s2),
		},
		currentEndpoints: []*discovery.EndpointSlice{
			makeTestEndpointSlice("ns1", "ep1", 1, complexAfter1_s1),
			makeTestEndpointSlice("ns1", "ep1", 2, complexAfter1_s2),

			nil,
			nil,

			makeTestEndpointSlice("ns3", "ep3", 1, complexAfter3),
			nil,

			makeTestEndpointSlice("ns4", "ep4", 1, complexAfter4),
			nil,
		},
		oldEndpoints: map[ServicePortName][]*BaseEndpointInfo{
			makeServicePortName("ns1", "ep1", "p11", v1.ProtocolUDP): {
				{Endpoint: "1.1.1.1:11", IsLocal: false, Ready: true, Serving: true, Terminating: false},
			},
			makeServicePortName("ns2", "ep2", "p22", v1.ProtocolUDP): {
				{Endpoint: "2.2.2.22:22", IsLocal: true, Ready: true, Serving: true, Terminating: false},
				{Endpoint: "2.2.2.2:22", IsLocal: true, Ready: true, Serving: true, Terminating: false},
			},
			makeServicePortName("ns2", "ep2", "p23", v1.ProtocolUDP): {
				{Endpoint: "2.2.2.3:23", IsLocal: true, Ready: true, Serving: true, Terminating: false},
			},
			makeServicePortName("ns4", "ep4", "p44", v1.ProtocolUDP): {
				{Endpoint: "4.4.4.4:44", IsLocal: true, Ready: true, Serving: true, Terminating: false},
				{Endpoint: "4.4.4.5:44", IsLocal: true, Ready: true, Serving: true, Terminating: false},
			},
			makeServicePortName("ns4", "ep4", "p45", v1.ProtocolUDP): {
				{Endpoint: "4.4.4.6:45", IsLocal: true, Ready: true, Serving: true, Terminating: false},
			},
		},
		expectedResult: map[ServicePortName][]*BaseEndpointInfo{
			makeServicePortName("ns1", "ep1", "p11", v1.ProtocolUDP): {
				{Endpoint: "1.1.1.11:11", IsLocal: false, Ready: true, Serving: true, Terminating: false},
				{Endpoint: "1.1.1.1:11", IsLocal: false, Ready: true, Serving: true, Terminating: false},
			},
			makeServicePortName("ns1", "ep1", "p12", v1.ProtocolUDP): {
				{Endpoint: "1.1.1.2:12", IsLocal: false, Ready: true, Serving: true, Terminating: false},
			},
			makeServicePortName("ns1", "ep1", "p122", v1.ProtocolUDP): {
				{Endpoint: "1.1.1.2:122", IsLocal: false, Ready: true, Serving: true, Terminating: false},
			},
			makeServicePortName("ns3", "ep3", "p33", v1.ProtocolUDP): {
				{Endpoint: "3.3.3.3:33", IsLocal: false, Ready: true, Serving: true, Terminating: false},
			},
			makeServicePortName("ns4", "ep4", "p44", v1.ProtocolUDP): {
				{Endpoint: "4.4.4.4:44", IsLocal: true, Ready: true, Serving: true, Terminating: false},
			},
		},
		expectedDeletedUDPEndpoints: []ServiceEndpoint{{
			Endpoint:        "2.2.2.2:22",
			ServicePortName: makeServicePortName("ns2", "ep2", "p22", v1.ProtocolUDP),
		}, {
			Endpoint:        "2.2.2.22:22",
			ServicePortName: makeServicePortName("ns2", "ep2", "p22", v1.ProtocolUDP),
		}, {
			Endpoint:        "2.2.2.3:23",
			ServicePortName: makeServicePortName("ns2", "ep2", "p23", v1.ProtocolUDP),
		}, {
			Endpoint:        "4.4.4.5:44",
			ServicePortName: makeServicePortName("ns4", "ep4", "p44", v1.ProtocolUDP),
		}, {
			Endpoint:        "4.4.4.6:45",
			ServicePortName: makeServicePortName("ns4", "ep4", "p45", v1.ProtocolUDP),
		}},
		expectedNewlyActiveUDPServices: map[ServicePortName]bool{
			makeServicePortName("ns1", "ep1", "p12", v1.ProtocolUDP):  true,
			makeServicePortName("ns1", "ep1", "p122", v1.ProtocolUDP): true,
			makeServicePortName("ns3", "ep3", "p33", v1.ProtocolUDP):  true,
		},
		expectedLocalEndpoints: map[types.NamespacedName]int{
			makeNSN("ns4", "ep4"): 1,
		},
		expectedChangedEndpoints: sets.New[string]("ns1/ep1", "ns2/ep2", "ns3/ep3", "ns4/ep4"),
	}, {
		name: "change from 0 endpoint address to 1 unnamed port",
		previousEndpoints: []*discovery.EndpointSlice{
			makeTestEndpointSlice("ns1", "ep1", 1, emptyEndpoint),
		},
		currentEndpoints: []*discovery.EndpointSlice{
			makeTestEndpointSlice("ns1", "ep1", 1, unnamedPort),
		},
		oldEndpoints: map[ServicePortName][]*BaseEndpointInfo{},
		expectedResult: map[ServicePortName][]*BaseEndpointInfo{
			makeServicePortName("ns1", "ep1", "", v1.ProtocolUDP): {
				{Endpoint: "1.1.1.1:11", IsLocal: false, Ready: true, Serving: true, Terminating: false},
			},
		},
		expectedDeletedUDPEndpoints: []ServiceEndpoint{},
		expectedNewlyActiveUDPServices: map[ServicePortName]bool{
			makeServicePortName("ns1", "ep1", "", v1.ProtocolUDP): true,
		},
		expectedLocalEndpoints:   map[types.NamespacedName]int{},
		expectedChangedEndpoints: sets.New[string]("ns1/ep1"),
	},
	}

	for tci, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			fp := newFakeProxier(v1.IPv4Protocol, time.Time{})
			fp.hostname = nodeName

			// First check that after adding all previous versions of endpoints,
			// the fp.oldEndpoints is as we expect.
			for i := range tc.previousEndpoints {
				if tc.previousEndpoints[i] != nil {
					fp.addEndpointSlice(tc.previousEndpoints[i])
				}
			}
			fp.endpointsMap.Update(fp.endpointsChanges)
			compareEndpointsMapsStr(t, fp.endpointsMap, tc.oldEndpoints)

			// Now let's call appropriate handlers to get to state we want to be.
			if len(tc.previousEndpoints) != len(tc.currentEndpoints) {
				t.Fatalf("[%d] different lengths of previous and current endpoints", tci)
				return
			}

			for i := range tc.previousEndpoints {
				prev, curr := tc.previousEndpoints[i], tc.currentEndpoints[i]
				switch {
				case prev == nil && curr == nil:
					continue
				case prev == nil:
					fp.addEndpointSlice(curr)
				case curr == nil:
					fp.deleteEndpointSlice(prev)
				default:
					fp.updateEndpointSlice(prev, curr)
				}
			}

			pendingChanges := fp.endpointsChanges.PendingChanges()
			if !pendingChanges.Equal(tc.expectedChangedEndpoints) {
				t.Errorf("[%d] expected changed endpoints %q, got %q", tci, tc.expectedChangedEndpoints.UnsortedList(), pendingChanges.UnsortedList())
			}

			result := fp.endpointsMap.Update(fp.endpointsChanges)
			newMap := fp.endpointsMap
			compareEndpointsMapsStr(t, newMap, tc.expectedResult)
			if len(result.DeletedUDPEndpoints) != len(tc.expectedDeletedUDPEndpoints) {
				t.Errorf("[%d] expected %d staleEndpoints, got %d: %v", tci, len(tc.expectedDeletedUDPEndpoints), len(result.DeletedUDPEndpoints), result.DeletedUDPEndpoints)
			}
			for _, x := range tc.expectedDeletedUDPEndpoints {
				found := false
				for _, stale := range result.DeletedUDPEndpoints {
					if stale == x {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("[%d] expected staleEndpoints[%v], but didn't find it: %v", tci, x, result.DeletedUDPEndpoints)
				}
			}
			if len(result.NewlyActiveUDPServices) != len(tc.expectedNewlyActiveUDPServices) {
				t.Errorf("[%d] expected %d newlyActiveUDPServices, got %d: %v", tci, len(tc.expectedNewlyActiveUDPServices), len(result.NewlyActiveUDPServices), result.NewlyActiveUDPServices)
			}
			for svcName := range tc.expectedNewlyActiveUDPServices {
				found := false
				for _, newSvcName := range result.NewlyActiveUDPServices {
					if newSvcName == svcName {
						found = true
					}
				}
				if !found {
					t.Errorf("[%d] expected newlyActiveUDPServices[%v], but didn't find it: %v", tci, svcName, result.NewlyActiveUDPServices)
				}
			}

			localReadyEndpoints := fp.endpointsMap.LocalReadyEndpoints()
			if !reflect.DeepEqual(localReadyEndpoints, tc.expectedLocalEndpoints) {
				t.Errorf("[%d] expected local ready endpoints %v, got %v", tci, tc.expectedLocalEndpoints, localReadyEndpoints)
			}
		})
	}
}

func TestLastChangeTriggerTime(t *testing.T) {
	startTime := time.Date(2018, 01, 01, 0, 0, 0, 0, time.UTC)
	t_1 := startTime.Add(-time.Second)
	t0 := startTime.Add(time.Second)
	t1 := t0.Add(time.Second)
	t2 := t1.Add(time.Second)
	t3 := t2.Add(time.Second)

	createEndpoints := func(namespace, name string, triggerTime time.Time) *discovery.EndpointSlice {
		tcp := v1.ProtocolTCP
		return &discovery.EndpointSlice{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
				Annotations: map[string]string{
					v1.EndpointsLastChangeTriggerTime: triggerTime.Format(time.RFC3339Nano),
				},
				Labels: map[string]string{
					discovery.LabelServiceName: name,
				},
			},
			AddressType: discovery.AddressTypeIPv4,
			Endpoints: []discovery.Endpoint{{
				Addresses: []string{"1.1.1.1"},
			}},
			Ports: []discovery.EndpointPort{{
				Name:     pointer.String("p11"),
				Port:     pointer.Int32(11),
				Protocol: &tcp,
			}},
		}
	}

	createName := func(namespace, name string) types.NamespacedName {
		return types.NamespacedName{Namespace: namespace, Name: name}
	}

	modifyEndpoints := func(slice *discovery.EndpointSlice, triggerTime time.Time) *discovery.EndpointSlice {
		e := slice.DeepCopy()
		(*e.Ports[0].Port)++
		e.Annotations[v1.EndpointsLastChangeTriggerTime] = triggerTime.Format(time.RFC3339Nano)
		return e
	}

	testCases := []struct {
		name     string
		scenario func(fp *FakeProxier)
		expected map[types.NamespacedName][]time.Time
	}{
		{
			name: "Single addEndpoints",
			scenario: func(fp *FakeProxier) {
				e := createEndpoints("ns", "ep1", t0)
				fp.addEndpointSlice(e)
			},
			expected: map[types.NamespacedName][]time.Time{createName("ns", "ep1"): {t0}},
		},
		{
			name: "addEndpoints then updatedEndpoints",
			scenario: func(fp *FakeProxier) {
				e := createEndpoints("ns", "ep1", t0)
				fp.addEndpointSlice(e)

				e1 := modifyEndpoints(e, t1)
				fp.updateEndpointSlice(e, e1)
			},
			expected: map[types.NamespacedName][]time.Time{createName("ns", "ep1"): {t0, t1}},
		},
		{
			name: "Add two endpoints then modify one",
			scenario: func(fp *FakeProxier) {
				e1 := createEndpoints("ns", "ep1", t1)
				fp.addEndpointSlice(e1)

				e2 := createEndpoints("ns", "ep2", t2)
				fp.addEndpointSlice(e2)

				e11 := modifyEndpoints(e1, t3)
				fp.updateEndpointSlice(e1, e11)
			},
			expected: map[types.NamespacedName][]time.Time{createName("ns", "ep1"): {t1, t3}, createName("ns", "ep2"): {t2}},
		},
		{
			name: "Endpoints without annotation set",
			scenario: func(fp *FakeProxier) {
				e := createEndpoints("ns", "ep1", t1)
				delete(e.Annotations, v1.EndpointsLastChangeTriggerTime)
				fp.addEndpointSlice(e)
			},
			expected: map[types.NamespacedName][]time.Time{},
		},
		{
			name: "Endpoints create before tracker started",
			scenario: func(fp *FakeProxier) {
				e := createEndpoints("ns", "ep1", t_1)
				fp.addEndpointSlice(e)
			},
			expected: map[types.NamespacedName][]time.Time{},
		},
		{
			name: "addEndpoints then deleteEndpoints",
			scenario: func(fp *FakeProxier) {
				e := createEndpoints("ns", "ep1", t1)
				fp.addEndpointSlice(e)
				fp.deleteEndpointSlice(e)
			},
			expected: map[types.NamespacedName][]time.Time{},
		},
		{
			name: "add then delete then add again",
			scenario: func(fp *FakeProxier) {
				e := createEndpoints("ns", "ep1", t1)
				fp.addEndpointSlice(e)
				fp.deleteEndpointSlice(e)
				e = modifyEndpoints(e, t2)
				fp.addEndpointSlice(e)
			},
			expected: map[types.NamespacedName][]time.Time{createName("ns", "ep1"): {t2}},
		},
		{
			name: "delete",
			scenario: func(fp *FakeProxier) {
				e := createEndpoints("ns", "ep1", t1)
				fp.deleteEndpointSlice(e)
			},
			expected: map[types.NamespacedName][]time.Time{},
		},
	}

	for _, tc := range testCases {
		fp := newFakeProxier(v1.IPv4Protocol, startTime)

		tc.scenario(fp)

		result := fp.endpointsMap.Update(fp.endpointsChanges)
		got := result.LastChangeTriggerTimes

		if !reflect.DeepEqual(got, tc.expected) {
			t.Errorf("%s: Invalid LastChangeTriggerTimes, expected: %v, got: %v",
				tc.name, tc.expected, result.LastChangeTriggerTimes)
		}
	}
}

func TestEndpointSliceUpdate(t *testing.T) {
	fqdnSlice := generateEndpointSlice("svc1", "ns1", 2, 5, 999, 999, []string{"host1"}, []*int32{pointer.Int32(80), pointer.Int32(443)})
	fqdnSlice.AddressType = discovery.AddressTypeFQDN

	testCases := map[string]struct {
		startingSlices           []*discovery.EndpointSlice
		endpointChangeTracker    *EndpointChangeTracker
		namespacedName           types.NamespacedName
		paramEndpointSlice       *discovery.EndpointSlice
		paramRemoveSlice         bool
		expectedReturnVal        bool
		expectedCurrentChange    map[ServicePortName][]*BaseEndpointInfo
		expectedChangedEndpoints sets.Set[string]
	}{
		// test starting from an empty state
		"add a simple slice that doesn't already exist": {
			startingSlices:        []*discovery.EndpointSlice{},
			endpointChangeTracker: NewEndpointChangeTracker("host1", nil, v1.IPv4Protocol, nil, nil),
			namespacedName:        types.NamespacedName{Name: "svc1", Namespace: "ns1"},
			paramEndpointSlice:    generateEndpointSlice("svc1", "ns1", 1, 3, 999, 999, []string{"host1", "host2"}, []*int32{pointer.Int32(80), pointer.Int32(443)}),
			paramRemoveSlice:      false,
			expectedReturnVal:     true,
			expectedCurrentChange: map[ServicePortName][]*BaseEndpointInfo{
				makeServicePortName("ns1", "svc1", "port-0", v1.ProtocolTCP): {
					&BaseEndpointInfo{Endpoint: "10.0.1.1:80", IsLocal: false, Ready: true, Serving: true, Terminating: false},
					&BaseEndpointInfo{Endpoint: "10.0.1.2:80", IsLocal: true, Ready: true, Serving: true, Terminating: false},
					&BaseEndpointInfo{Endpoint: "10.0.1.3:80", IsLocal: false, Ready: true, Serving: true, Terminating: false},
				},
				makeServicePortName("ns1", "svc1", "port-1", v1.ProtocolTCP): {
					&BaseEndpointInfo{Endpoint: "10.0.1.1:443", IsLocal: false, Ready: true, Serving: true, Terminating: false},
					&BaseEndpointInfo{Endpoint: "10.0.1.2:443", IsLocal: true, Ready: true, Serving: true, Terminating: false},
					&BaseEndpointInfo{Endpoint: "10.0.1.3:443", IsLocal: false, Ready: true, Serving: true, Terminating: false},
				},
			},
			expectedChangedEndpoints: sets.New[string]("ns1/svc1"),
		},
		// test no modification to state - current change should be nil as nothing changes
		"add the same slice that already exists": {
			startingSlices: []*discovery.EndpointSlice{
				generateEndpointSlice("svc1", "ns1", 1, 3, 999, 999, []string{"host1", "host2"}, []*int32{pointer.Int32(80), pointer.Int32(443)}),
			},
			endpointChangeTracker:    NewEndpointChangeTracker("host1", nil, v1.IPv4Protocol, nil, nil),
			namespacedName:           types.NamespacedName{Name: "svc1", Namespace: "ns1"},
			paramEndpointSlice:       generateEndpointSlice("svc1", "ns1", 1, 3, 999, 999, []string{"host1", "host2"}, []*int32{pointer.Int32(80), pointer.Int32(443)}),
			paramRemoveSlice:         false,
			expectedReturnVal:        false,
			expectedCurrentChange:    nil,
			expectedChangedEndpoints: sets.New[string](),
		},
		// ensure that only valide address types are processed
		"add an FQDN slice (invalid address type)": {
			startingSlices: []*discovery.EndpointSlice{
				generateEndpointSlice("svc1", "ns1", 1, 3, 999, 999, []string{"host1", "host2"}, []*int32{pointer.Int32(80), pointer.Int32(443)}),
			},
			endpointChangeTracker:    NewEndpointChangeTracker("host1", nil, v1.IPv4Protocol, nil, nil),
			namespacedName:           types.NamespacedName{Name: "svc1", Namespace: "ns1"},
			paramEndpointSlice:       fqdnSlice,
			paramRemoveSlice:         false,
			expectedReturnVal:        false,
			expectedCurrentChange:    nil,
			expectedChangedEndpoints: sets.New[string](),
		},
		// test additions to existing state
		"add a slice that overlaps with existing state": {
			startingSlices: []*discovery.EndpointSlice{
				generateEndpointSlice("svc1", "ns1", 1, 3, 999, 999, []string{"host1", "host2"}, []*int32{pointer.Int32(80), pointer.Int32(443)}),
				generateEndpointSlice("svc1", "ns1", 2, 2, 999, 999, []string{"host1", "host2"}, []*int32{pointer.Int32(80), pointer.Int32(443)}),
			},
			endpointChangeTracker: NewEndpointChangeTracker("host1", nil, v1.IPv4Protocol, nil, nil),
			namespacedName:        types.NamespacedName{Name: "svc1", Namespace: "ns1"},
			paramEndpointSlice:    generateEndpointSlice("svc1", "ns1", 1, 5, 999, 999, []string{"host1"}, []*int32{pointer.Int32(80), pointer.Int32(443)}),
			paramRemoveSlice:      false,
			expectedReturnVal:     true,
			expectedCurrentChange: map[ServicePortName][]*BaseEndpointInfo{
				makeServicePortName("ns1", "svc1", "port-0", v1.ProtocolTCP): {
					&BaseEndpointInfo{Endpoint: "10.0.1.1:80", IsLocal: true, Ready: true, Serving: true, Terminating: false},
					&BaseEndpointInfo{Endpoint: "10.0.1.2:80", IsLocal: true, Ready: true, Serving: true, Terminating: false},
					&BaseEndpointInfo{Endpoint: "10.0.1.3:80", IsLocal: true, Ready: true, Serving: true, Terminating: false},
					&BaseEndpointInfo{Endpoint: "10.0.1.4:80", IsLocal: true, Ready: true, Serving: true, Terminating: false},
					&BaseEndpointInfo{Endpoint: "10.0.1.5:80", IsLocal: true, Ready: true, Serving: true, Terminating: false},
					&BaseEndpointInfo{Endpoint: "10.0.2.1:80", IsLocal: false, Ready: true, Serving: true, Terminating: false},
					&BaseEndpointInfo{Endpoint: "10.0.2.2:80", IsLocal: true, Ready: true, Serving: true, Terminating: false},
				},
				makeServicePortName("ns1", "svc1", "port-1", v1.ProtocolTCP): {
					&BaseEndpointInfo{Endpoint: "10.0.1.1:443", IsLocal: true, Ready: true, Serving: true, Terminating: false},
					&BaseEndpointInfo{Endpoint: "10.0.1.2:443", IsLocal: true, Ready: true, Serving: true, Terminating: false},
					&BaseEndpointInfo{Endpoint: "10.0.1.3:443", IsLocal: true, Ready: true, Serving: true, Terminating: false},
					&BaseEndpointInfo{Endpoint: "10.0.1.4:443", IsLocal: true, Ready: true, Serving: true, Terminating: false},
					&BaseEndpointInfo{Endpoint: "10.0.1.5:443", IsLocal: true, Ready: true, Serving: true, Terminating: false},
					&BaseEndpointInfo{Endpoint: "10.0.2.1:443", IsLocal: false, Ready: true, Serving: true, Terminating: false},
					&BaseEndpointInfo{Endpoint: "10.0.2.2:443", IsLocal: true, Ready: true, Serving: true, Terminating: false},
				},
			},
			expectedChangedEndpoints: sets.New[string]("ns1/svc1"),
		},
		// test additions to existing state with partially overlapping slices and ports
		"add a slice that overlaps with existing state and partial ports": {
			startingSlices: []*discovery.EndpointSlice{
				generateEndpointSlice("svc1", "ns1", 1, 3, 999, 999, []string{"host1", "host2"}, []*int32{pointer.Int32(80), pointer.Int32(443)}),
				generateEndpointSlice("svc1", "ns1", 2, 2, 999, 999, []string{"host1", "host2"}, []*int32{pointer.Int32(80), pointer.Int32(443)}),
			},
			endpointChangeTracker: NewEndpointChangeTracker("host1", nil, v1.IPv4Protocol, nil, nil),
			namespacedName:        types.NamespacedName{Name: "svc1", Namespace: "ns1"},
			paramEndpointSlice:    generateEndpointSliceWithOffset("svc1", "ns1", 3, 1, 5, 999, 999, []string{"host1"}, []*int32{pointer.Int32(80)}),
			paramRemoveSlice:      false,
			expectedReturnVal:     true,
			expectedCurrentChange: map[ServicePortName][]*BaseEndpointInfo{
				makeServicePortName("ns1", "svc1", "port-0", v1.ProtocolTCP): {
					&BaseEndpointInfo{Endpoint: "10.0.1.1:80", IsLocal: true, Ready: true, Serving: true, Terminating: false},
					&BaseEndpointInfo{Endpoint: "10.0.1.2:80", IsLocal: true, Ready: true, Serving: true, Terminating: false},
					&BaseEndpointInfo{Endpoint: "10.0.1.3:80", IsLocal: true, Ready: true, Serving: true, Terminating: false},
					&BaseEndpointInfo{Endpoint: "10.0.1.4:80", IsLocal: true, Ready: true, Serving: true, Terminating: false},
					&BaseEndpointInfo{Endpoint: "10.0.1.5:80", IsLocal: true, Ready: true, Serving: true, Terminating: false},
					&BaseEndpointInfo{Endpoint: "10.0.2.1:80", IsLocal: false, Ready: true, Serving: true, Terminating: false},
					&BaseEndpointInfo{Endpoint: "10.0.2.2:80", IsLocal: true, Ready: true, Serving: true, Terminating: false},
				},
				makeServicePortName("ns1", "svc1", "port-1", v1.ProtocolTCP): {
					&BaseEndpointInfo{Endpoint: "10.0.1.1:443", IsLocal: false, Ready: true, Serving: true, Terminating: false},
					&BaseEndpointInfo{Endpoint: "10.0.1.2:443", IsLocal: true, Ready: true, Serving: true, Terminating: false},
					&BaseEndpointInfo{Endpoint: "10.0.1.3:443", IsLocal: false, Ready: true, Serving: true, Terminating: false},
					&BaseEndpointInfo{Endpoint: "10.0.2.1:443", IsLocal: false, Ready: true, Serving: true, Terminating: false},
					&BaseEndpointInfo{Endpoint: "10.0.2.2:443", IsLocal: true, Ready: true, Serving: true, Terminating: false},
				},
			},
			expectedChangedEndpoints: sets.New[string]("ns1/svc1"),
		},
		// test deletions from existing state with partially overlapping slices and ports
		"remove a slice that overlaps with existing state": {
			startingSlices: []*discovery.EndpointSlice{
				generateEndpointSlice("svc1", "ns1", 1, 3, 999, 999, []string{"host1", "host2"}, []*int32{pointer.Int32(80), pointer.Int32(443)}),
				generateEndpointSlice("svc1", "ns1", 2, 2, 999, 999, []string{"host1", "host2"}, []*int32{pointer.Int32(80), pointer.Int32(443)}),
			},
			endpointChangeTracker: NewEndpointChangeTracker("host1", nil, v1.IPv4Protocol, nil, nil),
			namespacedName:        types.NamespacedName{Name: "svc1", Namespace: "ns1"},
			paramEndpointSlice:    generateEndpointSlice("svc1", "ns1", 1, 5, 999, 999, []string{"host1"}, []*int32{pointer.Int32(80), pointer.Int32(443)}),
			paramRemoveSlice:      true,
			expectedReturnVal:     true,
			expectedCurrentChange: map[ServicePortName][]*BaseEndpointInfo{
				makeServicePortName("ns1", "svc1", "port-0", v1.ProtocolTCP): {
					&BaseEndpointInfo{Endpoint: "10.0.2.1:80", IsLocal: false, Ready: true, Serving: true, Terminating: false},
					&BaseEndpointInfo{Endpoint: "10.0.2.2:80", IsLocal: true, Ready: true, Serving: true, Terminating: false},
				},
				makeServicePortName("ns1", "svc1", "port-1", v1.ProtocolTCP): {
					&BaseEndpointInfo{Endpoint: "10.0.2.1:443", IsLocal: false, Ready: true, Serving: true, Terminating: false},
					&BaseEndpointInfo{Endpoint: "10.0.2.2:443", IsLocal: true, Ready: true, Serving: true, Terminating: false},
				},
			},
			expectedChangedEndpoints: sets.New[string]("ns1/svc1"),
		},
		// ensure a removal that has no effect turns into a no-op
		"remove a slice that doesn't even exist in current state": {
			startingSlices: []*discovery.EndpointSlice{
				generateEndpointSlice("svc1", "ns1", 1, 5, 999, 999, []string{"host1"}, []*int32{pointer.Int32(80), pointer.Int32(443)}),
				generateEndpointSlice("svc1", "ns1", 2, 2, 999, 999, []string{"host1"}, []*int32{pointer.Int32(80), pointer.Int32(443)}),
			},
			endpointChangeTracker:    NewEndpointChangeTracker("host1", nil, v1.IPv4Protocol, nil, nil),
			namespacedName:           types.NamespacedName{Name: "svc1", Namespace: "ns1"},
			paramEndpointSlice:       generateEndpointSlice("svc1", "ns1", 3, 5, 999, 999, []string{"host1"}, []*int32{pointer.Int32(80), pointer.Int32(443)}),
			paramRemoveSlice:         true,
			expectedReturnVal:        false,
			expectedCurrentChange:    nil,
			expectedChangedEndpoints: sets.New[string](),
		},
		// start with all endpoints ready, transition to no endpoints ready
		"transition all endpoints to unready state": {
			startingSlices: []*discovery.EndpointSlice{
				generateEndpointSlice("svc1", "ns1", 1, 3, 999, 999, []string{"host1", "host2"}, []*int32{pointer.Int32(80), pointer.Int32(443)}),
			},
			endpointChangeTracker: NewEndpointChangeTracker("host1", nil, v1.IPv4Protocol, nil, nil),
			namespacedName:        types.NamespacedName{Name: "svc1", Namespace: "ns1"},
			paramEndpointSlice:    generateEndpointSlice("svc1", "ns1", 1, 3, 1, 999, []string{"host1"}, []*int32{pointer.Int32(80), pointer.Int32(443)}),
			paramRemoveSlice:      false,
			expectedReturnVal:     true,
			expectedCurrentChange: map[ServicePortName][]*BaseEndpointInfo{
				makeServicePortName("ns1", "svc1", "port-0", v1.ProtocolTCP): {
					&BaseEndpointInfo{Endpoint: "10.0.1.1:80", IsLocal: true, Ready: false, Serving: false, Terminating: false},
					&BaseEndpointInfo{Endpoint: "10.0.1.2:80", IsLocal: true, Ready: false, Serving: false, Terminating: false},
					&BaseEndpointInfo{Endpoint: "10.0.1.3:80", IsLocal: true, Ready: false, Serving: false, Terminating: false},
				},
				makeServicePortName("ns1", "svc1", "port-1", v1.ProtocolTCP): {
					&BaseEndpointInfo{Endpoint: "10.0.1.1:443", IsLocal: true, Ready: false, Serving: false, Terminating: false},
					&BaseEndpointInfo{Endpoint: "10.0.1.2:443", IsLocal: true, Ready: false, Serving: false, Terminating: false},
					&BaseEndpointInfo{Endpoint: "10.0.1.3:443", IsLocal: true, Ready: false, Serving: false, Terminating: false},
				},
			},
			expectedChangedEndpoints: sets.New[string]("ns1/svc1"),
		},
		// start with no endpoints ready, transition to all endpoints ready
		"transition all endpoints to ready state": {
			startingSlices: []*discovery.EndpointSlice{
				generateEndpointSlice("svc1", "ns1", 1, 2, 1, 999, []string{"host1", "host2"}, []*int32{pointer.Int32(80), pointer.Int32(443)}),
			},
			endpointChangeTracker: NewEndpointChangeTracker("host1", nil, v1.IPv4Protocol, nil, nil),
			namespacedName:        types.NamespacedName{Name: "svc1", Namespace: "ns1"},
			paramEndpointSlice:    generateEndpointSlice("svc1", "ns1", 1, 2, 999, 999, []string{"host1"}, []*int32{pointer.Int32(80), pointer.Int32(443)}),
			paramRemoveSlice:      false,
			expectedReturnVal:     true,
			expectedCurrentChange: map[ServicePortName][]*BaseEndpointInfo{
				makeServicePortName("ns1", "svc1", "port-0", v1.ProtocolTCP): {
					&BaseEndpointInfo{Endpoint: "10.0.1.1:80", IsLocal: true, Ready: true, Serving: true, Terminating: false},
					&BaseEndpointInfo{Endpoint: "10.0.1.2:80", IsLocal: true, Ready: true, Serving: true, Terminating: false},
				},
				makeServicePortName("ns1", "svc1", "port-1", v1.ProtocolTCP): {
					&BaseEndpointInfo{Endpoint: "10.0.1.1:443", IsLocal: true, Ready: true, Serving: true, Terminating: false},
					&BaseEndpointInfo{Endpoint: "10.0.1.2:443", IsLocal: true, Ready: true, Serving: true, Terminating: false},
				},
			},
			expectedChangedEndpoints: sets.New[string]("ns1/svc1"),
		},
		// start with some endpoints ready, transition to more endpoints ready
		"transition some endpoints to ready state": {
			startingSlices: []*discovery.EndpointSlice{
				generateEndpointSlice("svc1", "ns1", 1, 3, 2, 999, []string{"host1"}, []*int32{pointer.Int32(80), pointer.Int32(443)}),
				generateEndpointSlice("svc1", "ns1", 2, 2, 2, 999, []string{"host1"}, []*int32{pointer.Int32(80), pointer.Int32(443)}),
			},
			endpointChangeTracker: NewEndpointChangeTracker("host1", nil, v1.IPv4Protocol, nil, nil),
			namespacedName:        types.NamespacedName{Name: "svc1", Namespace: "ns1"},
			paramEndpointSlice:    generateEndpointSlice("svc1", "ns1", 1, 3, 3, 999, []string{"host1"}, []*int32{pointer.Int32(80), pointer.Int32(443)}),
			paramRemoveSlice:      false,
			expectedReturnVal:     true,
			expectedCurrentChange: map[ServicePortName][]*BaseEndpointInfo{
				makeServicePortName("ns1", "svc1", "port-0", v1.ProtocolTCP): {
					&BaseEndpointInfo{Endpoint: "10.0.1.1:80", IsLocal: true, Ready: true, Serving: true, Terminating: false},
					&BaseEndpointInfo{Endpoint: "10.0.1.2:80", IsLocal: true, Ready: true, Serving: true, Terminating: false},
					&BaseEndpointInfo{Endpoint: "10.0.1.3:80", IsLocal: true, Ready: false, Serving: false, Terminating: false},
					&BaseEndpointInfo{Endpoint: "10.0.2.1:80", IsLocal: true, Ready: true, Serving: true, Terminating: false},
					&BaseEndpointInfo{Endpoint: "10.0.2.2:80", IsLocal: true, Ready: false, Serving: false, Terminating: false},
				},
				makeServicePortName("ns1", "svc1", "port-1", v1.ProtocolTCP): {
					&BaseEndpointInfo{Endpoint: "10.0.1.1:443", IsLocal: true, Ready: true, Serving: true, Terminating: false},
					&BaseEndpointInfo{Endpoint: "10.0.1.2:443", IsLocal: true, Ready: true, Serving: true, Terminating: false},
					&BaseEndpointInfo{Endpoint: "10.0.1.3:443", IsLocal: true, Ready: false, Serving: false, Terminating: false},
					&BaseEndpointInfo{Endpoint: "10.0.2.1:443", IsLocal: true, Ready: true, Serving: true, Terminating: false},
					&BaseEndpointInfo{Endpoint: "10.0.2.2:443", IsLocal: true, Ready: false, Serving: false, Terminating: false},
				},
			},
			expectedChangedEndpoints: sets.New[string]("ns1/svc1"),
		},
		// start with some endpoints ready, transition to some terminating
		"transition some endpoints to terminating state": {
			startingSlices: []*discovery.EndpointSlice{
				generateEndpointSlice("svc1", "ns1", 1, 3, 2, 2, []string{"host1"}, []*int32{pointer.Int32(80), pointer.Int32(443)}),
				generateEndpointSlice("svc1", "ns1", 2, 2, 2, 2, []string{"host1"}, []*int32{pointer.Int32(80), pointer.Int32(443)}),
			},
			endpointChangeTracker: NewEndpointChangeTracker("host1", nil, v1.IPv4Protocol, nil, nil),
			namespacedName:        types.NamespacedName{Name: "svc1", Namespace: "ns1"},
			paramEndpointSlice:    generateEndpointSlice("svc1", "ns1", 1, 3, 3, 2, []string{"host1"}, []*int32{pointer.Int32(80), pointer.Int32(443)}),
			paramRemoveSlice:      false,
			expectedReturnVal:     true,
			expectedCurrentChange: map[ServicePortName][]*BaseEndpointInfo{
				makeServicePortName("ns1", "svc1", "port-0", v1.ProtocolTCP): {
					&BaseEndpointInfo{Endpoint: "10.0.1.1:80", IsLocal: true, Ready: true, Serving: true, Terminating: false},
					&BaseEndpointInfo{Endpoint: "10.0.1.2:80", IsLocal: true, Ready: false, Serving: true, Terminating: true},
					&BaseEndpointInfo{Endpoint: "10.0.1.3:80", IsLocal: true, Ready: false, Serving: false, Terminating: false},
					&BaseEndpointInfo{Endpoint: "10.0.2.1:80", IsLocal: true, Ready: true, Serving: true, Terminating: false},
					&BaseEndpointInfo{Endpoint: "10.0.2.2:80", IsLocal: true, Ready: false, Serving: false, Terminating: true},
				},
				makeServicePortName("ns1", "svc1", "port-1", v1.ProtocolTCP): {
					&BaseEndpointInfo{Endpoint: "10.0.1.1:443", IsLocal: true, Ready: true, Serving: true, Terminating: false},
					&BaseEndpointInfo{Endpoint: "10.0.1.2:443", IsLocal: true, Ready: false, Serving: true, Terminating: true},
					&BaseEndpointInfo{Endpoint: "10.0.1.3:443", IsLocal: true, Ready: false, Serving: false, Terminating: false},
					&BaseEndpointInfo{Endpoint: "10.0.2.1:443", IsLocal: true, Ready: true, Serving: true, Terminating: false},
					&BaseEndpointInfo{Endpoint: "10.0.2.2:443", IsLocal: true, Ready: false, Serving: false, Terminating: true},
				},
			},
			expectedChangedEndpoints: sets.New[string]("ns1/svc1"),
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			initializeCache(tc.endpointChangeTracker.endpointSliceCache, tc.startingSlices)

			got := tc.endpointChangeTracker.EndpointSliceUpdate(tc.paramEndpointSlice, tc.paramRemoveSlice)
			if !reflect.DeepEqual(got, tc.expectedReturnVal) {
				t.Errorf("EndpointSliceUpdate return value got: %v, want %v", got, tc.expectedReturnVal)
			}

			pendingChanges := tc.endpointChangeTracker.PendingChanges()
			if !pendingChanges.Equal(tc.expectedChangedEndpoints) {
				t.Errorf("expected changed endpoints %q, got %q", tc.expectedChangedEndpoints.UnsortedList(), pendingChanges.UnsortedList())
			}

			changes := tc.endpointChangeTracker.checkoutChanges()
			if tc.expectedCurrentChange == nil {
				if len(changes) != 0 {
					t.Errorf("Expected %s to have no changes", tc.namespacedName)
				}
			} else {
				if len(changes) == 0 || changes[0] == nil {
					t.Fatalf("Expected %s to have changes", tc.namespacedName)
				}
				compareEndpointsMapsStr(t, changes[0].current, tc.expectedCurrentChange)
			}
		})
	}
}

func TestCheckoutChanges(t *testing.T) {
	svcPortName0 := ServicePortName{types.NamespacedName{Namespace: "ns1", Name: "svc1"}, "port-0", v1.ProtocolTCP}
	svcPortName1 := ServicePortName{types.NamespacedName{Namespace: "ns1", Name: "svc1"}, "port-1", v1.ProtocolTCP}

	testCases := map[string]struct {
		endpointChangeTracker *EndpointChangeTracker
		expectedChanges       []*endpointsChange
		items                 map[types.NamespacedName]*endpointsChange
		appliedSlices         []*discovery.EndpointSlice
		pendingSlices         []*discovery.EndpointSlice
	}{
		"empty slices": {
			endpointChangeTracker: NewEndpointChangeTracker("", nil, v1.IPv4Protocol, nil, nil),
			expectedChanges:       []*endpointsChange{},
			appliedSlices:         []*discovery.EndpointSlice{},
			pendingSlices:         []*discovery.EndpointSlice{},
		},
		"adding initial slice": {
			endpointChangeTracker: NewEndpointChangeTracker("", nil, v1.IPv4Protocol, nil, nil),
			expectedChanges: []*endpointsChange{{
				previous: EndpointsMap{},
				current: EndpointsMap{
					svcPortName0: []Endpoint{newTestEp("10.0.1.1:80", "host1", true, true, false), newTestEp("10.0.1.2:80", "host1", false, true, true), newTestEp("10.0.1.3:80", "host1", false, false, false)},
				},
			}},
			appliedSlices: []*discovery.EndpointSlice{},
			pendingSlices: []*discovery.EndpointSlice{
				generateEndpointSlice("svc1", "ns1", 1, 3, 3, 2, []string{"host1"}, []*int32{pointer.Int32(80)}),
			},
		},
		"removing port in update": {
			endpointChangeTracker: NewEndpointChangeTracker("", nil, v1.IPv4Protocol, nil, nil),
			expectedChanges: []*endpointsChange{{
				previous: EndpointsMap{
					svcPortName0: []Endpoint{newTestEp("10.0.1.1:80", "host1", true, true, false), newTestEp("10.0.1.2:80", "host1", true, true, false), newTestEp("10.0.1.3:80", "host1", false, false, false)},
					svcPortName1: []Endpoint{newTestEp("10.0.1.1:443", "host1", true, true, false), newTestEp("10.0.1.2:443", "host1", true, true, false), newTestEp("10.0.1.3:443", "host1", false, false, false)},
				},
				current: EndpointsMap{
					svcPortName0: []Endpoint{newTestEp("10.0.1.1:80", "host1", true, true, false), newTestEp("10.0.1.2:80", "host1", true, true, false), newTestEp("10.0.1.3:80", "host1", false, false, false)},
				},
			}},
			appliedSlices: []*discovery.EndpointSlice{
				generateEndpointSlice("svc1", "ns1", 1, 3, 3, 999, []string{"host1"}, []*int32{pointer.Int32(80), pointer.Int32(443)}),
			},
			pendingSlices: []*discovery.EndpointSlice{
				generateEndpointSlice("svc1", "ns1", 1, 3, 3, 999, []string{"host1"}, []*int32{pointer.Int32(80)}),
			},
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			for _, slice := range tc.appliedSlices {
				tc.endpointChangeTracker.EndpointSliceUpdate(slice, false)
			}
			tc.endpointChangeTracker.checkoutChanges()
			for _, slice := range tc.pendingSlices {
				tc.endpointChangeTracker.EndpointSliceUpdate(slice, false)
			}
			changes := tc.endpointChangeTracker.checkoutChanges()

			if len(tc.expectedChanges) != len(changes) {
				t.Fatalf("Expected %d changes, got %d", len(tc.expectedChanges), len(changes))
			}

			for i, change := range changes {
				expectedChange := tc.expectedChanges[i]

				if !reflect.DeepEqual(change.previous, expectedChange.previous) {
					t.Errorf("[%d] Expected change.previous: %+v, got: %+v", i, expectedChange.previous, change.previous)
				}

				if !reflect.DeepEqual(change.current, expectedChange.current) {
					t.Errorf("[%d] Expected change.current: %+v, got: %+v", i, expectedChange.current, change.current)
				}
			}
		})
	}
}

// Test helpers

func compareEndpointsMapsStr(t *testing.T, newMap EndpointsMap, expected map[ServicePortName][]*BaseEndpointInfo) {
	t.Helper()
	if len(newMap) != len(expected) {
		t.Fatalf("expected %d results, got %d: %v", len(expected), len(newMap), newMap)
	}
	endpointEqual := func(a, b *BaseEndpointInfo) bool {
		return a.Endpoint == b.Endpoint && a.IsLocal == b.IsLocal && a.Ready == b.Ready && a.Serving == b.Serving && a.Terminating == b.Terminating
	}
	for x := range expected {
		if len(newMap[x]) != len(expected[x]) {
			t.Logf("Endpoints %+v", newMap[x])
			t.Fatalf("expected %d endpoints for %v, got %d", len(expected[x]), x, len(newMap[x]))
		} else {
			for i := range expected[x] {
				newEp, ok := newMap[x][i].(*BaseEndpointInfo)
				if !ok {
					t.Fatalf("Failed to cast endpointsInfo")
				}
				if !endpointEqual(newEp, expected[x][i]) {
					t.Fatalf("expected new[%v][%d] to be %v, got %v"+
						"(IsLocal expected %v, got %v) (Ready expected %v, got %v) (Serving expected %v, got %v) (Terminating expected %v got %v)",
						x, i, expected[x][i], newEp, expected[x][i].IsLocal, newEp.IsLocal, expected[x][i].Ready, newEp.Ready,
						expected[x][i].Serving, newEp.Serving, expected[x][i].Terminating, newEp.Terminating)
				}
			}
		}
	}
}

func newTestEp(ep, host string, ready, serving, terminating bool) *BaseEndpointInfo {
	endpointInfo := &BaseEndpointInfo{Endpoint: ep, Ready: ready, Serving: serving, Terminating: terminating}
	if host != "" {
		endpointInfo.NodeName = host
	}
	return endpointInfo
}

func initializeCache(endpointSliceCache *EndpointSliceCache, endpointSlices []*discovery.EndpointSlice) {
	for _, endpointSlice := range endpointSlices {
		endpointSliceCache.updatePending(endpointSlice, false)
	}

	for _, tracker := range endpointSliceCache.trackerByServiceMap {
		tracker.applied = tracker.pending
		tracker.pending = endpointSliceInfoByName{}
	}
}
