// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

package main

// Engine instances. Every instance is stateless and may
// serve any device: they subscribe through $share/engine/... (MQTT_SHARE_GROUP),
// so the broker hands each message to one instance.
//
// What must still happen once:
//   - jobs for the whole fleet (TTL renewal): only the leader runs them;
//   - the reaction to a database notification (a new limit, a new
//     configuration), which every instance receives: only the instance that
//     owns the device reacts. Owner = rendezvous hash of the device id over
//     the live instances. Every instance drops its caches for the device.
//
// Membership and leadership live in Valkey. Without a Valkey client (the
// engine's tests, memstate_test.go) the one instance is the leader and owns
// everything.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"hash/fnv"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

type coordinator struct {
	id string
	c  *redis.Client // nil: tests, a single instance

	mu      sync.RWMutex
	members []string
	leader  bool
	stopped atomic.Bool // drained: no more heartbeats or lease
}

// leaseScript: KEYS[1] lease key; ARGV[1] instance id, ARGV[2] ttl ms.
// Returns 1 when this instance holds the lease after the call.
var leaseScript = redis.NewScript(`
local cur = redis.call('GET', KEYS[1])
if cur == ARGV[1] then
  redis.call('PEXPIRE', KEYS[1], ARGV[2])
  return 1
end
if not cur then
  redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])
  return 1
end
return 0`)

const (
	beat      = 2 * time.Second
	memberTTL = 6 * time.Second
	// 6 s: a dead leader's activations resume within about that (found
	// 10.11.2026: with 15 s a stage waited 16 s). A slow tick that loses the
	// lease cannot double the leader's work: stages and jobs are taken with
	// row locks (SKIP LOCKED) in Postgres.
	leaderTTL = 6 * time.Second
)

func newCoordinator(e *engine) *coordinator {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	host, _ := os.Hostname()
	k := &coordinator{id: host + "-" + hex.EncodeToString(b)}
	v, ok := e.st.(*vkState)
	if !ok {
		k.members, k.leader = []string{k.id}, true
		return k
	}
	k.c = v.c
	k.tick(e)
	go func() {
		for range time.Tick(beat) {
			k.tick(e)
		}
	}()
	return k
}

// tick: heartbeat, live members, leader lease.
func (k *coordinator) tick(e *engine) {
	if k.stopped.Load() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), beat)
	defer cancel()
	now := time.Now()
	k.c.ZAdd(ctx, "engines", redis.Z{Score: float64(now.UnixMilli()), Member: k.id})
	k.c.ZRemRangeByScore(ctx, "engines", "-inf", itoa(now.Add(-memberTTL).UnixMilli()))
	members, err := k.c.ZRange(ctx, "engines", 0, -1).Result()
	if err != nil {
		e.log.Warn("engine members", "err", err)
		return
	}
	sort.Strings(members)
	// Leader: take the lease if free, extend it only if we hold it — one
	// atomic step. A separate GET then EXPIRE could extend a lease that
	// expired and was taken by another instance in between: two leaders for
	// a tick (review 09.11.2026). Harmless for TTL renewal, not for
	// leader-only work with a rate limit.
	leader := false
	if n, err := leaseScript.Run(ctx, k.c, []string{"engine-leader"}, k.id, leaderTTL.Milliseconds()).Int(); err == nil {
		leader = n == 1
	} else {
		e.log.Warn("leader lease", "err", err)
	}
	k.mu.Lock()
	// Compare the set, not the count: in a rolling restart one instance
	// leaves and another joins, the count stays, the owners change (review
	// 09.11.2026, R2). Both lists are sorted.
	changed := leader != k.leader || len(members) != len(k.members)
	for i := 0; !changed && i < len(members); i++ {
		changed = members[i] != k.members[i]
	}
	k.members, k.leader = members, leader
	k.mu.Unlock()
	if changed {
		e.log.Info("engine instances", "me", k.id, "members", len(members), "leader", leader)
		// Membership changed: devices may have a new owner, which catches up
		// on them (after start-up the MQTT connect does it anyway).
		if e.client != nil && e.client.IsConnected() {
			go e.refreshAll()
		}
	}
}

// leave: this instance stops (drain): its leader lease and membership go at
// once, so another instance takes over without waiting for the TTLs.
func (k *coordinator) leave() {
	if k.c == nil {
		return
	}
	k.stopped.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = releaseScript.Run(ctx, k.c, []string{"engine-leader"}, k.id).Err()
	k.c.ZRem(ctx, "engines", k.id)
	k.mu.Lock()
	k.leader = false
	k.mu.Unlock()
}

// releaseScript: KEYS[1] lease key; ARGV[1] instance id. Drops the lease only if held.
var releaseScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end
return 0`)

func (k *coordinator) isLeader() bool {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.leader
}

// owns: this instance reacts to notifications about device.
func (k *coordinator) owns(device string) bool {
	k.mu.RLock()
	defer k.mu.RUnlock()
	if len(k.members) <= 1 {
		return true
	}
	var best string
	var bestH uint64
	for _, m := range k.members {
		h := fnv.New64a()
		h.Write([]byte(m))
		h.Write([]byte{0})
		h.Write([]byte(device))
		if v := h.Sum64(); best == "" || v > bestH {
			best, bestH = m, v
		}
	}
	return best == k.id
}

func itoa(n int64) string {
	b := make([]byte, 0, 20)
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
