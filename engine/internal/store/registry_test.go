// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// Needs a Postgres: PG_TEST_DSN="host=127.0.0.1 user=postgres dbname=postgres sslmode=disable".
// A fresh database is created from api/migrations for the run and dropped after.
func testDB(t *testing.T) *Store {
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set")
	}
	admin, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("regtest_%d", time.Now().UnixNano())
	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		admin.Exec("DROP DATABASE " + name + " WITH (FORCE)")
		admin.Close()
	})
	s, err := open(dsn+" dbname="+name, 2)
	if err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob("../../../api/migrations/*.sql")
	sort.Strings(files)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(string(b)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
	return s
}

func TestRegistryIdempotent(t *testing.T) {
	s := testDB(t)
	ctx := context.Background()
	lat, lon := 61.1719, 28.7529
	e := func(state string, ver int64) RegistryEntry {
		return RegistryEntry{V: 1, Device: "t-reg-1", State: state, Type: "adapter", Lat: &lat, Lon: &lon,
			BindingVer: ver, TZ: "Europe/Helsinki"}
	}
	step := func(name string, x RegistryEntry, wantApplied bool, wantPrev string) {
		t.Helper()
		ok, prev, err := s.ApplyRegistry(ctx, x, time.Now())
		if err != nil || ok != wantApplied || (ok && prev != wantPrev) {
			t.Fatalf("%s: applied %v prev %q err %v; want %v %q", name, ok, prev, err, wantApplied, wantPrev)
		}
	}
	step("first binding", e("bound", 1), true, "")
	step("repeat", e("bound", 1), false, "")
	step("unbound", e("unbound", 2), true, "bound")
	step("late bound (older version)", e("bound", 1), false, "")
	// three changes in a row, each a new version
	step("bound again", e("bound", 3), true, "unbound")
	step("unbound again", e("unbound", 4), true, "bound")
	step("bound a third time", e("bound", 5), true, "unbound")
	// a new leader reads every retained message again: nothing changes
	for v := int64(1); v <= 5; v++ {
		step(fmt.Sprintf("re-read v%d", v), e("bound", v), false, "")
	}
	var st, tz string
	var ver int64
	var la float64
	var connected sql.NullTime
	if err := s.db.QueryRow(`SELECT bind_state, binding_ver, bound_tz, lat::float8, last_seen_at FROM devices WHERE device_id = 't-reg-1'`).
		Scan(&st, &ver, &tz, &la, &connected); err != nil {
		t.Fatal(err)
	}
	if st != "bound" || ver != 5 || tz != "Europe/Helsinki" || la != 61.172 || connected.Valid {
		t.Fatalf("stored %s v%d %s %v connected %v", st, ver, tz, la, connected.Valid)
	}
	bound, maxVer, unknown, err := s.RegistryStats(ctx)
	if err != nil || bound != 1 || maxVer != 5 || unknown != 0 {
		t.Fatalf("stats %d %d %d %v", bound, maxVer, unknown, err)
	}
	// revoked: blocked as by the operator, bound again: the layer's block goes
	step("revoked", e("revoked", 6), true, "bound")
	if err := s.RevokeDevice(ctx, "t-reg-1"); err != nil {
		t.Fatal(err)
	}
	if ids, _ := s.BlockedDevices(ctx); len(ids) != 1 {
		t.Fatalf("blocked %v", ids)
	}
	step("bound after revocation", e("bound", 7), true, "revoked")
	if err := s.UnrevokeDevice(ctx, "t-reg-1"); err != nil {
		t.Fatal(err)
	}
	if ids, _ := s.BlockedDevices(ctx); len(ids) != 0 {
		t.Fatalf("still blocked %v", ids)
	}
}

func TestRegistryRules(t *testing.T) {
	s := testDB(t)
	ctx := context.Background()
	rules := func() string {
		b, err := s.DeviceRules(ctx, "t-rules")
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	apply := func(ver int64, r string) {
		t.Helper()
		e := RegistryEntry{V: 1, Device: "t-rules", State: "bound", BindingVer: ver}
		if r != "" {
			e.Rules = json.RawMessage(r)
		}
		if ok, _, err := s.ApplyRegistry(ctx, e, time.Now()); !ok || err != nil {
			t.Fatalf("ver %d: %v %v", ver, ok, err)
		}
	}
	apply(1, `{"rated_current_a":16,"future":{"a":1}}`)
	if got := rules(); got != `{"future": {"a": 1}, "rated_current_a": 16}` {
		t.Fatalf("stored as given, unknown keys too: %s", got)
	}
	apply(2, "") // no rules in the message: the stored set stays
	if got := rules(); got != `{"future": {"a": 1}, "rated_current_a": 16}` {
		t.Fatalf("kept: %s", got)
	}
	apply(3, `{}`) // an empty set replaces it
	if got := rules(); got != `{}` {
		t.Fatalf("replaced: %s", got)
	}
	apply(4, `[1,2]`) // not an object: ignored, the stored set stays
	if got := rules(); got != `{}` {
		t.Fatalf("not an object: %s", got)
	}
}
