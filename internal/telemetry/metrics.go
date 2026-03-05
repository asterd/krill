package telemetry

import (
	"context"
	"fmt"
	"maps"
	"sort"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/metric"
)

const (
	MetricActiveLoops         = "krill.active_loops"
	MetricInboundQueueDepth   = "krill.inbound_queue_depth"
	MetricSkillActivations    = "krill.skill.activations_total"
	MetricMemoryOpsTotal      = "krill.memory.ops_total"
	MetricMemoryBytes         = "krill.memory.bytes"
	MetricSandboxExecDuration = "krill.sandbox.exec_duration_ms"
	MetricAgentHandoffTotal   = "krill.agent.handoff_total"
	MetricSchedulerTriggers   = "krill.scheduler.trigger_total"
	MetricSessionResumeTotal  = "krill.session.resume_total"
)

type metricType string

const (
	metricCounter metricType = "counter"
	metricGauge   metricType = "gauge"
	metricHist    metricType = "histogram"
)

type metricSample struct {
	Name      string            `json:"name"`
	Type      string            `json:"type"`
	Value     float64           `json:"value"`
	Count     int64             `json:"count,omitempty"`
	Sum       float64           `json:"sum,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
	UpdatedAt time.Time         `json:"updated_at"`
}

type metricStore struct {
	mu      sync.RWMutex
	defs    map[string]metricType
	samples map[string]metricSample
}

func newMetricStore() *metricStore {
	return &metricStore{defs: map[string]metricType{}, samples: map[string]metricSample{}}
}

func preRegisterCoreMetrics(store *metricStore) {
	store.register(MetricActiveLoops, metricGauge)
	store.register(MetricInboundQueueDepth, metricGauge)
	store.register(MetricSkillActivations, metricCounter)
	store.register(MetricMemoryOpsTotal, metricCounter)
	store.register(MetricMemoryBytes, metricGauge)
	store.register(MetricSandboxExecDuration, metricHist)
	store.register(MetricAgentHandoffTotal, metricCounter)
	store.register(MetricSchedulerTriggers, metricCounter)
	store.register(MetricSessionResumeTotal, metricCounter)
	zero := time.Now().UTC()
	store.upsert(metricSample{Name: MetricAgentHandoffTotal, Type: string(metricCounter), Value: 0, UpdatedAt: zero})
	store.upsert(metricSample{Name: MetricSchedulerTriggers, Type: string(metricCounter), Value: 0, UpdatedAt: zero})
	store.upsert(metricSample{Name: MetricSessionResumeTotal, Type: string(metricCounter), Value: 0, UpdatedAt: zero})
}

func (s *metricStore) register(name string, typ metricType) {
	s.mu.Lock()
	s.defs[name] = typ
	s.mu.Unlock()
}

func (s *metricStore) isKnown(name string) bool {
	s.mu.RLock()
	_, ok := s.defs[name]
	s.mu.RUnlock()
	return ok
}

func (s *metricStore) upsert(sample metricSample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.defs[sample.Name]; !ok {
		return
	}
	key := metricKey(sample.Name, sample.Labels)
	prev, exists := s.samples[key]
	switch metricType(sample.Type) {
	case metricCounter:
		if exists {
			sample.Value += prev.Value
		}
	case metricHist:
		if exists {
			sample.Count += prev.Count
			sample.Sum += prev.Sum
		}
	}
	s.samples[key] = sample
}

func (s *metricStore) snapshot() []metricSample {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]metricSample, 0, len(s.samples))
	for _, sample := range s.samples {
		cp := sample
		if cp.Labels != nil {
			cp.Labels = maps.Clone(cp.Labels)
		}
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool {
		return metricKey(out[i].Name, out[i].Labels) < metricKey(out[j].Name, out[j].Labels)
	})
	return out
}

func metricKey(name string, labels map[string]string) string {
	if len(labels) == 0 {
		return name
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, labels[k]))
	}
	return name + "|" + strings.Join(parts, ",")
}

func IncCounter(name string, delta int64, labels map[string]string) {
	if delta == 0 || !metricEnabled(name) {
		return
	}
	rt := current.Load()
	if rt == nil {
		return
	}
	inst := rt.counter(name)
	attrs := labelsToOTel(labels)
	inst.Add(context.Background(), delta, metric.WithAttributes(attrs...))
	rt.knownMetrics.upsert(metricSample{
		Name:      name,
		Type:      string(metricCounter),
		Value:     float64(delta),
		Labels:    labels,
		UpdatedAt: time.Now().UTC(),
	})
}

func SetGauge(name string, value int64, labels map[string]string) {
	if !metricEnabled(name) {
		return
	}
	rt := current.Load()
	if rt == nil {
		return
	}
	g := rt.gauge(name)
	key := metricKey(name, labels)
	prevAny, _ := g.vals.LoadOrStore(key, int64(0))
	prev, _ := prevAny.(int64)
	delta := value - prev
	g.vals.Store(key, value)
	if delta != 0 {
		attrs := labelsToOTel(labels)
		g.inst.Add(context.Background(), delta, metric.WithAttributes(attrs...))
	}
	rt.knownMetrics.upsert(metricSample{
		Name:      name,
		Type:      string(metricGauge),
		Value:     float64(value),
		Labels:    labels,
		UpdatedAt: time.Now().UTC(),
	})
}

func ObserveDurationMs(name string, d time.Duration, labels map[string]string) {
	if !metricEnabled(name) {
		return
	}
	rt := current.Load()
	if rt == nil {
		return
	}
	inst := rt.histogram(name)
	ms := d.Milliseconds()
	attrs := labelsToOTel(labels)
	inst.Record(context.Background(), ms, metric.WithAttributes(attrs...))
	rt.knownMetrics.upsert(metricSample{
		Name:      name,
		Type:      string(metricHist),
		Value:     float64(ms),
		Count:     1,
		Sum:       float64(ms),
		Labels:    labels,
		UpdatedAt: time.Now().UTC(),
	})
}

func (rt *runtimeState) counter(name string) metric.Int64Counter {
	if inst, ok := rt.counters.Load(name); ok {
		return inst.(metric.Int64Counter)
	}
	created, _ := rt.meter.Int64Counter(name)
	actual, _ := rt.counters.LoadOrStore(name, created)
	return actual.(metric.Int64Counter)
}

func (rt *runtimeState) histogram(name string) metric.Int64Histogram {
	if inst, ok := rt.histos.Load(name); ok {
		return inst.(metric.Int64Histogram)
	}
	created, _ := rt.meter.Int64Histogram(name)
	actual, _ := rt.histos.LoadOrStore(name, created)
	return actual.(metric.Int64Histogram)
}

func (rt *runtimeState) gauge(name string) *gaugeState {
	if inst, ok := rt.gauges.Load(name); ok {
		return inst.(*gaugeState)
	}
	counter, _ := rt.meter.Int64UpDownCounter(name)
	created := &gaugeState{inst: counter}
	actual, _ := rt.gauges.LoadOrStore(name, created)
	return actual.(*gaugeState)
}
