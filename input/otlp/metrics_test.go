// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

// Portions copied from OpenTelemetry Collector (contrib), from the
// elastic exporter.
//
// Copyright 2020, OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package otlp_test

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"golang.org/x/sync/semaphore"
	"google.golang.org/protobuf/testing/protocmp"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/elastic/apm-data/input/otlp"
	"github.com/elastic/apm-data/model/modelpb"
)

func TestConsumeMetrics(t *testing.T) {
	metrics := pmetric.NewMetrics()
	resourceMetrics := metrics.ResourceMetrics().AppendEmpty()
	scopeMetrics := resourceMetrics.ScopeMetrics().AppendEmpty()
	metricSlice := scopeMetrics.Metrics()
	appendMetric := func(name string) pmetric.Metric {
		metric := metricSlice.AppendEmpty()
		metric.SetName(name)
		return metric
	}

	timestamp0 := time.Unix(123, 0).UTC()
	timestamp1 := time.Unix(456, 0).UTC()

	var expectDropped int64

	gauge := appendMetric("gauge_metric").SetEmptyGauge()
	gaugeDP0 := gauge.DataPoints().AppendEmpty()
	gaugeDP0.SetTimestamp(pcommon.NewTimestampFromTime(timestamp0))
	gaugeDP0.SetIntValue(1)
	gaugeDP1 := gauge.DataPoints().AppendEmpty()
	gaugeDP1.SetTimestamp(pcommon.NewTimestampFromTime(timestamp1))
	gaugeDP1.SetDoubleValue(2.3)
	gaugeDP1.Attributes().PutStr("k", "v")
	gaugeDP2 := gauge.DataPoints().AppendEmpty()
	gaugeDP2.SetTimestamp(pcommon.NewTimestampFromTime(timestamp1))
	gaugeDP2.SetIntValue(4)
	gaugeDP3 := gauge.DataPoints().AppendEmpty()
	gaugeDP3.SetTimestamp(pcommon.NewTimestampFromTime(timestamp1))
	gaugeDP3.SetDoubleValue(5.6)
	gaugeDP3.Attributes().PutStr("k", "v2")

	sum := appendMetric("sum_metric").SetEmptySum()
	sumDP0 := sum.DataPoints().AppendEmpty()
	sumDP0.SetTimestamp(pcommon.NewTimestampFromTime(timestamp0))
	sumDP0.SetIntValue(7)
	sumDP1 := sum.DataPoints().AppendEmpty()
	sumDP1.SetTimestamp(pcommon.NewTimestampFromTime(timestamp1))
	sumDP1.SetDoubleValue(8.9)
	sumDP1.Attributes().PutStr("k", "v")
	sumDP2 := sum.DataPoints().AppendEmpty()
	sumDP2.SetTimestamp(pcommon.NewTimestampFromTime(timestamp1))
	sumDP2.SetIntValue(10)
	sumDP2.Attributes().PutStr("k2", "v")
	sumDP3 := sum.DataPoints().AppendEmpty()
	sumDP3.SetTimestamp(pcommon.NewTimestampFromTime(timestamp1))
	sumDP3.SetDoubleValue(11.12)
	sumDP3.Attributes().PutStr("k", "v2")

	histogram := appendMetric("histogram_metric").SetEmptyHistogram()
	histogramDP := histogram.DataPoints().AppendEmpty()
	histogramDP.SetTimestamp(pcommon.NewTimestampFromTime(timestamp0))
	histogramDP.BucketCounts().Append(1, 1, 2, 3)
	histogramDP.ExplicitBounds().Append(-1.0, 2.0, 3.5)

	summary := appendMetric("summary_metric").SetEmptySummary()
	summaryDP := summary.DataPoints().AppendEmpty()
	summaryDP.SetTimestamp(pcommon.NewTimestampFromTime(timestamp0))
	summaryDP.SetCount(10)
	summaryDP.SetSum(123.456)
	summaryDP.QuantileValues().AppendEmpty() // quantiles are not stored

	invalidHistogram := appendMetric("invalid_histogram_metric").SetEmptyHistogram()
	invalidHistogramDP := invalidHistogram.DataPoints().AppendEmpty()
	invalidHistogramDP.SetTimestamp(pcommon.NewTimestampFromTime(timestamp0))
	// should be one more bucket count than bounds
	invalidHistogramDP.BucketCounts().Append(1, 2, 3)
	invalidHistogramDP.ExplicitBounds().Append(1, 2, 3)
	expectDropped++

	invalidHistogram = appendMetric("invalid_histogram_metric2").SetEmptyHistogram()
	invalidHistogramDP = invalidHistogram.DataPoints().AppendEmpty()
	invalidHistogramDP.SetTimestamp(pcommon.NewTimestampFromTime(timestamp0))
	invalidHistogramDP.BucketCounts().Append(1)
	invalidHistogramDP.ExplicitBounds().Append( /* should be non-empty */ )
	expectDropped++

	events, stats := transformMetrics(t, metrics)
	assert.Equal(t, expectDropped, stats.UnsupportedMetricsDropped)

	service := modelpb.Service{Name: "unknown", Language: &modelpb.Language{Name: "unknown"}}
	agent := modelpb.Agent{Name: "otlp", Version: "unknown"}
	expected := []*modelpb.APMEvent{{
		Agent:     &agent,
		Service:   &service,
		Timestamp: timestamppb.New(timestamp0),
		Processor: modelpb.MetricsetProcessor(),
		Metricset: &modelpb.Metricset{
			Name: "app",
			Samples: []*modelpb.MetricsetSample{
				{Name: "gauge_metric", Value: 1, Type: modelpb.MetricType_METRIC_TYPE_GAUGE},
				{Name: "sum_metric", Value: 7, Type: modelpb.MetricType_METRIC_TYPE_COUNTER},
				{
					Name: "histogram_metric",
					Type: modelpb.MetricType_METRIC_TYPE_HISTOGRAM,
					Histogram: &modelpb.Histogram{
						Counts: []int64{1, 1, 2, 3},
						Values: []float64{-1, 0.5, 2.75, 3.5},
					},
				},
				{
					Name: "summary_metric",
					Type: modelpb.MetricType_METRIC_TYPE_SUMMARY,
					Summary: &modelpb.SummaryMetric{
						Count: 10,
						Sum:   123.456,
					},
				},
			},
		},
	}, {
		Agent:     &agent,
		Service:   &service,
		Timestamp: timestamppb.New(timestamp1),
		Processor: modelpb.MetricsetProcessor(),
		Metricset: &modelpb.Metricset{
			Name: "app",
			Samples: []*modelpb.MetricsetSample{
				{Name: "gauge_metric", Value: 4, Type: modelpb.MetricType_METRIC_TYPE_GAUGE},
			},
		},
	}, {
		Agent:     &agent,
		Service:   &service,
		Labels:    modelpb.Labels{"k": {Value: "v"}},
		Timestamp: timestamppb.New(timestamp1),
		Processor: modelpb.MetricsetProcessor(),
		Metricset: &modelpb.Metricset{
			Name: "app",
			Samples: []*modelpb.MetricsetSample{
				{Name: "gauge_metric", Value: 2.3, Type: modelpb.MetricType_METRIC_TYPE_GAUGE},
				{Name: "sum_metric", Value: 8.9, Type: modelpb.MetricType_METRIC_TYPE_COUNTER},
			},
		},
	}, {
		Agent:     &agent,
		Service:   &service,
		Labels:    modelpb.Labels{"k": {Value: "v2"}},
		Timestamp: timestamppb.New(timestamp1),
		Processor: modelpb.MetricsetProcessor(),
		Metricset: &modelpb.Metricset{
			Name: "app",
			Samples: []*modelpb.MetricsetSample{
				{Name: "gauge_metric", Value: 5.6, Type: modelpb.MetricType_METRIC_TYPE_GAUGE},
				{Name: "sum_metric", Value: 11.12, Type: modelpb.MetricType_METRIC_TYPE_COUNTER},
			},
		},
	}, {
		Agent:     &agent,
		Service:   &service,
		Labels:    modelpb.Labels{"k2": {Value: "v"}},
		Timestamp: timestamppb.New(timestamp1),
		Processor: modelpb.MetricsetProcessor(),
		Metricset: &modelpb.Metricset{
			Name: "app",
			Samples: []*modelpb.MetricsetSample{
				{Name: "sum_metric", Value: 10, Type: modelpb.MetricType_METRIC_TYPE_COUNTER},
			},
		},
	}}

	eventsMatch(t, expected, events)
}

