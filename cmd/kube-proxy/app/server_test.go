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

package app

import (
	"errors"
	"fmt"
	"io/ioutil"
	"path"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientsetfake "k8s.io/client-go/kubernetes/fake"
	componentbaseconfig "k8s.io/component-base/config"
	logsapi "k8s.io/component-base/logs/api/v1"
	kubeproxyconfig "k8s.io/kubernetes/pkg/proxy/apis/config"
	"k8s.io/utils/pointer"
)

// TestLoadConfig tests proper operation of loadConfig()
func TestLoadConfig(t *testing.T) {

	yamlTemplate := `apiVersion: kubeproxy.config.k8s.io/v1alpha1
bindAddress: %s
clientConnection:
  acceptContentTypes: "abc"
  burst: 100
  contentType: content-type
  kubeconfig: "/path/to/kubeconfig"
  qps: 7
clusterCIDR: "%s"
configSyncPeriod: 15s
conntrack:
  maxPerCore: 2
  min: 1
  tcpCloseWaitTimeout: 10s
  tcpEstablishedTimeout: 20s
healthzBindAddress: "%s"
hostnameOverride: "foo"
iptables:
  masqueradeAll: true
  masqueradeBit: 17
  minSyncPeriod: 10s
  syncPeriod: 60s
  localhostNodePorts: true
ipvs:
  minSyncPeriod: 10s
  syncPeriod: 60s
  excludeCIDRs:
    - "10.20.30.40/16"
    - "fd00:1::0/64"
kind: KubeProxyConfiguration
metricsBindAddress: "%s"
mode: "%s"
oomScoreAdj: 17
portRange: "2-7"
detectLocalMode: "ClusterCIDR"
detectLocal:
  bridgeInterface: "cbr0"
  interfaceNamePrefix: "veth"
nodePortAddresses:
  - "10.20.30.40/16"
  - "fd00:1::0/64"
`

	testCases := []struct {
		name               string
		mode               string
		bindAddress        string
		clusterCIDR        string
		healthzBindAddress string
		metricsBindAddress string
		extraConfig        string
	}{
		{
			name:               "iptables mode, IPv4 all-zeros bind address",
			mode:               "iptables",
			bindAddress:        "0.0.0.0",
			clusterCIDR:        "1.2.3.0/24",
			healthzBindAddress: "1.2.3.4:12345",
			metricsBindAddress: "2.3.4.5:23456",
		},
		{
			name:               "iptables mode, non-zeros IPv4 config",
			mode:               "iptables",
			bindAddress:        "9.8.7.6",
			clusterCIDR:        "1.2.3.0/24",
			healthzBindAddress: "1.2.3.4:12345",
			metricsBindAddress: "2.3.4.5:23456",
		},
		{
			// Test for 'bindAddress: "::"' (IPv6 all-zeros) in kube-proxy
			// config file. The user will need to put quotes around '::' since
			// 'bindAddress: ::' is invalid yaml syntax.
			name:               "iptables mode, IPv6 \"::\" bind address",
			mode:               "iptables",
			bindAddress:        "\"::\"",
			clusterCIDR:        "fd00:1::0/64",
			healthzBindAddress: "[fd00:1::5]:12345",
			metricsBindAddress: "[fd00:2::5]:23456",
		},
		{
			// Test for 'bindAddress: "[::]"' (IPv6 all-zeros in brackets)
			// in kube-proxy config file. The user will need to use
			// surrounding quotes here since 'bindAddress: [::]' is invalid
			// yaml syntax.
			name:               "iptables mode, IPv6 \"[::]\" bind address",
			mode:               "iptables",
			bindAddress:        "\"[::]\"",
			clusterCIDR:        "fd00:1::0/64",
			healthzBindAddress: "[fd00:1::5]:12345",
			metricsBindAddress: "[fd00:2::5]:23456",
		},
		{
			// Test for 'bindAddress: ::0' (another form of IPv6 all-zeros).
			// No surrounding quotes are required around '::0'.
			name:               "iptables mode, IPv6 ::0 bind address",
			mode:               "iptables",
			bindAddress:        "::0",
			clusterCIDR:        "fd00:1::0/64",
			healthzBindAddress: "[fd00:1::5]:12345",
			metricsBindAddress: "[fd00:2::5]:23456",
		},
		{
			name:               "ipvs mode, IPv6 config",
			mode:               "ipvs",
			bindAddress:        "2001:db8::1",
			clusterCIDR:        "fd00:1::0/64",
			healthzBindAddress: "[fd00:1::5]:12345",
			metricsBindAddress: "[fd00:2::5]:23456",
		},
		{
			// Test for unknown field within config.
			// For v1alpha1 a lenient path is implemented and will throw a
			// strict decoding warning instead of failing to load
			name:               "unknown field",
			mode:               "iptables",
			bindAddress:        "9.8.7.6",
			clusterCIDR:        "1.2.3.0/24",
			healthzBindAddress: "1.2.3.4:12345",
			metricsBindAddress: "2.3.4.5:23456",
			extraConfig:        "foo: bar",
		},
		{
			// Test for duplicate field within config.
			// For v1alpha1 a lenient path is implemented and will throw a
			// strict decoding warning instead of failing to load
			name:               "duplicate field",
			mode:               "iptables",
			bindAddress:        "9.8.7.6",
			clusterCIDR:        "1.2.3.0/24",
			healthzBindAddress: "1.2.3.4:12345",
			metricsBindAddress: "2.3.4.5:23456",
			extraConfig:        "bindAddress: 9.8.7.6",
		},
	}

	for _, tc := range testCases {
		expBindAddr := tc.bindAddress
		if tc.bindAddress[0] == '"' {
			// Surrounding double quotes will get stripped by the yaml parser.
			expBindAddr = expBindAddr[1 : len(tc.bindAddress)-1]
		}
		expected := &kubeproxyconfig.KubeProxyConfiguration{
			BindAddress: expBindAddr,
			ClientConnection: componentbaseconfig.ClientConnectionConfiguration{
				AcceptContentTypes: "abc",
				Burst:              100,
				ContentType:        "content-type",
				Kubeconfig:         "/path/to/kubeconfig",
				QPS:                7,
			},
			ClusterCIDR:      tc.clusterCIDR,
			ConfigSyncPeriod: metav1.Duration{Duration: 15 * time.Second},
			Conntrack: kubeproxyconfig.KubeProxyConntrackConfiguration{
				MaxPerCore:            pointer.Int32(2),
				Min:                   pointer.Int32(1),
				TCPCloseWaitTimeout:   &metav1.Duration{Duration: 10 * time.Second},
				TCPEstablishedTimeout: &metav1.Duration{Duration: 20 * time.Second},
			},
			FeatureGates:       map[string]bool{},
			HealthzBindAddress: tc.healthzBindAddress,
			HostnameOverride:   "foo",
			IPTables: kubeproxyconfig.KubeProxyIPTablesConfiguration{
				MasqueradeAll:      true,
				MasqueradeBit:      pointer.Int32(17),
				LocalhostNodePorts: pointer.Bool(true),
				MinSyncPeriod:      metav1.Duration{Duration: 10 * time.Second},
				SyncPeriod:         metav1.Duration{Duration: 60 * time.Second},
			},
			IPVS: kubeproxyconfig.KubeProxyIPVSConfiguration{
				MinSyncPeriod: metav1.Duration{Duration: 10 * time.Second},
				SyncPeriod:    metav1.Duration{Duration: 60 * time.Second},
				ExcludeCIDRs:  []string{"10.20.30.40/16", "fd00:1::0/64"},
			},
			MetricsBindAddress: tc.metricsBindAddress,
			Mode:               kubeproxyconfig.ProxyMode(tc.mode),
			OOMScoreAdj:        pointer.Int32(17),
			PortRange:          "2-7",
			NodePortAddresses:  []string{"10.20.30.40/16", "fd00:1::0/64"},
			DetectLocalMode:    kubeproxyconfig.LocalModeClusterCIDR,
			DetectLocal: kubeproxyconfig.DetectLocalConfiguration{
				BridgeInterface:     string("cbr0"),
				InterfaceNamePrefix: string("veth"),
			},
			Logging: logsapi.LoggingConfiguration{
				Format:         "text",
				FlushFrequency: logsapi.TimeOrMetaDuration{Duration: metav1.Duration{Duration: 5 * time.Second}, SerializeAsString: true},
			},
		}

		options := NewOptions()

		baseYAML := fmt.Sprintf(
			yamlTemplate, tc.bindAddress, tc.clusterCIDR,
			tc.healthzBindAddress, tc.metricsBindAddress, tc.mode)

		// Append additional configuration to the base yaml template
		yaml := fmt.Sprintf("%s\n%s", baseYAML, tc.extraConfig)

		config, err := options.loadConfig([]byte(yaml))

		assert.NoError(t, err, "unexpected error for %s: %v", tc.name, err)

		if diff := cmp.Diff(config, expected); diff != "" {
			t.Fatalf("unexpected config for %s, diff = %s", tc.name, diff)
		}
	}
}

