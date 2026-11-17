// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

package main

import (
	"context"
	"testing"
	"time"

	"github.com/chargetronic/imatra.iso15118_server/engine/internal/store"
)

// A heartbeat that says nothing new is not written; a connect, a change of
// online or of version, and last_seen_at once its period is over are.
func TestPresenceOnlyNews(t *testing.T) {
	v := testValkey(t)
	ctx := context.Background()
	dev := "t-tp-pres"
	v.c.Del(ctx, "hs:"+dev)
	defer v.c.Del(ctx, "hs:"+dev)
	p := &presenceWriter{vk: v.c, last: map[string]store.Presence{}, seen: 300 * time.Second,
		always: func(store.Presence) bool { return false }}
	t0 := time.Now()
	st := func(online bool, sw string, at time.Duration) store.Presence {
		return store.Presence{Device: dev, Online: online, At: t0.Add(at),
			Status: []byte(`{"online":true,"sw":"` + sw + `","uptime_s":1}`)}
	}
	steps := []struct {
		x       store.Presence
		connect bool
		want    bool
	}{
		{st(true, "1.0", 0), true, true},                // connect
		{st(true, "1.0", 60*time.Second), false, false}, // heartbeat
		{st(true, "1.1", 120*time.Second), false, true}, // new software
		{st(true, "1.1", 180*time.Second), false, false},
		{st(true, "1.1", 430*time.Second), false, true},  // last_seen_at due
		{st(false, "1.1", 440*time.Second), false, true}, // offline
	}
	for i, s := range steps {
		p.last = map[string]store.Presence{}
		p.put(s.x, s.connect)
		if _, got := p.last[dev]; got != s.want {
			t.Fatalf("step %d: written %v, want %v", i, got, s.want)
		}
	}
}