func TestConsumeMetricsSemaphore(t *testing.T) {
	metrics := pmetric.NewMetrics()
	var batches []*modelpb.Batch

	doneCh := make(chan struct{})
	recorder := modelpb.ProcessBatchFunc(func(ctx context.Context, batch *modelpb.Batch) error {
		<-doneCh
		batches = append(batches, batch)
		return nil
	})
	consumer := otlp.NewConsumer(otlp.ConsumerConfig{
		Processor: recorder,
		Semaphore: semaphore.NewWeighted(1),
	})

	startCh := make(chan struct{})
	go func() {
		close(startCh)
		assert.NoError(t, consumer.ConsumeMetrics(context.Background(), metrics))
	}()

	<-startCh
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	assert.Equal(t, consumer.ConsumeMetrics(ctx, metrics).Error(), "context deadline exceeded")
	close(doneCh)

	assert.NoError(t, consumer.ConsumeMetrics(context.Background(), metrics))
}

func TestConsumeMetricsNaN(t *testing.T) {
	timestamp := time.Unix(123, 0).UTC()
	metrics := pmetric.NewMetrics()
	resourceMetrics := metrics.ResourceMetrics().AppendEmpty()
	scopeMetrics := resourceMetrics.ScopeMetrics().AppendEmpty()
	metricSlice := scopeMetrics.Metrics()

	for _, value := range []float64{math.NaN(), math.Inf(-1), math.Inf(1)} {
		metric := metricSlice.AppendEmpty()
		metric.SetName("gauge")
		gauge := metric.SetEmptyGauge()
		dp := gauge.DataPoints().AppendEmpty()
		dp.SetTimestamp(pcommon.NewTimestampFromTime(timestamp))
		dp.SetDoubleValue(value)
	}

	events, stats := transformMetrics(t, metrics)
	assert.Equal(t, int64(3), stats.UnsupportedMetricsDropped)
	assert.Empty(t, events)
}

