// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

// Package metrics: a few counters, gauges and histograms in the Prometheus
// text format (GET /metrics), without a client library. Used by the engine,
// the relay and the projector for monitoring and autoscaling
// (docs/metrics.md).
package metrics

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Registry holds the metrics of a process.
type Registry struct {
	mu    sync.Mutex
	items []item
}

type item struct {
	name, help, kind string
	write            func(w io.Writer, name string)
}

// Counter: a value that only grows.
type Counter struct{ v atomic.Uint64 }

func (c *Counter) Inc()         { c.v.Add(1) }
func (c *Counter) Add(n uint64) { c.v.Add(n) }

// Histogram: observations in fixed buckets (seconds).
type Histogram struct {
	bounds []float64
	counts []atomic.Uint64
	sumUS  atomic.Uint64
	n      atomic.Uint64
}

// LatencyBuckets: 1 ms .. 10 s; ISO 15118-2 limits (1.5 s performance,
// 2 s EVCC timeout) fall on bucket bounds.
var LatencyBuckets = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 1.5, 2, 5, 10}

func (h *Histogram) Observe(d time.Duration) {
	s := d.Seconds()
	i := sort.SearchFloat64s(h.bounds, s)
	if i < len(h.counts) {
		h.counts[i].Add(1)
	}
	h.n.Add(1)
	h.sumUS.Add(uint64(max(0, d.Microseconds())))
}

func (r *Registry) add(name, help, kind string, write func(io.Writer, string)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items = append(r.items, item{name, help, kind, write})
}

// Counter registers a counter (name ends in _total).
func (r *Registry) Counter(name, help string) *Counter {
	c := &Counter{}
	r.add(name, help, "counter", func(w io.Writer, n string) { fmt.Fprintf(w, "%s %d\n", n, c.v.Load()) })
	return c
}

// CounterVec: counters by one label value.
type CounterVec struct {
	mu sync.Mutex
	m  map[string]*Counter
}

func (v *CounterVec) With(label string) *Counter {
	v.mu.Lock()
	defer v.mu.Unlock()
	c := v.m[label]
	if c == nil {
		c = &Counter{}
		v.m[label] = c
	}
	return c
}

func (r *Registry) CounterVec(name, help, label string) *CounterVec {
	v := &CounterVec{m: map[string]*Counter{}}
	r.add(name, help, "counter", func(w io.Writer, n string) {
		v.mu.Lock()
		keys := make([]string, 0, len(v.m))
		for k := range v.m {
			keys = append(keys, k)
		}
		v.mu.Unlock()
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(w, "%s{%s=%q} %d\n", n, label, k, v.With(k).v.Load())
		}
	})
	return v
}

// Gauge registers a value read when scraped.
func (r *Registry) Gauge(name, help string, f func() float64) {
	r.add(name, help, "gauge", func(w io.Writer, n string) { fmt.Fprintf(w, "%s %s\n", n, num(f())) })
}

// GaugeVec: several values by one label, read when scraped.
func (r *Registry) GaugeVec(name, help, label string, f func() map[string]float64) {
	r.add(name, help, "gauge", func(w io.Writer, n string) {
		m := f()
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(w, "%s{%s=%q} %s\n", n, label, k, num(m[k]))
		}
	})
}

// CounterFunc: a counter kept elsewhere, read when scraped.
func (r *Registry) CounterFunc(name, help string, f func() float64) {
	r.add(name, help, "counter", func(w io.Writer, n string) { fmt.Fprintf(w, "%s %s\n", n, num(f())) })
}

// Histogram registers a histogram with the given bounds (seconds).
func (r *Registry) Histogram(name, help string, bounds []float64) *Histogram {
	h := &Histogram{bounds: bounds, counts: make([]atomic.Uint64, len(bounds))}
	r.add(name, help, "histogram", func(w io.Writer, n string) {
		var cum uint64
		for i, b := range h.bounds {
			cum += h.counts[i].Load()
			fmt.Fprintf(w, "%s_bucket{le=%q} %d\n", n, num(b), cum)
		}
		fmt.Fprintf(w, "%s_bucket{le=\"+Inf\"} %d\n", n, h.n.Load())
		fmt.Fprintf(w, "%s_sum %s\n", n, num(float64(h.sumUS.Load())/1e6))
		fmt.Fprintf(w, "%s_count %d\n", n, h.n.Load())
	})
	return h
}

// Process adds CPU, memory and goroutines of this process.
func (r *Registry) Process() {
	r.CounterFunc("process_cpu_seconds_total", "CPU time of the process (user + system).", func() float64 {
		var u syscall.Rusage
		if syscall.Getrusage(syscall.RUSAGE_SELF, &u) != nil {
			return 0
		}
		return float64(u.Utime.Sec+u.Stime.Sec) + float64(u.Utime.Usec+u.Stime.Usec)/1e6
	})
	r.Gauge("go_goroutines", "Goroutines.", func() float64 { return float64(runtime.NumGoroutine()) })
	r.Gauge("go_memstats_heap_inuse_bytes", "Heap in use.", func() float64 {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return float64(m.HeapInuse)
	})
}

// Handler serves the registry in the Prometheus text format.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		r.mu.Lock()
		items := append([]item(nil), r.items...)
		r.mu.Unlock()
		var b strings.Builder
		for _, it := range items {
			fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", it.name, it.help, it.name, it.kind)
			it.write(&b, it.name)
		}
		_, _ = io.WriteString(w, b.String())
	})
}

func num(f float64) string {
	if math.IsInf(f, 1) {
		return "+Inf"
	}
	return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.6f", f), "0"), ".")
}
