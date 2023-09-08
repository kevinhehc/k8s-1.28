/*
Copyright 2018 The Kubernetes Authors.

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

package fuzzer

import (
	"fmt"
	"time"

	"github.com/google/gofuzz"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtimeserializer "k8s.io/apimachinery/pkg/runtime/serializer"
	kubeproxyconfig "k8s.io/kubernetes/pkg/proxy/apis/config"
	"k8s.io/utils/pointer"
)

// Funcs returns the fuzzer functions for the kube-proxy apis.
func Funcs(codecs runtimeserializer.CodecFactory) []interface{} {
	return []interface{}{
		func(obj *kubeproxyconfig.KubeProxyConfiguration, c fuzz.Continue) {
			c.FuzzNoCustom(obj)
			obj.BindAddress = fmt.Sprintf("%d.%d.%d.%d", c.Intn(256), c.Intn(256), c.Intn(256), c.Intn(256))
			obj.ClientConnection.ContentType = c.RandString()
			obj.Conntrack.MaxPerCore = pointer.Int32(c.Int31())
			obj.Conntrack.Min = pointer.Int32(c.Int31())
			obj.Conntrack.TCPCloseWaitTimeout = &metav1.Duration{Duration: time.Duration(c.Int63()) * time.Hour}
			obj.Conntrack.TCPEstablishedTimeout = &metav1.Duration{Duration: time.Duration(c.Int63()) * time.Hour}
			obj.FeatureGates = map[string]bool{c.RandString(): true}
			obj.HealthzBindAddress = fmt.Sprintf("%d.%d.%d.%d:%d", c.Intn(256), c.Intn(256), c.Intn(256), c.Intn(256), c.Intn(65536))
			obj.IPTables.MasqueradeBit = pointer.Int32(c.Int31())
			obj.IPTables.LocalhostNodePorts = pointer.Bool(c.RandBool())
			obj.MetricsBindAddress = fmt.Sprintf("%d.%d.%d.%d:%d", c.Intn(256), c.Intn(256), c.Intn(256), c.Intn(256), c.Intn(65536))
			obj.OOMScoreAdj = pointer.Int32(c.Int31())
			obj.ClientConnection.ContentType = "bar"
			obj.NodePortAddresses = []string{"1.2.3.0/24"}
			if obj.Logging.Format == "" {
				obj.Logging.Format = "text"
			}
		},
	}
}
