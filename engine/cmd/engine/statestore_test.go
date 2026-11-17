// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/chargetronic/imatra.iso15118_server/engine/internal/events"
	"github.com/chargetronic/imatra.iso15118_server/engine/internal/secc"
	"github.com/chargetronic/imatra.iso15118_server/engine/internal/store"
)

// Needs a Valkey: VALKEY_TEST_ADDR=127.0.0.1:6379 go test ./cmd/engine/
func testValkey(t *testing.T) *vkState {
	addr := os.Getenv("VALKEY_TEST_ADDR")
	if addr == "" {
		t.Skip("VALKEY_TEST_ADDR not set")
	}
	c := redis.NewClient(&redis.Options{Addr: addr})
	if err := c.Ping(context.Background()).Err(); err != nil {
		t.Skip(err)
	}
	streamKey = "events-test"
	c.Del(context.Background(), streamKey)
	return &vkState{c: c, log: slog.Default(), maxLen: "1000"}
}

func streamTypes(t *testing.T, v *vkState) []string {
	t.Helper()
	xs, err := v.c.XRange(context.Background(), streamKey, "-", "+").Result()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, x := range xs {
		out = append(out, x.Values["t"].(string))
	}
	return out
}

// ADR-001: the EV is unplugged (cp, instance A) while a late request of the
// same session is being stepped (v2g/up, instance B). B's save must fail and
// the session must read as ended, so B answers nothing.
func TestCloseBeatsLateStep(t *testing.T) {
	v := testValkey(t)
	ctx := context.Background()
	dev, conn := "t-test-adr1", "c1"
	s := &store.Session{ID: uuid4(), State: secc.NewState(time.Now())}
	if _, err := v.seed(ctx, dev, conn, s, true); err != nil {
		t.Fatal(err)
	}
	defer v.c.Del(ctx, sKey(s.ID), cKey(dev, conn), oKey(dev))

	// B loaded the session (version 1) and is stepping it...
	b, err := v.Load(ctx, dev, conn)
	if err != nil || b == nil {
		t.Fatalf("load: %v %v", b, err)
	}
	// ...A closes it meanwhile.
	v.end(ctx, dev, s.ID, "EV unplugged")
	v.unopen(ctx, dev, s.ID)

	if err := v.Save(ctx, b); !errors.Is(err, errConflict) {
		t.Fatalf("save after close: got %v, want errConflict", err)
	}
	if _, err := v.Load(ctx, dev, conn); !errors.Is(err, errEnded) {
		t.Fatalf("reload after close: got %v, want errEnded", err)
	}
	if n, _ := v.c.Exists(ctx, oKey(dev)).Result(); n != 0 {
		t.Fatalf("open-session pointer still there")
	}
	if in, _ := v.c.SIsMember(ctx, "open", dev).Result(); in {
		t.Fatalf("device still in the open set")
	}
}

// Two saves from the same version: exactly one wins.
func TestCompareAndSet(t *testing.T) {
	v := testValkey(t)
	ctx := context.Background()
	dev, conn := "t-test-cas", "c1"
	s := &store.Session{ID: uuid4(), State: secc.NewState(time.Now())}
	if _, err := v.seed(ctx, dev, conn, s, true); err != nil {
		t.Fatal(err)
	}
	defer v.c.Del(ctx, sKey(s.ID), cKey(dev, conn), oKey(dev))
	a, _ := v.Load(ctx, dev, conn)
	b, _ := v.Load(ctx, dev, conn)
	if err := v.Save(ctx, a); err != nil {
		t.Fatalf("first save: %v", err)
	}
	if err := v.Save(ctx, b); !errors.Is(err, errConflict) {
		t.Fatalf("second save from the same version: got %v, want errConflict", err)
	}
}

