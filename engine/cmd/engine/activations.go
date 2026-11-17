// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

package main

// Activations executor.
//
//	scheduler — the leader only: starts stages on time (selection, limits,
//	  send times over the spread: store.StartDueStage), queues the sends at a
//	  fleet-wide rate (ACT_RATE commands/s, default 5000), reports stages,
//	  ends activations. One place of decision: pace and order do not depend on
//	  the number of instances. A new leader resumes from Postgres.
//	execution — every instance: with Valkey the sends go through the stream
//	  act-jobs (consumer group "engines"); each instance runs its share on the
//	  device's worker: the limit in force now goes out (new Charging Loop pair,
//	  or power/set). Without a Valkey client (tests) the leader runs them itself.
//
// A send is idempotent: it sends whatever limit is in force for the device
// (highest priority, newest), confirmed by the device per command id
// (ADR-001, rule 4). A repeated or late job changes nothing.

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"
	"github.com/redis/go-redis/v9"

	"github.com/chargetronic/imatra.iso15118_server/engine/internal/events"
	"github.com/chargetronic/imatra.iso15118_server/engine/internal/store"
)

// whyResend: a send not confirmed the first time; the Charging Loop pair
// goes out as a new version even if unchanged, so the device acknowledges
// it as installed (an unchanged pair is not sent at all).
const whyResend = "activation resend"

const (
	actStream = "act-jobs"
	actGroup  = "engines"
	actTick   = 50 * time.Millisecond
)

func (e *engine) activations(dsn string) {
	rate := envF("ACT_RATE", 5000)
	perTick := int(rate * actTick.Seconds())
	if perTick < 1 {
		perTick = 1
	}
	var vk *redis.Client
	if v, ok := e.st.(*vkState); ok {
		vk = v.c
		go e.actWorker(vk)
	}
	wake := make(chan string, 64)
	go e.actListen(dsn, wake)
	t := time.NewTicker(actTick)
	defer t.Stop()
	next := time.Now() // when the leader next has something to do
	for {
		select {
		case <-t.C:
		case msg := <-wake: // a new activation or stage, extend, end: at once
			e.actNotice(msg)
			next = time.Now()
		}
		if !e.coord.isLeader() {
			next = time.Now() // a new leader looks at once
			continue
		}
		if time.Now().Before(next) {
			continue
		}
		e.actStep(vk, perTick)
		next = e.actNext()
	}
}

// actNext: when the scheduler has work again (10.11.2026). Before, the leader
// asked Postgres five questions 20 times a second (~100 statements/s) even
// with no activation at all. Now: every tick (50 ms) only while a stage is
// being handed out or sends of a spread are due; once a second while a stage
// waits for its confirmations; otherwise asleep until the next stage start,
// send or activation end, or a notice from the API (NOTIFY activations), and
// at the latest ACT_IDLE_CHECK_S (10 s) for safety.
func (e *engine) actNext() time.Time {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	now := time.Now()
	w, err := e.db.NextActivationWork(ctx)
	if err != nil {
		return now.Add(time.Second)
	}
	switch {
	case w.Busy:
		return now
	case w.Waiting:
		return now.Add(time.Second)
	}
	idle := now.Add(time.Duration(envF("ACT_IDLE_CHECK_S", 10) * float64(time.Second)))
	if !w.Next.IsZero() && w.Next.Before(idle) {
		return w.Next
	}
	return idle
}

// actListen: API notices (a new activation or stage, extend, end).
func (e *engine) actListen(dsn string, wake chan<- string) {
	l := pq.NewListener(dsn, time.Second, 30*time.Second, nil)
	if err := l.Listen("activations"); err != nil {
		e.log.Error("listen activations", "err", err)
		return
	}
	for n := range l.Notify {
		if n == nil {
			continue // the ticker covers anything missed
		}
		wake <- n.Extra
	}
}

