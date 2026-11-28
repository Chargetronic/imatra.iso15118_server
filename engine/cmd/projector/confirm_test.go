// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

package main

import (
	"database/sql"
	"testing"
	"time"
)

// The device confirms a limit within milliseconds; the engine writes the
// confirmation (limit.applied) and the limit.changed of the same command in
// different batches, so limit.applied may come first in the feed. The
// device's row must show the limit confirmed either way.
func TestProjectorConfirmBeforeLimitChanged(t *testing.T) {
	for _, order := range []string{"applied first", "changed first"} {
		t.Run(order, func(t *testing.T) {
			db := testDB(t)
			acked := at("36:22.139245")
			evs := []feedEv{{6, "session.started", "21.268224", "ccd6a0a9-1", map[string]any{"cp": "B"}, ""}}
			applied := feedEv{7, "limit.applied", "22.139245", "ccd6a0a9-1", map[string]any{"command_id": 1, "via": "loop"}, ""}
			changed := feedEv{8, "limit.changed", "22.140000", "", map[string]any{"command_id": 1, "limit_a": 8, "source": "hems",
				"until": "2026-11-25T05:36:22Z"}, ""}
			if order == "applied first" {
				evs = append(evs, applied, changed)
			} else {
				applied.seq, changed.seq = 8, 7
				evs = append(evs, changed, applied)
			}
			feed(t, db, evs)
			if _, err := db.Exec(`INSERT INTO limits (command_id, device_id, limit_a, expires_at, source, acked_at, ack_via)
				VALUES (1, 'dev-01', 8, $1, 'hems', $2, 'loop')`, acked.Add(time.Hour), acked); err != nil {
				t.Fatal(err)
			}
			project(t, db, 2000)
			var cmd sql.NullInt64
			var conf sql.NullTime
			if err := db.QueryRow(`SELECT command_id, limit_confirmed_at FROM device_live WHERE device_id = 'dev-01'`).Scan(&cmd, &conf); err != nil {
				t.Fatal(err)
			}
			if cmd.Int64 != 1 || !conf.Valid || !conf.Time.Equal(acked) {
				t.Errorf("command %v, limit_confirmed_at %v; want command 1 confirmed at %s", cmd, conf, acked.Format(time.RFC3339Nano))
			}
		})
	}
}