// TestLoadConfigFailures tests failure modes for loadConfig()
func TestLoadConfigFailures(t *testing.T) {
	// TODO(phenixblue): Uncomment below template when v1alpha2+ of kube-proxy config is
	// released with strict decoding. These associated tests will fail with
	// the lenient codec and only one config API version.
	/*
			yamlTemplate := `bindAddress: 0.0.0.0
		clusterCIDR: "1.2.3.0/24"
		configSyncPeriod: 15s
		kind: KubeProxyConfiguration`
	*/

	testCases := []struct {
		name    string
		config  string
		expErr  string
		checkFn func(err error) bool
	}{
		{
			name:   "Decode error test",
			config: "Twas bryllyg, and ye slythy toves",
			expErr: "could not find expected ':'",
		},
		{
			name:   "Bad config type test",
			config: "kind: KubeSchedulerConfiguration",
			expErr: "no kind",
		},
		{
			name:   "Missing quotes around :: bindAddress",
			config: "bindAddress: ::",
			expErr: "mapping values are not allowed in this context",
		},
		// TODO(phenixblue): Uncomment below tests when v1alpha2+ of kube-proxy config is
		// released with strict decoding. These tests will fail with the
		// lenient codec and only one config API version.
		/*
			{
				name:    "Duplicate fields",
				config:  fmt.Sprintf("%s\nbindAddress: 1.2.3.4", yamlTemplate),
				checkFn: kuberuntime.IsStrictDecodingError,
			},
			{
				name:    "Unknown field",
				config:  fmt.Sprintf("%s\nfoo: bar", yamlTemplate),
				checkFn: kuberuntime.IsStrictDecodingError,
			},
		*/
	}

	version := "apiVersion: kubeproxy.config.k8s.io/v1alpha1"
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			options := NewOptions()
			config := fmt.Sprintf("%s\n%s", version, tc.config)
			_, err := options.loadConfig([]byte(config))

			if assert.Error(t, err, tc.name) {
				if tc.expErr != "" {
					assert.Contains(t, err.Error(), tc.expErr)
				}
				if tc.checkFn != nil {
					assert.True(t, tc.checkFn(err), tc.name)
				}
			}
		})
	}
}

