// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

package store

// Activations (activations, activation_stages, activation_targets): what the leader's
// scheduler needs. Every step is one transaction, so a new leader resumes
// where the old one stopped: a stage is started (devices selected, limits
// written) or not; a target is queued for the engines or not.

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"strings"
	"time"

	"github.com/lib/pq"
)

// StageStart: a stage whose devices were just selected.
type StageStart struct {
	Activation string
	Stage      int
	Eligible   int
	Selected   int
	LimitA     float64 // per device (group levels: the share of one device)
	Priority   int
	Until      time.Time
	Devices    []string // selected, with their commands: the engine writes the
	Commands   []int64  // limit into the hot station state before the sends
	FromPool   bool     // chosen from the pool (Valkey), not in Postgres
}

type actSelection struct {
	Devices  []string `json:"devices"`
	Online   *bool    `json:"online"`
	Charging *bool    `json:"charging"`
	Mode     *string  `json:"mode"`
}

type actLevel struct {
	CurrentA     *float64 `json:"current_a"`
	PowerKW      *float64 `json:"power_kw"`
	GroupPowerKW *float64 `json:"group_power_kw"`
}

// Pick: what a stage start asks the pool for (engine: pool.go).
type Pick struct {
	Activation string
	Priority   int
	Ends       time.Time
	Charging   *bool
	Mode       *string
	Max        int // 0: all eligible
}

type Candidate struct {
	Device string
	PowerW float64
}

// Picker chooses the devices of a stage from the hot state (Valkey); nil:
// from the operator tables in Postgres.
type Picker func(ctx context.Context, p Pick) ([]Candidate, error)

const minLimitA = 6 // IEC 61851-1: below that a limit would stop the charging

