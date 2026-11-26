// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// Needs a Postgres: PG_TEST_DSN="host=127.0.0.1 user=postgres password=... sslmode=disable".
// A fresh database is created from api/migrations for the test and dropped after.
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set")
	}
	admin, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("projtest_%d", time.Now().UnixNano())
	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("postgres", dsn+" dbname="+name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		admin.Exec("DROP DATABASE " + name + " WITH (FORCE)")
		admin.Close()
	})
	files, _ := filepath.Glob("../../../api/migrations/*.sql")
	sort.Strings(files)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(b)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
	return db
}

// feedEv: one row of domain_events.
type feedEv struct {
	seq  int64
	typ  string
	at   string // 2026-11-25 04:36:<at> UTC
	plug string
	data map[string]any
	sid  string // V2G session
}

const (
	v2g1 = "6f1c2a4e-0000-4000-8000-000000000001"
	v2g3 = "6f1c2a4e-0000-4000-8000-000000000003"
)

// issue15: the events of dev-01 from the failing aarch64 run (issue #15,
// comment 7155422387). Seq 8-13 (other types) are not in the capture: the
// feed has holes there, which the projector passes (hole wait 0 here).
var issue15 = []feedEv{
	{6, "session.started", "21.268224", "ccd6a0a9-1", map[string]any{"cp": "B"}, ""},
	{7, "v2g.started", "21.436684", "ccd6a0a9-1", map[string]any{"protocol": "iso15118-2"}, v2g1},
	{14, "limit.applied", "22.139245", "ccd6a0a9-2", map[string]any{"command_id": 1}, ""},
	{17, "charging.started", "23.434921", "ccd6a0a9-2", nil, ""},
	{21, "session.ended", "34.003232", "ccd6a0a9-2", map[string]any{"reason": "ev_unplugged"}, ""},
	{23, "session.started", "49.864027", "ccd6a0a9-3", map[string]any{"cp": "B"}, ""},
	{24, "v2g.started", "50.02809", "ccd6a0a9-3", map[string]any{"protocol": "iso15118-2"}, v2g3},
	{32, "session.ended", "03.035762", "ccd6a0a9-3", map[string]any{"reason": "ev_unplugged"}, ""},
}

func feed(t *testing.T, db *sql.DB, evs []feedEv) {
	t.Helper()
	// the device row, as the engine creates it (EnsureDevice)
	if _, err := db.Exec(`INSERT INTO devices (device_id, online) VALUES ('dev-01', true)`); err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		day := "2026-11-25 04:36:"
		if e.seq == 32 {
			day = "2026-11-25 04:37:"
		}
		d := map[string]any{"plug": e.plug}
		for k, v := range e.data {
			d[k] = v
		}
		raw, _ := json.Marshal(d)
		if _, err := db.Exec(`INSERT INTO domain_events (seq, id, type, device_id, session_id, dseq, time, data)
			VALUES ($1, $2, $3, 'dev-01', NULLIF($4, '')::uuid, $5, $6::timestamptz, $7)`,
			e.seq, fmt.Sprintf("1795581379%03d-%d", e.seq, e.seq%2), e.typ, e.sid, 1795581379000+e.seq,
			day+e.at+"+00", string(raw)); err != nil {
			t.Fatalf("event %d: %v", e.seq, err)
		}
	}
}

// project: the projector over the whole feed, batch events at a time.
func project(t *testing.T, db *sql.DB, batch int) {
	t.Helper()
	p := &projector{log: slog.New(slog.NewTextHandler(io.Discard, nil)), db: db, batch: batch, workers: 2,
		dirty: map[string]bool{}}
	ctx := context.Background()
	var hole time.Time
	for i := 0; i < 1000; i++ {
		n, _, err := p.step(ctx, &hole)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return
		}
	}
	t.Fatal("the projector did not reach the end of the feed")
}

type sumRow struct {
	reason   sql.NullString
	limits   int
	implicit bool
	started  time.Time
	ended    sql.NullTime
}

