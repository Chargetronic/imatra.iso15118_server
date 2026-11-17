// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

package main

// Device workers:
// parallel processing with per-device order.
//
// Before: the MQTT client delivered every message of every device to one
// goroutine (SetOrderMatters), and each message waited for 3..8 database
// round trips — about 400 messages/s for the whole fleet (load test
// 08.11.2026).
//
// Now: the MQTT goroutine only hands a message to the worker that owns the
// device (hash of the device id). A device's messages stay in order (one
// worker), different devices run in parallel. Engine-side work on a device
// (limit refresh, expiry, catch-up) goes through the same worker, so it never
// races with that device's V2G steps.
//
// Two lanes per worker (load test 16.11.2026): a reconnect wave of 5000
// devices queued 10 000 connect-time jobs (settings, power/set, pair
// re-send) ahead of the EV requests, which waited up to 23 s and timed out.
// The fast lane (EV requests, CP, telemetry, limits and activations) always
// goes first; the slow lane (status: presence and the connect-time sync)
// runs when the fast lane is empty. Order holds within a lane; a device's
// status may run after its later EV request, which the connect-time sync
// tolerates (it re-sends the pair of the newest open session).

import (
	"github.com/chargetronic/imatra.iso15118_server/engine/internal/tunnel"
	"hash/fnv"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

type workers struct {
	qs   []chan func() // fast lane
	slow []chan func() // slow lane: runs when the fast lane is empty
	busy atomic.Int64  // queued + running jobs, for the load log
	log  *slog.Logger
}

func newWorkers(n, depth int, log *slog.Logger) *workers {
	w := &workers{qs: make([]chan func(), n), slow: make([]chan func(), n), log: log}
	for i := range w.qs {
		q, sq := make(chan func(), depth), make(chan func(), 4*depth)
		w.qs[i], w.slow[i] = q, sq
		go func() {
			run := func(f func()) { f(); w.busy.Add(-1) }
			for {
				select { // the fast lane first
				case f := <-q:
					run(f)
					continue
				default:
				}
				select {
				case f := <-q:
					run(f)
				case f := <-sq:
					run(f)
				}
			}
		}()
	}
	go w.report()
	return w
}

func (w *workers) index(device string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(device))
	return h.Sum32() % uint32(len(w.qs))
}

// do runs f on the worker that owns device, after that device's earlier jobs
// of the fast lane. It blocks when that worker's queue is full: back-pressure
// on the MQTT reader instead of dropping messages.
func (w *workers) do(device string, f func()) {
	w.busy.Add(1)
	w.qs[w.index(device)] <- f
}

// doSlow: the same worker, the slow lane (after the device's earlier slow
// jobs, and after whatever is in the fast lane).
func (w *workers) doSlow(device string, f func()) {
	w.busy.Add(1)
	w.slow[w.index(device)] <- f
}

// report logs the backlog once a minute while there is one.
func (w *workers) report() {
	for range time.Tick(time.Minute) {
		if n := w.busy.Load(); n > int64(len(w.qs)) {
			w.log.Warn("engine backlog", "jobs", n, "workers", len(w.qs))
		}
	}
}

// byDevice wraps an MQTT handler so it runs on the device's worker. The
// device is the second topic level: dev/<device>/...
func (e *engine) byDevice(h mqtt.MessageHandler) mqtt.MessageHandler { return e.onWorker(h, false) }

// byDeviceSlow: the same on the slow lane (status, connect-time sync).
func (e *engine) byDeviceSlow(h mqtt.MessageHandler) mqtt.MessageHandler { return e.onWorker(h, true) }

func (e *engine) onWorker(h mqtt.MessageHandler, slow bool) mqtt.MessageHandler {
	return func(c mqtt.Client, m mqtt.Message) {
		parts := strings.SplitN(m.Topic(), "/", 3)
		key := m.Topic()
		if len(parts) >= 2 {
			key = parts[1]
		}
		if parts[0] == tunnel.Root && e.isBlocked(key) { // operator's block (block.go); the broker denies it too
			mBlockedDrop.With("in").Inc()
			return
		}
		kind := topicKind(m.Topic())
		queued := time.Now()
		rm := received{m, queued}
		do := e.w.do
		if slow {
			do = e.w.doSlow
		}
		do(key, func() {
			started := time.Now()
			mWait.Observe(started.Sub(queued))
			mMessages.With(kind).Inc()
			h(c, rm)
			if kind == tunnel.Root+"/v2g/up" {
				mV2G.Observe(time.Since(queued))
			}
		})
	}
}

// received: a message with the time it reached the engine, before the
// device's worker queue. The time of the events it gives (ADR-003): waiting
// in the queue is not part of it.
type received struct {
	mqtt.Message
	at time.Time
}

// recvAt: when the message reached the engine (now for one not from byDevice).
func recvAt(m mqtt.Message) time.Time {
	if r, ok := m.(received); ok {
		return r.at.UTC()
	}
	return time.Now().UTC()
}