// A step's events are written with its state or not at all; the
// close adds v2g.ended only when the step did not end the session itself.
func TestStepEventsWithState(t *testing.T) {
	v := testValkey(t)
	ctx := context.Background()
	dev, conn := "t-test-ev", "c1"
	s := &store.Session{ID: uuid4(), State: secc.NewState(time.Now())}
	if _, err := v.seed(ctx, dev, conn, s, true); err != nil {
		t.Fatal(err)
	}
	defer v.c.Del(ctx, sKey(s.ID), cKey(dev, conn), oKey(dev), dseqKey(dev), streamKey)
	ev := func(typ string) events.Event {
		return events.Event{Type: typ, Device: dev, SessionID: s.ID, Time: time.Now(), Data: map[string]any{}}
	}
	a, _ := v.Load(ctx, dev, conn)
	b, _ := v.Load(ctx, dev, conn)
	if err := v.Save(ctx, a, ev(events.V2GStarted)); err != nil {
		t.Fatal(err)
	}
	if err := v.Save(ctx, b, ev(events.ChargingStarted)); !errors.Is(err, errConflict) {
		t.Fatalf("stale save: %v", err)
	}
	if got := strings.Join(streamTypes(t, v), ","); got != events.V2GStarted {
		t.Fatalf("after a conflict the stream has %q, want only v2g.started", got)
	}
	if err := v.Save(ctx, a, ev(events.V2GEnded)); err != nil { // SessionStop
		t.Fatal(err)
	}
	v.end(ctx, dev, s.ID, "connection closed") // then the TCP connection goes
	want := events.V2GStarted + "," + events.V2GEnded
	if got := strings.Join(streamTypes(t, v), ","); got != want {
		t.Fatalf("stream %q, want %q", got, want)
	}
	// dseq grows by one per event of the device
	xs, _ := v.c.XRange(ctx, streamKey, "-", "+").Result()
	q0, _ := strconv.ParseInt(xs[0].Values["q"].(string), 10, 64)
	q1, _ := strconv.ParseInt(xs[1].Values["q"].(string), 10, 64)
	if q1 != q0+1 || q0 < 1_000_000_000_000 {
		t.Fatalf("dseq %d, %d", q0, q1)
	}
}

// A session closed while still running gets v2g.ended from the close.
func TestCloseEmitsEnd(t *testing.T) {
	v := testValkey(t)
	ctx := context.Background()
	dev, conn := "t-test-close", "c1"
	s := &store.Session{ID: uuid4(), State: secc.NewState(time.Now())}
	_, _ = v.seed(ctx, dev, conn, s, true)
	defer v.c.Del(ctx, sKey(s.ID), cKey(dev, conn), oKey(dev), dseqKey(dev), streamKey)
	v.end(ctx, dev, s.ID, "EV unplugged")
	v.end(ctx, dev, s.ID, "connection closed")
	if got := strings.Join(streamTypes(t, v), ","); got != events.V2GEnded {
		t.Fatalf("stream %q, want one v2g.ended", got)
	}
}

// power.changed: only on a change by the threshold; an older reading (it
// came through another instance late) is ignored by its number, not by any
// clock; a reading of a new boot (numbers from 1 again) is taken (ADR-003).
func TestPowerChanged(t *testing.T) {
	v := testValkey(t)
	ctx := context.Background()
	dev := "t-test-pw"
	defer v.c.Del(ctx, pwKey(dev), dseqKey(dev), streamKey)
	steps := []struct {
		boot string
		n    int64
		w    float64
		want bool
	}{
		{"a", 1, 7000, true},        // first reading
		{"a", 2, 7100, false},       // below the threshold
		{"a", 4, 3000, true},        // limit applied
		{"a", 3, 7100, false},       // late reading from before
		{"a", 5, 3000 + 299, false}, // still below
		{"a", 6, 7000 - 1000, true}, // up again
		{"b", 1, 0, true},           // the device restarted: numbers from 1, taken
		{"b", 2, 0, false},
	}
	for i, st := range steps {
		got, err := v.Power(ctx, dev, "", order{st.boot, st.n}, time.Now(), "", st.w, 0)
		if err != nil || got != st.want {
			t.Fatalf("step %d: got %v %v, want %v", i, got, err, st.want)
		}
	}
	if n := len(streamTypes(t, v)); n != 4 {
		t.Fatalf("%d power events, want 4", n)
	}
}

