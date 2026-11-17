// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

package store

// Operator event feed in Postgres (domain_events).
// The relay fills domain_events from the Valkey stream; the engine never
// writes them (ADR-002, OSS scope B1).

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/lib/pq"
)

// DomainRow is one feed entry as stored.
type DomainRow struct {
	ID        string
	Type      string
	SchemaVer int
	Device    string
	SessionID string
	DSeq      int64
	Time      time.Time
	Data      json.RawMessage
}

// InsertDomainEvents stores a batch; entries already there (same id) are
// skipped, so a batch repeated after a failure gives no duplicates.
// Returns how many were new.
func (s *Store) InsertDomainEvents(ctx context.Context, rows []DomainRow) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	n := len(rows)
	id, typ, dev, sid, data := make([]string, n), make([]string, n), make([]string, n), make([]string, n), make([]string, n)
	ver, dseq := make([]int64, n), make([]int64, n)
	ts := make([]time.Time, n)
	for i, r := range rows {
		id[i], typ[i], dev[i], sid[i], ver[i], dseq[i], ts[i] = r.ID, r.Type, r.Device, r.SessionID, int64(r.SchemaVer), r.DSeq, r.Time
		data[i] = string(r.Data)
		if data[i] == "" {
			data[i] = "{}"
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, feedLock); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO domain_events (id, type, schema_ver, device_id, session_id, dseq, time, data)
		SELECT u.id, u.type, u.ver, NULLIF(u.dev, ''), NULLIF(u.sid, '')::uuid, NULLIF(u.dseq, 0), u.ts, u.data::jsonb
		FROM unnest($1::text[], $2::text[], $3::int[], $4::text[], $5::text[], $6::bigint[], $7::timestamptz[], $8::text[])
		  WITH ORDINALITY AS u(id, type, ver, dev, sid, dseq, ts, data, ord)
		ORDER BY u.ord
		ON CONFLICT (id) DO NOTHING`,
		pq.Array(id), pq.Array(typ), pq.Array(ver), pq.Array(dev), pq.Array(sid), pq.Array(dseq), pq.Array(ts), pq.Array(data))
	if err != nil {
		return 0, err
	}
	added, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return added, tx.Commit()
}

// feedLock: every writer of domain_events takes it right before its insert and
// holds it to its commit, so seq follows commit order. A reader that sees seq
// N committed then knows every lower seq is committed or rolled back: a feed
// cursor never moves past an event still to commit, however slow the commit
// (found 09.11.2026: with a 1 s settle time instead, commits slower than 1 s
// lost events). The lock is held only from the insert to the commit.
const feedLock = `SELECT pg_advisory_xact_lock(15118)`

// PurgeDomainEvents keeps the feed to days.
func (s *Store) PurgeDomainEvents(ctx context.Context, days int) (int64, error) {
	r, err := s.db.ExecContext(ctx, `DELETE FROM domain_events WHERE recorded_at < now() - make_interval(days => $1)`, days)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

// historyTables: tables kept to a retention period, with their time column.
var historyTables = map[string]string{"telemetry": "server_ts", "events": "server_ts", "messages": "server_ts"}

// PurgeHistory deletes rows of a history table (telemetry, events, messages)
// older than days, in batches of batch rows (short transactions, no long
// lock on a busy table). Returns the rows deleted.
func (s *Store) PurgeHistory(ctx context.Context, table string, days, batch int) (int64, error) {
	col, ok := historyTables[table]
	if !ok {
		return 0, fmt.Errorf("purge: %q is not a history table", table)
	}
	// by row address: the batch is found through the time index (BRIN on
	// telemetry and messages) and deleted without another scan of the table
	q := `DELETE FROM ` + table + ` WHERE ctid = ANY (ARRAY(SELECT ctid FROM ` + table + ` WHERE ` + col +
		` < now() - make_interval(days => $1) LIMIT $2))` // #nosec G202: table and column from the fixed list above
	var total int64
	for ctx.Err() == nil {
		r, err := s.db.ExecContext(ctx, q, days, batch)
		if err != nil {
			return total, err
		}
		n, _ := r.RowsAffected()
		total += n
		if n < int64(batch) {
			break
		}
	}
	return total, ctx.Err()
}

// RelayState: progress of the relay (relay_state).
type RelayState struct {
	LastID, RunID, GapMark string
}

func (s *Store) RelayState(ctx context.Context, name string) (RelayState, error) {
	var r RelayState
	var last, run, gap sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT last_id, run_id, gap_mark FROM relay_state WHERE name = $1`, name).
		Scan(&last, &run, &gap)
	if err == sql.ErrNoRows {
		return r, nil
	}
	r.LastID, r.RunID, r.GapMark = last.String, run.String, gap.String
	return r, err
}

