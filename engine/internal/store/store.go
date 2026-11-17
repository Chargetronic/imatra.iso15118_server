// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

// Package store keeps engine state and the message journal in PostgreSQL.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"

	"github.com/chargetronic/imatra.iso15118_server/engine/internal/secc"
)

type Store struct{ db *sql.DB }

func Open(dsn string) (*Store, error) { return open(dsn, 0) }

// SchemaNeeded: the newest migration (api/migrations, applied by the
// API at its start) this code relies on.
const SchemaNeeded = "002_history_retention.sql"

// WaitSchema blocks until the API has applied the migrations this code needs.
// The engines, the relay and the projector start together with the API (a
// deployment, a restart of everything); before 11.11.2026 they ran their
// first queries against the old schema and logged errors until the API was
// done. Logs every 10 s while waiting; ctx ends the wait.
func WaitSchema(ctx context.Context, db *sql.DB, log interface{ Info(string, ...any) }) error {
	t0 := time.Now()
	for i := 0; ; i++ {
		var ok bool
		err := db.QueryRowContext(ctx, `SELECT to_regclass('schema_migrations') IS NOT NULL
			AND EXISTS (SELECT 1 FROM schema_migrations WHERE name = $1)`, SchemaNeeded).Scan(&ok)
		if err == nil && ok {
			if i > 0 {
				log.Info("database schema ready", "migration", SchemaNeeded, "waited_s", int(time.Since(t0).Seconds()))
			}
			return nil
		}
		if i%10 == 0 {
			log.Info("waiting for the database migrations (the API applies them)", "needed", SchemaNeeded)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// WaitSchema on this store's pool.
func (s *Store) WaitSchema(ctx context.Context, log interface{ Info(string, ...any) }) error {
	return WaitSchema(ctx, s.db, log)
}

// OpenBackground: a small pool of its own for writes that are history, not
// records (twin timeline, telemetry, presence): at most conns statements at
// a time, whatever the load, so the background never takes Postgres from
// the commands and the feed; commit without waiting for the disk
// (synchronous_commit off: a crash of Postgres may lose the last fraction of
// a second of these rows, never corrupt them). BG_DB_CONNS, default 3.
func OpenBackground(dsn string) (*Store, error) {
	n := 3
	if v, err := strconv.Atoi(os.Getenv("BG_DB_CONNS")); err == nil && v > 0 {
		n = v
	}
	return open(withOption(dsn, "-c synchronous_commit=off"), n)
}

func withOption(dsn, opt string) string {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		return dsn + sep + "options=" + url.QueryEscape(opt)
	}
	return dsn + " options='" + opt + "'"
}

func open(dsn string, fixed int) (*Store, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	// Engine workers (ENGINE_WORKERS) each hold at most one connection; a few
	// more for the listener loops. Postgres max_connections must allow it.
	// The hot path is in Valkey (ADR-002); Postgres gets the device tables,
	// commands and the listeners. Several instances share max_connections.
	conns := 12
	if n, err := strconv.Atoi(os.Getenv("DB_MAX_CONNS")); err == nil && n > 0 {
		conns = n
	}
	if fixed > 0 {
		conns = fixed
	}
	db.SetMaxOpenConns(conns)
	db.SetMaxIdleConns(conns)
	db.SetConnMaxIdleTime(5 * time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return &Store{db: db}, db.PingContext(ctx)
}

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// Stats of the connection pool (metrics).
func (s *Store) Stats() sql.DBStats { return s.db.Stats() }

// Session is the engine's view of one V2G session (one TCP connection).
type Session struct {
	ID    string // sessions.session_id (uuid)
	State *secc.State
	// Ver: version of the state in the hot store (Valkey), for the
	// compare-and-set save.
	Ver int64
}

// EnsureDevice registers a device seen on the tunnel. A known one is
// left alone: online and last_seen_at are the presence writer's (status
// reports), not rewritten by every engine instance on its first message.
func (s *Store) EnsureDevice(ctx context.Context, device string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO devices (device_id, last_seen_at, online) VALUES ($1, now(), true)
		ON CONFLICT (device_id) DO NOTHING`, device)
	return err
}

// Message is one journal row.
type Message struct {
	SessionID                    string
	Conn                         string
	Seq                          uint32
	Direction                    string // ev_to_server | server_to_ev
	Source                       string // device | server
	Type                         string
	EXI                          []byte
	Decoded                      any
	Payload                      int
	Wire                         int
	TEVRx, TUpTx, TSrvRx, TSrvTx int64
	PairID                       *int64
}

func nz(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func (s *Store) AddMessage(ctx context.Context, m *Message) (int64, error) {
	dec, _ := json.Marshal(m.Decoded)
	var id int64
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO messages (session_id, conn_id, seq, direction, source, msg_type, exi, decoded,
		                      payload_bytes, wire_bytes, t_ev_rx, t_up_tx, t_srv_rx, t_srv_tx, pair_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15) RETURNING id`,
		m.SessionID, m.Conn, m.Seq, m.Direction, m.Source, m.Type, m.EXI, dec,
		m.Payload, m.Wire, nz(m.TEVRx), nz(m.TUpTx), nz(m.TSrvRx), nz(m.TSrvTx), m.PairID).Scan(&id)
	return id, err
}

// AddTrace completes the timing of a response with the device's report.
func (s *Store) AddTrace(ctx context.Context, sessionID string, seq uint32, downRx, evTx int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE messages SET t_down_rx = $3, t_ev_tx = $4
		WHERE session_id = $1 AND seq = $2 AND direction = 'server_to_ev'`, sessionID, seq, nz(downRx), nz(evTx))
	return err
}

// DeviceConfig returns the station configuration of a device: the latest
// version in device_configs, merged over defaults.
func (s *Store) DeviceConfig(ctx context.Context, device string, def secc.Config) (secc.Config, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT config FROM device_configs WHERE device_id = $1
		ORDER BY version DESC LIMIT 1`, device).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return def, nil
	}
	if err != nil {
		return def, err
	}
	var wrap struct {
		Station *secc.Config `json:"station"`
	}
	cfg := def
	wrap.Station = &cfg
	if err := json.Unmarshal(raw, &wrap); err != nil {
		return def, err
	}
	return cfg, nil
}