func TestConsumeMetricsHostCPU(t *testing.T) {
	metrics := pmetric.NewMetrics()
	resourceMetrics := metrics.ResourceMetrics().AppendEmpty()
	scopeMetrics := resourceMetrics.ScopeMetrics().AppendEmpty()
	metricSlice := scopeMetrics.Metrics()

	timestamp := time.Unix(123, 0).UTC()
	addFloat64Gauge := func(name string, value float64, attributes map[string]interface{}) {
		metric := metricSlice.AppendEmpty()
		metric.SetName(name)
		gauge := metric.SetEmptyGauge()
		dp := gauge.DataPoints().AppendEmpty()
		dp.SetTimestamp(pcommon.NewTimestampFromTime(timestamp))
		dp.SetDoubleValue(value)
		dp.Attributes().FromRaw(attributes)
	}

	addFloat64Gauge("system.cpu.utilization", 0.8, map[string]interface{}{
		"state": "idle",
		"cpu":   "0",
	})
	addFloat64Gauge("system.cpu.utilization", 0.1, map[string]interface{}{
		"state": "system",
		"cpu":   "0",
	})
	addFloat64Gauge("system.cpu.utilization", 0.1, map[string]interface{}{
		"state": "user",
		"cpu":   "0",
	})

	addFloat64Gauge("system.cpu.utilization", 0.45, map[string]interface{}{
		"state": "idle",
		"cpu":   "1",
	})
	addFloat64Gauge("system.cpu.utilization", 0.05, map[string]interface{}{
		"state": "system",
		"cpu":   "1",
	})
	addFloat64Gauge("system.cpu.utilization", 0.5, map[string]interface{}{
		"state": "user",
		"cpu":   "1",
	})

	addFloat64Gauge("system.cpu.utilization", 0.59, map[string]interface{}{
		"state": "idle",
		"cpu":   "2",
	})
	addFloat64Gauge("system.cpu.utilization", 0.01, map[string]interface{}{
		"state": "system",
		"cpu":   "2",
	})
	addFloat64Gauge("system.cpu.utilization", 0.4, map[string]interface{}{
		"state": "user",
		"cpu":   "2",
	})

	addFloat64Gauge("system.cpu.utilization", 0.6, map[string]interface{}{
		"state": "idle",
		"cpu":   "3",
	})
	addFloat64Gauge("system.cpu.utilization", 0.3, map[string]interface{}{
		"state": "system",
		"cpu":   "3",
	})
	addFloat64Gauge("system.cpu.utilization", 0.1, map[string]interface{}{
		"state": "user",
		"cpu":   "3",
	})

	events, _ := transformMetrics(t, metrics)
	service := modelpb.Service{Name: "unknown", Language: &modelpb.Language{Name: "unknown"}}
	agent := modelpb.Agent{Name: "otlp", Version: "unknown"}
	expected := []*modelpb.APMEvent{{
		Agent:     &agent,
		Service:   &service,
		Labels:    modelpb.Labels{"state": {Value: "idle"}, "cpu": {Value: "0"}},
		Timestamp: timestamppb.New(timestamp),
		Processor: modelpb.MetricsetProcessor(),
		Metricset: &modelpb.Metricset{
			Name: "app",
			Samples: []*modelpb.MetricsetSample{
				{
					Name:  "system.cpu.utilization",
					Type:  modelpb.MetricType_METRIC_TYPE_GAUGE,
					Value: 0.8,
				},
			},
		},
	}, {
		Agent:     &agent,
		Service:   &service,
		Labels:    modelpb.Labels{"state": {Value: "system"}, "cpu": {Value: "0"}},
		Timestamp: timestamppb.New(timestamp),
		Processor: modelpb.MetricsetProcessor(),
		Metricset: &modelpb.Metricset{
			Name: "app",
			Samples: []*modelpb.MetricsetSample{
				{
					Name:  "system.cpu.utilization",
					Type:  modelpb.MetricType_METRIC_TYPE_GAUGE,
					Value: 0.1,
				},
			},
		},
	}, {
		Agent:     &agent,
		Service:   &service,
		Labels:    modelpb.Labels{"state": {Value: "user"}, "cpu": {Value: "0"}},
		Timestamp: timestamppb.New(timestamp),
		Processor: modelpb.MetricsetProcessor(),
		Metricset: &modelpb.Metricset{
			Name: "app",
			Samples: []*modelpb.MetricsetSample{
				{
					Name:  "system.cpu.utilization",
					Type:  modelpb.MetricType_METRIC_TYPE_GAUGE,
					Value: 0.1,
				},
			},
		},
	}, {
		Agent:     &agent,
		Service:   &service,
		Labels:    modelpb.Labels{"state": {Value: "idle"}, "cpu": {Value: "1"}},
		Timestamp: timestamppb.New(timestamp),
		Processor: modelpb.MetricsetProcessor(),
		Metricset: &modelpb.Metricset{
			Name: "app",
			Samples: []*modelpb.MetricsetSample{
				{
					Name:  "system.cpu.utilization",
					Type:  modelpb.MetricType_METRIC_TYPE_GAUGE,
					Value: 0.45,
				},
			},
		},
	}, {
		Agent:     &agent,
		Service:   &service,
		Labels:    modelpb.Labels{"state": {Value: "system"}, "cpu": {Value: "1"}},
		Timestamp: timestamppb.New(timestamp),
		Processor: modelpb.MetricsetProcessor(),
		Metricset: &modelpb.Metricset{
			Name: "app",
			Samples: []*modelpb.MetricsetSample{
				{
					Name:  "system.cpu.utilization",
					Type:  modelpb.MetricType_METRIC_TYPE_GAUGE,
					Value: 0.05,
				},
			},
		},
	}, {
		Agent:     &agent,
		Service:   &service,
		Labels:    modelpb.Labels{"state": {Value: "user"}, "cpu": {Value: "1"}},
		Timestamp: timestamppb.New(timestamp),
		Processor: modelpb.MetricsetProcessor(),
		Metricset: &modelpb.Metricset{
			Name: "app",
			Samples: []*modelpb.MetricsetSample{
				{
					Name:  "system.cpu.utilization",
					Type:  modelpb.MetricType_METRIC_TYPE_GAUGE,
					Value: 0.5,
				},
			},
		},
	}, {
		Agent:     &agent,
		Service:   &service,
		Labels:    modelpb.Labels{"state": {Value: "idle"}, "cpu": {Value: "2"}},
		Timestamp: timestamppb.New(timestamp),
		Processor: modelpb.MetricsetProcessor(),
		Metricset: &modelpb.Metricset{
			Name: "app",
			Samples: []*modelpb.MetricsetSample{
				{
					Name:  "system.cpu.utilization",
					Type:  modelpb.MetricType_METRIC_TYPE_GAUGE,
					Value: 0.59,
				},
			},
		},
	}, {
		Agent:     &agent,
		Service:   &service,
		Labels:    modelpb.Labels{"state": {Value: "system"}, "cpu": {Value: "2"}},
		Timestamp: timestamppb.New(timestamp),
		Processor: modelpb.MetricsetProcessor(),
		Metricset: &modelpb.Metricset{
			Name: "app",
			Samples: []*modelpb.MetricsetSample{
				{
					Name:  "system.cpu.utilization",
					Type:  modelpb.MetricType_METRIC_TYPE_GAUGE,
					Value: 0.01,
				},
			},
		},
	}, {
		Agent:     &agent,
		Service:   &service,
		Labels:    modelpb.Labels{"state": {Value: "user"}, "cpu": {Value: "2"}},
		Timestamp: timestamppb.New(timestamp),
		Processor: modelpb.MetricsetProcessor(),
		Metricset: &modelpb.Metricset{
			Name: "app",
			Samples: []*modelpb.MetricsetSample{
				{
					Name:  "system.cpu.utilization",
					Type:  modelpb.MetricType_METRIC_TYPE_GAUGE,
					Value: 0.4,
				},
			},
		},
	}, {
		Agent:     &agent,
		Service:   &service,
		Labels:    modelpb.Labels{"state": {Value: "idle"}, "cpu": {Value: "3"}},
		Timestamp: timestamppb.New(timestamp),
		Processor: modelpb.MetricsetProcessor(),
		Metricset: &modelpb.Metricset{
			Name: "app",
			Samples: []*modelpb.MetricsetSample{
				{
					Name:  "system.cpu.utilization",
					Type:  modelpb.MetricType_METRIC_TYPE_GAUGE,
					Value: 0.6,
				},
			},
		},
	}, {
		Agent:     &agent,
		Service:   &service,
		Labels:    modelpb.Labels{"state": {Value: "system"}, "cpu": {Value: "3"}},
		Timestamp: timestamppb.New(timestamp),
		Processor: modelpb.MetricsetProcessor(),
		Metricset: &modelpb.Metricset{
			Name: "app",
			Samples: []*modelpb.MetricsetSample{
				{
					Name:  "system.cpu.utilization",
					Type:  modelpb.MetricType_METRIC_TYPE_GAUGE,
					Value: 0.3,
				},
			},
		},
	}, {
		Agent:     &agent,
		Service:   &service,
		Labels:    modelpb.Labels{"state": {Value: "user"}, "cpu": {Value: "3"}},
		Timestamp: timestamppb.New(timestamp),
		Processor: modelpb.MetricsetProcessor(),
		Metricset: &modelpb.Metricset{
			Name: "app",
			Samples: []*modelpb.MetricsetSample{
				{
					Name:  "system.cpu.utilization",
					Type:  modelpb.MetricType_METRIC_TYPE_GAUGE,
					Value: 0.1,
				},
			},
		},
	}}

	eventsMatch(t, expected, events)
}