// SaveRelayState: last_id and gap_mark only move forward (several relays,
// one group); a negative lag or backlog keeps the stored one.
func (s *Store) SaveRelayState(ctx context.Context, name string, r RelayState, lagMs, backlog int64) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO relay_state (name, last_id, run_id, gap_mark, lag_ms, backlog, updated_at)
		VALUES ($1, NULLIF($2, ''), NULLIF($3, ''), NULLIF($4, ''), GREATEST($5, 0), GREATEST($6, 0), now())
		ON CONFLICT (name) DO UPDATE SET
		  last_id = CASE WHEN EXCLUDED.last_id IS NULL THEN relay_state.last_id
		                 WHEN relay_state.last_id IS NULL THEN EXCLUDED.last_id
		                 ELSE stream_id_max(relay_state.last_id, EXCLUDED.last_id) END,
		  run_id = COALESCE(EXCLUDED.run_id, relay_state.run_id),
		  gap_mark = CASE WHEN EXCLUDED.gap_mark IS NULL THEN relay_state.gap_mark
		                  WHEN relay_state.gap_mark IS NULL THEN EXCLUDED.gap_mark
		                  ELSE stream_id_max(relay_state.gap_mark, EXCLUDED.gap_mark) END,
		  lag_ms = CASE WHEN $5 < 0 THEN relay_state.lag_ms ELSE $5 END,
		  backlog = CASE WHEN $6 < 0 THEN relay_state.backlog ELSE $6 END,
		  updated_at = now()`,
		name, r.LastID, r.RunID, r.GapMark, lagMs, backlog)
	return err
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// AckCommand records the device's confirmation of a limit command. Only the
// first confirmation counts (rule 4 of ADR-001): first is false for a repeat,
// for an unknown command or one of another device.
// at: when the device took it over (its clock when plausible, else arrival).
func (s *Store) AckCommand(ctx context.Context, commandID int64, device, via string, applied bool, at time.Time) (reactionMs int64, first bool, err error) {
	if !applied {
		_, err = s.db.ExecContext(ctx, `
			UPDATE limits SET delivered_at = COALESCE(delivered_at, now())
			WHERE command_id = $1 AND device_id = $2`, commandID, device)
		return 0, false, err
	}
	err = s.db.QueryRowContext(ctx, `
		UPDATE limits SET delivered_at = COALESCE(delivered_at, $4), acked_at = $4, ack_via = $3,
		  applied_at = COALESCE(applied_at, $4),
		  reaction_ms = GREATEST(0, (extract(epoch FROM $4 - created_at) * 1000)::int)
		WHERE command_id = $1 AND device_id = $2 AND acked_at IS NULL
		RETURNING reaction_ms`, commandID, device, via, at).Scan(&reactionMs)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	return reactionMs, err == nil, err
}

// EventRow: one row of the twin timeline (table events), for batched writes.
type EventRow struct {
	Device, SessionID, Kind string
	Data                    any
	At                      time.Time
}

// AddEvents writes twin events in one statement (the engine batches them off
// the device workers; server_ts is the time they happened, not the flush).
func (s *Store) AddEvents(ctx context.Context, rows []EventRow) error {
	n := len(rows)
	dev, sid, kind, data := make([]string, n), make([]string, n), make([]string, n), make([]string, n)
	ts := make([]time.Time, n)
	for i, r := range rows {
		raw, _ := json.Marshal(r.Data)
		dev[i], sid[i], kind[i], data[i], ts[i] = r.Device, r.SessionID, r.Kind, string(raw), r.At
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO events (device_id, session_id, kind, data, server_ts)
		SELECT d, NULLIF(s, '')::uuid, k, j::jsonb, t
		FROM unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::timestamptz[]) AS u(d, s, k, j, t)`,
		pq.Array(dev), pq.Array(sid), pq.Array(kind), pq.Array(data), pq.Array(ts))
	return err
}