// actNotice: "end:<id>" / "extend:<id>": the devices get the limit now in force.
func (e *engine) actNotice(msg string) {
	what, id, _ := strings.Cut(msg, ":")
	if !e.coord.isLeader() || (what != "end" && what != "extend") {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	devs, err := e.db.Resend(ctx, id)
	if err != nil {
		e.log.Error("activation resend", "activation", id, "err", err)
		return
	}
	e.forget(ctx, devs) // the limit in force is another one now: read from Postgres at the send
	n := len(devs)
	e.log.Info("activation "+what, "activation", id, "devices", n)
	if what == "end" {
		e.emit(ctx, events.Event{Type: events.ActivationEnded, Time: time.Now().UTC(),
			Data: map[string]any{"activation_id": id, "reason": "ended by the operator", "devices": n}})
	}
}

// actStep: one tick of the leader.
func (e *engine) actStep(vk *redis.Client, perTick int) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var pick store.Picker
	if vk != nil {
		e.poolSeed(ctx, vk)
		pick = e.poolPicker(vk)
	}
	for { // stages due: selection (from the pool) and limits, one transaction each
		t0 := time.Now()
		st, err := e.db.StartDueStage(ctx, e.def.Phases, pick)
		if err != nil {
			e.log.Error("activation stage start", "err", err)
			break
		}
		if st == nil {
			break
		}
		// The new limits straight into the shared station state: the sends
		// read nothing from Postgres, and every instance answers the EVs
		// with the new limit from now on.
		if vk != nil {
			poolTaken(ctx, vk, st.Activation, st.Devices, st.Until)
			e.hotLimits(ctx, vk, st.Devices, st.Commands, st.LimitA, st.Priority, st.Until)
		} else {
			e.forget(ctx, st.Devices)
		}
		e.log.Info("activation stage started", "activation", st.Activation, "stage", st.Stage,
			"eligible", st.Eligible, "selected", st.Selected, "limit_a", st.LimitA,
			"from", map[bool]string{true: "pool", false: "postgres"}[st.FromPool], "start_ms", time.Since(t0).Milliseconds())
		e.emit(ctx, events.Event{Type: events.ActivationStageStarted, Time: time.Now().UTC(),
			Data: map[string]any{"activation_id": st.Activation, "stage": st.Stage, "devices": st.Selected,
				"eligible": st.Eligible, "level": st.LimitA}})
	}
	// sends due, at the fleet-wide rate
	jobs, err := e.db.TakeJobs(ctx, perTick)
	if err != nil {
		e.log.Error("activation jobs", "err", err)
	} else if len(jobs) > 0 {
		if err := e.actQueue(ctx, vk, jobs); err != nil {
			e.log.Error("queue activation jobs", "jobs", len(jobs), "err", err)
			_ = e.db.ReleaseJobs(ctx, jobs)
		}
	}
	if err := e.db.Dispatched(ctx); err != nil {
		e.log.Error("activation dispatched", "err", err)
	}
	if reps, err := e.db.Reports(ctx, 10*time.Second); err == nil {
		for _, r := range reps {
			e.emit(ctx, events.Event{Type: events.ActivationStageDelivered, Time: time.Now().UTC(),
				Data: map[string]any{"activation_id": r.Activation, "stage": r.Stage, "devices": r.Selected,
					"sent": r.Sent, "confirmed": r.Confirmed, "duration_ms": r.DurationMs,
					"reduction_kw_est": float64(int(r.ReductionW/100)) / 10}})
			e.log.Info("activation stage delivered", "activation", r.Activation, "stage", r.Stage,
				"devices", r.Selected, "sent", r.Sent, "confirmed", r.Confirmed, "ms", r.DurationMs)
		}
	}
	if ended, err := e.db.EndExpired(ctx); err == nil {
		for _, id := range ended {
			e.emit(ctx, events.Event{Type: events.ActivationEnded, Time: time.Now().UTC(),
				Data: map[string]any{"activation_id": id, "reason": "duration over"}})
			devs, err := e.db.Resend(ctx, id) // the limits before apply again now
			if err != nil {
				e.log.Error("activation end resend", "activation", id, "err", err)
			}
			e.forget(ctx, devs)
		}
	}
}

// forget: the devices' station state is rebuilt from Postgres at next use.
func (e *engine) forget(ctx context.Context, devs []string) {
	if vk := e.vk(); vk != nil {
		e.hotDrop(ctx, vk, devs...)
		return
	}
	for _, d := range devs {
		e.dropStation(d)
	}
}