// TestProcessHostnameOverrideFlag tests processing hostname-override arg
func TestProcessHostnameOverrideFlag(t *testing.T) {
	testCases := []struct {
		name                 string
		hostnameOverrideFlag string
		expectedHostname     string
		expectError          bool
	}{
		{
			name:                 "Hostname from config file",
			hostnameOverrideFlag: "",
			expectedHostname:     "foo",
			expectError:          false,
		},
		{
			name:                 "Hostname from flag",
			hostnameOverrideFlag: "  bar ",
			expectedHostname:     "bar",
			expectError:          false,
		},
		{
			name:                 "Hostname is space",
			hostnameOverrideFlag: "   ",
			expectError:          true,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			options := NewOptions()
			options.config = &kubeproxyconfig.KubeProxyConfiguration{
				HostnameOverride: "foo",
			}

			options.hostnameOverride = tc.hostnameOverrideFlag

			err := options.processHostnameOverrideFlag()
			if tc.expectError {
				if err == nil {
					t.Fatalf("should error for this case %s", tc.name)
				}
			} else {
				assert.NoError(t, err, "unexpected error %v", err)
				if tc.expectedHostname != options.config.HostnameOverride {
					t.Fatalf("expected hostname: %s, but got: %s", tc.expectedHostname, options.config.HostnameOverride)
				}
			}
		})
	}
}

