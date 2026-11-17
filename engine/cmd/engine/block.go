// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

package main

// Local block of a device by the operator (13.11.2026, OSS scope, task A5).
//
// The deployment keeps no revocation list: a device revoked by the service layer
// holds a valid client certificate until it expires (7 days). The operator
// cuts it off at once with PUT /v1/devices/{id}/block (devices.blocked_at,
// NOTIFY "blocked"). Two independent barriers:
//
//   - every engine instance keeps the blocked set in memory: it drops the
//     device's messages before they reach a worker and sends it nothing
//     (guardedClient below: V2G answers, pairs, limits, settings);
//   - the leader publishes the set, retained, on "svc/blocked"; the broker
//     container renders it into a deny section of the device ACL and reloads
//     (deploy/mqtt/mqtt-run.sh), so the broker itself stops the device's
//     topics both ways while its TLS connection stays up.
//
// A device just blocked is reported offline (feed device.offline with
// reason "blocked") so it leaves the activation pool; after unblocking its
// next heartbeat brings it back.

import (
	"context"
	"github.com/chargetronic/imatra.iso15118_server/engine/internal/tunnel"
	"sort"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"github.com/chargetronic/imatra.iso15118_server/engine/internal/events"
	"github.com/chargetronic/imatra.iso15118_server/engine/internal/store"
)

// BlockedTopic: the retained list of blocked devices for the broker
// ("v1 <id> <id> ..."; "v1" alone when none). Services only (ACL).
const BlockedTopic = "svc/blocked"

var mBlockedDrop = reg.CounterVec("engine_blocked_messages_total",
	"Messages of blocked devices dropped by the engine, by direction (in, out).", "dir")

type blocklist struct {
	mu  sync.RWMutex
	set map[string]struct{}
}

func (b *blocklist) has(device string) bool {
	if b == nil {
		return false
	}
	b.mu.RLock()
	_, ok := b.set[device]
	b.mu.RUnlock()
	return ok
}

// replace sets the whole list; returns the devices newly blocked.
func (b *blocklist) replace(ids []string) (added []string) {
	next := make(map[string]struct{}, len(ids))
	for _, d := range ids {
		next[d] = struct{}{}
	}
	b.mu.Lock()
	for d := range next {
		if _, ok := b.set[d]; !ok {
			added = append(added, d)
		}
	}
	b.set = next
	b.mu.Unlock()
	return added
}

func (b *blocklist) list() []string {
	b.mu.RLock()
	out := make([]string, 0, len(b.set))
	for d := range b.set {
		out = append(out, d)
	}
	b.mu.RUnlock()
	sort.Strings(out)
	return out
}

// blockedPayload: what the broker container reads (plain words, no JSON
// parser in the broker image). Ids are those of devices the engine has seen
// on dev/<id>/...; the broker side checks the characters again.
func blockedPayload(ids []string) string {
	return strings.TrimSpace("v1 " + strings.Join(ids, " "))
}

// isBlocked: messages of this device are not handled, nothing is sent to it.
func (e *engine) isBlocked(device string) bool { return e.blocked.has(device) }

// reloadBlocked reads the list from Postgres; the devices newly blocked are
// reported offline by their owner. The leader republishes the list.
func (e *engine) reloadBlocked(why string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ids, err := e.db.BlockedDevices(ctx)
	if err != nil {
		e.log.Warn("blocked devices", "err", err)
		return
	}
	for _, d := range e.blocked.replace(ids) {
		e.log.Info("device blocked", "device", d, "why", why)
		if e.coord.owns(d) {
			e.w.do(d, func() { e.cutOff(d) })
		}
	}
	if e.coord.isLeader() {
		e.publishBlockedSoon()
	}
}

// The broker's ACL changes in batches (block D, mass revocations): every
// change within BLOCKED_BATCH_MS (3 s) goes out as one list, so the broker
// reloads its configuration once, not once per device. The engines refuse a
// blocked device at once regardless (the in-memory set above).
var (
	blockedBatch = time.Duration(envF("BLOCKED_BATCH_MS", 3000)) * time.Millisecond
	blockedPubMu sync.Mutex
	blockedPend  bool
)

func (e *engine) publishBlockedSoon() {
	blockedPubMu.Lock()
	defer blockedPubMu.Unlock()
	if blockedPend {
		return
	}
	blockedPend = true
	time.AfterFunc(blockedBatch, func() {
		blockedPubMu.Lock()
		blockedPend = false
		blockedPubMu.Unlock()
		e.publishBlocked()
	})
}

// cutOff: a device just blocked leaves the pool and the operator sees it
// offline; its hot station record goes (a later unblock rebuilds it).
func (e *engine) cutOff(device string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	now := time.Now()
	e.pres.put(store.Presence{Device: device, Online: false,
		Status: []byte(`{"online":false,"event":"blocked"}`), At: now}, false)
	e.emit(ctx, events.Event{Type: events.DeviceOffline, Device: device, Time: now.UTC(),
		Data: map[string]any{"reason": "blocked"}})
	e.dropStation(device)
}

func (e *engine) publishBlocked() {
	if e.client == nil || !e.client.IsConnectionOpen() {
		return
	}
	e.client.Publish(BlockedTopic, 1, true, blockedPayload(e.blocked.list()))
}

// blockedLoop: a missed notification (listener reconnect) or a new leader
// is caught within a minute.
func (e *engine) blockedLoop() {
	e.reloadBlocked("start")
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		e.reloadBlocked("periodic")
	}
}

// deviceOfTopic: <id> of dev/<id>/..., or "".
func deviceOfTopic(topic string) string {
	p := strings.SplitN(topic, "/", 3)
	if len(p) < 3 || p[0] != tunnel.Root {
		return ""
	}
	return p[1]
}

// guardedClient: the engine's broker client; nothing goes to a blocked device.
type guardedClient struct {
	mqtt.Client
	e *engine
}

func (g guardedClient) Publish(topic string, qos byte, retained bool, payload interface{}) mqtt.Token {
	if d := deviceOfTopic(topic); d != "" && g.e.isBlocked(d) {
		mBlockedDrop.With("out").Inc()
		return doneToken{}
	}
	return g.Client.Publish(topic, qos, retained, payload)
}

// doneToken: a publish not made, completed at once without an error.
type doneToken struct{}

var closedCh = func() chan struct{} { c := make(chan struct{}); close(c); return c }()

func (doneToken) Wait() bool                     { return true }
func (doneToken) WaitTimeout(time.Duration) bool { return true }
func (doneToken) Done() <-chan struct{}          { return closedCh }
func (doneToken) Error() error                   { return nil }