func TestConsumeMetricsHostMemory(t *testing.T) {
	metrics := pmetric.NewMetrics()
	resourceMetrics := metrics.ResourceMetrics().AppendEmpty()
	scopeMetrics := resourceMetrics.ScopeMetrics().AppendEmpty()
	metricSlice := scopeMetrics.Metrics()

	timestamp := time.Unix(123, 0).UTC()
	addInt64Sum := func(name string, value int64, attributes map[string]interface{}) {
		metric := metricSlice.AppendEmpty()
		metric.SetName(name)
		sum := metric.SetEmptySum()
		dp := sum.DataPoints().AppendEmpty()
		dp.SetTimestamp(pcommon.NewTimestampFromTime(timestamp))
		dp.SetIntValue(value)
		dp.Attributes().FromRaw(attributes)
	}
	addInt64Sum("system.memory.usage", 4773351424, map[string]interface{}{
		"state": "free",
	})
	addInt64Sum("system.memory.usage", 3563778048, map[string]interface{}{
		"state": "used",
	})
	events, _ := transformMetrics(t, metrics)
	service := modelpb.Service{Name: "unknown", Language: &modelpb.Language{Name: "unknown"}}
	agent := modelpb.Agent{Name: "otlp", Version: "unknown"}
	expected := []*modelpb.APMEvent{{
		Agent:     &agent,
		Service:   &service,
		Labels:    modelpb.Labels{"state": {Value: "free"}},
		Timestamp: timestamppb.New(timestamp),
		Processor: modelpb.MetricsetProcessor(),
		Metricset: &modelpb.Metricset{
			Name: "app",
			Samples: []*modelpb.MetricsetSample{
				{
					Name:  "system.memory.usage",
					Type:  modelpb.MetricType_METRIC_TYPE_COUNTER,
					Value: 4773351424,
				},
			},
		},
	}, {
		Agent:     &agent,
		Service:   &service,
		Labels:    modelpb.Labels{"state": {Value: "used"}},
		Timestamp: timestamppb.New(timestamp),
		Processor: modelpb.MetricsetProcessor(),
		Metricset: &modelpb.Metricset{
			Name: "app",
			Samples: []*modelpb.MetricsetSample{
				{
					Name:  "system.memory.usage",
					Type:  modelpb.MetricType_METRIC_TYPE_COUNTER,
					Value: 3563778048,
				},
			},
		},
	}}

	eventsMatch(t, expected, events)
}

