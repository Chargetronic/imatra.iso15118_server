// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// RegistryEntry: one message of the service layer's device registry
// (registry/dev/<id>, retained). Unknown fields are ignored by the caller.
type RegistryEntry struct {
	V          int      `json:"v"`
	Device     string   `json:"device_id"`
	State      string   `json:"state"` // bound | unbound | revoked
	Type       string   `json:"type"`
	Lat        *float64 `json:"lat"`
	Lon        *float64 `json:"lon"`
	BindingVer int64    `json:"binding_ver"`
	At         string   `json:"at"`
	TZ         string   `json:"tz"`
	// Rules: the device's business rules (internal/rules), the whole set as
	// the layer gives it; absent: the stored set stays.
	Rules json.RawMessage `json:"rules,omitempty"`
}

// ApplyRegistry stores the entry if its binding_ver is greater than the one
// stored; prev is the state before ("" when the device was not in the
// registry). applied=false: a repeat, a late message or one already seen.
// One statement: two engines applying the same message cannot both win.
func (s *Store) ApplyRegistry(ctx context.Context, e RegistryEntry, at time.Time) (applied bool, prev string, err error) {
	var p sql.NullString
	err = s.db.QueryRowContext(ctx, `
		WITH old AS (SELECT bind_state FROM devices WHERE device_id = $1)
		INSERT INTO devices (device_id, online, bind_state, binding_ver, device_type, lat, lon, location_at,
		                     bound_at, unbound_at, bound_tz, registry_at, rules)
		VALUES ($1, false, $2, $3, NULLIF($4, ''), round($5::numeric, 3), round($6::numeric, 3),
		        CASE WHEN $5::numeric IS NULL THEN NULL ELSE $7::timestamptz END,
		        CASE WHEN $2 = 'bound' THEN $7::timestamptz END, CASE WHEN $2 <> 'bound' THEN $7::timestamptz END,
		        NULLIF($8, ''), now(), COALESCE($9::jsonb, '{}'))
		ON CONFLICT (device_id) DO UPDATE SET
		  bind_state = EXCLUDED.bind_state, binding_ver = EXCLUDED.binding_ver,
		  device_type = COALESCE(EXCLUDED.device_type, devices.device_type),
		  lat = COALESCE(EXCLUDED.lat, devices.lat), lon = COALESCE(EXCLUDED.lon, devices.lon),
		  location_at = COALESCE(EXCLUDED.location_at, devices.location_at),
		  bound_at = COALESCE(EXCLUDED.bound_at, devices.bound_at),
		  unbound_at = CASE WHEN EXCLUDED.bind_state = 'bound' THEN NULL ELSE EXCLUDED.unbound_at END,
		  bound_tz = COALESCE(EXCLUDED.bound_tz, devices.bound_tz), registry_at = now(),
		  rules = CASE WHEN $9::jsonb IS NULL THEN devices.rules ELSE $9::jsonb END
		WHERE devices.binding_ver < EXCLUDED.binding_ver
		RETURNING (SELECT bind_state FROM old)`,
		e.Device, e.State, e.BindingVer, e.Type, e.Lat, e.Lon, at, e.TZ, rulesArg(e.Rules)).Scan(&p)
	if err == sql.ErrNoRows {
		return false, "", nil
	}
	return err == nil, p.String, err
}

// CancelLimits ends every limit of the device in force (direct and from
// activations): it left this operator.
func (s *Store) CancelLimits(ctx context.Context, device string) (int64, error) {
	r, err := s.db.ExecContext(ctx, `UPDATE limits SET cancelled_at = now()
		WHERE device_id = $1 AND cancelled_at IS NULL AND expires_at > now()`, device)
	if err != nil {
		return 0, err
	}
	n, _ := r.RowsAffected()
	if n > 0 {
		_, err = s.db.ExecContext(ctx, `SELECT pg_notify('limits', $1)`, device)
	}
	return n, err
}

// RevokeDevice: the layer revoked the device: the same barrier as the
// operator's local block (blocked_at, NOTIFY blocked).
func (s *Store) RevokeDevice(ctx context.Context, device string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE devices SET blocked_at = COALESCE(blocked_at, now()),
		blocked_reason = COALESCE(blocked_reason, 'revoked by the service layer'), blocked_by = COALESCE(blocked_by, 'layer')
		WHERE device_id = $1`, device)
	if err == nil {
		_, err = s.db.ExecContext(ctx, `SELECT pg_notify('blocked', $1)`, device)
	}
	return err
}

// RegistryStats: devices bound, the greatest binding_ver, devices with no
// registry entry (admitted by their certificate alone).
func (s *Store) RegistryStats(ctx context.Context) (bound int64, maxVer int64, unknown int64, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE bind_state = 'bound'),
		COALESCE(max(binding_ver), 0), count(*) FILTER (WHERE bind_state IS NULL)
		FROM devices`).Scan(&bound, &maxVer, &unknown)
	return
}

// UnrevokeDevice: bound again after a revocation: the layer's block goes (an
// operator's own block stays).
func (s *Store) UnrevokeDevice(ctx context.Context, device string) error {
	r, err := s.db.ExecContext(ctx, `UPDATE devices SET blocked_at = NULL, blocked_reason = NULL, blocked_by = NULL
		WHERE device_id = $1 AND blocked_by = 'layer'`, device)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n > 0 {
		_, err = s.db.ExecContext(ctx, `SELECT pg_notify('blocked', $1)`, device)
	}
	return err
}

// rulesArg: the rules object as a query argument (nil: none given).
func rulesArg(b json.RawMessage) any {
	var m map[string]json.RawMessage
	if len(b) == 0 || json.Unmarshal(b, &m) != nil || m == nil {
		return nil
	}
	return string(b)
}

// DeviceRules: the device's business rules as stored ({} when none).
func (s *Store) DeviceRules(ctx context.Context, device string) ([]byte, error) {
	var b []byte
	err := s.db.QueryRowContext(ctx, `SELECT rules FROM devices WHERE device_id = $1`, device).Scan(&b)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return b, err
}

// NotifyConfig: the device's settings or rules changed; the engine owning it
// reads them again (channel "config", as a change from the API).
func (s *Store) NotifyConfig(ctx context.Context, device string) error {
	_, err := s.db.ExecContext(ctx, `SELECT pg_notify('config', $1)`, device)
	return err
}

// AddSupplyLimit: a window of charging at limitA for the device's own
// battery (rule "supply"). Priority 0: any other limit in force wins (it is
// 6 A or more, so current flows as well).
func (s *Store) AddSupplyLimit(ctx context.Context, device string, limitA float64, d time.Duration) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `INSERT INTO limits (device_id, limit_a, expires_at, source, priority)
		VALUES ($1, $2, now() + $3 * interval '1 second', 'device_supply', 0) RETURNING command_id`,
		device, limitA, d.Seconds()).Scan(&id)
	if err == nil {
		_, err = s.db.ExecContext(ctx, `SELECT pg_notify('limits', $1)`, device)
	}
	return id, err
}

// PluggedIdle: the EV is plugged in and draws no current (operator tables).
func (s *Store) PluggedIdle(ctx context.Context, device string) (bool, error) {
	var idle bool
	err := s.db.QueryRowContext(ctx, `SELECT plugged_since IS NOT NULL AND NOT COALESCE(charging, false)
		FROM device_live WHERE device_id = $1`, device).Scan(&idle)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return idle, err
}
