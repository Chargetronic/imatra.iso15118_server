// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

package main

// device_live in memory per batch (10.11.2026). Before, every event was its
// own UPDATE of the device's row (and one more for last_seq / as_of): ~36
// writes per plug-in for ~19 events. Now applyPart reads the rows of its
// devices once (FOR UPDATE), the events change them in memory with the same
// rules as the SQL they replace (last writer by event time, the current
// plug-in only, ADR-001), and each changed row is written once, together
// with last_seq and as_of, in one statement for the part.

import (
	"context"
	"database/sql"
	"time"

	"github.com/lib/pq"
)

// liveRow: the device_live columns; times as Unix microseconds (exact both ways).
type liveRow struct {
	online                        sql.NullBool
	cp, mode, v2g, proto          sql.NullString
	limitSrc, powerQ              sql.NullString
	charging                      bool
	plugged, plugDseq, limitUntil sql.NullInt64
	cmd, confirmed, soc, dep      sql.NullInt64
	onlineT, cpT, powerT          sql.NullInt64
	limitA, powerW, eAmount       sql.NullFloat64
	lastSeq                       int64
	asOf                          sql.NullInt64
	dirty                         bool // a column other than last_seq / as_of changed
}

var liveCols = `online, cp, charging, control_mode, ` + us("plugged_since") + `, plug_dseq, v2g_session_id::text, protocol,
	limit_a::float8, limit_source, ` + us("limit_until") + `, command_id, ` + us("limit_confirmed_at") + `,
	power_w::float8, power_quality, e_amount_wh::float8, soc, departure_time_s,
	` + us("online_time") + `, ` + us("cp_time") + `, ` + us("power_time") + `, last_seq, ` + us("as_of")

func us(col string) string {
	return `(CASE WHEN ` + col + ` IN ('-infinity', 'infinity') THEN NULL ELSE (extract(epoch FROM ` + col + `) * 1000000)::bigint END)`
}