func TestConsumeMetrics_JVM(t *testing.T) {
	metrics := pmetric.NewMetrics()
	resourceMetrics := metrics.ResourceMetrics().AppendEmpty()
	scopeMetrics := resourceMetrics.ScopeMetrics().AppendEmpty()
	metricSlice := scopeMetrics.Metrics()

	timestamp := time.Unix(123, 0).UTC()

	addInt64Gauge := func(name string, value int64, attributes map[string]interface{}) {
		metric := metricSlice.AppendEmpty()
		metric.SetName(name)
		gauge := metric.SetEmptyGauge()
		dp := gauge.DataPoints().AppendEmpty()
		dp.SetTimestamp(pcommon.NewTimestampFromTime(timestamp))
		dp.SetIntValue(value)
		dp.Attributes().FromRaw(attributes)
	}
	addInt64Histogram := func(name string, counts []uint64, values []float64, attributes map[string]interface{}) {
		metric := metricSlice.AppendEmpty()
		metric.SetName(name)
		histogram := metric.SetEmptyHistogram()
		dp := histogram.DataPoints().AppendEmpty()
		dp.SetTimestamp(pcommon.NewTimestampFromTime(timestamp))
		dp.BucketCounts().Append(counts...)
		dp.ExplicitBounds().Append(values...)
		dp.Attributes().FromRaw(attributes)
	}

	addInt64Gauge("process.runtime.jvm.memory.limit", 20000, map[string]interface{}{
		"type": "heap",
		"pool": "G1 Eden Space",
	})

	// JVM metrics convention with 'process.runtime.jvm' prefix
	// defined in https://github.com/open-telemetry/opentelemetry-specification/blob/main/specification/metrics/semantic_conventions/runtime-environment-metrics.md#jvm-metrics

	addInt64Histogram("process.runtime.jvm.gc.duration", []uint64{1, 1, 2, 3}, []float64{0.5, 1.5, 2.5}, map[string]interface{}{
		"gc":     "G1 Young Generation",
		"action": "end of minor GC",
	})

	events, _ := transformMetrics(t, metrics)
	service := modelpb.Service{Name: "unknown", Language: &modelpb.Language{Name: "unknown"}}
	agent := modelpb.Agent{Name: "otlp", Version: "unknown"}
	expected := []*modelpb.APMEvent{{
		Agent:     &agent,
		Service:   &service,
		Labels:    modelpb.Labels{"type": {Value: "heap"}, "pool": {Value: "G1 Eden Space"}},
		Timestamp: timestamppb.New(timestamp),
		Processor: modelpb.MetricsetProcessor(),
		Metricset: &modelpb.Metricset{
			Name: "app",
			Samples: []*modelpb.MetricsetSample{
				{
					Name:  "process.runtime.jvm.memory.limit",
					Type:  modelpb.MetricType_METRIC_TYPE_GAUGE,
					Value: 20000,
				},
			},
		},
	}, {
		Agent:     &agent,
		Service:   &service,
		Labels:    modelpb.Labels{"action": {Value: "end of minor GC"}, "gc": {Value: "G1 Young Generation"}},
		Timestamp: timestamppb.New(timestamp),
		Processor: modelpb.MetricsetProcessor(),
		Metricset: &modelpb.Metricset{
			Name: "app",
			Samples: []*modelpb.MetricsetSample{
				{
					Name: "process.runtime.jvm.gc.duration",
					Type: modelpb.MetricType_METRIC_TYPE_HISTOGRAM,
					Histogram: &modelpb.Histogram{
						Counts: []int64{1, 1, 2, 3},
						Values: []float64{0.25, 1, 2, 2.5},
					},
				},
			},
		},
	}}

	eventsMatch(t, expected, events)
}

