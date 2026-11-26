// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

// Projector: domain_events -> operator tables.
//
//	device_live        one row per device: online, CP, charging, limit, power, EV need
//	session_summaries  one row per plug-in (session.started .. session.ended)
//	device_attributes  recalculated after each plug-in
//
// Reads the feed by seq from its own position (projector_state), one
// transaction per batch: the tables and the position move together, so a
// restart repeats nothing and loses nothing. Order (ADR-001): a device's
// events are matched by dseq (a plug-in owns the dseq range from its
// session.started to its session.ended; since ADR-003 by its plug-in id) and
// device state is last-writer-by-time (the server's), so arrival order does
// not matter.
//
// seq is given at insert; a transaction committing late leaves a hole for a
// moment. The projector waits for a hole up to PROJECTOR_HOLE_WAIT_MS
// (2000) before it moves past it (a rolled-back insert never fills it).
//
// One active projector: a Postgres advisory lock; a second instance waits.
//
//	projector -rebuild   empty the tables and replay the feed
package main

import (
	"github.com/chargetronic/imatra.iso15118_server/engine/internal/metrics"
	"github.com/chargetronic/imatra.iso15118_server/engine/internal/store"

	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const name = "operator"

type ev struct {
	Seq      int64
	ID       string
	Type     string
	Device   sql.NullString
	Session  sql.NullString
	DSeq     sql.NullInt64
	Time     time.Time
	Data     map[string]any
	Recorded time.Time
	skip     bool                // replaced by a newer state report in the same batch
	ended    map[string]bool     // set by applyPart: devices whose plug-in ended
	replay   bool                // an orphan applied again when its plug-in appeared
	live     map[string]*liveRow // device_live rows of the part, in memory (live.go)
}

type projector struct {
	active   atomic.Bool // holds the advisory lock (the others wait)
	log      *slog.Logger
	db       *sql.DB
	batch    int
	workers  int
	holeWait time.Duration
	applied  atomic.Int64
	lagMs    atomic.Int64

	mu    sync.Mutex
	dirty map[string]bool // devices whose attributes wait for recalculation
	busy  atomic.Int64    // ... being recalculated now
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envI(k string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return n
	}
	return def
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		r, err := (&http.Client{Timeout: 3 * time.Second}).Get("http://127.0.0.1:8092/health")
		if err != nil || r.StatusCode != 200 {
			os.Exit(1)
		}
		os.Exit(0)
	}
	rebuild := flag.Bool("rebuild", false, "empty the operator tables and replay the feed")
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	dsn := fmt.Sprintf("host=%s port=%s dbname=%s user=%s password=%s sslmode=disable connect_timeout=5",
		env("POSTGRES_HOST", "postgres"), env("POSTGRES_PORT", "5432"), os.Getenv("POSTGRES_DB"),
		os.Getenv("POSTGRES_USER"), os.Getenv("POSTGRES_PASSWORD"))
	// Commit without waiting for the disk: the tables are rebuilt from the
	// feed (projector -rebuild), and a crash of Postgres only repeats the
	// last fraction of a second (every apply is guarded by last_seq).
	db, err := sql.Open("postgres", dsn+" options='-c synchronous_commit=off'")
	if err != nil {
		log.Error("database", "err", err)
		os.Exit(1)
	}
	db.SetMaxOpenConns(envI("PROJECTOR_WORKERS", 4) + 3) // parts, the lock, the cursor, attributes
	p := &projector{log: log, db: db, batch: envI("PROJECTOR_BATCH", 2000), workers: max(1, envI("PROJECTOR_WORKERS", 4)),
		holeWait: time.Duration(envI("PROJECTOR_HOLE_WAIT_MS", 2000)) * time.Millisecond, dirty: map[string]bool{}}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// Health and metrics also on a standby instance (it waits for the lock;
	// its liveness must not fail meanwhile).
	go p.serveHealth()
	if err := store.WaitSchema(ctx, db, log); err != nil {
		return
	}
	// One active projector: hold an advisory lock on a dedicated connection.
	var lock *sql.Conn
	for i := 0; ctx.Err() == nil; i++ {
		if lock, err = db.Conn(ctx); err == nil {
			var ok bool
			if err = lock.QueryRowContext(ctx, `SELECT pg_try_advisory_lock(15119)`).Scan(&ok); err == nil && ok {
				break
			}
			lock.Close()
		}
		if i%30 == 0 {
			log.Info("waiting: database not reachable or another projector is active", "err", err)
		}
		time.Sleep(2 * time.Second)
	}
	if ctx.Err() != nil {
		return
	}
	defer lock.Close()
	p.active.Store(true)
	go p.attributesLoop(ctx)
	if *rebuild {
		if err := p.reset(ctx); err != nil {
			log.Error("rebuild", "err", err)
			os.Exit(1)
		}
		log.Info("rebuild: tables emptied, replaying the feed")
	}
	p.run(ctx, *rebuild)
}