// loadLive: the rows of the devices, created if missing, locked for the part.
func loadLive(ctx context.Context, tx *sql.Tx, devs []string) (map[string]*liveRow, error) {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO device_live (device_id, as_of) SELECT d, '-infinity' FROM unnest($1::text[]) d ON CONFLICT DO NOTHING`,
		pq.Array(devs)); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT device_id, `+liveCols+` FROM device_live WHERE device_id = ANY($1) FOR UPDATE`, pq.Array(devs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*liveRow{}
	for rows.Next() {
		var d string
		r := &liveRow{}
		if err := rows.Scan(&d, &r.online, &r.cp, &r.charging, &r.mode, &r.plugged, &r.plugDseq, &r.v2g, &r.proto,
			&r.limitA, &r.limitSrc, &r.limitUntil, &r.cmd, &r.confirmed, &r.powerW, &r.powerQ, &r.eAmount, &r.soc, &r.dep,
			&r.onlineT, &r.cpT, &r.powerT, &r.lastSeq, &r.asOf); err != nil {
			return nil, err
		}
		out[d] = r
	}
	return out, rows.Err()
}

// saveLive: changed rows in one statement; the others get last_seq / as_of only.
func saveLive(ctx context.Context, tx *sql.Tx, live map[string]*liveRow, touched map[string]bool) error {
	var (
		dev                               []string
		online                            []sql.NullBool
		charging, dirty                   []bool
		cp, mode, v2g, proto, lsrc, pq_   []sql.NullString
		plugged, pdseq, luntil, cmd, conf []sql.NullInt64
		soc, dep, ont, cpt, pwt, asof     []sql.NullInt64
		lseq                              []int64
		lima, pw, ea                      []sql.NullFloat64
	)
	for d := range touched {
		r := live[d]
		dev, online, charging, dirty = append(dev, d), append(online, r.online), append(charging, r.charging), append(dirty, r.dirty)
		cp, mode, v2g, proto, lsrc, pq_ = append(cp, r.cp), append(mode, r.mode), append(v2g, r.v2g), append(proto, r.proto),
			append(lsrc, r.limitSrc), append(pq_, r.powerQ)
		plugged, pdseq, luntil, cmd, conf = append(plugged, r.plugged), append(pdseq, r.plugDseq), append(luntil, r.limitUntil),
			append(cmd, r.cmd), append(conf, r.confirmed)
		soc, dep, ont, cpt, pwt, asof = append(soc, r.soc), append(dep, r.dep), append(ont, r.onlineT), append(cpt, r.cpT),
			append(pwt, r.powerT), append(asof, r.asOf)
		lseq, lima, pw, ea = append(lseq, r.lastSeq), append(lima, r.limitA), append(pw, r.powerW), append(ea, r.eAmount)
	}
	if len(dev) == 0 {
		return nil
	}
	ts := func(col string) string {
		return `CASE WHEN u.` + col + ` IS NULL THEN NULL ELSE timestamptz 'epoch' + u.` + col + ` * interval '1 microsecond' END`
	}
	g := func(a any) any { return pq.GenericArray{A: a} }
	_, err := tx.ExecContext(ctx, `
		UPDATE device_live l SET
		  online = CASE WHEN u.dirty THEN u.online ELSE l.online END,
		  cp = CASE WHEN u.dirty THEN u.cp ELSE l.cp END,
		  charging = CASE WHEN u.dirty THEN u.charging ELSE l.charging END,
		  control_mode = CASE WHEN u.dirty THEN u.mode ELSE l.control_mode END,
		  plugged_since = CASE WHEN u.dirty THEN `+ts("plugged")+` ELSE l.plugged_since END,
		  plug_dseq = CASE WHEN u.dirty THEN u.pdseq ELSE l.plug_dseq END,
		  v2g_session_id = CASE WHEN u.dirty THEN u.v2g::uuid ELSE l.v2g_session_id END,
		  protocol = CASE WHEN u.dirty THEN u.proto ELSE l.protocol END,
		  limit_a = CASE WHEN u.dirty THEN u.lima ELSE l.limit_a END,
		  limit_source = CASE WHEN u.dirty THEN u.lsrc ELSE l.limit_source END,
		  limit_until = CASE WHEN u.dirty THEN `+ts("luntil")+` ELSE l.limit_until END,
		  command_id = CASE WHEN u.dirty THEN u.cmd ELSE l.command_id END,
		  limit_confirmed_at = CASE WHEN u.dirty THEN `+ts("conf")+` ELSE l.limit_confirmed_at END,
		  power_w = CASE WHEN u.dirty THEN u.pw ELSE l.power_w END,
		  power_quality = CASE WHEN u.dirty THEN u.pq ELSE l.power_quality END,
		  e_amount_wh = CASE WHEN u.dirty THEN u.ea ELSE l.e_amount_wh END,
		  soc = CASE WHEN u.dirty THEN u.soc ELSE l.soc END,
		  departure_time_s = CASE WHEN u.dirty THEN u.dep ELSE l.departure_time_s END,
		  online_time = CASE WHEN u.dirty THEN `+ts("ont")+` ELSE l.online_time END,
		  cp_time = CASE WHEN u.dirty THEN `+ts("cpt")+` ELSE l.cp_time END,
		  power_time = CASE WHEN u.dirty THEN `+ts("pwt")+` ELSE l.power_time END,
		  updated_at = CASE WHEN u.dirty THEN now() ELSE l.updated_at END,
		  last_seq = u.lseq,
		  as_of = GREATEST(l.as_of, COALESCE(`+ts("asof")+`, l.as_of))
		FROM unnest($1::text[], $2::bool[], $3::bool[], $4::text[], $5::bool[], $6::text[], $7::bigint[], $8::bigint[],
		  $9::text[], $10::text[], $11::float8[], $12::text[], $13::bigint[], $14::bigint[], $15::bigint[], $16::float8[],
		  $17::text[], $18::float8[], $19::bigint[], $20::bigint[], $21::bigint[], $22::bigint[], $23::bigint[], $24::bigint[],
		  $25::bigint[])
		  AS u(d, dirty, online, cp, charging, mode, plugged, pdseq, v2g, proto, lima, lsrc, luntil, cmd, conf, pw, pq, ea,
		       soc, dep, ont, cpt, pwt, lseq, asof)
		WHERE l.device_id = u.d`,
		pq.Array(dev), pq.Array(dirty), g(online), g(cp), pq.Array(charging), g(mode), g(plugged), g(pdseq),
		g(v2g), g(proto), g(lima), g(lsrc), g(luntil), g(cmd), g(conf), g(pw),
		g(pq_), g(ea), g(soc), g(dep), g(ont), g(cpt), g(pwt), pq.Array(lseq), g(asof))
	return err
}

func micros(t time.Time) sql.NullInt64 { return sql.NullInt64{Int64: t.UnixMicro(), Valid: true} }

// later: the event time t (server's, ADR-003) is not older than the field's (nil = never set).
func later(field sql.NullInt64, t time.Time) bool {
	return !field.Valid || field.Int64 <= t.UnixMicro()
}

// curPlug (the Go form of the SQL one): an event of the current plug-in,
// not a late one of an older plug-in.
func (r *liveRow) curPlug(t time.Time) bool {
	return !r.plugged.Valid || r.plugged.Int64 <= t.Add(plugSlack).UnixMicro()
}

func nstr(v any) sql.NullString {
	s, ok := v.(string)
	return sql.NullString{String: s, Valid: ok && s != ""}
}

func nnum(v any) sql.NullFloat64 {
	f, ok := v.(float64)
	return sql.NullFloat64{Float64: f, Valid: ok}
}

func nint(v any) sql.NullInt64 {
	f, ok := v.(float64)
	return sql.NullInt64{Int64: int64(f), Valid: ok}
}

func ntime(v any) sql.NullInt64 {
	s, ok := v.(string)
	if !ok || s == "" {
		return sql.NullInt64{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return sql.NullInt64{}
	}
	return micros(t)
}

// applyLive: what the event changes in the device's row (in memory).
func (r *liveRow) applyLive(e *ev) {
	t, dseq := e.Time, e.DSeq.Int64
	switch e.Type {
	case "device.online", "device.offline":
		if later(r.onlineT, t) {
			r.online, r.onlineT, r.dirty = sql.NullBool{Bool: e.Type == "device.online", Valid: true}, micros(t), true
		}
	case "cp.changed":
		mode, _ := e.Data["mode"].(string)
		pwmSide := mode == "basic" || mode == "transit"
		if later(r.cpT, t) {
			cp := nstr(e.Data["ev"])
			if r.mode.String == "pwm" && r.mode.Valid || pwmSide {
				r.charging = cp.Valid && cp.String == "C"
			}
			r.cp, r.cpT, r.dirty = cp, micros(t), true
		}
	case "power.changed":
		if later(r.powerT, t) {
			r.powerW, r.powerQ, r.powerT, r.dirty = nnum(e.Data["power_w"]), nstr(e.Data["quality"]), micros(t), true
		}
	case "session.started":
		if !r.plugDseq.Valid || r.plugDseq.Int64 < dseq {
			r.plugged, r.plugDseq = micros(t), sql.NullInt64{Int64: dseq, Valid: true}
			r.mode, r.charging = sql.NullString{String: "pwm", Valid: true}, false
			r.v2g, r.proto, r.eAmount, r.soc, r.dep = sql.NullString{}, sql.NullString{}, sql.NullFloat64{}, sql.NullInt64{}, sql.NullInt64{}
			r.dirty = true
		}
	case "session.ended":
		if !r.plugDseq.Valid || r.plugDseq.Int64 <= dseq {
			r.plugged, r.charging, r.v2g, r.dirty = sql.NullInt64{}, false, sql.NullString{}, true
		}
	case "v2g.started":
		if r.curPlug(t) {
			proto, _ := e.Data["protocol"].(string)
			cm := "iso"
			if proto == "din70121" {
				cm = "din"
			}
			r.v2g, r.proto, r.mode = nstr(e.Session.String), nstr(proto), sql.NullString{String: cm, Valid: true}
			r.dirty = true
		}
	case "v2g.ended":
		if r.curPlug(t) && e.Session.String != "" && r.v2g.Valid && r.v2g.String == e.Session.String {
			r.v2g, r.dirty = sql.NullString{}, true
		}
	case "ev.parameters":
		if r.curPlug(t) {
			if v := nnum(e.Data["e_amount_wh"]); v.Valid {
				r.eAmount = v
			}
			if v := nint(e.Data["soc"]); v.Valid {
				r.soc = v
			}
			if v := nint(e.Data["departure_time_s"]); v.Valid {
				r.dep = v
			}
			r.dirty = true
		}
	case "charging.started", "charging.stopped":
		if r.curPlug(t) {
			r.charging, r.dirty = e.Type == "charging.started", true
		}
	case "limit.changed":
		r.limitA, r.limitSrc, r.limitUntil = nnum(e.Data["limit_a"]), nstr(e.Data["source"]), ntime(e.Data["until"])
		r.cmd, r.confirmed, r.dirty = nint(e.Data["command_id"]), sql.NullInt64{}, true
	case "limit.applied":
		if !e.replay {
			if c := nint(e.Data["command_id"]); c.Valid && r.cmd.Valid && r.cmd.Int64 == c.Int64 {
				r.confirmed, r.dirty = micros(t), true
			}
		}
	}
}