func TestConsumeMetricsExportTimestamp(t *testing.T) {
	metrics := pmetric.NewMetrics()
	resourceMetrics := metrics.ResourceMetrics().AppendEmpty()

	// The actual timestamps will be non-deterministic, as they are adjusted
	// based on the server's clock.
	//
	// Use a large delta so that we can allow for a significant amount of
	// delay in the test environment affecting the timestamp adjustment.
	const timeDelta = time.Hour
	const allowedError = 5 // seconds

	now := time.Now()
	exportTimestamp := now.Add(-timeDelta)
	resourceMetrics.Resource().Attributes().PutInt("telemetry.sdk.elastic_export_timestamp", exportTimestamp.UnixNano())

	// Timestamp relative to the export timestamp.
	dataPointOffset := -time.Second
	exportedDataPointTimestamp := exportTimestamp.Add(dataPointOffset)

	scopeMetrics := resourceMetrics.ScopeMetrics().AppendEmpty()
	metric := scopeMetrics.Metrics().AppendEmpty()
	metric.SetName("int_gauge")
	intGauge := metric.SetEmptyGauge()
	dp := intGauge.DataPoints().AppendEmpty()
	dp.SetTimestamp(pcommon.NewTimestampFromTime(exportedDataPointTimestamp))
	dp.SetIntValue(1)

	events, _ := transformMetrics(t, metrics)
	require.Len(t, events, 1)
	assert.InDelta(t, now.Add(dataPointOffset).Unix(), events[0].Timestamp.AsTime().Unix(), allowedError)

	for _, e := range events {
		// telemetry.sdk.elastic_export_timestamp should not be sent as a label.
		assert.Empty(t, e.NumericLabels)
	}
}