// StartDueStage starts one stage whose time has come: selects the devices
// eligible now, writes their limits (the activation's priority, expiring at
// its end) and the targets with their send times over the spread, and tells
// every engine instance to drop its cached limits of those devices. nil: none due.
// phases: to turn a power level into a current (230 V per phase).
// pick: the pool (Valkey) for a selection by criteria; an explicit list of
// devices, or one that takes offline devices too, is chosen in Postgres.
func (s *Store) StartDueStage(ctx context.Context, phases int, pick Picker) (*StageStart, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var st StageStart
	var count sql.NullInt64
	var share sql.NullFloat64
	var spreadMs int
	var starts, ends time.Time
	var selRaw, lvlRaw []byte
	err = tx.QueryRowContext(ctx, `
		SELECT s.activation_id, s.stage, s.count, s.share, s.spread_ms, s.starts_at, a.priority, a.selection, a.level, a.ends_at
		FROM activation_stages s JOIN activations a USING (activation_id)
		WHERE s.state = 'scheduled' AND s.starts_at <= now()
		ORDER BY s.starts_at LIMIT 1 FOR UPDATE OF s SKIP LOCKED`).
		Scan(&st.Activation, &st.Stage, &count, &share, &spreadMs, &starts, &st.Priority, &selRaw, &lvlRaw, &ends)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var sel actSelection
	var lvl actLevel
	_ = json.Unmarshal(selRaw, &sel)
	_ = json.Unmarshal(lvlRaw, &lvl)
	var active bool
	if err := tx.QueryRowContext(ctx, `SELECT state = 'active' AND ends_at > now() FROM activations WHERE activation_id = $1`,
		st.Activation).Scan(&active); err != nil {
		return nil, err
	}
	if !active {
		_, err := tx.ExecContext(ctx, `UPDATE activation_stages SET state = 'skipped' WHERE activation_id = $1 AND stage = $2`,
			st.Activation, st.Stage)
		if err != nil {
			return nil, err
		}
		return &st, tx.Commit()
	}

	var devs []string
	var pw []sql.NullFloat64
	if pick != nil && len(sel.Devices) == 0 && (sel.Online == nil || *sel.Online) {
		st.FromPool = true
		max := 0
		if count.Valid {
			max = int(count.Int64)
		}
		c, err := pick(ctx, Pick{Activation: st.Activation, Priority: st.Priority, Ends: ends,
			Charging: sel.Charging, Mode: sel.Mode, Max: max})
		if err != nil {
			return nil, err
		}
		// a stage start that died after its commit, before the pool learnt
		// its devices: they are in the targets, not taken twice
		ids := make([]string, len(c))
		for i := range c {
			ids[i] = c[i].Device
		}
		taken := map[string]bool{}
		r, err := tx.QueryContext(ctx, `SELECT device_id FROM activation_targets WHERE activation_id = $1 AND device_id = ANY($2)`,
			st.Activation, pq.Array(ids))
		if err != nil {
			return nil, err
		}
		for r.Next() {
			var d string
			_ = r.Scan(&d)
			taken[d] = true
		}
		r.Close()
		for _, x := range c {
			if !taken[x.Device] {
				devs, pw = append(devs, x.Device), append(pw, sql.NullFloat64{Float64: x.PowerW, Valid: true})
			}
		}
	} else {
		if devs, pw, err = selectInPG(ctx, tx, st, sel); err != nil {
			return nil, err
		}
	}
	st.Eligible = len(devs)
	n := len(devs)
	if count.Valid && int(count.Int64) < n {
		n = int(count.Int64)
	} else if share.Valid {
		n = int(math.Ceil(share.Float64 * float64(len(devs))))
	}
	devs, pw = devs[:n], pw[:n]
	st.Selected = n

	if phases <= 0 {
		phases = 3
	}
	perPhase := func(kw float64) float64 { return kw * 1000 / (230 * float64(phases)) }
	switch {
	case lvl.CurrentA != nil:
		st.LimitA = *lvl.CurrentA
	case lvl.PowerKW != nil:
		st.LimitA = perPhase(*lvl.PowerKW)
	case lvl.GroupPowerKW != nil && n > 0:
		st.LimitA = perPhase(*lvl.GroupPowerKW / float64(n))
	}
	st.LimitA = math.Max(minLimitA, math.Floor(st.LimitA*10)/10)
	limitW := st.LimitA * 230 * float64(phases)

	if n > 0 {
		// limits: one row per device, expiring with the activation
		lim := make([]float64, n)
		for i := range lim {
			lim[i] = st.LimitA
		}
		r, err := tx.QueryContext(ctx, `
			INSERT INTO limits (device_id, limit_a, expires_at, source, priority, activation_id)
			SELECT d, l, $3, 'activation', $4, $5 FROM unnest($1::text[], $2::numeric[]) AS u(d, l)
			RETURNING device_id, command_id`, pq.Array(devs), pq.Array(lim), ends, st.Priority, st.Activation)
		if err != nil {
			return nil, err
		}
		cmd := map[string]int64{}
		for r.Next() {
			var d string
			var c int64
			if err := r.Scan(&d, &c); err != nil {
				r.Close()
				return nil, err
			}
			cmd[d] = c
		}
		r.Close()
		// targets: send times spread evenly over the spread, in selection order
		cmds, sendAt, pws := make([]int64, n), make([]time.Time, n), make([]sql.NullFloat64, n)
		base := time.Now()
		for i, d := range devs {
			cmds[i], pws[i] = cmd[d], pw[i]
			sendAt[i] = base.Add(time.Duration(spreadMs) * time.Millisecond * time.Duration(i) / time.Duration(n))
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO activation_targets (activation_id, device_id, stage, command_id, limit_a, power_w_before, limit_w, send_at)
			SELECT $1, d, $2, c, $3, p, $4, t FROM unnest($5::text[], $6::bigint[], $7::numeric[], $8::timestamptz[]) AS u(d, c, p, t)`,
			st.Activation, st.Stage, st.LimitA, limitW, pq.Array(devs), pq.Array(cmds), pq.Array(pws), pq.Array(sendAt)); err != nil {
			return nil, err
		}
		st.Devices, st.Commands, st.Until = devs, cmds, ends
	}
	if _, err := tx.ExecContext(ctx, `UPDATE activation_stages SET state = $3, selected = $4, started_at = now()
		WHERE activation_id = $1 AND stage = $2`, st.Activation, st.Stage, map[bool]string{true: "dispatching", false: "dispatched"}[n > 0], st.Eligible); err != nil {
		return nil, err
	}
	return &st, tx.Commit()
}

// Job: send a device its limit now (the engine's refresh).
type Job struct {
	Device    string
	CommandID int64
	Resend    bool // not confirmed the first time: the pair goes out as a new version
}

// TakeJobs marks up to max targets whose send time has come as queued and
// returns them, earliest first. ReleaseJobs undoes it when they could not be queued.
func (s *Store) TakeJobs(ctx context.Context, max int) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, `
		UPDATE activation_targets t SET queued_at = now()
		FROM (SELECT activation_id, device_id FROM activation_targets
		      WHERE queued_at IS NULL AND send_at <= now() ORDER BY send_at LIMIT $1 FOR UPDATE SKIP LOCKED) q
		WHERE t.activation_id = q.activation_id AND t.device_id = q.device_id
		RETURNING t.device_id, t.command_id, t.resent`, max)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		var j Job
		if err := rows.Scan(&j.Device, &j.CommandID, &j.Resend); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *Store) ReleaseJobs(ctx context.Context, jobs []Job) error {
	cmds := make([]int64, len(jobs))
	for i, j := range jobs {
		cmds[i] = j.CommandID
	}
	_, err := s.db.ExecContext(ctx, `UPDATE activation_targets SET queued_at = NULL WHERE command_id = ANY($1)`, pq.Array(cmds))
	return err
}

// StageReport: a stage finished sending (all queued) and, once confirmed or
// after the wait, its counts for activation.stage.delivered.
type StageReport struct {
	Activation                       string
	Stage, Selected, Sent, Confirmed int
	DurationMs                       int64 // stage start -> last confirmation (or the wait)
	ReductionW                       float64
}

// Dispatched marks stages with every target queued; Reports returns stages
// that are confirmed in full, or dispatched more than wait ago, once each.
func (s *Store) Dispatched(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE activation_stages s SET state = 'dispatched', dispatched_at = now()
		WHERE state = 'dispatching' AND NOT EXISTS (SELECT 1 FROM activation_targets t
		  WHERE t.activation_id = s.activation_id AND t.stage = s.stage AND t.queued_at IS NULL)`)
	return err
}

func (s *Store) Reports(ctx context.Context, wait time.Duration) ([]StageReport, error) {
	// Not confirmed a wait after the stage was out (the ack went to an
	// instance that died, the device was briefly away): sent once more — the
	// same limit, idempotent per command id. The report waits for it.
	if _, err := s.db.ExecContext(ctx, `
		UPDATE activation_targets t SET queued_at = NULL, resent = true
		FROM activation_stages s, limits l
		WHERE s.activation_id = t.activation_id AND s.stage = t.stage AND l.command_id = t.command_id
		  AND s.state = 'dispatched' AND s.reported_at IS NULL AND s.dispatched_at < now() - $1::interval
		  AND NOT t.resent AND t.queued_at IS NOT NULL AND l.acked_at IS NULL AND l.cancelled_at IS NULL`, wait.String()); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		WITH c AS (
		  SELECT s.activation_id, s.stage, s.started_at, s.dispatched_at, count(t.device_id) n,
		         count(COALESCE(l.applied_at, l.acked_at)) sent, count(l.acked_at) conf, max(l.acked_at) last,
		         COALESCE(sum(GREATEST(0, t.power_w_before - LEAST(t.power_w_before, t.limit_w))) FILTER (WHERE l.acked_at IS NOT NULL), 0) red
		  FROM activation_stages s
		  LEFT JOIN activation_targets t ON t.activation_id = s.activation_id AND t.stage = s.stage
		  LEFT JOIN limits l ON l.command_id = t.command_id
		  WHERE s.state = 'dispatched' AND s.reported_at IS NULL
		  GROUP BY s.activation_id, s.stage)
		UPDATE activation_stages s SET reported_at = now()
		FROM c WHERE s.activation_id = c.activation_id AND s.stage = c.stage
		  AND (c.conf = c.n OR c.dispatched_at < now() - 2 * $1::interval)
		RETURNING c.activation_id, c.stage, c.n, c.sent, c.conf,
		  (extract(epoch FROM COALESCE(CASE WHEN c.conf = c.n THEN c.last END, now()) - c.started_at) * 1000)::bigint, c.red`,
		wait.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StageReport
	for rows.Next() {
		var r StageReport
		if err := rows.Scan(&r.Activation, &r.Stage, &r.Selected, &r.Sent, &r.Confirmed, &r.DurationMs, &r.ReductionW); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// EndExpired: activations past their end; their limits have expired.
func (s *Store) EndExpired(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		UPDATE activations SET state = 'ended', ended_at = ends_at
		WHERE state = 'active' AND ends_at <= now() RETURNING activation_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Resend queues every target of the activation again (it ended or was
// extended: the devices get the limit now in force). Returns the devices: the
// engine drops their hot station state first, so the sends read the limit
// now in force from Postgres (ending is not on the fast path).
func (s *Store) Resend(ctx context.Context, activation string) ([]string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
		UPDATE activation_targets SET queued_at = NULL WHERE activation_id = $1 RETURNING device_id`, activation)
	if err != nil {
		return nil, err
	}
	var devs []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			rows.Close()
			return nil, err
		}
		devs = append(devs, d)
	}
	rows.Close()
	return devs, tx.Commit()
}