func summaries(t *testing.T, db *sql.DB) map[string]sumRow {
	t.Helper()
	rows, err := db.Query(`SELECT plug_id, end_reason, limits_applied, implicit_start, started_at, ended_at
		FROM session_summaries WHERE device_id = 'dev-01'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]sumRow{}
	for rows.Next() {
		var plug string
		var r sumRow
		if err := rows.Scan(&plug, &r.reason, &r.limits, &r.implicit, &r.started, &r.ended); err != nil {
			t.Fatal(err)
		}
		out[plug] = r
	}
	return out
}

func at(s string) time.Time {
	v, _ := time.Parse("2006-01-02 15:04:05.999999", "2026-11-25 04:"+s)
	return v
}

func want(t *testing.T, got map[string]sumRow, plug, reason string, limits int, implicit bool, started string) {
	t.Helper()
	r, ok := got[plug]
	if !ok {
		t.Fatalf("%s: no summary; summaries %+v", plug, got)
	}
	if r.reason.String != reason || r.limits != limits || r.implicit != implicit || !r.started.Equal(at(started)) {
		t.Errorf("%s: end_reason %q, limits_applied %d, implicit_start %v, started %s; want %q, %d, %v, %s",
			plug, r.reason.String, r.limits, r.implicit, r.started.UTC().Format("15:04:05.000000"), reason, limits, implicit, started)
	}
}

func noOrphans(t *testing.T, db *sql.DB) {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM projector_orphans`).Scan(&n); err != nil || n != 0 {
		t.Errorf("projector_orphans: %d rows (err %v); want none", n, err)
	}
}

// Issue #15: the plug-in id changed -1 -> -2 without session.ended(-1) and
// session.started(-2). The limit and the unplug of -2 must count on a plug-in
// of their own, -1 ends superseded, nothing waits as an orphan. In one batch
// and one event per batch (each its own transaction).
func TestProjectorReplugWithoutStart(t *testing.T) {
	for _, batch := range []int{2000, 1} {
		t.Run(fmt.Sprintf("batch%d", batch), func(t *testing.T) {
			db := testDB(t)
			feed(t, db, issue15)
			project(t, db, batch)
			got := summaries(t, db)
			if len(got) != 3 {
				t.Fatalf("%d summaries, want 3: %+v", len(got), got)
			}
			want(t, got, "ccd6a0a9-1", "superseded", 0, false, "36:21.268224")
			if e := got["ccd6a0a9-1"].ended; !e.Valid || !e.Time.Equal(at("36:22.139245")) {
				t.Errorf("-1 ended %v, want at the first event of -2 (04:36:22.139245)", e)
			}
			want(t, got, "ccd6a0a9-2", "ev_unplugged", 1, true, "36:22.139245")
			want(t, got, "ccd6a0a9-3", "ev_unplugged", 0, false, "36:49.864027")
			noOrphans(t, db)
		})
	}
}

// The session.started of -2 comes after all (late): it joins the summary
// opened implicitly, its start moves back and the mark is cleared.
func TestProjectorLateStartJoins(t *testing.T) {
	db := testDB(t)
	evs := append([]feedEv{}, issue15...)
	evs = append(evs, feedEv{40, "session.started", "22.001000", "ccd6a0a9-2", map[string]any{"cp": "B", "supersedes": "ccd6a0a9-1"}, ""})
	feed(t, db, evs)
	project(t, db, 2000)
	got := summaries(t, db)
	if len(got) != 3 {
		t.Fatalf("%d summaries, want 3: %+v", len(got), got)
	}
	want(t, got, "ccd6a0a9-1", "superseded", 0, false, "36:21.268224")
	want(t, got, "ccd6a0a9-2", "ev_unplugged", 1, false, "36:22.001000")
	want(t, got, "ccd6a0a9-3", "ev_unplugged", 0, false, "36:49.864027")
	noOrphans(t, db)
}

// The fixed engine: session.started(-2) with "supersedes" -1 before the limit.
// -1 ends superseded, -2 is an ordinary plug-in, and the V2G session of the
// EV (started on -1) is not closed by the id change, only by the unplug.
func TestProjectorSupersedingStart(t *testing.T) {
	db := testDB(t)
	evs := append([]feedEv{}, issue15[:2]...)
	evs = append(evs, feedEv{13, "session.started", "22.050000", "ccd6a0a9-2", map[string]any{"cp": "B", "supersedes": "ccd6a0a9-1"}, ""})
	evs = append(evs, issue15[2:]...)
	feed(t, db, evs)
	project(t, db, 2000)
	got := summaries(t, db)
	want(t, got, "ccd6a0a9-1", "superseded", 0, false, "36:21.268224")
	want(t, got, "ccd6a0a9-2", "ev_unplugged", 1, false, "36:22.050000")
	want(t, got, "ccd6a0a9-3", "ev_unplugged", 0, false, "36:49.864027")
	noOrphans(t, db)
	var ended time.Time
	var why string
	if err := db.QueryRow(`SELECT ended_at, end_reason FROM sessions WHERE session_id = $1`, v2g1).Scan(&ended, &why); err != nil {
		t.Fatal(err)
	}
	if !ended.Equal(at("36:34.003232")) || why != "EV unplugged" {
		t.Errorf("V2G session of -1 ended %s (%s); want at the unplug 04:36:34.003232 (EV unplugged)", ended.UTC(), why)
	}
}