func (p *projector) reset(ctx context.Context) error {
	_, err := p.db.ExecContext(ctx, `
		TRUNCATE device_live, session_summaries, projector_orphans;
		UPDATE device_attributes SET firmware = NULL, connectivity = NULL, attrs = '{}', updated_at = now();
		DELETE FROM device_attributes;
		INSERT INTO projector_state (name, cursor) VALUES ('operator', 0)
		  ON CONFLICT (name) DO UPDATE SET cursor = 0, as_of = NULL, updated_at = now()`)
	return err
}

func (p *projector) run(ctx context.Context, exitWhenDone bool) {
	var holeSince time.Time
	for ctx.Err() == nil {
		n, hole, err := p.step(ctx, &holeSince)
		if err != nil {
			p.log.Error("apply batch", "err", err)
			time.Sleep(2 * time.Second)
			continue
		}
		if n == 0 {
			if exitWhenDone && !hole {
				p.log.Info("rebuild done", "events", p.applied.Load())
				exitWhenDone = false // keep following the feed
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
}

// step applies the next batch; returns how many events, and whether it is
// waiting at a hole.
func (p *projector) step(ctx context.Context, holeSince *time.Time) (int, bool, error) {
	var cursor int64
	if err := p.db.QueryRowContext(ctx, `SELECT cursor FROM projector_state WHERE name = $1`, name).Scan(&cursor); err == sql.ErrNoRows {
		if _, err := p.db.ExecContext(ctx, `INSERT INTO projector_state (name) VALUES ($1) ON CONFLICT DO NOTHING`, name); err != nil {
			return 0, false, err
		}
	} else if err != nil {
		return 0, false, err
	}
	rows, err := p.db.QueryContext(ctx, `
		SELECT seq, id, type, device_id, session_id, dseq, time, data, recorded_at
		FROM domain_events WHERE seq > $1 ORDER BY seq LIMIT $2`, cursor, p.batch)
	if err != nil {
		return 0, false, err
	}
	var batch []ev
	for rows.Next() {
		var e ev
		var raw []byte
		if err := rows.Scan(&e.Seq, &e.ID, &e.Type, &e.Device, &e.Session, &e.DSeq, &e.Time, &raw, &e.Recorded); err != nil {
			rows.Close()
			return 0, false, err
		}
		_ = json.Unmarshal(raw, &e.Data)
		batch = append(batch, e)
	}
	rows.Close()

	// The part of the batch without holes (seq is given at insert: a hole is
	// an insert not committed yet, or one rolled back).
	n, hole, next := 0, false, cursor
	for _, e := range batch {
		if e.Seq != next+1 {
			if holeSince.IsZero() {
				*holeSince = time.Now()
			}
			if time.Since(*holeSince) < p.holeWait {
				hole = true
				break
			}
			p.log.Info("feed hole skipped", "after", next, "next", e.Seq)
		}
		*holeSince = time.Time{}
		next = e.Seq
		n++
	}
	if n == 0 {
		return 0, hole, nil
	}
	batch = batch[:n]

	// Device state (CP, power, presence) is last-writer-by-time (ADR-003: the server's): of a
	// device's reports of one kind in this batch only the newest can win.
	newest := map[string]int{}
	for i, e := range batch {
		if canSkip(&e) {
			k := e.Type + "|" + e.Device.String
			if j, ok := newest[k]; !ok || !batch[j].Time.After(e.Time) {
				newest[k] = i
			}
		}
	}
	// Devices are independent: the batch is split by device over
	// PROJECTOR_WORKERS transactions in parallel. Each device keeps the seq of
	// its last applied event (device_live.last_seq), so a part committed
	// before a crash is not applied twice when the batch is read again.
	parts := make([][]*ev, p.workers)
	var global []*ev // no device: system.events_gap
	for i := range batch {
		e := &batch[i]
		if !e.Device.Valid {
			global = append(global, e)
			continue
		}
		if canSkip(e) && newest[e.Type+"|"+e.Device.String] != i {
			e.skip = true
		}
		h := fnv.New32a()
		h.Write([]byte(e.Device.String))
		w := int(h.Sum32()) % p.workers
		parts[w] = append(parts[w], e)
	}
	errs := make(chan error, p.workers)
	for _, part := range parts {
		go func(part []*ev) { errs <- p.applyPart(ctx, part) }(part)
	}
	for range parts {
		if err := <-errs; err != nil {
			return 0, false, err
		}
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()
	for _, e := range global {
		if err := p.apply(ctx, tx, e); err != nil {
			return 0, false, fmt.Errorf("event %d (%s): %w", e.Seq, e.Type, err)
		}
	}
	last := batch[n-1]
	lag := time.Since(last.Recorded).Milliseconds()
	if _, err := tx.ExecContext(ctx, `UPDATE projector_state SET cursor = $2, as_of = $3, lag_ms = $4, updated_at = now() WHERE name = $1`,
		name, next, last.Recorded, lag); err != nil {
		return 0, false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	p.applied.Add(int64(n))
	p.lagMs.Store(lag)
	return n, hole, nil
}

// applyPart: the events of some devices, in feed order, in one transaction.
func (p *projector) applyPart(ctx context.Context, part []*ev) error {
	if len(part) == 0 {
		return nil
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	devs := []string{}
	seen := map[string]bool{}
	for _, e := range part {
		if !seen[e.Device.String] {
			seen[e.Device.String] = true
			devs = append(devs, e.Device.String)
		}
	}
	// rows exist, locked, in memory: how far each device got and its state
	live, err := loadLive(ctx, tx, devs)
	if err != nil {
		return err
	}
	done := map[string]int64{}
	for d, r := range live {
		done[d] = r.lastSeq
	}
	lastSeq, asOf, ended := map[string]int64{}, map[string]time.Time{}, map[string]bool{}
	for _, e := range part {
		e.ended, e.live = ended, live
		dev := e.Device.String
		if e.Seq <= done[dev] {
			continue // applied before a restart
		}
		if !e.skip {
			if err := p.apply(ctx, tx, e); err != nil {
				return fmt.Errorf("event %d (%s): %w", e.Seq, e.Type, err)
			}
		}
		lastSeq[dev] = e.Seq
		if e.Recorded.After(asOf[dev]) {
			asOf[dev] = e.Recorded
		}
	}
	touched := map[string]bool{}
	for d, q := range lastSeq {
		if r := live[d]; r != nil {
			r.lastSeq, r.asOf, touched[d] = q, micros(asOf[d]), true
		}
	}
	if err := saveLive(ctx, tx, live, touched); err != nil { // one statement: every changed row once
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// Only now: the attributes read the committed summaries.
	p.mu.Lock()
	for d := range ended {
		p.dirty[d] = true
	}
	p.mu.Unlock()
	return nil
}

func str(d map[string]any, k string) any {
	if v, ok := d[k].(string); ok && v != "" {
		return v
	}
	return nil
}

func num(d map[string]any, k string) any {
	if v, ok := d[k].(float64); ok {
		return v
	}
	return nil
}

// slack: events without a plug-in id (a device before 0.3.2) belong to
// the plug-in from slack before its start to slack after its end. Instances
// handle a device's events in their own order (ADR-001): the EV's first V2G
// request can be handled before the CP report of its plug-in (09.11.2026).
// With the id (ADR-003) no window is used.
const slack = "5 seconds"
const plugSlack = 5 * time.Second // the same, for Go

// plugIn: the summary of the plug-in an event of device dev belongs to; ""
// when it does not exist yet (the event waits as an orphan).
//
//   - plug id given: the summary with that id. None, and the device's open
//     plug-in has an id of another boot (the device's software restarted
//     with the EV plugged in, or was updated): that plug-in takes the new id.
//   - no id: the time window (slack).
func (p *projector) plugIn(ctx context.Context, tx *sql.Tx, dev, plug string, t time.Time) (string, error) {
	var id string
	if plug == "" {
		err := tx.QueryRowContext(ctx, `SELECT summary_id FROM session_summaries WHERE device_id = $1
			AND started_at <= $2::timestamptz + interval '`+slack+`'
			AND (ended_at IS NULL OR ended_at >= $2::timestamptz - interval '`+slack+`')
			ORDER BY started_at DESC LIMIT 1`, dev, t).Scan(&id)
		if err == sql.ErrNoRows {
			return "", nil
		}
		return id, err
	}
	err := tx.QueryRowContext(ctx, `SELECT summary_id FROM session_summaries WHERE device_id = $1 AND plug_id = $2`, dev, plug).Scan(&id)
	if err != sql.ErrNoRows {
		return id, err
	}
	var had string
	err = tx.QueryRowContext(ctx, `SELECT summary_id, COALESCE(plug_id, '') FROM session_summaries
		WHERE device_id = $1 AND ended_at IS NULL ORDER BY started_at DESC LIMIT 1`, dev).Scan(&id, &had)
	if err == sql.ErrNoRows || (err == nil && plugBoot(had) == plugBoot(plug)) {
		return "", nil // same boot: a plug-in not seen yet
	}
	if err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `UPDATE session_summaries SET plug_id = $2 WHERE summary_id = $1`, id, plug)
	return id, err
}

// plugBoot: the boot part of a plug-in id "<boot>-<n>".
func plugBoot(plug string) string {
	if i := strings.LastIndexByte(plug, '-'); i >= 0 {
		return plug[:i]
	}
	return plug
}

// plugScoped: events that belong to a plug-in; one that arrives before its
// plug-in exists waits (projector_orphans) and is applied when it appears.
var plugScoped = map[string]bool{"v2g.started": true, "ev.parameters": true, "charging.started": true,
	"charging.stopped": true, "limit.applied": true}

// orphan: keep the event until its plug-in appears (adopt).
func orphan(ctx context.Context, tx *sql.Tx, e *ev, plug string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO projector_orphans (device_id, seq, time, plug) VALUES ($1, $2, $3, NULLIF($4, ''))
		ON CONFLICT DO NOTHING`, e.Device.String, e.Seq, e.Time, plug)
	return err
}

// apply one event inside the batch transaction.
func (p *projector) apply(ctx context.Context, tx *sql.Tx, e *ev) error {
	if e.Type == "system.events_gap" {
		// Plug-ins overlapping the interval: their totals may miss events.
		from, to := str(e.Data, "from"), str(e.Data, "to")
		_, err := tx.ExecContext(ctx, `
			UPDATE session_summaries SET data_gap = true, updated_at = now()
			WHERE started_at <= $2::timestamptz AND (ended_at IS NULL OR ended_at >= $1::timestamptz)`, from, to)
		if err == nil { // device presence from the devices table, which the engines keep directly
			_, err = tx.ExecContext(ctx, `
				UPDATE device_live l SET online = d.online, online_time = now(), updated_at = now()
				FROM devices d WHERE d.device_id = l.device_id`)
		}
		return err
	}
	if !e.Device.Valid {
		return nil
	}
	dev, dseq, t := e.Device.String, e.DSeq.Int64, e.Time // device_live row exists (applyPart)
	ex := func(q string, args ...any) error {
		_, err := tx.ExecContext(ctx, q, args...)
		return err
	}
	plug, _ := e.Data["plug"].(string) // ADR-003; "" from a device before 0.3.2
	// summary: update the plug-in of the event; none yet -> the event waits.
	summary := func(set string, args ...any) error {
		id, err := p.plugIn(ctx, tx, dev, plug, t)
		if err != nil {
			return err
		}
		if id == "" && plug != "" && !e.replay {
			if id, err = p.implicitStart(ctx, tx, e, plug); err != nil {
				return err
			}
		}
		if id == "" {
			if plugScoped[e.Type] && !e.replay {
				return orphan(ctx, tx, e, plug)
			}
			return nil
		}
		// $2 (the event time) is typed here too: not every set uses it
		return ex(`UPDATE session_summaries SET `+set+`, updated_at = now() WHERE summary_id = $1 AND $2::timestamptz IS NOT NULL`,
			append([]any{id, t}, args...)...)
	}

	if err := p.digital(ctx, tx, e); err != nil {
		return err
	}
	if r := e.live[dev]; r != nil {
		r.applyLive(e) // device_live: in memory, written once per part (live.go)
	}
	switch e.Type {
	case "device.online", "device.offline":
		if e.Type == "device.online" {
			fw, link := str(e.Data, "fw"), str(e.Data, "link")
			if fw == nil {
				fw = str(e.Data, "firmware")
			}
			return ex(`INSERT INTO device_attributes (device_id, firmware, connectivity) VALUES ($1, $2, $3)
				ON CONFLICT (device_id) DO UPDATE SET
				  firmware = COALESCE($2, device_attributes.firmware), connectivity = COALESCE($3, device_attributes.connectivity),
				  updated_at = now()`, dev, fw, link)
		}
	case "cp.changed":
		cp := str(e.Data, "ev")
		mode, _ := e.Data["mode"].(string)
		pwmSide := mode == "basic" || mode == "transit"
		// Charging on the station PWM (no digital session, or a DIN SoC read
		// handed over to it): the CP is the only sign; C charges, B/A not.
		set := `charging_since = CASE WHEN NOT (mode IS NULL OR mode LIKE '%pwm' OR $4) THEN charging_since
			  WHEN $3 = 'C' THEN COALESCE(charging_since, $2) ELSE NULL END,
			charging_s = charging_s + CASE WHEN (mode IS NULL OR mode LIKE '%pwm' OR $4) AND $3 <> 'C' AND charging_since IS NOT NULL
			  THEN GREATEST(0, extract(epoch FROM $2::timestamptz - charging_since)::int) ELSE 0 END`
		if pwmSide {
			// a DIN SoC read handed over to the station PWM: the plug-in goes on in basic charging
			set += `, mode = CASE WHEN mode IS NULL THEN NULL WHEN mode NOT LIKE '%pwm' THEN mode || ' -> pwm' ELSE mode END`
		}
		return summary(set, cp, pwmSide)
	case "power.changed":
		return nil
	case "session.started":
		// The plug-in opened already by an event of its id (implicitStart,
		// issue #15): this start joins it, the start moves back.
		sid := e.ID
		if plug != "" {
			err := tx.QueryRowContext(ctx, `UPDATE session_summaries SET started_at = LEAST(started_at, $3::timestamptz),
				  start_dseq = LEAST(start_dseq, $4), implicit_start = false,
				  charging_since = CASE WHEN $5 = 'C' AND (charging_since IS NULL OR charging_since > $3::timestamptz) AND ended_at IS NULL
				    THEN $3::timestamptz ELSE charging_since END, updated_at = now()
				WHERE device_id = $1 AND plug_id = $2 RETURNING summary_id`, dev, plug, t, dseq, str(e.Data, "cp")).Scan(&sid)
			if err == sql.ErrNoRows {
				sid = e.ID
			} else if err != nil {
				return err
			}
		}
		if sid == e.ID {
			// EV plugged in already in C (basic charging at once): charging from the start
			if err := ex(`INSERT INTO session_summaries (summary_id, device_id, start_dseq, started_at, charging_since, plug_id)
				VALUES ($1, $2, $3, $4::timestamptz, CASE WHEN $5 = 'C' THEN $4::timestamptz END, NULLIF($6, ''))
				ON CONFLICT (summary_id) DO NOTHING`, e.ID, dev, dseq, t, str(e.Data, "cp"), plug); err != nil {
				return err
			}
		}
		// an older plug-in that never got its end: closed by this one;
		// "superseded" when the device said so (its id changed without an
		// unplug, issue #15)
		why := "no unplug seen"
		if str(e.Data, "supersedes") != nil {
			why = "superseded"
		}
		if err := ex(`UPDATE session_summaries SET ended_at = COALESCE(ended_at, $3), end_reason = COALESCE(end_reason, $4),
			  updated_at = now() WHERE device_id = $1 AND started_at < $3 AND ended_at IS NULL AND summary_id <> $2`, dev, sid, t, why); err != nil {
			return err
		}
		return p.adopt(ctx, tx, dev, plug, t, e.live)
	case "session.ended":
		// The end: the arrival, or the start plus the plug-in's duration by
		// the device's own count (plugged_ms), whichever is earlier; the start
		// moves back to the end minus the duration if that is earlier. So a
		// plug-in or unplug report delivered late (the device was offline)
		// keeps its real span (ADR-003).
		const end = `UPDATE session_summaries s SET end_dseq = $3, ended_at = x.e, started_at = LEAST(s.started_at, x.e - x.d),
			  energy_wh = $4, energy_quality = $5,
			  duration_s = COALESCE($6, extract(epoch FROM x.e - LEAST(s.started_at, x.e - x.d))::int), end_reason = $7, mode = COALESCE(mode, 'pwm'),
			  charging_s = charging_s + CASE WHEN charging_since IS NULL THEN 0 ELSE GREATEST(0, extract(epoch FROM x.e - charging_since)::int) END,
			  charging_since = NULL, updated_at = now()
			FROM (SELECT summary_id,
			        CASE WHEN $8::bigint IS NULL THEN $2::timestamptz ELSE LEAST($2::timestamptz, started_at + $8::bigint * interval '1 millisecond') END AS e,
			        COALESCE($8::bigint, 0) * interval '1 millisecond' AS d
			      FROM session_summaries WHERE `
		args := []any{dev, t, dseq, num(e.Data, "energy_wh"), str(e.Data, "energy_quality"), num(e.Data, "duration_s"),
			str(e.Data, "reason"), num(e.Data, "plugged_ms")}
		if plug == "" {
			if err := ex(end+`device_id = $1 AND ended_at IS NULL
			  AND started_at <= $2::timestamptz + interval '`+slack+`' ORDER BY started_at DESC LIMIT 1) x
			WHERE s.summary_id = x.summary_id`, args...); err != nil {
				return err
			}
		} else {
			id, err := p.plugIn(ctx, tx, dev, plug, t)
			if err != nil {
				return err
			}
			if id == "" && !e.replay { // its session.started not seen: opened now (issue #15)
				if id, err = p.implicitStart(ctx, tx, e, plug); err != nil {
					return err
				}
			}
			if id == "" { // the unplug handled before its plug-in: waits for it
				if !e.replay {
					return orphan(ctx, tx, e, plug)
				}
				return nil
			}
			args[0] = id
			if err := ex(end+`summary_id = $1) x WHERE s.summary_id = x.summary_id`, args...); err != nil {
				return err
			}
		}
		if e.ended != nil {
			// attributes: recalculated in the background once this commits
			e.ended[dev] = true
		}
		return nil
	case "v2g.started":
		proto, _ := e.Data["protocol"].(string)
		if err := summary(`v2g_sessions = v2g_sessions + 1,
			  mode = CASE WHEN mode IS NULL THEN $3 WHEN mode LIKE '%' || $3 THEN mode ELSE mode || ' -> ' || $3 END`, proto); err != nil {
			return err
		}
		return nil
	case "v2g.ended":
		return nil
	case "ev.parameters":
		ea, soc, dep := num(e.Data, "e_amount_wh"), num(e.Data, "soc"), num(e.Data, "departure_time_s")
		if err := summary(`has_eamount = has_eamount OR $3::numeric IS NOT NULL,
			  has_soc = has_soc OR $4::numeric IS NOT NULL, e_amount_wh = COALESCE($3, e_amount_wh),
			  soc_start = COALESCE(soc_start, $4), departure_time_s = COALESCE($5, departure_time_s)`, ea, soc, dep); err != nil {
			return err
		}
		return nil
	case "charging.started":
		if err := summary(`charging_since = COALESCE(charging_since, $2)`); err != nil {
			return err
		}
		return nil
	case "charging.stopped":
		if err := summary(`charging_s = charging_s + CASE WHEN charging_since IS NULL THEN 0
			  ELSE GREATEST(0, extract(epoch FROM $2::timestamptz - charging_since)::int) END, charging_since = NULL`); err != nil {
			return err
		}
		return nil
	case "limit.changed":
		return nil
	case "limit.applied":
		return summary(`limits_applied = limits_applied + 1`)
	}
	return nil
}

// implicitStart (issue #15): an event of a plug-in id that no summary has and
// no open plug-in takes (plugIn) opens that plug-in at the time of the event,
// marked implicit_start. Its session.started was never emitted (the engine of
// v0.1.0 took a "plugged" with a new id for a repeat) or comes later
// (it joins the summary then). A device has one EV connector: the plug-in
// still open that started before ends here, "superseded"; one that started
// after (this event was handled late) ends this one at its start. Events
// waiting for this id are applied to it.
func (p *projector) implicitStart(ctx context.Context, tx *sql.Tx, e *ev, plug string) (string, error) {
	dev, t := e.Device.String, e.Time
	if _, err := tx.ExecContext(ctx, `INSERT INTO session_summaries (summary_id, device_id, start_dseq, started_at, plug_id, implicit_start)
		VALUES ($1, $2, $3, $4, $5, true) ON CONFLICT (summary_id) DO NOTHING`, e.ID, dev, e.DSeq.Int64, t, plug); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE session_summaries SET ended_at = $3, end_reason = 'superseded', updated_at = now()
		WHERE device_id = $1 AND summary_id <> $2 AND ended_at IS NULL AND started_at <= $3`, dev, e.ID, t); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE session_summaries s SET ended_at = n.next, end_reason = 'superseded', updated_at = now()
		FROM (SELECT min(started_at) AS next FROM session_summaries WHERE device_id = $1 AND summary_id <> $2 AND started_at > $3) n
		WHERE s.summary_id = $2 AND s.ended_at IS NULL AND n.next IS NOT NULL`, dev, e.ID, t); err != nil {
		return "", err
	}
	p.log.Warn("plug-in opened without its session.started", "device", dev, "plug", plug, "seq", e.Seq, "type", e.Type)
	return e.ID, p.adopt(ctx, tx, dev, plug, t, e.live)
}

// adopt: events of the device that came before their plug-in existed now
// belong to it: those with its plug-in id, or, without ids, those in its
// window (applied to its summary only).
func (p *projector) adopt(ctx context.Context, tx *sql.Tx, dev, plug string, started time.Time, live map[string]*liveRow) error {
	q, arg := `o.plug = $2`, any(plug)
	if plug == "" {
		q, arg = `o.plug IS NULL AND o.time >= $2::timestamptz - interval '`+slack+`'`, started
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT e.seq, e.id, e.type, e.device_id, e.session_id, e.dseq, e.time, e.data, e.recorded_at
		FROM projector_orphans o JOIN domain_events e ON e.seq = o.seq
		WHERE o.device_id = $1 AND `+q+` ORDER BY e.seq`, dev, arg)
	if err != nil {
		return err
	}
	var list []ev
	for rows.Next() {
		var e ev
		var raw []byte
		if err := rows.Scan(&e.Seq, &e.ID, &e.Type, &e.Device, &e.Session, &e.DSeq, &e.Time, &raw, &e.Recorded); err != nil {
			rows.Close()
			return err
		}
		_ = json.Unmarshal(raw, &e.Data)
		e.replay, e.live = true, live
		list = append(list, e)
	}
	rows.Close()
	for i := range list {
		e := &list[i]
		// summary and device_live (the plug-in's start reset the live EV fields)
		if err := p.apply(ctx, tx, e); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM projector_orphans WHERE seq = $1`, e.Seq); err != nil {
			return err
		}
	}
	// orphans no plug-in will take
	_, err = tx.ExecContext(ctx, `DELETE FROM projector_orphans WHERE device_id = $1 AND time < now() - interval '1 hour'`, dev)
	return err
}

// cp.changed is not collapsed: the CP transitions give the charging time of
// basic (PWM) charging.
var collapsible = map[string]bool{"power.changed": true, "device.online": true, "device.offline": true}

// canSkip: a state report that a newer one in the batch replaces.
func canSkip(e *ev) bool { return collapsible[e.Type] && e.Device.Valid }

// attributesLoop recalculates the attributes of devices whose plug-in ended,
// off the feed's path (they need not be seconds fresh). After a restart the
// next plug-in of a device recalculates it; -rebuild does all.
func (p *projector) attributesLoop(ctx context.Context) {
	for ctx.Err() == nil {
		time.Sleep(5 * time.Second)
		p.mu.Lock()
		devs := p.dirty
		p.dirty = map[string]bool{}
		p.busy.Store(int64(len(devs)))
		p.mu.Unlock()
		for dev := range devs {
			tx, err := p.db.BeginTx(ctx, nil)
			if err != nil {
				break
			}
			if err := p.attributes(ctx, tx, dev); err != nil {
				p.log.Warn("attributes", "device", dev, "err", err)
				tx.Rollback()
				continue
			}
			tx.Commit()
			p.busy.Add(-1)
		}
		p.busy.Store(0)
	}
}

// attributes: recalculated after each plug-in.
func (p *projector) attributes(ctx context.Context, tx *sql.Tx, dev string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO device_attributes (device_id, attrs) VALUES ($1, '{}')
		ON CONFLICT (device_id) DO NOTHING`, dev)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		WITH r AS (
		  SELECT percentile_disc(0.5) WITHIN GROUP (ORDER BY reaction_ms) AS p50,
		         percentile_disc(0.95) WITHIN GROUP (ORDER BY reaction_ms) AS p95, count(*) AS n
		  FROM limits WHERE device_id = $1 AND acked_at > now() - interval '30 days'),
		s AS (
		  SELECT count(*) AS n, count(*) FILTER (WHERE has_eamount) AS eamount, count(*) FILTER (WHERE has_soc) AS soc,
		         (array_agg(mode ORDER BY start_dseq DESC))[1] AS last_mode
		  FROM session_summaries WHERE device_id = $1 AND ended_at > now() - interval '30 days')
		UPDATE device_attributes a SET attrs = (a.attrs - 'reaction_ms' - 'sessions_30d' - 'ev_eamount_ratio' - 'ev_soc_ratio' - 'last_mode')
		  || jsonb_strip_nulls(jsonb_build_object(
		       'reaction_ms', CASE WHEN r.n > 0 THEN jsonb_build_object('p50', r.p50, 'p95', r.p95, 'n', r.n) END,
		       'sessions_30d', s.n,
		       'ev_eamount_ratio', CASE WHEN s.n > 0 THEN round(s.eamount::numeric / s.n, 2) END,
		       'ev_soc_ratio', CASE WHEN s.n > 0 THEN round(s.soc::numeric / s.n, 2) END,
		       'last_mode', s.last_mode)),
		  updated_at = now()
		FROM r, s WHERE a.device_id = $1`, dev)
	return err
}

func (p *projector) serveHealth() {
	reg := &metrics.Registry{}
	reg.Process()
	reg.CounterFunc("projector_applied_total", "Feed events applied to the operator tables.", func() float64 { return float64(p.applied.Load()) })
	reg.Gauge("projector_active", "1 on the instance that holds the lock (the others wait).", func() float64 {
		if p.active.Load() {
			return 1
		}
		return 0
	})
	reg.Gauge("projector_lag_seconds", "How far the operator tables trail the feed (last batch).", func() float64 {
		return float64(p.lagMs.Load()) / 1000
	})
	reg.Gauge("projector_attributes_pending", "Devices whose attributes wait for recalculation.", func() float64 {
		p.mu.Lock()
		defer p.mu.Unlock()
		return float64(len(p.dirty) + int(p.busy.Load()))
	})
	http.Handle("/metrics", reg.Handler())
	http.HandleFunc("/live", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	http.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		p.mu.Lock()
		dirty := len(p.dirty) + int(p.busy.Load())
		p.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"applied": p.applied.Load(), "lag_ms": p.lagMs.Load(),
			"attributes_pending": dirty})
	})
	_ = (&http.Server{Addr: env("PROJECTOR_HEALTH_ADDR", ":8092"), ReadHeaderTimeout: 5 * time.Second}).ListenAndServe()
}

// digital: the sessions table (one row per digital V2G session) from the
// feed (ADR-002): the engines keep the state of a running session in Valkey
// only and write nothing of it to Postgres. Idempotent: an event applied
// again (orphan replay, rebuild) changes nothing.
func (p *projector) digital(ctx context.Context, tx *sql.Tx, e *ev) error {
	dev, t := e.Device.String, e.Time
	ex := func(q string, args ...any) error {
		_, err := tx.ExecContext(ctx, q, args...)
		return err
	}
	// plug-in boundaries: a digital session still open is over
	closeOpen := func(why string, before time.Time) error {
		return ex(`UPDATE sessions SET ended_at = $2, end_reason = COALESCE(end_reason, $3), phase = 'Ended', updated_at = now()
			WHERE device_id = $1 AND ended_at IS NULL AND started_at <= $4`, dev, t, why, before)
	}
	switch e.Type {
	case "session.started":
		if str(e.Data, "supersedes") != nil {
			return nil // the plug-in id changed, the EV was not unplugged: its V2G session goes on (issue #15)
		}
		// not a session of this plug-in whose first request came before
		// the CP report (ADR-001: within the slack)
		return closeOpen("no end seen", t.Add(-plugSlack))
	case "session.ended":
		return closeOpen("EV unplugged", t)
	}
	sid := e.Session.String
	if !e.Session.Valid || sid == "" {
		return nil
	}
	set := func(cols string, args ...any) error {
		if err := ex(`INSERT INTO sessions (session_id, device_id, started_at) VALUES ($1::uuid, $2, $3)
			ON CONFLICT (session_id) DO NOTHING`, sid, dev, t); err != nil {
			return err
		}
		return ex(`UPDATE sessions SET `+cols+`, updated_at = now() WHERE session_id = $1::uuid`, append([]any{sid}, args...)...)
	}
	switch e.Type {
	case "v2g.started":
		proto, mode := "", ""
		switch str(e.Data, "protocol") {
		case "iso15118-2":
			proto, mode = "urn:iso:15118:2:2013:MsgDef", "iso15118-2-ac"
		case "din70121":
			proto, mode = "urn:din:70121:2012:MsgDef", "din70121-soc"
		}
		evcc, _ := str(e.Data, "evcc_id").(string)
		return set(`protocol = COALESCE(protocol, NULLIF($2, '')), mode = COALESCE(mode, NULLIF($3, '')),
			evcc_id = COALESCE(evcc_id, NULLIF($4, '')), started_at = LEAST(started_at, $5),
			phase = CASE WHEN phase IS NULL OR phase LIKE 'Wait%' THEN 'WaitServiceDiscovery' ELSE phase END`,
			proto, mode, evcc, t)
	case "ev.parameters":
		raw, _ := json.Marshal(e.Data)
		return set(`ev_data = COALESCE(ev_data, '{}'::jsonb) || $2::jsonb, ev_soc_start = COALESCE(ev_soc_start, $3)`,
			string(raw), num(e.Data, "soc"))
	case "charging.started":
		return set(`phase = CASE WHEN ended_at IS NULL THEN 'Charging' ELSE phase END`)
	case "charging.stopped":
		return set(`phase = CASE WHEN ended_at IS NULL THEN 'ChargingStopped' ELSE phase END`)
	case "v2g.ended":
		// the session's energy: background work here, not on the device's worker
		// Its energy comes with the event (the device counter in Valkey);
		// only without it (one engine on Postgres, a record lost with
		// Valkey) from the telemetry table.
		reason, _ := str(e.Data, "reason").(string)
		if wh := num(e.Data, "energy_wh"); wh != nil {
			return set(`ended_at = COALESCE(ended_at, $2), end_reason = COALESCE(end_reason, NULLIF($3, '')), phase = 'Ended',
				energy_wh = $4`, t, reason, wh)
		}
		return set(`ended_at = COALESCE(ended_at, $2), end_reason = COALESCE(end_reason, NULLIF($3, '')), phase = 'Ended',
			energy_wh = round(telemetry_energy(device_id, started_at, COALESCE(ended_at, $2) + interval '2 seconds')::numeric, 2)`,
			t, reason)
	}
	return nil
}
