// Package metrics exposes guvna's operational state in the Prometheus
// exposition format, with no new dependencies: the format is plain text
// and the registry is a few mutex-guarded maps. Counters and histograms
// are incremented on the request path; gauges are sampled from live
// subsystem state at scrape time, so they can never drift from reality.
//
// Label discipline: label values come only from config (provider, model,
// chain names) or fixed vocabularies (outcomes, classes). Nothing
// client-controlled (model strings from requests, IPs, prompts) ever
// becomes a label — a hostile client must not be able to blow up series
// cardinality.
package metrics

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// DefaultBuckets covers LLM gateway latencies: fast cache hits to slow
// multi-minute generations. Shared by both latency histograms.
var DefaultBuckets = []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 25, 60, 120}

// Metric names. The Grafana dashboard and alert rules reference these
// exact strings; renaming one requires updating deploy/observability/.
const (
	ChatRequests   = "guvna_chat_requests_total"
	ChatDuration   = "guvna_chat_duration_seconds"
	StepAttempts   = "guvna_step_attempts_total"
	StepDuration   = "guvna_step_duration_seconds"
	ChainFallbacks = "guvna_chain_fallbacks_total"
	Quarantine     = "guvna_quarantine_events_total"
	Tokens         = "guvna_tokens_total"
	KeypoolKeys    = "guvna_keypool_keys"
	StepDown       = "guvna_step_down"
	StepFailures   = "guvna_step_failures"
	Uptime         = "guvna_uptime_seconds"
)

// Outcome vocabularies, kept fixed so dashboards can match on them.
const (
	OutcomeSuccess     = "success"
	OutcomeFailed      = "failed"
	OutcomeRejected    = "rejected"
	OutcomeAuthError   = "auth_error"
	OutcomeRateLimited = "rate_limited"
	OutcomeServerError = "server_error"
	OutcomeClientError = "client_error"
	OutcomeNetworkErr  = "network_error"
)

// Labels is an ordered label set. Keys must be unique; order is normalized
// on use, so callers need not sort.
type Labels map[string]string

type seriesKey struct {
	name   string
	labels string // canonical "k=v,k=v" encoding, keys sorted
}

type counter struct {
	help   string
	labels Labels
	value  float64
}

// Sample is one gauge reading produced by a GaugeFunc.
type Sample struct {
	Labels Labels
	Value  float64
}

// GaugeFunc samples gauge values at scrape time. It must be cheap and
// non-blocking: it runs on the scrape path under no registry lock.
type GaugeFunc struct {
	help string
	fn   func() []Sample
}

type histogram struct {
	help    string
	labels  Labels
	buckets []float64
	counts  []uint64 // len(buckets)+1; last slot is +Inf
	sum     float64
	count   uint64
}

// Registry holds all series. It is safe for concurrent use.
type Registry struct {
	mu         sync.Mutex
	counters   map[seriesKey]*counter
	gauges     map[seriesKey]*counter // same shape as a counter, rendered as gauge
	gaugeFuncs map[string]*GaugeFunc
	histograms map[seriesKey]*histogram
	help       map[string]string // metric name -> HELP text, first registration wins
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		counters:   make(map[seriesKey]*counter),
		gauges:     make(map[seriesKey]*counter),
		gaugeFuncs: make(map[string]*GaugeFunc),
		histograms: make(map[seriesKey]*histogram),
		help:       make(map[string]string),
	}
}

func canonical(l Labels) string {
	if len(l) == 0 {
		return ""
	}
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(l[k])
	}
	return b.String()
}

func cloneLabels(l Labels) Labels {
	out := make(Labels, len(l))
	for k, v := range l {
		out[k] = v
	}
	return out
}

// IncCounter adds delta to the named counter series, creating it if needed.
func (r *Registry) IncCounter(name, help string, delta float64, labels Labels) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.registerHelp(name, help)
	k := seriesKey{name, canonical(labels)}
	c, ok := r.counters[k]
	if !ok {
		c = &counter{help: help, labels: cloneLabels(labels)}
		r.counters[k] = c
	}
	c.value += delta
}

// SetGauge sets the named gauge series to value, creating it if needed.
func (r *Registry) SetGauge(name, help string, value float64, labels Labels) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.registerHelp(name, help)
	k := seriesKey{name, canonical(labels)}
	g, ok := r.gauges[k]
	if !ok {
		g = &counter{help: help, labels: cloneLabels(labels)}
		r.gauges[k] = g
	}
	g.value = value
}

// RegisterGaugeFunc registers a scrape-time gauge. The function is called
// on every scrape; it must be cheap. Re-registering a name replaces the
// previous function.
func (r *Registry) RegisterGaugeFunc(name, help string, fn func() []Sample) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.registerHelp(name, help)
	r.gaugeFuncs[name] = &GaugeFunc{help: help, fn: fn}
}

// NewSample builds one gauge sample for a GaugeFunc return value.
func NewSample(value float64, labels Labels) Sample {
	return Sample{Labels: cloneLabels(labels), Value: value}
}