// TestOptionsComplete checks that command line flags are combined with a
// config properly.
func TestOptionsComplete(t *testing.T) {
	header := `apiVersion: kubeproxy.config.k8s.io/v1alpha1
kind: KubeProxyConfiguration
`

	// Determine default config (depends on platform defaults).
	o := NewOptions()
	require.NoError(t, o.Complete(new(pflag.FlagSet)))
	expected := o.config

	config := header + `logging:
  format: json
  flushFrequency: 1s
  verbosity: 10
  vmodule:
  - filePattern: foo.go
    verbosity: 6
  - filePattern: bar.go
    verbosity: 8
`
	expectedLoggingConfig := logsapi.LoggingConfiguration{
		Format:         "json",
		FlushFrequency: logsapi.TimeOrMetaDuration{Duration: metav1.Duration{Duration: time.Second}, SerializeAsString: true},
		Verbosity:      10,
		VModule: []logsapi.VModuleItem{
			{
				FilePattern: "foo.go",
				Verbosity:   6,
			},
			{
				FilePattern: "bar.go",
				Verbosity:   8,
			},
		},
		Options: logsapi.FormatOptions{
			JSON: logsapi.JSONOptions{
				InfoBufferSize: resource.QuantityValue{Quantity: resource.MustParse("0")},
			},
		},
	}

	for name, tc := range map[string]struct {
		config   string
		flags    []string
		expected *kubeproxyconfig.KubeProxyConfiguration
	}{
		"empty": {
			expected: expected,
		},
		"empty-config": {
			config:   header,
			expected: expected,
		},
		"logging-config": {
			config: config,
			expected: func() *kubeproxyconfig.KubeProxyConfiguration {
				c := expected.DeepCopy()
				c.Logging = *expectedLoggingConfig.DeepCopy()
				return c
			}(),
		},
		"flags": {
			flags: []string{
				"-v=7",
				"--vmodule", "goo.go=8",
			},
			expected: func() *kubeproxyconfig.KubeProxyConfiguration {
				c := expected.DeepCopy()
				c.Logging.Verbosity = 7
				c.Logging.VModule = append(c.Logging.VModule, logsapi.VModuleItem{
					FilePattern: "goo.go",
					Verbosity:   8,
				})
				return c
			}(),
		},
		"both": {
			config: config,
			flags: []string{
				"-v=7",
				"--vmodule", "goo.go=8",
				"--ipvs-scheduler", "some-scheduler", // Overwritten by config.
			},
			expected: func() *kubeproxyconfig.KubeProxyConfiguration {
				c := expected.DeepCopy()
				c.Logging = *expectedLoggingConfig.DeepCopy()
				// Flag wins.
				c.Logging.Verbosity = 7
				// Flag and config get merged with command line flags first.
				c.Logging.VModule = append([]logsapi.VModuleItem{
					{
						FilePattern: "goo.go",
						Verbosity:   8,
					},
				}, c.Logging.VModule...)
				return c
			}(),
		},
	} {
		t.Run(name, func(t *testing.T) {
			options := NewOptions()
			fs := new(pflag.FlagSet)
			options.AddFlags(fs)
			flags := tc.flags
			if len(tc.config) > 0 {
				tmp := t.TempDir()
				configFile := path.Join(tmp, "kube-proxy.conf")
				require.NoError(t, ioutil.WriteFile(configFile, []byte(tc.config), 0666))
				flags = append(flags, "--config", configFile)
			}
			require.NoError(t, fs.Parse(flags))
			require.NoError(t, options.Complete(fs))
			assert.Equal(t, tc.expected, options.config)
		})
	}
}

type fakeProxyServerLongRun struct{}

// Run runs the specified ProxyServer.
func (s *fakeProxyServerLongRun) Run() error {
	for {
		time.Sleep(2 * time.Second)
	}
}

// CleanupAndExit runs in the specified ProxyServer.
func (s *fakeProxyServerLongRun) CleanupAndExit() error {
	return nil
}

type fakeProxyServerError struct{}

// Run runs the specified ProxyServer.
func (s *fakeProxyServerError) Run() error {
	for {
		time.Sleep(2 * time.Second)
		return fmt.Errorf("mocking error from ProxyServer.Run()")
	}
}

// CleanupAndExit runs in the specified ProxyServer.
func (s *fakeProxyServerError) CleanupAndExit() error {
	return errors.New("mocking error from ProxyServer.CleanupAndExit()")
}