func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}

// selectInPG: the eligible devices from the operator tables. Order: digital
// (ISO 15118-2) sessions first — the limit is a new Charging Loop answer,
// confirmed by the device, in force from the EV's next loop request; then
// charging devices; then the ones drawing most power (the largest reduction
// per command).
func selectInPG(ctx context.Context, tx *sql.Tx, st StageStart, sel actSelection) (devs []string, pw []sql.NullFloat64, err error) {
	// Eligible: the selection, online, not yet in this activation, not held
	// by another activation of higher priority.
	where := []string{
		"NOT EXISTS (SELECT 1 FROM activation_targets t WHERE t.activation_id = $1 AND t.device_id = d.device_id)",
		`NOT EXISTS (SELECT 1 FROM limits x WHERE x.device_id = d.device_id AND x.cancelled_at IS NULL
		   AND x.expires_at > now() AND x.activation_id IS NOT NULL AND x.activation_id <> $1 AND x.priority > $2)`}
	args := []any{st.Activation, st.Priority}
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, strings.ReplaceAll(cond, "?", "$"+itoa(len(args))))
	}
	if len(sel.Devices) > 0 {
		add("d.device_id = ANY(?)", pq.Array(sel.Devices))
	}
	if sel.Online == nil || *sel.Online {
		where = append(where, "d.online")
	}
	if sel.Charging != nil {
		add("COALESCE(l.charging, false) = ?", *sel.Charging)
	}
	if sel.Mode != nil {
		add("l.control_mode = ?", *sel.Mode)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT d.device_id, l.power_w FROM devices d
		LEFT JOIN device_live l USING (device_id) LEFT JOIN device_attributes a USING (device_id)
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY (l.control_mode = 'iso') IS TRUE DESC, COALESCE(l.charging, false) DESC, l.power_w DESC NULLS LAST, d.device_id`, args...)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var d string
		var p sql.NullFloat64
		if err := rows.Scan(&d, &p); err != nil {
			rows.Close()
			return nil, nil, err
		}
		devs, pw = append(devs, d), append(pw, p)
	}
	rows.Close()
	return devs, pw, rows.Err()
}

// SeedRow: a device for the activation pool when Valkey starts empty.
type SeedRow struct {
	Device   string
	Plugged  *time.Time
	Online   bool
	Mode     string
	Charging bool
	PowerW   float64
	Lat, Lon *float64
}

// PoolSeed: devices plugged in now (operator tables) and every located device.
func (s *Store) PoolSeed(ctx context.Context) ([]SeedRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT d.device_id, l.plugged_since, COALESCE(d.online, false), COALESCE(l.control_mode, 'pwm'),
		       COALESCE(l.charging, false), COALESCE(l.power_w, 0), d.lat::float8, d.lon::float8
		FROM devices d LEFT JOIN device_live l USING (device_id)
		WHERE d.bind_state IS DISTINCT FROM 'unbound' AND d.bind_state IS DISTINCT FROM 'revoked'
		  AND d.blocked_at IS NULL AND (l.plugged_since IS NOT NULL OR d.lat IS NOT NULL)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SeedRow
	for rows.Next() {
		var r SeedRow
		var pl sql.NullTime
		var lat, lon sql.NullFloat64
		if err := rows.Scan(&r.Device, &pl, &r.Online, &r.Mode, &r.Charging, &r.PowerW, &lat, &lon); err != nil {
			return nil, err
		}
		if pl.Valid {
			r.Plugged = &pl.Time
		}
		if lat.Valid && lon.Valid {
			r.Lat, r.Lon = &lat.Float64, &lon.Float64
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Location: the coarse coordinates of a device (nil: not set).
func (s *Store) Location(ctx context.Context, device string) (lat, lon *float64, err error) {
	var a, b sql.NullFloat64
	err = s.db.QueryRowContext(ctx, `SELECT lat::float8, lon::float8 FROM devices WHERE device_id = $1`, device).Scan(&a, &b)
	if err == sql.ErrNoRows {
		return nil, nil, nil
	}
	if err != nil || !a.Valid || !b.Valid {
		return nil, nil, err
	}
	return &a.Float64, &b.Float64, nil
}

// ActivationWork: what the scheduler has before it (engine: actNext).
type ActivationWork struct {
	Busy    bool      // a stage is being handed out, or sends are due now
	Waiting bool      // a stage waits for its confirmations (report)
	Next    time.Time // next stage start, send or activation end; zero: none
}

// NextActivationWork: one cheap query instead of the scheduler's full round.
func (s *Store) NextActivationWork(ctx context.Context) (ActivationWork, error) {
	var w ActivationWork
	var next sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM activation_stages WHERE state = 'dispatching')
		       OR EXISTS (SELECT 1 FROM activation_targets WHERE queued_at IS NULL AND send_at <= now()),
		       EXISTS (SELECT 1 FROM activation_stages WHERE state = 'dispatched' AND reported_at IS NULL),
		       LEAST((SELECT min(starts_at) FROM activation_stages WHERE state = 'scheduled'),
		             (SELECT min(send_at) FROM activation_targets WHERE queued_at IS NULL),
		             (SELECT min(ends_at) FROM activations WHERE state = 'active'))`).Scan(&w.Busy, &w.Waiting, &next)
	if next.Valid {
		w.Next = next.Time
	}
	return w, err
}
