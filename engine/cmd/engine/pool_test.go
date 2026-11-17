// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chargetronic/imatra.iso15118_server/engine/internal/events"
	"github.com/chargetronic/imatra.iso15118_server/engine/internal/store"
)

// The pool follows the feed events and a pick applies the criteria:
// ISO first, departure, energy still needed, reaction, a stronger limit.
func TestPoolPick(t *testing.T) {
	v := testValkey(t)
	ctx := context.Background()
	now := time.Now().UTC()
	ev := func(dev, typ string, d map[string]any) events.Event {
		return events.Event{Type: typ, Device: dev, Time: now, Data: d}
	}
	plug := func(dev, proto string, pw float64, more ...events.Event) {
		evs := []events.Event{ev(dev, events.DeviceOnline, map[string]any{}), ev(dev, events.SessionStarted, map[string]any{"cp": "B"})}
		if proto != "" {
			evs = append(evs, ev(dev, events.V2GStarted, map[string]any{"protocol": proto}),
				ev(dev, events.ChargingStarted, map[string]any{}))
		} else {
			evs = append(evs, ev(dev, events.CPChanged, map[string]any{"ev": "C", "mode": "basic"}))
		}
		evs = append(evs, ev(dev, events.PowerChanged, map[string]any{"power_w": pw}))
		if err := v.Emit(ctx, append(evs, more...)...); err != nil {
			t.Fatal(err)
		}
	}
	devs := []string{"t-tp-iso", "t-tp-din", "t-tp-pwm", "t-tp-leaves", "t-tp-full", "t-tp-slow", "t-tp-held", "t-tp-off", "t-tp-gone"}
	clean := func() {
		for _, d := range devs {
			v.c.Del(ctx, "hs:"+d, "st:"+d, dseqKey(d))
			v.c.ZRem(ctx, "pool", d)
		}
		v.c.Del(ctx, "act:tp")
	}
	clean()
	defer clean()
	plug("t-tp-iso", "iso15118-2", 7000)
	plug("t-tp-din", "din70121", 11000)
	plug("t-tp-pwm", "", 11000)
	plug("t-tp-leaves", "iso15118-2", 7000, ev("t-tp-leaves", events.EVParameters, map[string]any{"departure_time_s": 600}))
	plug("t-tp-full", "iso15118-2", 7000, ev("t-tp-full", events.EVParameters, map[string]any{"e_amount_wh": 1000}))
	plug("t-tp-slow", "iso15118-2", 7000,
		ev("t-tp-slow", events.LimitApplied, map[string]any{"reaction_ms": 9000}))
	plug("t-tp-held", "iso15118-2", 7000)
	v.c.HSet(ctx, "st:t-tp-held", "cmd", 5, "prio", 90, "until", now.Add(time.Hour).UnixMilli())
	plug("t-tp-off", "iso15118-2", 7000, ev("t-tp-off", events.DeviceOffline, map[string]any{}))
	plug("t-tp-gone", "iso15118-2", 7000, ev("t-tp-gone", events.SessionEnded, map[string]any{}))

	e := &engine{}
	pick := e.poolPicker(v.c)
	got, err := pick(ctx, store.Pick{Activation: "tp", Priority: 50, Ends: now.Add(30 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	var mine []string
	for _, c := range got {
		if strings.HasPrefix(c.Device, "t-tp-") {
			mine = append(mine, c.Device)
		}
	}
	want := "t-tp-iso t-tp-din t-tp-pwm"
	if strings.Join(mine, " ") != want {
		t.Fatalf("picked %v, want %s", mine, want)
	}
	poolTaken(ctx, v.c, "tp", []string{"t-tp-iso"}, now.Add(time.Hour))
	got, _ = pick(ctx, store.Pick{Activation: "tp", Priority: 50, Ends: now.Add(30 * time.Minute), Mode: ptr("din")})
	if len(got) != 1 || got[0].Device != "t-tp-din" || got[0].PowerW != 11000 {
		t.Fatalf("din pick %v", got)
	}
	// a new plug-in forgets the last one's EV data, keeps the reaction
	plug("t-tp-leaves", "iso15118-2", 7000)
	if dep, _ := v.c.HExists(ctx, "hs:t-tp-leaves", "dep").Result(); dep {
		t.Fatal("departure of the last plug-in kept")
	}
	if rx, _ := v.c.HGet(ctx, "hs:t-tp-slow", "rx").Result(); rx != "9000" {
		t.Fatalf("rx %q", rx)
	}
}

func ptr[T any](v T) *T { return &v }

// Energy of a plug-in from the device counter (ADR-002): increments only,
// a counter restart adds nothing, a new plug-in starts at 0.
func TestPlugEnergy(t *testing.T) {
	v := testValkey(t)
	ctx := context.Background()
	dev := "t-tp-energy"
	clean := func() {
		v.c.Del(ctx, "hs:"+dev, pwKey(dev), dseqKey(dev))
		v.c.ZRem(ctx, "pool", dev) // a plug-in puts it in the pool; other tests read the pool
	}
	clean()
	defer clean()
	e := &engine{st: v}
	n := int64(0)
	read := func(wh float64) {
		n++
		if _, err := v.Power(ctx, dev, "", order{"b1", n}, time.Now(), "", 7000, wh); err != nil {
			t.Fatal(err)
		}
	}
	read(100)
	_ = v.Emit(ctx, events.Event{Type: events.SessionStarted, Device: dev, Time: time.Now(), Data: map[string]any{"cp": "B"}})
	read(150)
	read(40) // the device restarted: counter from 0
	read(60)
	wh, _, ok := e.plugEnergy(ctx, dev)
	if !ok || wh != 70 {
		t.Fatalf("energy %v %v, want 70", wh, ok)
	}
	_ = v.Emit(ctx, events.Event{Type: events.SessionStarted, Device: dev, Time: time.Now(), Data: map[string]any{"cp": "B"}})
	read(65)
	if wh, _, _ := e.plugEnergy(ctx, dev); wh != 5 {
		t.Fatalf("new plug-in: energy %v, want 5", wh)
	}
	// a digital session in it: its energy comes with v2g.ended
	_ = v.Emit(ctx, events.Event{Type: events.V2GStarted, Device: dev, SessionID: "s1", Time: time.Now(),
		Data: map[string]any{"protocol": "iso15118-2"}})
	read(77.5)
	v.c.Del(ctx, streamKey)
	_ = v.Emit(ctx, events.Event{Type: events.V2GEnded, Device: dev, SessionID: "s1", Time: time.Now(),
		Data: map[string]any{"reason": "terminated by EV"}})
	xs, _ := v.c.XRange(ctx, streamKey, "-", "+").Result()
	if len(xs) != 1 || !strings.Contains(xs[0].Values["j"].(string), `"energy_wh":12.5`) {
		t.Fatalf("v2g.ended without its energy: %v", xs)
	}
}

// limit.changed reaches the feed only when the limit changed.
func TestLimitChangedOnlyOnChange(t *testing.T) {
	v := testValkey(t)
	ctx := context.Background()
	dev := "t-tp-lev"
	v.c.Del(ctx, "hs:"+dev, dseqKey(dev))
	defer v.c.Del(ctx, "hs:"+dev, dseqKey(dev))
	lim := func(a float64, cmd int64, why string) events.Event {
		d := map[string]any{"limit_a": a, "source": "station", "reason": why}
		if cmd != 0 {
			d["source"], d["command_id"], d["until"] = "activation", cmd, "2026-11-10T20:00:00Z"
		}
		return events.Event{Type: events.LimitChanged, Device: dev, Time: time.Now(), Data: d}
	}
	for _, e := range []events.Event{lim(32, 0, "config"), lim(32, 0, "reconnect"), lim(8, 7, "activation"),
		lim(8, 7, "activation resend"), lim(32, 0, "activation ended"), lim(32, 0, "refresh")} {
		if err := v.Emit(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if got := streamTypes(t, v); len(got) != 3 {
		t.Fatalf("%d limit.changed in the feed, want 3 (32 A, 8 A, 32 A)", len(got))
	}
}
