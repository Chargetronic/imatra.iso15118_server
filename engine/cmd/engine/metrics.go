// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

package main

// Metrics of an engine instance (GET :8090/metrics, Prometheus text format)
// and probes for an orchestrator (10.11.2026):
//
//	/live   the process runs (liveness)
//	/ready  takes traffic: connected to the broker, Valkey answers (with
//	        Valkey), not draining. Postgres is not required: the answer path
//	        does not use it (ADR-002).
//
// What to scale the engines by (docs/metrics.md,
// section on scaling): the wait in the device workers and the server time of
// a V2G answer, against the ISO 15118-2 budget; CPU as a second signal.

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/chargetronic/imatra.iso15118_server/engine/internal/metrics"
)

var (
	reg = &metrics.Registry{}

	mMessages = reg.CounterVec("engine_mqtt_messages_total", "Device messages handled, by topic.", "topic")
	mWait     = reg.Histogram("engine_worker_wait_seconds",
		"Time a device message waits in its worker queue before it is handled.", metrics.LatencyBuckets)
	mV2G = reg.Histogram("engine_v2g_server_seconds",
		"V2G request: from arrival at the engine to the end of its handling (queue + step + answer); ISO 15118-2 budget 1.5 s.",
		metrics.LatencyBuckets)
	mActWait = reg.Histogram("engine_activation_send_wait_seconds", "Activation send: wait in the send queue.", metrics.LatencyBuckets)
	mActRun  = reg.Histogram("engine_activation_send_seconds", "Activation send: the send itself.", metrics.LatencyBuckets)
	mFailed  = reg.Counter("engine_v2g_failed_total", "V2G requests answered with a failure or closed for an engine error.")
	mLost    = reg.Counter("engine_v2g_unknown_session_total", "Requests of a session the server does not know (state lost).")
)

// topicKind: the part after dev/<device>/ (bounded set of label values).
func topicKind(topic string) string {
	p := strings.SplitN(topic, "/", 3)
	if len(p) < 3 {
		return "other"
	}
	return p[0] + "/" + p[2]
}

func (e *engine) startMetrics() {
	reg.Process()
	reg.Gauge("engine_ready", "1 while the instance takes traffic.", func() float64 { return b2f(e.ready()) })
	reg.Gauge("engine_leader", "1 on the leader (activation scheduler, fleet jobs).", func() float64 { return b2f(e.coord.isLeader()) })
	reg.Gauge("engine_mqtt_connected", "1 while connected to the broker.", func() float64 { return b2f(e.client != nil && e.client.IsConnectionOpen()) })
	reg.Gauge("engine_workers", "Device workers (ENGINE_WORKERS).", func() float64 { return float64(len(e.w.qs)) })
	reg.Gauge("engine_worker_backlog", "Device messages queued or running in the workers.", func() float64 { return float64(e.w.busy.Load()) })
	reg.Gauge("engine_sender_backlog", "Activation sends queued or running.", func() float64 { return float64(e.senders.busy.Load()) })
	reg.GaugeVec("engine_write_queue", "Background writes waiting, by queue.", "queue", func() map[string]float64 {
		m := map[string]float64{"twin": float64(len(e.twin)), "acks": float64(len(e.acks)),
			"feed": float64(len(e.feed)), "sent": float64(len(e.sent))}
		if e.tb != nil {
			m["telemetry"] = float64(len(e.tb.in))
		}
		if e.pres != nil {
			e.pres.mu.Lock()
			m["presence"] = float64(len(e.pres.last))
			e.pres.mu.Unlock()
		}
		return m
	})
	reg.GaugeVec("engine_db_connections", "Postgres pool connections in use, by pool.", "pool", func() map[string]float64 {
		return map[string]float64{"main": float64(e.db.Stats().InUse), "background": float64(e.bg.Stats().InUse)}
	})
	reg.GaugeVec("engine_db_wait_seconds_total", "Time spent waiting for a Postgres connection, by pool.", "pool", func() map[string]float64 {
		return map[string]float64{"main": e.db.Stats().WaitDuration.Seconds(), "background": e.bg.Stats().WaitDuration.Seconds()}
	})
	if vk := e.vk(); vk != nil {
		reg.GaugeVec("engine_valkey_pool", "Valkey client pool: connections, timeouts.", "stat", func() map[string]float64 {
			s := vk.PoolStats()
			return map[string]float64{"total": float64(s.TotalConns), "idle": float64(s.IdleConns),
				"timeouts": float64(s.Timeouts), "misses": float64(s.Misses)}
		})
	}
}

// ready: takes traffic.
func (e *engine) ready() bool {
	if e.draining.Load() || e.client == nil || !e.client.IsConnectionOpen() {
		return false
	}
	if vk := e.vk(); vk != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if vk.Ping(ctx).Err() != nil {
			return false
		}
	}
	return true
}

func (e *engine) probes() {
	http.Handle("/metrics", reg.Handler())
	http.HandleFunc("/live", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	http.HandleFunc("/ready", func(w http.ResponseWriter, _ *http.Request) {
		if !e.ready() {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(200)
	})
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
