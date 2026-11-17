// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

package main

// Presence writer (10.11.2026, ADR-002): a device's status report (connect,
// heartbeat, last will) updated devices.online / status / last_seen_at with
// one Postgres write on the device's worker; a mass reconnect then held every
// worker on Postgres, like the unplugs did. Now the latest report of each
// device is kept here and written in one statement every PRESENCE_FLUSH_MS
// (500 ms) through the background pool. The feed events device.online /
// device.offline go to Valkey at once, as before.
//
// Only what is news is written: online / offline, a (re)connect, another
// software or settings version; a heartbeat that only says "still here"
// moves last_seen_at at most every PRESENCE_SEEN_S (300 s). At 20 000
// devices that is a few writes a second instead of ~330. Devices with the debug journal: every report, as before.

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/chargetronic/imatra.iso15118_server/engine/internal/store"
)

// presenceNews: KEYS hs:<dev>; ARGV key, now s, seen s, force (1/0). Shared
// by the instances (a device's heartbeats go to any of them): 1 when the
// report is news (another key, or last_seen_at due), and then remembered.
var presenceNews = redis.NewScript(`
local h = redis.call('HMGET', KEYS[1], 'pkey', 'pat')
local now = tonumber(ARGV[2])
if ARGV[4] ~= '1' and h[1] == ARGV[1] and h[2] and now - tonumber(h[2]) < tonumber(ARGV[3]) then return 0 end
redis.call('HSET', KEYS[1], 'pkey', ARGV[1], 'pat', now)
redis.call('EXPIRE', KEYS[1], 172800)
return 1`)

type presenceWriter struct {
	vk      *redis.Client // with several instances: what was written is shared (presenceNews)
	mu      sync.Mutex
	last    map[string]store.Presence // device -> latest report to write
	written map[string]presenceSeen   // device -> what this instance wrote last
	seen    time.Duration
	always  func(store.Presence) bool
}

type presenceSeen struct {
	key string // online + versions
	at  time.Time
}

// presenceKey: the fields of a status report that are news when they change.
func presenceKey(x store.Presence) string {
	var st map[string]any
	_ = json.Unmarshal(x.Status, &st)
	k := fmt.Sprint(x.Online)
	for _, f := range []string{"sw", "fw", "firmware", "platform", "config_ver", "config_rev"} {
		if v, ok := st[f]; ok {
			k += "|" + fmt.Sprint(v)
		}
	}
	return k
}

func (e *engine) startPresenceWriter() *presenceWriter {
	p := &presenceWriter{vk: e.vk(), last: map[string]store.Presence{}, written: map[string]presenceSeen{},
		seen:   time.Duration(envF("PRESENCE_SEEN_S", 300)) * time.Second,
		always: func(x store.Presence) bool { return e.journal(x.Device) }}
	every := time.Duration(envF("PRESENCE_FLUSH_MS", 500)) * time.Millisecond
	go func() {
		for range time.Tick(every) {
			p.mu.Lock()
			if len(p.last) == 0 {
				p.mu.Unlock()
				continue
			}
			batch := make([]store.Presence, 0, len(p.last))
			for _, x := range p.last {
				batch = append(batch, x)
			}
			p.last = map[string]store.Presence{}
			p.mu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := e.bg.SetPresenceBatch(ctx, batch); err != nil {
				e.log.Error("presence batch", "devices", len(batch), "err", err)
				p.mu.Lock() // kept for the next round unless a newer report came
				for _, x := range batch {
					if _, newer := p.last[x.Device]; !newer {
						p.last[x.Device] = x
					}
				}
				p.mu.Unlock()
			}
			cancel()
		}
	}()
	return p
}

// put: the device's latest report, if it is news (see above); an older one
// still waiting is replaced: only the newest of one device counts.
func (p *presenceWriter) put(x store.Presence, connect bool) {
	key := presenceKey(x)
	force := connect || p.always(x)
	if p.vk != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		n, err := presenceNews.Run(ctx, p.vk, []string{"hs:" + x.Device}, key, x.At.Unix(), int(p.seen.Seconds()), b01(force)).Int()
		cancel()
		if err == nil && n == 0 {
			return // a heartbeat with nothing new
		}
		p.mu.Lock()
		p.last[x.Device] = x
		p.mu.Unlock()
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	w, ok := p.written[x.Device]
	if ok && !connect && w.key == key && x.At.Sub(w.at) < p.seen && !p.always(x) {
		if _, waiting := p.last[x.Device]; !waiting {
			return // a heartbeat with nothing new
		}
	}
	p.written[x.Device] = presenceSeen{key: key, at: x.At}
	p.last[x.Device] = x
}