/* TODO
func TestMetricsLogging(t *testing.T) {
	for _, level := range []logp.Level{logp.InfoLevel, logp.DebugLevel} {
		t.Run(level.String(), func(t *testing.T) {
			logp.DevelopmentSetup(logp.ToObserverOutput(), logp.WithLevel(level))
			transformMetrics(t, pmetric.NewMetrics())
			logs := logp.ObserverLogs().TakeAll()
			if level == logp.InfoLevel {
				assert.Empty(t, logs)
			} else {
				assert.NotEmpty(t, logs)
			}
		})
	}
}
*/

func transformMetrics(t *testing.T, metrics pmetric.Metrics) ([]*modelpb.APMEvent, otlp.ConsumerStats) {
	var batches []*modelpb.Batch
	recorder := batchRecorderBatchProcessor(&batches)

	consumer := otlp.NewConsumer(otlp.ConsumerConfig{
		Processor: recorder,
		Semaphore: semaphore.NewWeighted(100),
	})
	err := consumer.ConsumeMetrics(context.Background(), metrics)
	require.NoError(t, err)
	require.Len(t, batches, 1)
	return *batches[0], consumer.Stats()
}

func eventsMatch(t *testing.T, expected []*modelpb.APMEvent, actual []*modelpb.APMEvent) {
	t.Helper()
	sort.Slice(expected, func(i, j int) bool {
		return strings.Compare(expected[i].String(), expected[j].String()) == -1
	})
	sort.Slice(actual, func(i, j int) bool {
		return strings.Compare(actual[i].String(), actual[j].String()) == -1
	})

	now := time.Now().Unix()
	for i, e := range actual {
		assert.InDelta(t, now, e.Event.Received.AsTime().Unix(), 2)
		e.Event.Received = nil
		if expected[i].Event == nil {
			e.Event = nil
		}
	}

	diff := cmp.Diff(
		expected, actual,
		protocmp.Transform(),
		// Ignore order of events and their metrics. Some other slices
		// have a defined order (e.g. histogram counts/values), so we
		// don't ignore the order of all slices.
		//
		// Comparing string representations is a bit of a hack; ideally
		// we would use like https://github.com/google/go-cmp/issues/67
		protocmp.SortRepeated(func(x, y *modelpb.MetricsetSample) bool {
			return fmt.Sprint(x) < fmt.Sprint(y)
		}),
	)
	if diff != "" {
		t.Fatal(diff)
	}
}
