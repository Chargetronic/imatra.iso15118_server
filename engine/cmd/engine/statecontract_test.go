// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/chargetronic/imatra.iso15118_server/engine/internal/events"
	"github.com/chargetronic/imatra.iso15118_server/engine/internal/secc"
)

// The behaviour every stateStore must have (OSS scope B2): run on the store
// in memory always, on Valkey when VALKEY_TEST_ADDR is set.
func TestStateContractMemory(t *testing.T) {
	m := newMemState()
	stateContract(t, m, m.types)
}

func TestStateContractValkey(t *testing.T) {
	v := testValkey(t)
	defer v.c.Del(context.Background(), streamKey)
	stateContract(t, v, func() []string { return streamTypes(t, v) })
}

func stateContract(t *testing.T, st stateStore, types func() []string) {
	ctx := context.Background()
	run := uuid4()[:8] // fresh keys on a shared Valkey
	dev := func(n string) string { return "t-contract-" + n + "-" + run }
	ev := func(d, sid, typ string) events.Event {
		return events.Event{Type: typ, Device: d, SessionID: sid, Time: time.Now(), Data: map[string]any{}}
	}
	count := func(typ string) int {
		n := 0
		for _, x := range types() {
			if x == typ {
				n++
			}
		}
		return n
	}

	t.Run("create, load, open", func(t *testing.T) {
		d := dev("open")
		if s, err := st.Load(ctx, d, "c1"); s != nil || err != nil {
			t.Fatalf("load before create: %v %v", s, err)
		}
		s, err := st.Create(ctx, d, "c1", secc.NewState(time.Now()))
		if err != nil || s == nil || s.ID == "" {
			t.Fatalf("create: %v %v", s, err)
		}
		again, _ := st.Create(ctx, d, "c1", secc.NewState(time.Now()))
		if again == nil || again.ID != s.ID {
			t.Fatal("a second create of the same connection made another session")
		}
		l, err := st.Load(ctx, d, "c1")
		if err != nil || l == nil || l.ID != s.ID {
			t.Fatalf("load: %v %v", l, err)
		}
		o, err := st.Open(ctx, d)
		if err != nil || len(o) != 1 || o[0].ID != s.ID || o[0].Conn != "c1" {
			t.Fatalf("open: %+v %v", o, err)
		}
		_, _ = st.CloseAll(ctx, d, "test end")
		if o, _ := st.Open(ctx, d); len(o) != 0 {
			t.Fatalf("open after close: %+v", o)
		}
	})

	t.Run("compare-and-set, events with the state", func(t *testing.T) {
		d := dev("cas")
		s, _ := st.Create(ctx, d, "c1", secc.NewState(time.Now()))
		a, _ := st.Load(ctx, d, "c1")
		b, _ := st.Load(ctx, d, "c1")
		before := count(events.ChargingStarted)
		if err := st.Save(ctx, a, ev(d, s.ID, events.V2GStarted)); err != nil {
			t.Fatalf("first save: %v", err)
		}
		if err := st.Save(ctx, b, ev(d, s.ID, events.ChargingStarted)); !errors.Is(err, errConflict) {
			t.Fatalf("stale save: %v, want errConflict", err)
		}
		if count(events.ChargingStarted) != before {
			t.Fatal("a refused save wrote its events")
		}
		if err := st.Save(ctx, a); err != nil {
			t.Fatalf("save from the new version: %v", err)
		}
		_, _ = st.CloseAll(ctx, d, "test end")
	})

	t.Run("close beats a late step (ADR-001)", func(t *testing.T) {
		d := dev("late")
		_, _ = st.Create(ctx, d, "c1", secc.NewState(time.Now()))
		late, _ := st.Load(ctx, d, "c1")
		if err := st.Close(ctx, d, "c1", "EV unplugged"); err != nil {
			t.Fatal(err)
		}
		if err := st.Save(ctx, late); !errors.Is(err, errConflict) {
			t.Fatalf("save after close: %v, want errConflict", err)
		}
		if _, err := st.Load(ctx, d, "c1"); !errors.Is(err, errEnded) {
			t.Fatalf("load after close: %v, want errEnded", err)
		}
	})

	t.Run("one v2g.ended per session", func(t *testing.T) {
		d := dev("end")
		before := count(events.V2GEnded)
		s, _ := st.Create(ctx, d, "c1", secc.NewState(time.Now()))
		_ = st.Close(ctx, d, "c1", "EV unplugged")
		_ = st.Close(ctx, d, "c1", "connection closed")
		if n := count(events.V2GEnded) - before; n != 1 {
			t.Fatalf("%d v2g.ended from two closes, want 1", n)
		}
		// a session that ended itself (SessionStop) gets none from the close
		d2 := dev("end2")
		s2, _ := st.Create(ctx, d2, "c1", secc.NewState(time.Now()))
		_ = st.Save(ctx, s2, ev(d2, s2.ID, events.V2GEnded))
		_ = st.Close(ctx, d2, "c1", "connection closed")
		if n := count(events.V2GEnded) - before; n != 2 {
			t.Fatalf("%d v2g.ended, want 2 (%s)", n, s.ID)
		}
	})

	t.Run("power.changed by threshold and reading order (ADR-003)", func(t *testing.T) {
		d := dev("pw")
		before := count(events.PowerChanged)
		steps := []struct {
			boot string
			n    int64
			w    float64
			want bool
		}{
			{"a", 1, 7000, true}, {"a", 2, 7100, false}, {"a", 4, 3000, true}, {"a", 3, 7100, false},
			{"a", 5, 3299, false}, {"a", 6, 6000, true}, {"b", 1, 0, true}, {"b", 2, 0, false},
		}
		for i, x := range steps {
			got, err := st.Power(ctx, d, "", order{x.boot, x.n}, time.Now(), "", x.w, 0)
			if err != nil || got != x.want {
				t.Fatalf("step %d: %v %v, want %v", i, got, err, x.want)
			}
		}
		if n := count(events.PowerChanged) - before; n != 4 {
			t.Fatalf("%d power.changed, want 4", n)
		}
	})

	t.Run("emit", func(t *testing.T) {
		d := dev("emit")
		before := strings.Count(strings.Join(types(), ","), events.DeviceOnline)
		if err := st.Emit(ctx, ev(d, "", events.DeviceOnline), ev(dev("emit2"), "", events.DeviceOnline)); err != nil {
			t.Fatal(err)
		}
		if n := strings.Count(strings.Join(types(), ","), events.DeviceOnline) - before; n != 2 {
			t.Fatalf("%d device.online, want 2", n)
		}
	})
}