// AddEvent appends to the session timeline.
// AddTelemetry stores one metering report.
func (s *Store) AddTelemetry(ctx context.Context, device, sessionID string, deviceUS int64, iA []float64,
	pW, eWh float64, data []byte) error {
	ia := make([]string, len(iA))
	for i, v := range iA {
		ia[i] = strconv.FormatFloat(v, 'f', -1, 64)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO telemetry (device_id, session_id, device_ts, i_a, p_w, e_wh, data)
		VALUES ($1, NULLIF($2, '')::uuid, to_timestamp($3 / 1e6), ('{' || $4 || '}')::real[], $5, $6, $7)`,
		device, sessionID, deviceUS, strings.Join(ia, ","), pW, eWh, data)
	return err
}

// PluggedSince: time of the last ev.plugged event, zero if none.
func (s *Store) PluggedSince(ctx context.Context, device string) (time.Time, error) {
	var t sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT max(server_ts) FROM events WHERE device_id = $1 AND kind = 'ev.plugged'`, device).Scan(&t)
	return t.Time, err
}

// EventsSince: how many events of a kind the device has since t, and the time of the first.
func (s *Store) EventsSince(ctx context.Context, device, kind string, since time.Time) (int, time.Time, error) {
	var n int
	var t sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT count(*), min(server_ts) FROM events WHERE device_id = $1 AND kind = $2 AND server_ts >= $3`,
		device, kind, since).Scan(&n, &t)
	return n, t.Time, err
}

// Energy of the device between t0 and now, Wh.
func (s *Store) Energy(ctx context.Context, device string, t0 time.Time) (float64, error) {
	var e float64
	err := s.db.QueryRowContext(ctx, `SELECT telemetry_energy($1, $2, now())`, device, t0).Scan(&e)
	return e, err
}

// LimitDevices: devices with an active limit (their TTL is renewed).
func (s *Store) LimitDevices(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT device_id FROM limits
		WHERE cancelled_at IS NULL AND starts_at <= now() AND expires_at > now()`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) AddEvent(ctx context.Context, device, sessionID, kind string, data any) error {
	raw, _ := json.Marshal(data)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO events (device_id, session_id, kind, data) VALUES ($1, NULLIF($2, '')::uuid, $3, $4)`,
		device, sessionID, kind, raw)
	return err
}

// OpenSession is a session whose EV connection is still up.
type OpenSession struct {
	Device, Conn string
	Session
}

// DeviceSettings merges the "device" section of the latest device
// configuration over def and returns that configuration's version (0: none).
func (s *Store) DeviceSettings(ctx context.Context, device string, def any) (int, error) {
	var raw []byte
	var rev int
	err := s.db.QueryRowContext(ctx, `
		SELECT version, config FROM device_configs WHERE device_id = $1
		ORDER BY version DESC LIMIT 1`, device).Scan(&rev, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var wrap struct {
		Device any `json:"device"`
	}
	wrap.Device = def
	return rev, json.Unmarshal(raw, &wrap)
}

// DeviceExists: the device is in this deployment's registry of devices.
func (s *Store) DeviceExists(ctx context.Context, device string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM devices WHERE device_id = $1`, device).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// Limit is the active limit command of a device (operator or UI).
type Limit struct {
	CommandID int64
	LimitA    float64
	Source    string
	ExpiresAt time.Time
	Applied   bool
	Priority  int
}

