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

package config

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NodeLifecycleControllerConfiguration contains elements describing NodeLifecycleController.
type NodeLifecycleControllerConfiguration struct {
	// nodeEvictionRate is the number of nodes per second on which pods are deleted in case of node failure when a zone is healthy
	// 通过--node-eviction-rate设置， 默认 0.1，表示当集群下某个 zone 为 unhealthy 时，每秒应该剔除的 node 数量，默认即每 10s 剔除1个 node；
	NodeEvictionRate float32
	// secondaryNodeEvictionRate is the number of nodes per second on which pods are deleted in case of node failure when a zone is unhealthy
	// 通过 --secondary-node-eviction-rate设置，默认为 0.01，表示如果某个 zone 下的 unhealthy 节点的百分比超过 --unhealthy-zone-threshold （默认为 0.55）时，
	// 驱逐速率将会减小，如果集群较小（小于等于 --large-cluster-size-threshold 个 节点 - 默认为 50），驱逐操作将会停止，
	// 否则驱逐速率将降为每秒 --secondary-node-eviction-rate 个（默认为 0.01）；
	SecondaryNodeEvictionRate float32
	// nodeStartupGracePeriod is the amount of time which we allow starting a node to
	// be unresponsive before marking it unhealthy.
	NodeStartupGracePeriod metav1.Duration
	// NodeMonitorGracePeriod is the amount of time which we allow a running node to be
	// unresponsive before marking it unhealthy. Must be N times more than kubelet's
	// nodeStatusUpdateFrequency, where N means number of retries allowed for kubelet
	// to post node status.
	NodeMonitorGracePeriod metav1.Duration
	// secondaryNodeEvictionRate is implicitly overridden to 0 for clusters smaller than or equal to largeClusterSizeThreshold
	// 通过--large-cluster-size-threshold 设置，默认为 50，当该 zone 的节点超过该阈值时，则认为该 zone 是一个大集群；
	LargeClusterSizeThreshold int32
	// Zone is treated as unhealthy in nodeEvictionRate and secondaryNodeEvictionRate when at least
	// unhealthyZoneThreshold (no less than 3) of Nodes in the zone are NotReady
	// 通过--unhealthy-zone-threshold 设置，默认为 0.55，不健康 zone 阈值，
	// 会影响什么时候开启二级驱赶速率，即当该 zone 中节点宕机数目超过 55%，认为该 zone 不健康；
	UnhealthyZoneThreshold float32
}