func TestAddressFromDeprecatedFlags(t *testing.T) {
	testCases := []struct {
		name               string
		healthzPort        int32
		healthzBindAddress string
		metricsPort        int32
		metricsBindAddress string
		expHealthz         string
		expMetrics         string
	}{
		{
			name:               "IPv4 bind address",
			healthzBindAddress: "1.2.3.4",
			healthzPort:        12345,
			metricsBindAddress: "2.3.4.5",
			metricsPort:        23456,
			expHealthz:         "1.2.3.4:12345",
			expMetrics:         "2.3.4.5:23456",
		},
		{
			name:               "IPv4 bind address has port",
			healthzBindAddress: "1.2.3.4:12345",
			healthzPort:        23456,
			metricsBindAddress: "2.3.4.5:12345",
			metricsPort:        23456,
			expHealthz:         "1.2.3.4:12345",
			expMetrics:         "2.3.4.5:12345",
		},
		{
			name:               "IPv6 bind address",
			healthzBindAddress: "fd00:1::5",
			healthzPort:        12345,
			metricsBindAddress: "fd00:1::6",
			metricsPort:        23456,
			expHealthz:         "[fd00:1::5]:12345",
			expMetrics:         "[fd00:1::6]:23456",
		},
		{
			name:               "IPv6 bind address has port",
			healthzBindAddress: "[fd00:1::5]:12345",
			healthzPort:        56789,
			metricsBindAddress: "[fd00:1::6]:56789",
			metricsPort:        12345,
			expHealthz:         "[fd00:1::5]:12345",
			expMetrics:         "[fd00:1::6]:56789",
		},
		{
			name:               "Invalid IPv6 Config",
			healthzBindAddress: "[fd00:1::5]",
			healthzPort:        12345,
			metricsBindAddress: "[fd00:1::6]",
			metricsPort:        56789,
			expHealthz:         "[fd00:1::5]",
			expMetrics:         "[fd00:1::6]",
		},
	}

	for i := range testCases {
		gotHealthz := addressFromDeprecatedFlags(testCases[i].healthzBindAddress, testCases[i].healthzPort)
		gotMetrics := addressFromDeprecatedFlags(testCases[i].metricsBindAddress, testCases[i].metricsPort)

		errFn := func(name, except, got string) {
			t.Errorf("case %s: expected %v, got %v", name, except, got)
		}

		if gotHealthz != testCases[i].expHealthz {
			errFn(testCases[i].name, testCases[i].expHealthz, gotHealthz)
		}

		if gotMetrics != testCases[i].expMetrics {
			errFn(testCases[i].name, testCases[i].expMetrics, gotMetrics)
		}

	}
}

func makeNodeWithAddresses(name, internal, external string) *v1.Node {
	if name == "" {
		return &v1.Node{}
	}

	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Status: v1.NodeStatus{
			Addresses: []v1.NodeAddress{},
		},
	}

	if internal != "" {
		node.Status.Addresses = append(node.Status.Addresses,
			v1.NodeAddress{Type: v1.NodeInternalIP, Address: internal},
		)
	}

	if external != "" {
		node.Status.Addresses = append(node.Status.Addresses,
			v1.NodeAddress{Type: v1.NodeExternalIP, Address: external},
		)
	}

	return node
}

