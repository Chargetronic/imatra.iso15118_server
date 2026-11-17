// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"time"
)

// metrics collects every measured duration by name; percentiles at the end.
// Up to 20k devices x a few dozen samples each: plain slices are enough.
type metrics struct {
	mu      sync.Mutex
	samples map[string][]float64 // ms
	counts  map[string]int64
	started time.Time
}

func newMetrics() *metrics {
	return &metrics{samples: map[string][]float64{}, counts: map[string]int64{}, started: time.Now()}
}

func (m *metrics) observe(name string, d time.Duration) {
	m.mu.Lock()
	m.samples[name] = append(m.samples[name], float64(d.Microseconds())/1000)
	m.mu.Unlock()
}

func (m *metrics) inc(name string) {
	m.mu.Lock()
	m.counts[name]++
	m.mu.Unlock()
}

func (m *metrics) count(name string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.counts[name]
}

// Stat is one line of the report.
type Stat struct {
	N    int     `json:"n"`
	P50  float64 `json:"p50_ms"`
	P95  float64 `json:"p95_ms"`
	P99  float64 `json:"p99_ms"`
	Max  float64 `json:"max_ms"`
	Mean float64 `json:"mean_ms"`
}

func statOf(v []float64) Stat {
	if len(v) == 0 {
		return Stat{}
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	q := func(p float64) float64 { return s[int(p*float64(len(s)-1)+0.5)] }
	sum := 0.0
	for _, x := range s {
		sum += x
	}
	return Stat{N: len(s), P50: q(0.50), P95: q(0.95), P99: q(0.99), Max: s[len(s)-1], Mean: sum / float64(len(s))}
}

// Report is what a run writes (-report) and prints.
type Report struct {
	Params     map[string]any    `json:"params"`
	Elapsed    float64           `json:"elapsed_s"`
	Latency    map[string]Stat   `json:"latency"`
	Counts     map[string]int64  `json:"counts"`
	Activation *ActivationReport `json:"activation,omitempty"`
	Reconnect  *ReconnectReport  `json:"reconnect,omitempty"`
}

// ActivationReport: one stage of current limits sent through the operator API.
type ActivationReport struct {
	Devices      int     `json:"devices"`
	APIAccepted  int     `json:"api_accepted"`
	APIErrors    int     `json:"api_errors"`
	SendSpanS    float64 `json:"send_span_s"`    // first to last API call
	Delivered    int     `json:"delivered"`      // pair or power/set reached the device
	DeliverSpanS float64 `json:"deliver_span_s"` // first API call to last delivery
	PerDevice    Stat    `json:"per_device"`     // API call -> delivery
}

// ReconnectReport: all devices dropped and came back with jitter.
type ReconnectReport struct {
	Devices     int     `json:"devices"`
	Reconnected int     `json:"reconnected"`
	AllBackS    float64 `json:"all_back_s"`   // drop -> last device connected again
	PairsBack   int     `json:"pairs_back"`   // devices in a session that got their pair again
	PairsBackS  float64 `json:"pairs_back_s"` // drop -> last re-sent pair
}

func (m *metrics) report(params map[string]any) *Report {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := &Report{Params: params, Elapsed: time.Since(m.started).Seconds(),
		Latency: map[string]Stat{}, Counts: map[string]int64{}}
	for k, v := range m.samples {
		r.Latency[k] = statOf(v)
	}
	for k, v := range m.counts {
		r.Counts[k] = v
	}
	return r
}

func (r *Report) print(w io.Writer) {
	fmt.Fprintf(w, "\n== fleetgen report (%.0f s) ==\n", r.Elapsed)
	keys := make([]string, 0, len(r.Latency))
	for k := range r.Latency {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Fprintf(w, "%-34s %7s %9s %9s %9s %9s\n", "latency", "n", "p50 ms", "p95 ms", "p99 ms", "max ms")
	for _, k := range keys {
		s := r.Latency[k]
		fmt.Fprintf(w, "%-34s %7d %9.1f %9.1f %9.1f %9.1f\n", k, s.N, s.P50, s.P95, s.P99, s.Max)
	}
	ck := make([]string, 0, len(r.Counts))
	for k := range r.Counts {
		ck = append(ck, k)
	}
	sort.Strings(ck)
	fmt.Fprintln(w, "\ncounts:")
	for _, k := range ck {
		fmt.Fprintf(w, "  %-32s %d\n", k, r.Counts[k])
	}
	if a := r.Activation; a != nil {
		fmt.Fprintf(w, "\nactivation: %d devices, %d accepted, %d API errors; API calls over %.2f s\n",
			a.Devices, a.APIAccepted, a.APIErrors, a.SendSpanS)
		fmt.Fprintf(w, "  delivered %d, last delivery %.2f s after the first call; per device p50 %.0f / p95 %.0f / max %.0f ms\n",
			a.Delivered, a.DeliverSpanS, a.PerDevice.P50, a.PerDevice.P95, a.PerDevice.Max)
	}
	if c := r.Reconnect; c != nil {
		fmt.Fprintf(w, "\nreconnect: %d/%d back in %.1f s; pairs re-sent to %d devices, last after %.1f s\n",
			c.Reconnected, c.Devices, c.AllBackS, c.PairsBack, c.PairsBackS)
	}
}

func (r *Report) save(path string) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