// actQueue: to the instances (Valkey) or, with one engine, to its own workers.
func (e *engine) actQueue(ctx context.Context, vk *redis.Client, jobs []store.Job) error {
	if vk == nil {
		for _, j := range jobs {
			e.actRun(j.Device, j.Resend, nil)
		}
		return nil
	}
	p := vk.Pipeline()
	for _, j := range jobs {
		p.XAdd(ctx, &redis.XAddArgs{Stream: actStream, MaxLen: 200000, Approx: true,
			Values: []any{"d", j.Device, "c", j.CommandID, "r", map[bool]string{true: "1", false: "0"}[j.Resend]}})
	}
	_, err := p.Exec(ctx)
	return err
}

// actRun: send the device the limit in force now, on its worker.
func (e *engine) actRun(device string, resend bool, done func()) {
	queued := time.Now()
	why := "activation"
	if resend {
		why = whyResend
	}
	e.senders.do(device, func() {
		started := time.Now()
		e.refresh(device, why) // the limit is in the hot state already: no Postgres read
		actStats.add(started.Sub(queued), time.Since(started))
		mActWait.Observe(started.Sub(queued))
		mActRun.Observe(time.Since(started))
		if done != nil {
			done()
		}
	})
}

// actStats: sends of this instance, logged once a burst is over.
var actStats = &sendStats{}

type sendStats struct {
	mu          sync.Mutex
	n           int
	wait, run   []time.Duration
	last        time.Time
	reportArmed bool
}

func (s *sendStats) add(wait, run time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	s.wait, s.run, s.last = append(s.wait, wait), append(s.run, run), time.Now()
	if !s.reportArmed {
		s.reportArmed = true
		go s.report()
	}
}

func (s *sendStats) report() {
	for {
		time.Sleep(time.Second)
		s.mu.Lock()
		if time.Since(s.last) < 2*time.Second {
			s.mu.Unlock()
			continue
		}
		n, wait, run := s.n, s.wait, s.run
		s.n, s.wait, s.run, s.reportArmed = 0, nil, nil, false
		s.mu.Unlock()
		slog.Info("activation sends done", "sends", n, "queue_p50_ms", pct(wait, 50), "queue_p95_ms", pct(wait, 95),
			"send_p50_ms", pct(run, 50), "send_p95_ms", pct(run, 95))
		return
	}
}

func pct(d []time.Duration, p int) int64 {
	if len(d) == 0 {
		return 0
	}
	c := append([]time.Duration(nil), d...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	return c[(len(c)-1)*p/100].Milliseconds()
}

// actWorker: this instance's share of the sends (every instance).
func (e *engine) actWorker(vk *redis.Client) {
	ctx := context.Background()
	if err := vk.XGroupCreateMkStream(ctx, actStream, actGroup, "$").Err(); err != nil && !strings.HasPrefix(err.Error(), "BUSYGROUP") {
		e.log.Error("act-jobs group", "err", err)
	}
	me := e.coord.id
	ack := func(id string) func() {
		return func() { vk.XAck(context.Background(), actStream, actGroup, id) }
	}
	run := func(msgs []redis.XMessage) {
		for _, m := range msgs {
			d, _ := m.Values["d"].(string)
			if d == "" {
				vk.XAck(ctx, actStream, actGroup, m.ID)
				continue
			}
			r, _ := m.Values["r"].(string)
			e.actRun(d, r == "1", ack(m.ID))
		}
	}
	nextClaim := time.Now()
	for {
		if time.Now().After(nextClaim) { // jobs of an instance that died
			if msgs, _, err := vk.XAutoClaim(ctx, &redis.XAutoClaimArgs{Stream: actStream, Group: actGroup,
				Consumer: me, MinIdle: 15 * time.Second, Start: "0-0", Count: 1000}).Result(); err == nil && len(msgs) > 0 {
				e.log.Info("activation jobs taken over", "jobs", len(msgs))
				run(msgs)
			}
			nextClaim = time.Now().Add(5 * time.Second)
		}
		res, err := vk.XReadGroup(ctx, &redis.XReadGroupArgs{Group: actGroup, Consumer: me,
			// small portions: every instance takes its share of a stage (with
			// 1000 the first reader took nearly all of it: 1888 of 2000)
			Streams: []string{actStream, ">"}, Count: 64, Block: time.Second}).Result()
		if err != nil {
			if err != redis.Nil {
				if strings.HasPrefix(err.Error(), "NOGROUP") {
					_ = vk.XGroupCreateMkStream(ctx, actStream, actGroup, "$").Err()
				}
				time.Sleep(time.Second)
			}
			continue
		}
		for _, s := range res {
			run(s.Messages)
		}
	}
}