// A second seed of the same session (another instance found it in Postgres,
// or the creator seeds after the other one stepped) never resets the state.
func TestSeedKeepsNewerState(t *testing.T) {
	v := testValkey(t)
	ctx := context.Background()
	dev, conn := "t-test-seed", "c1"
	id := uuid4()
	defer v.c.Del(ctx, sKey(id), cKey(dev, conn), oKey(dev))
	b, err := v.seed(ctx, dev, conn, &store.Session{ID: id, State: secc.NewState(time.Now())}, true)
	if err != nil {
		t.Fatal(err)
	}
	b.State.Phase = "WaitSessionSetup" // B steps the handshake
	if err := v.Save(ctx, b); err != nil {
		t.Fatal(err)
	}
	a, err := v.seed(ctx, dev, conn, &store.Session{ID: id, State: secc.NewState(time.Now())}, true) // A's late seed
	if err != nil || a.Ver != 2 || a.State.Phase != "WaitSessionSetup" {
		t.Fatalf("late seed: ver %d phase %q err %v, want the stepped state", a.Ver, a.State.Phase, err)
	}
}

// hotLimit: a stage's limit goes into the shared station record only where
// it wins: higher priority, or same priority and newer command, or the
// stored one expired; never into a record that does not exist.
func TestHotLimitPriority(t *testing.T) {
	v := testValkey(t)
	ctx := context.Background()
	k := hotKey("t-test-hot")
	defer v.c.Del(ctx, k)
	run := func(cmd int64, lim float64, prio int, until time.Time) int {
		n, err := hotLimit.Run(ctx, v.c, []string{k}, cmd, lim, prio, until.UnixMilli(), "test").Int()
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	later := time.Now().Add(time.Hour)
	if n := run(1, 10, 0, later); n != 0 {
		t.Fatalf("no record: got %d, want 0", n)
	}
	v.c.HSet(ctx, k, "cfg", "{}", "cmd", 0, "prio", 0, "until", 0)
	steps := []struct {
		cmd  int64
		prio int
		want int
	}{
		{10, 0, 1},  // direct limit
		{11, 50, 1}, // activation over it
		{12, 0, -1}, // newer direct limit does not beat the activation
		{9, 50, -1}, // older command of the same priority
		{13, 50, 1}, // newer command of the same priority
	}
	for i, s := range steps {
		if n := run(s.cmd, 8, s.prio, later); n != s.want {
			t.Fatalf("step %d: got %d, want %d", i, n, s.want)
		}
	}
	v.c.HSet(ctx, k, "until", time.Now().Add(-time.Second).UnixMilli()) // the stored one expired
	if n := run(14, 8, 0, later); n != 1 {
		t.Fatalf("after expiry: got %d, want 1", n)
	}
}

// Open(device) in one round trip: the session with its version; nothing
// once it ended.
func TestOpenOneCall(t *testing.T) {
	v := testValkey(t)
	ctx := context.Background()
	dev, conn := "t-test-open", "c1"
	s := &store.Session{ID: uuid4(), State: secc.NewState(time.Now())}
	if _, err := v.seed(ctx, dev, conn, s, true); err != nil {
		t.Fatal(err)
	}
	defer v.c.Del(ctx, sKey(s.ID), cKey(dev, conn), oKey(dev))
	open, err := v.Open(ctx, dev)
	if err != nil || len(open) != 1 || open[0].ID != s.ID || open[0].Conn != conn || open[0].Ver != 1 {
		t.Fatalf("open: %+v %v", open, err)
	}
	v.c.HSet(ctx, sKey(s.ID), "ended", 1)
	if open, err := v.Open(ctx, dev); err != nil || len(open) != 0 {
		t.Fatalf("after end: %+v %v", open, err)
	}
}