// ActiveLimit returns the newest limit of the device in force now, or nil.
// (highest priority first: an activation over a direct limit; then the newest)
func (s *Store) ActiveLimit(ctx context.Context, device string) (*Limit, error) {
	var l Limit
	var applied sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT command_id, limit_a, source, expires_at, applied_at, priority FROM limits
		WHERE device_id = $1 AND cancelled_at IS NULL AND starts_at <= now() AND expires_at > now()
		ORDER BY priority DESC, command_id DESC LIMIT 1`, device).Scan(&l.CommandID, &l.LimitA, &l.Source, &l.ExpiresAt, &applied, &l.Priority)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	l.Applied = applied.Valid
	return &l, nil
}

// MarkApplied records when a limit first reached the device.
func (s *Store) MarkApplied(ctx context.Context, commandID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE limits SET applied_at = COALESCE(applied_at, now()) WHERE command_id = $1`, commandID)
	return err
}

// SetPresence records a heartbeat or the LWT of a device. An offline marker
// keeps the last full heartbeat for the UI.
// Presence: one status report of a device (the latest of a batch counts).
type Presence struct {
	Device string
	Online bool
	Status []byte
	At     time.Time
}

// SetPresenceBatch writes the latest presence of many devices in one
// statement (the engine's presence writer: off the device workers).
func (s *Store) SetPresenceBatch(ctx context.Context, ps []Presence) error {
	if len(ps) == 0 {
		return nil
	}
	n := len(ps)
	dev, st := make([]string, n), make([]sql.NullString, n)
	on, at := make([]bool, n), make([]time.Time, n)
	for i, p := range ps {
		dev[i], on[i], at[i] = p.Device, p.Online, p.At
		if p.Online {
			st[i] = sql.NullString{String: string(p.Status), Valid: true}
		}
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO devices (device_id, last_seen_at, online, status)
		SELECT u.d, u.t, u.o, u.s::jsonb FROM unnest($1::text[], $2::timestamptz[], $3::bool[], $4::text[])
		  AS u(d, t, o, s) ORDER BY u.d
		ON CONFLICT (device_id) DO UPDATE SET last_seen_at = EXCLUDED.last_seen_at, online = EXCLUDED.online,
		  status = COALESCE(EXCLUDED.status, devices.status)`,
		pq.Array(dev), pq.Array(at), pq.Array(on), pq.Array(st))
	return err
}

func (s *Store) SetPresence(ctx context.Context, device string, online bool, status []byte) error {
	var st any = string(status) // jsonb: text, not bytea
	if !online {
		st = nil
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO devices (device_id, last_seen_at, online, status) VALUES ($1, now(), $2, $3)
		ON CONFLICT (device_id) DO UPDATE SET last_seen_at = now(), online = $2,
		  status = COALESCE($3, devices.status)`, device, online, st)
	return err
}

// TelemetryRow is one metering report for AddTelemetryBatch.
type TelemetryRow struct {
	Device, SessionID string
	DeviceUS          int64     // the device's wall clock: diagnostics only (device_ts)
	At                time.Time // arrival at the server: the reading's time (server_ts, ADR-003)
	IA                []float64
	PW, EWh           float64
	Data              []byte
}

// AddTelemetryBatch stores many metering reports in one statement.
func (s *Store) AddTelemetryBatch(ctx context.Context, rows []TelemetryRow) error {
	if len(rows) == 0 {
		return nil
	}
	var q strings.Builder
	q.WriteString(`INSERT INTO telemetry (device_id, session_id, device_ts, i_a, p_w, e_wh, data, server_ts) VALUES `)
	args := make([]any, 0, len(rows)*8)
	for i, r := range rows {
		ia := make([]string, len(r.IA))
		for j, v := range r.IA {
			ia[j] = strconv.FormatFloat(v, 'f', -1, 64)
		}
		if i > 0 {
			q.WriteString(",")
		}
		n := i * 8
		fmt.Fprintf(&q, "($%d, NULLIF($%d, '')::uuid, CASE WHEN $%d::bigint > 0 THEN to_timestamp($%d / 1e6) END, ('{' || $%d || '}')::real[], $%d, $%d, $%d, COALESCE($%d, now()))",
			n+1, n+2, n+3, n+3, n+4, n+5, n+6, n+7, n+8)
		at := sql.NullTime{Time: r.At, Valid: !r.At.IsZero()}
		args = append(args, r.Device, r.SessionID, r.DeviceUS, strings.Join(ia, ","), r.PW, r.EWh, r.Data, at)
	}
	_, err := s.db.ExecContext(ctx, q.String(), args...)
	return err
}

// JournalDevices: devices whose V2G messages are journalled in
// JOURNAL_MODE=flag (devices.debug_journal).
func (s *Store) JournalDevices(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT device_id FROM devices WHERE debug_journal`)
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

// BlockedDevices: devices the operator blocked (devices.blocked_at):
// the engines drop their messages and send them nothing.
func (s *Store) BlockedDevices(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT device_id FROM devices WHERE blocked_at IS NOT NULL ORDER BY device_id`)
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