// ObserveHistogram records value in the named histogram series, creating
// it if needed. Buckets are fixed at first observation for a metric name;
// later calls with different buckets keep the original set.
func (r *Registry) ObserveHistogram(name, help string, buckets []float64, value float64, labels Labels) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.registerHelp(name, help)
	k := seriesKey{name, canonical(labels)}
	h, ok := r.histograms[k]
	if !ok {
		b := make([]float64, len(buckets))
		copy(b, buckets)
		h = &histogram{help: help, labels: cloneLabels(labels), buckets: b, counts: make([]uint64, len(b)+1)}
		r.histograms[k] = h
	}
	for i, upper := range h.buckets {
		if value <= upper {
			h.counts[i]++
		}
	}
	h.counts[len(h.buckets)]++ // +Inf always counts
	h.sum += value
	h.count++
}

func (r *Registry) registerHelp(name, help string) {
	if _, ok := r.help[name]; !ok {
		r.help[name] = help
	}
}

// Write renders the registry in the Prometheus text exposition format
// (version 0.0.4). Output is deterministic: metric names, label sets, and
// buckets are all sorted.
func (r *Registry) Write(w io.Writer) error {
	r.mu.Lock()
	// Snapshot under lock; gauge funcs run outside it.
	counters := make([]namedSeries, 0, len(r.counters))
	for k, c := range r.counters {
		counters = append(counters, namedSeries{name: k.name, labels: c.labels, value: c.value})
	}
	gauges := make([]namedSeries, 0, len(r.gauges))
	for k, g := range r.gauges {
		gauges = append(gauges, namedSeries{name: k.name, labels: g.labels, value: g.value})
	}
	funcs := make([]struct {
		name string
		fn   *GaugeFunc
	}, 0, len(r.gaugeFuncs))
	for name, gf := range r.gaugeFuncs {
		funcs = append(funcs, struct {
			name string
			fn   *GaugeFunc
		}{name, gf})
	}
	histograms := make([]*histogram, 0, len(r.histograms))
	histNames := make([]string, 0, len(r.histograms))
	for k, h := range r.histograms {
		cp := *h
		cp.counts = append([]uint64(nil), h.counts...)
		histograms = append(histograms, &cp)
		histNames = append(histNames, k.name)
	}
	help := make(map[string]string, len(r.help))
	for n, h := range r.help {
		help[n] = h
	}
	r.mu.Unlock()

	for _, gf := range funcs {
		for _, s := range gf.fn.fn() {
			gauges = append(gauges, namedSeries{name: gf.name, labels: s.Labels, value: s.Value})
		}
	}

	sort.Slice(counters, func(i, j int) bool { return seriesLess(counters[i], counters[j]) })
	sort.Slice(gauges, func(i, j int) bool { return seriesLess(gauges[i], gauges[j]) })

	var histOrder []int
	for i := range histograms {
		histOrder = append(histOrder, i)
	}
	sort.Slice(histOrder, func(a, b int) bool {
		ia, ib := histOrder[a], histOrder[b]
		if histNames[ia] != histNames[ib] {
			return histNames[ia] < histNames[ib]
		}
		return canonical(histograms[ia].labels) < canonical(histograms[ib].labels)
	})

	bw := &errWriter{w: w}
	// Counters, grouped by name with one HELP/TYPE header each.
	writeGrouped(bw, "counter", counters, help)
	writeGrouped(bw, "gauge", gauges, help)
	prevHist := ""
	for _, idx := range histOrder {
		h := histograms[idx]
		name := histNames[idx]
		if name != prevHist {
			writeHelpType(bw, name, help[name], "histogram")
			prevHist = name
		}
		for i, upper := range h.buckets {
			lb := cloneLabels(h.labels)
			lb["le"] = formatFloat(upper)
			fmt.Fprintf(bw, "%s_bucket%s %d\n", name, renderLabels(lb), h.counts[i])
		}
		lb := cloneLabels(h.labels)
		lb["le"] = "+Inf"
		fmt.Fprintf(bw, "%s_bucket%s %d\n", name, renderLabels(lb), h.counts[len(h.buckets)])
		fmt.Fprintf(bw, "%s_sum%s %s\n", name, renderLabels(h.labels), formatFloat(h.sum))
		fmt.Fprintf(bw, "%s_count%s %d\n", name, renderLabels(h.labels), h.count)
	}
	return bw.err
}

type namedSeries struct {
	name   string
	labels Labels
	value  float64
}

func seriesLess(a, b namedSeries) bool {
	if a.name != b.name {
		return a.name < b.name
	}
	return canonical(a.labels) < canonical(b.labels)
}

func writeGrouped(bw *errWriter, typ string, series []namedSeries, help map[string]string) {
	prev := ""
	for _, s := range series {
		if s.name != prev {
			writeHelpType(bw, s.name, help[s.name], typ)
			prev = s.name
		}
		fmt.Fprintf(bw, "%s%s %s\n", s.name, renderLabels(s.labels), formatFloat(s.value))
	}
}

func writeHelpType(bw *errWriter, name, help, typ string) {
	if help != "" {
		fmt.Fprintf(bw, "# HELP %s %s\n", name, help)
	}
	fmt.Fprintf(bw, "# TYPE %s %s\n", name, typ)
}

// renderLabels renders {k="v",k="v"} with keys sorted, or "" when empty.
func renderLabels(l Labels) string {
	if len(l) == 0 {
		return ""
	}
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteString(`="`)
		b.WriteString(escapeLabelValue(l[k]))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

func escapeLabelValue(v string) string {
	var b strings.Builder
	for _, r := range v {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) Write(p []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	var n int
	n, e.err = e.w.Write(p)
	return n, e.err
}