func Test_detectNodeIPs(t *testing.T) {
	cases := []struct {
		name           string
		nodeInfo       *v1.Node
		hostname       string
		bindAddress    string
		expectedFamily v1.IPFamily
		expectedIPv4   string
		expectedIPv6   string
	}{
		{
			name:           "Bind address IPv4 unicast address and no Node object",
			nodeInfo:       makeNodeWithAddresses("", "", ""),
			hostname:       "fakeHost",
			bindAddress:    "10.0.0.1",
			expectedFamily: v1.IPv4Protocol,
			expectedIPv4:   "10.0.0.1",
			expectedIPv6:   "::",
		},
		{
			name:           "Bind address IPv6 unicast address and no Node object",
			nodeInfo:       makeNodeWithAddresses("", "", ""),
			hostname:       "fakeHost",
			bindAddress:    "fd00:4321::2",
			expectedFamily: v1.IPv6Protocol,
			expectedIPv4:   "0.0.0.0",
			expectedIPv6:   "fd00:4321::2",
		},
		{
			name:           "No Valid IP found",
			nodeInfo:       makeNodeWithAddresses("", "", ""),
			hostname:       "fakeHost",
			bindAddress:    "",
			expectedFamily: v1.IPv4Protocol,
			expectedIPv4:   "127.0.0.1",
			expectedIPv6:   "::",
		},
		// Disabled because the GetNodeIP method has a backoff retry mechanism
		// and the test takes more than 30 seconds
		// ok  	k8s.io/kubernetes/cmd/kube-proxy/app	34.136s
		// {
		//	name:           "No Valid IP found and unspecified bind address",
		//	nodeInfo:       makeNodeWithAddresses("", "", ""),
		//	hostname:       "fakeHost",
		//	bindAddress:    "0.0.0.0",
		//	expectedFamily: v1.IPv4Protocol,
		//	expectedIPv4:   "127.0.0.1",
		//	expectedIPv6:   "::",
		// },
		{
			name:           "Bind address 0.0.0.0 and node with IPv4 InternalIP set",
			nodeInfo:       makeNodeWithAddresses("fakeHost", "192.168.1.1", "90.90.90.90"),
			hostname:       "fakeHost",
			bindAddress:    "0.0.0.0",
			expectedFamily: v1.IPv4Protocol,
			expectedIPv4:   "192.168.1.1",
			expectedIPv6:   "::",
		},
		{
			name:           "Bind address :: and node with IPv4 InternalIP set",
			nodeInfo:       makeNodeWithAddresses("fakeHost", "192.168.1.1", "90.90.90.90"),
			hostname:       "fakeHost",
			bindAddress:    "::",
			expectedFamily: v1.IPv4Protocol,
			expectedIPv4:   "192.168.1.1",
			expectedIPv6:   "::",
		},
		{
			name:           "Bind address 0.0.0.0 and node with IPv6 InternalIP set",
			nodeInfo:       makeNodeWithAddresses("fakeHost", "fd00:1234::1", "2001:db8::2"),
			hostname:       "fakeHost",
			bindAddress:    "0.0.0.0",
			expectedFamily: v1.IPv6Protocol,
			expectedIPv4:   "0.0.0.0",
			expectedIPv6:   "fd00:1234::1",
		},
		{
			name:           "Bind address :: and node with IPv6 InternalIP set",
			nodeInfo:       makeNodeWithAddresses("fakeHost", "fd00:1234::1", "2001:db8::2"),
			hostname:       "fakeHost",
			bindAddress:    "::",
			expectedFamily: v1.IPv6Protocol,
			expectedIPv4:   "0.0.0.0",
			expectedIPv6:   "fd00:1234::1",
		},
		{
			name:           "Bind address 0.0.0.0 and node with only IPv4 ExternalIP set",
			nodeInfo:       makeNodeWithAddresses("fakeHost", "", "90.90.90.90"),
			hostname:       "fakeHost",
			bindAddress:    "0.0.0.0",
			expectedFamily: v1.IPv4Protocol,
			expectedIPv4:   "90.90.90.90",
			expectedIPv6:   "::",
		},
		{
			name:           "Bind address :: and node with only IPv4 ExternalIP set",
			nodeInfo:       makeNodeWithAddresses("fakeHost", "", "90.90.90.90"),
			hostname:       "fakeHost",
			bindAddress:    "::",
			expectedFamily: v1.IPv4Protocol,
			expectedIPv4:   "90.90.90.90",
			expectedIPv6:   "::",
		},
		{
			name:           "Bind address 0.0.0.0 and node with only IPv6 ExternalIP set",
			nodeInfo:       makeNodeWithAddresses("fakeHost", "", "2001:db8::2"),
			hostname:       "fakeHost",
			bindAddress:    "0.0.0.0",
			expectedFamily: v1.IPv6Protocol,
			expectedIPv4:   "0.0.0.0",
			expectedIPv6:   "2001:db8::2",
		},
		{
			name:           "Bind address :: and node with only IPv6 ExternalIP set",
			nodeInfo:       makeNodeWithAddresses("fakeHost", "", "2001:db8::2"),
			hostname:       "fakeHost",
			bindAddress:    "::",
			expectedFamily: v1.IPv6Protocol,
			expectedIPv4:   "0.0.0.0",
			expectedIPv6:   "2001:db8::2",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			client := clientsetfake.NewSimpleClientset(c.nodeInfo)
			primaryFamily, ips := detectNodeIPs(client, c.hostname, c.bindAddress)
			if primaryFamily != c.expectedFamily {
				t.Errorf("Expected family %q got %q", c.expectedFamily, primaryFamily)
			}
			if ips[v1.IPv4Protocol].String() != c.expectedIPv4 {
				t.Errorf("Expected IPv4 %q got %q", c.expectedIPv4, ips[v1.IPv4Protocol].String())
			}
			if ips[v1.IPv6Protocol].String() != c.expectedIPv6 {
				t.Errorf("Expected IPv6 %q got %q", c.expectedIPv6, ips[v1.IPv6Protocol].String())
			}
		})
	}
}

