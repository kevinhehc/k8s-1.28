/*
Copyright 2020 The Kubernetes Authors.

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

package main

import (
	"bytes"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/template"
	"time"

	flag "github.com/spf13/pflag"
	"gopkg.in/yaml.v2"

	"k8s.io/component-base/metrics"
)

var (
	GOROOT    string = os.Getenv("GOROOT")
	GOOS      string = os.Getenv("GOOS")
	KUBE_ROOT string = os.Getenv("KUBE_ROOT")
	funcMap          = template.FuncMap{
		"ToLower": strings.ToLower,
	}
)

const (
	templ = `---
title: Kubernetes Metrics Reference
content_type: reference
auto_generated: true
description: >-
  Details of the metric data that Kubernetes components export.
---

## Metrics (v{{.GeneratedVersion}})

<!-- (auto-generated {{.GeneratedDate.Format "2006 Jan 02"}}) -->
<!-- (auto-generated v{{.GeneratedVersion}}) -->
This page details the metrics that different Kubernetes components export. You can query the metrics endpoint for these 
components using an HTTP scrape, and fetch the current metrics data in Prometheus format.

### List of Stable Kubernetes Metrics

Stable metrics observe strict API contracts and no labels can be added or removed from stable metrics during their lifetime.

<table class="table metrics" caption="This is the list of STABLE metrics emitted from core Kubernetes components">
<thead>
	<tr>
		<th class="metric_name">Name</th>
		<th class="metric_stability_level">Stability Level</th>
		<th class="metric_type">Type</th>
		<th class="metric_help">Help</th>
		<th class="metric_labels">Labels</th>
		<th class="metric_const_labels">Const Labels</th>
		<th class="metric_deprecated_version">Deprecated Version</th>
	</tr>
</thead>
<tbody>
{{range $index, $metric := .StableMetrics}}
<tr class="metric"><td class="metric_name">{{with $metric}}{{.BuildFQName}}{{end}}</td>
<td class="metric_stability_level" data-stability="{{$metric.StabilityLevel | ToLower}}">{{$metric.StabilityLevel}}</td>
<td class="metric_type" data-type="{{$metric.Type | ToLower}}">{{$metric.Type}}</td>
<td class="metric_description">{{$metric.Help}}</td>
{{if not $metric.Labels }}<td class="metric_labels_varying"></td>{{else }}<td class="metric_labels_varying">{{range $label := $metric.Labels}}<div class="metric_label">{{$label}}</div>{{end}}</td>{{end}}
{{if not $metric.ConstLabels }}<td class="metric_labels_constant"></td>{{else }}<td class="metric_labels_constant">{{range $key, $value := $metric.ConstLabels}}<div class="metric_label">{{$key}}:{{$value}}</div>{{end}}</td>{{end}}
{{if not $metric.DeprecatedVersion }}<td class="metric_deprecated_version"></td>{{else }}<td class="metric_deprecated_version">{{$metric.DeprecatedVersion}}</td>{{end}}</tr>{{end}}
</tbody>
</table>

### List of Beta Kubernetes Metrics

Beta metrics observe a looser API contract than its stable counterparts. No labels can be removed from beta metrics during their lifetime, however, labels can be added while the metric is in the beta stage. This offers the assurance that beta metrics will honor existing dashboards and alerts, while allowing for amendments in the future. 

<table class="table metrics" caption="This is the list of BETA metrics emitted from core Kubernetes components">
<thead>
	<tr>
		<th class="metric_name">Name</th>
		<th class="metric_stability_level">Stability Level</th>
		<th class="metric_type">Type</th>
		<th class="metric_help">Help</th>
		<th class="metric_labels">Labels</th>
		<th class="metric_const_labels">Const Labels</th>
		<th class="metric_deprecated_version">Deprecated Version</th>
	</tr>
</thead>
<tbody>
{{range $index, $metric := .BetaMetrics}}
<tr class="metric"><td class="metric_name">{{with $metric}}{{.BuildFQName}}{{end}}</td>
<td class="metric_stability_level" data-stability="{{$metric.StabilityLevel | ToLower}}">{{$metric.StabilityLevel}}</td>
<td class="metric_type" data-type="{{$metric.Type | ToLower}}">{{$metric.Type}}</td>
<td class="metric_description">{{$metric.Help}}</td>
{{if not $metric.Labels }}<td class="metric_labels_varying"></td>{{else }}<td class="metric_labels_varying">{{range $label := $metric.Labels}}<div class="metric_label">{{$label}}</div>{{end}}</td>{{end}}
{{if not $metric.ConstLabels }}<td class="metric_labels_constant"></td>{{else }}<td class="metric_labels_constant">{{range $key, $value := $metric.ConstLabels}}<div class="metric_label">{{$key}}:{{$value}}</div>{{end}}</td>{{end}}
{{if not $metric.DeprecatedVersion }}<td class="metric_deprecated_version"></td>{{else }}<td class="metric_deprecated_version">{{$metric.DeprecatedVersion}}</td>{{end}}</tr>{{end}}
</tbody>
</table>

### List of Alpha Kubernetes Metrics

Alpha metrics do not have any API guarantees. These metrics must be used at your own risk, subsequent versions of Kubernetes may remove these metrics altogether, or mutate the API in such a way that breaks existing dashboards and alerts. 

<table class="table metrics" caption="This is the list of ALPHA metrics emitted from core Kubernetes components">
<thead>
	<tr>
		<th class="metric_name">Name</th>
		<th class="metric_stability_level">Stability Level</th>
		<th class="metric_type">Type</th>
		<th class="metric_help">Help</th>
		<th class="metric_labels">Labels</th>
		<th class="metric_const_labels">Const Labels</th>
		<th class="metric_deprecated_version">Deprecated Version</th>
	</tr>
</thead>
<tbody>
{{range $index, $metric := .AlphaMetrics}}
<tr class="metric"><td class="metric_name">{{with $metric}}{{.BuildFQName}}{{end}}</td>
<td class="metric_stability_level" data-stability="{{$metric.StabilityLevel | ToLower}}">{{$metric.StabilityLevel}}</td>
<td class="metric_type" data-type="{{$metric.Type | ToLower}}">{{$metric.Type}}</td>
<td class="metric_description">{{$metric.Help}}</td>
{{if not $metric.Labels }}<td class="metric_labels_varying"></td>{{else }}<td class="metric_labels_varying">{{range $label := $metric.Labels}}<div class="metric_label">{{$label}}</div>{{end}}</td>{{end}}
{{if not $metric.ConstLabels }}<td class="metric_labels_constant"></td>{{else }}<td class="metric_labels_constant">{{range $key, $value := $metric.ConstLabels}}<div class="metric_label">{{$key}}:{{$value}}</div>{{end}}</td>{{end}}
{{if not $metric.DeprecatedVersion }}<td class="metric_deprecated_version"></td>{{else }}<td class="metric_deprecated_version">{{$metric.DeprecatedVersion}}</td>{{end}}</tr>{{end}}
</tbody>
</table>
`
)

type templateData struct {
	AlphaMetrics     []metric
	BetaMetrics      []metric
	StableMetrics    []metric
	GeneratedDate    time.Time
	GeneratedVersion string
}

func main() {
	var major string
	var minor string
	flag.StringVar(&major, "major", "", "k8s major version")
	flag.StringVar(&minor, "minor", "", "k8s minor version")
	flag.Parse()
	println(major, minor)
	dat, err := os.ReadFile("test/instrumentation/documentation/documentation-list.yaml")
	if err == nil {
		var parsedMetrics []metric
		err = yaml.Unmarshal(dat, &parsedMetrics)
		if err != nil {
			println("err", err)
		}
		sort.Sort(byFQName(parsedMetrics))
		t := template.New("t").Funcs(funcMap)
		t, err := t.Parse(templ)
		if err != nil {
			println("err", err)
		}
		var tpl bytes.Buffer
		for i, m := range parsedMetrics {
			m.Help = strings.Join(strings.Split(m.Help, "\n"), ", ")
			_ = m.BuildFQName() // ignore golint error
			parsedMetrics[i] = m
		}
		sortedMetrics := byStabilityLevel(parsedMetrics)
		data := templateData{
			AlphaMetrics:     sortedMetrics["ALPHA"],
			BetaMetrics:      sortedMetrics["BETA"],
			StableMetrics:    sortedMetrics["STABLE"],
			GeneratedDate:    time.Now(),
			GeneratedVersion: fmt.Sprintf("%v.%v", major, parseMinor(minor)),
		}
		err = t.Execute(&tpl, data)
		if err != nil {
			println("err", err)
		}
		fmt.Print(tpl.String())
	} else {
		fmt.Fprintf(os.Stderr, "%s\n", err)
	}

}

type metric struct {
	Name              string              `yaml:"name" json:"name"`
	Subsystem         string              `yaml:"subsystem,omitempty" json:"subsystem,omitempty"`
	Namespace         string              `yaml:"namespace,omitempty" json:"namespace,omitempty"`
	Help              string              `yaml:"help,omitempty" json:"help,omitempty"`
	Type              string              `yaml:"type,omitempty" json:"type,omitempty"`
	DeprecatedVersion string              `yaml:"deprecatedVersion,omitempty" json:"deprecatedVersion,omitempty"`
	StabilityLevel    string              `yaml:"stabilityLevel,omitempty" json:"stabilityLevel,omitempty"`
	Labels            []string            `yaml:"labels,omitempty" json:"labels,omitempty"`
	Buckets           []float64           `yaml:"buckets,omitempty" json:"buckets,omitempty"`
	Objectives        map[float64]float64 `yaml:"objectives,omitempty" json:"objectives,omitempty"`
	AgeBuckets        uint32              `yaml:"ageBuckets,omitempty" json:"ageBuckets,omitempty"`
	BufCap            uint32              `yaml:"bufCap,omitempty" json:"bufCap,omitempty"`
	MaxAge            int64               `yaml:"maxAge,omitempty" json:"maxAge,omitempty"`
	ConstLabels       map[string]string   `yaml:"constLabels,omitempty" json:"constLabels,omitempty"`
}

func (m metric) BuildFQName() string {
	return metrics.BuildFQName(m.Namespace, m.Subsystem, m.Name)
}

type byFQName []metric

func (ms byFQName) Len() int { return len(ms) }
func (ms byFQName) Less(i, j int) bool {
	if ms[i].StabilityLevel < ms[j].StabilityLevel {
		return true
	} else if ms[i].StabilityLevel > ms[j].StabilityLevel {
		return false
	}
	return ms[i].BuildFQName() < ms[j].BuildFQName()
}
func (ms byFQName) Swap(i, j int) {
	ms[i], ms[j] = ms[j], ms[i]
}

func byStabilityLevel(ms []metric) map[string][]metric {
	res := map[string][]metric{}
	for _, m := range ms {
		if _, ok := res[m.StabilityLevel]; !ok {
			res[m.StabilityLevel] = []metric{}
		}
		res[m.StabilityLevel] = append(res[m.StabilityLevel], m)
	}
	return res
}

func parseMinor(m string) string {
	return strings.Trim(m, `+`)
}