// MarkAppliedBatch: commands sent (MarkApplied for many, with their times).
// Rows are locked in command id order, as in AckCommands: two batches over
// the same commands (sent marks, confirmations; two instances) then wait for
// each other instead of deadlocking (found 10.11.2026: a batch of 166
// confirmations lost to "deadlock detected"). A deadlock is retried anyway.
func (s *Store) MarkAppliedBatch(ctx context.Context, ids []int64, at []time.Time) error {
	return retryDeadlock(func() error {
		_, err := s.db.ExecContext(ctx, `
			UPDATE limits l SET applied_at = COALESCE(l.applied_at, u.t)
			FROM (SELECT c, min(t) t FROM unnest($1::bigint[], $2::timestamptz[]) AS x(c, t) GROUP BY c ORDER BY c) u
			WHERE l.command_id = u.c`, pq.Array(ids), pq.Array(at))
		return err
	})
}

// retryDeadlock: a statement that lost a deadlock or a serialization race
// is run again (Postgres rolled it back), up to 3 times.
func retryDeadlock(f func() error) error {
	var err error
	for i := 0; i < 4; i++ {
		if err = f(); err == nil {
			return nil
		}
		if e, ok := err.(*pq.Error); !ok || (e.Code != "40P01" && e.Code != "40001") {
			return err
		}
		time.Sleep(time.Duration(10*(i+1)) * time.Millisecond)
	}
	return err
}

// Ack: a device confirmation, for AckCommands.
type Ack struct {
	CommandID int64
	Device    string
	Via       string    // loop | pwm | cp_report
	Applied   bool      // installed / applied; false: held (the device has it)
	At        time.Time // when its ack reached the server (ADR-003)
	Plug      string    // plug-in id the device was in
}

// Confirmed: the first confirmation of a command (rule 4 of ADR-001).
type Confirmed struct {
	CommandID  int64
	Device     string
	Via        string
	ReactionMs int64
}

// AckCommands records a batch of confirmations in two statements; returns
// the commands confirmed for the first time (repeats, unknown commands and
// another device's commands give nothing).
func (s *Store) AckCommands(ctx context.Context, acks []Ack) ([]Confirmed, error) {
	var hc, ac []int64
	var hd, ad, av []string
	var ht, at []time.Time
	for _, a := range acks {
		if a.Applied {
			ac, ad, av, at = append(ac, a.CommandID), append(ad, a.Device), append(av, a.Via), append(at, a.At)
		} else {
			hc, hd, ht = append(hc, a.CommandID), append(hd, a.Device), append(ht, a.At)
		}
	}
	if len(hc) > 0 {
		if err := retryDeadlock(func() error {
			_, err := s.db.ExecContext(ctx, `
				UPDATE limits l SET delivered_at = COALESCE(l.delivered_at, u.t)
				FROM (SELECT DISTINCT ON (c) c, d, t FROM unnest($1::bigint[], $2::text[], $3::timestamptz[]) AS x(c, d, t)
				      ORDER BY c, t) u
				WHERE l.command_id = u.c AND l.device_id = u.d`, pq.Array(hc), pq.Array(hd), pq.Array(ht))
			return err
		}); err != nil {
			return nil, err
		}
	}
	if len(ac) == 0 {
		return nil, nil
	}
	var out []Confirmed
	err := retryDeadlock(func() error {
		out = out[:0]
		rows, err := s.db.QueryContext(ctx, `
		UPDATE limits l SET delivered_at = COALESCE(l.delivered_at, u.t), acked_at = u.t, ack_via = u.v,
		  applied_at = COALESCE(l.applied_at, u.t),
		  reaction_ms = GREATEST(0, (extract(epoch FROM u.t - l.created_at) * 1000)::int)
		FROM (SELECT DISTINCT ON (c) c, d, v, t FROM unnest($1::bigint[], $2::text[], $3::text[], $4::timestamptz[]) AS x(c, d, v, t)
		      ORDER BY c, t) u
		WHERE l.command_id = u.c AND l.device_id = u.d AND l.acked_at IS NULL
		RETURNING l.command_id, l.device_id, u.v, l.reaction_ms`, pq.Array(ac), pq.Array(ad), pq.Array(av), pq.Array(at))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c Confirmed
			if err := rows.Scan(&c.CommandID, &c.Device, &c.Via, &c.ReactionMs); err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
}