func Test_checkIPConfig(t *testing.T) {
	cases := []struct {
		name  string
		proxy *ProxyServer
		ssErr bool
		dsErr bool
		fatal bool
	}{
		{
			name: "empty config",
			proxy: &ProxyServer{
				Config:          &kubeproxyconfig.KubeProxyConfiguration{},
				PrimaryIPFamily: v1.IPv4Protocol,
			},
			ssErr: false,
			dsErr: false,
		},

		{
			name: "ok single-stack clusterCIDR",
			proxy: &ProxyServer{
				Config: &kubeproxyconfig.KubeProxyConfiguration{
					ClusterCIDR: "10.0.0.0/8",
				},
				PrimaryIPFamily: v1.IPv4Protocol,
			},
			ssErr: false,
			dsErr: false,
		},
		{
			name: "ok dual-stack clusterCIDR",
			proxy: &ProxyServer{
				Config: &kubeproxyconfig.KubeProxyConfiguration{
					ClusterCIDR: "10.0.0.0/8,fd01:2345::/64",
				},
				PrimaryIPFamily: v1.IPv4Protocol,
			},
			ssErr: false,
			dsErr: false,
		},
		{
			name: "ok reversed dual-stack clusterCIDR",
			proxy: &ProxyServer{
				Config: &kubeproxyconfig.KubeProxyConfiguration{
					ClusterCIDR: "fd01:2345::/64,10.0.0.0/8",
				},
				PrimaryIPFamily: v1.IPv4Protocol,
			},
			ssErr: false,
			dsErr: false,
		},
		{
			name: "wrong-family clusterCIDR",
			proxy: &ProxyServer{
				Config: &kubeproxyconfig.KubeProxyConfiguration{
					ClusterCIDR: "fd01:2345::/64",
				},
				PrimaryIPFamily: v1.IPv4Protocol,
			},
			ssErr: true,
			dsErr: true,
			fatal: false,
		},
		{
			name: "wrong-family clusterCIDR when using ClusterCIDR LocalDetector",
			proxy: &ProxyServer{
				Config: &kubeproxyconfig.KubeProxyConfiguration{
					ClusterCIDR:     "fd01:2345::/64",
					DetectLocalMode: kubeproxyconfig.LocalModeClusterCIDR,
				},
				PrimaryIPFamily: v1.IPv4Protocol,
			},
			ssErr: true,
			dsErr: true,
			fatal: true,
		},

		{
			name: "ok single-stack nodePortAddresses",
			proxy: &ProxyServer{
				Config: &kubeproxyconfig.KubeProxyConfiguration{
					NodePortAddresses: []string{"10.0.0.0/8", "192.168.0.0/24"},
				},
				PrimaryIPFamily: v1.IPv4Protocol,
			},
			ssErr: false,
			dsErr: false,
		},
		{
			name: "ok dual-stack nodePortAddresses",
			proxy: &ProxyServer{
				Config: &kubeproxyconfig.KubeProxyConfiguration{
					NodePortAddresses: []string{"10.0.0.0/8", "fd01:2345::/64", "fd01:abcd::/64"},
				},
				PrimaryIPFamily: v1.IPv4Protocol,
			},
			ssErr: false,
			dsErr: false,
		},
		{
			name: "ok reversed dual-stack nodePortAddresses",
			proxy: &ProxyServer{
				Config: &kubeproxyconfig.KubeProxyConfiguration{
					NodePortAddresses: []string{"fd01:2345::/64", "fd01:abcd::/64", "10.0.0.0/8"},
				},
				PrimaryIPFamily: v1.IPv4Protocol,
			},
			ssErr: false,
			dsErr: false,
		},
		{
			name: "wrong-family nodePortAddresses",
			proxy: &ProxyServer{
				Config: &kubeproxyconfig.KubeProxyConfiguration{
					NodePortAddresses: []string{"10.0.0.0/8"},
				},
				PrimaryIPFamily: v1.IPv6Protocol,
			},
			ssErr: true,
			dsErr: true,
			fatal: false,
		},

		{
			name: "ok single-stack node.spec.podCIDRs",
			proxy: &ProxyServer{
				Config: &kubeproxyconfig.KubeProxyConfiguration{
					DetectLocalMode: kubeproxyconfig.LocalModeNodeCIDR,
				},
				PrimaryIPFamily: v1.IPv4Protocol,
				podCIDRs:        []string{"10.0.0.0/8"},
			},
			ssErr: false,
			dsErr: false,
		},
		{
			name: "ok dual-stack node.spec.podCIDRs",
			proxy: &ProxyServer{
				Config: &kubeproxyconfig.KubeProxyConfiguration{
					DetectLocalMode: kubeproxyconfig.LocalModeNodeCIDR,
				},
				PrimaryIPFamily: v1.IPv4Protocol,
				podCIDRs:        []string{"10.0.0.0/8", "fd01:2345::/64"},
			},
			ssErr: false,
			dsErr: false,
		},
		{
			name: "ok reversed dual-stack node.spec.podCIDRs",
			proxy: &ProxyServer{
				Config: &kubeproxyconfig.KubeProxyConfiguration{
					DetectLocalMode: kubeproxyconfig.LocalModeNodeCIDR,
				},
				PrimaryIPFamily: v1.IPv4Protocol,
				podCIDRs:        []string{"fd01:2345::/64", "10.0.0.0/8"},
			},
			ssErr: false,
			dsErr: false,
		},
		{
			name: "wrong-family node.spec.podCIDRs",
			proxy: &ProxyServer{
				Config: &kubeproxyconfig.KubeProxyConfiguration{
					DetectLocalMode: kubeproxyconfig.LocalModeNodeCIDR,
				},
				PrimaryIPFamily: v1.IPv4Protocol,
				podCIDRs:        []string{"fd01:2345::/64"},
			},
			ssErr: true,
			dsErr: true,
			fatal: true,
		},

		{
			name: "ok winkernel.sourceVip",
			proxy: &ProxyServer{
				Config: &kubeproxyconfig.KubeProxyConfiguration{
					Winkernel: kubeproxyconfig.KubeProxyWinkernelConfiguration{
						SourceVip: "10.0.0.1",
					},
				},
				PrimaryIPFamily: v1.IPv4Protocol,
			},
			ssErr: false,
			dsErr: false,
		},
		{
			name: "wrong family winkernel.sourceVip",
			proxy: &ProxyServer{
				Config: &kubeproxyconfig.KubeProxyConfiguration{
					Winkernel: kubeproxyconfig.KubeProxyWinkernelConfiguration{
						SourceVip: "fd01:2345::1",
					},
				},
				PrimaryIPFamily: v1.IPv4Protocol,
			},
			ssErr: true,
			dsErr: true,
			fatal: false,
		},

		{
			name: "ok IPv4 metricsBindAddress",
			proxy: &ProxyServer{
				Config: &kubeproxyconfig.KubeProxyConfiguration{
					MetricsBindAddress: "10.0.0.1:9999",
				},
				PrimaryIPFamily: v1.IPv4Protocol,
			},
			ssErr: false,
			dsErr: false,
		},
		{
			name: "ok IPv6 metricsBindAddress",
			proxy: &ProxyServer{
				Config: &kubeproxyconfig.KubeProxyConfiguration{
					MetricsBindAddress: "[fd01:2345::1]:9999",
				},
				PrimaryIPFamily: v1.IPv6Protocol,
			},
			ssErr: false,
			dsErr: false,
		},
		{
			name: "ok unspecified wrong-family metricsBindAddress",
			proxy: &ProxyServer{
				Config: &kubeproxyconfig.KubeProxyConfiguration{
					MetricsBindAddress: "0.0.0.0:9999",
				},
				PrimaryIPFamily: v1.IPv6Protocol,
			},
			ssErr: false,
			dsErr: false,
		},
		{
			name: "wrong family metricsBindAddress",
			proxy: &ProxyServer{
				Config: &kubeproxyconfig.KubeProxyConfiguration{
					MetricsBindAddress: "10.0.0.1:9999",
				},
				PrimaryIPFamily: v1.IPv6Protocol,
			},
			ssErr: true,
			dsErr: false,
			fatal: false,
		},

		{
			name: "ok ipvs.excludeCIDRs",
			proxy: &ProxyServer{
				Config: &kubeproxyconfig.KubeProxyConfiguration{
					IPVS: kubeproxyconfig.KubeProxyIPVSConfiguration{
						ExcludeCIDRs: []string{"10.0.0.0/8"},
					},
				},
				PrimaryIPFamily: v1.IPv4Protocol,
			},
			ssErr: false,
			dsErr: false,
		},
		{
			name: "wrong family ipvs.excludeCIDRs",
			proxy: &ProxyServer{
				Config: &kubeproxyconfig.KubeProxyConfiguration{
					IPVS: kubeproxyconfig.KubeProxyIPVSConfiguration{
						ExcludeCIDRs: []string{"10.0.0.0/8", "192.168.0.0/24"},
					},
				},
				PrimaryIPFamily: v1.IPv6Protocol,
			},
			ssErr: true,
			dsErr: false,
			fatal: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err, fatal := checkIPConfig(c.proxy, false)
			if err != nil && !c.ssErr {
				t.Errorf("unexpected error in single-stack case: %v", err)
			} else if err == nil && c.ssErr {
				t.Errorf("unexpected lack of error in single-stack case")
			} else if fatal != c.fatal {
				t.Errorf("expected fatal=%v, got %v", c.fatal, fatal)
			}

			err, fatal = checkIPConfig(c.proxy, true)
			if err != nil && !c.dsErr {
				t.Errorf("unexpected error in dual-stack case: %v", err)
			} else if err == nil && c.dsErr {
				t.Errorf("unexpected lack of error in dual-stack case")
			} else if fatal != c.fatal {
				t.Errorf("expected fatal=%v, got %v", c.fatal, fatal)
			}
		})
	}
}
