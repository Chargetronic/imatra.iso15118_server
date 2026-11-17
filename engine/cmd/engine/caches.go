// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

package main

// Fewer database round trips on the hot path.
//
//   - devices seen: EnsureDevice once per device and process (presence keeps
//     last_seen_at/online up to date from the heartbeat);
//   - station configuration per device, dropped on NOTIFY config;
//   - device settings (the published config), dropped on NOTIFY config and
//     all of them when the listener reconnects: a reconnect wave of devices
//     does not read Postgres once per device;
//   - the open session of each device (id, connection), kept from the engine's
//     own create/close, read from the database only when unknown;
//   - journal mode: JOURNAL_MODE=flag (default) writes every V2G message
//     and the full twin timeline only for devices with devices.debug_journal;
//     JOURNAL_MODE=all writes them for every device (a small bench). The
//     default follows the architecture (decision 08.11.2026; load test
//     16.11.2026: the full journal of 5000 devices kept Postgres at a full
//     core);
//   - telemetry written in batches.

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/chargetronic/imatra.iso15118_server/engine/internal/secc"
	"github.com/chargetronic/imatra.iso15118_server/engine/internal/store"
	"github.com/chargetronic/imatra.iso15118_server/engine/internal/tunnel"
)

type caches struct {
	mu      sync.Mutex
	seen    map[string]bool
	station map[string]secc.Config
	open    map[string]openRef // device -> newest open session ("" id = none)
	setts   map[string]settingsRef

	journalAll bool
	jmu        sync.RWMutex
	journalOn  map[string]bool
}

type openRef struct{ id, conn string }

type settingsRef struct {
	conf tunnel.Config
	rev  int
}

func newCaches() *caches {
	return &caches{seen: map[string]bool{}, station: map[string]secc.Config{}, open: map[string]openRef{}, setts: map[string]settingsRef{},
		journalAll: os.Getenv("JOURNAL_MODE") == "all", journalOn: map[string]bool{}}
}

func (e *engine) ensureDevice(ctx context.Context, device string) error {
	e.c.mu.Lock()
	ok := e.c.seen[device]
	e.c.mu.Unlock()
	if ok {
		return nil
	}
	if err := e.db.EnsureDevice(ctx, device); err != nil {
		return err
	}
	e.c.mu.Lock()
	e.c.seen[device] = true
	e.c.mu.Unlock()
	return nil
}

func (e *engine) cachedStation(device string) (secc.Config, bool) {
	e.c.mu.Lock()
	defer e.c.mu.Unlock()
	c, ok := e.c.station[device]
	return c, ok
}

func (e *engine) storeStation(device string, c secc.Config) {
	e.c.mu.Lock()
	e.c.station[device] = c
	e.c.mu.Unlock()
}

func (e *engine) cachedSettings(device string) (tunnel.Config, int, bool) {
	e.c.mu.Lock()
	defer e.c.mu.Unlock()
	r, ok := e.c.setts[device]
	return r.conf, r.rev, ok
}

func (e *engine) storeSettings(device string, conf tunnel.Config, rev int) {
	e.c.mu.Lock()
	e.c.setts[device] = settingsRef{conf, rev}
	e.c.mu.Unlock()
}

// dropSettings: one device, or all ("").
func (e *engine) dropSettings(device string) {
	e.c.mu.Lock()
	if device == "" {
		e.c.setts = map[string]settingsRef{}
	} else {
		delete(e.c.setts, device)
	}
	e.c.mu.Unlock()
}

// dropStation: the station settings and limit of the device are read again
// (from Postgres) on the next use — the hot state with Valkey, this
// instance's memory without.
func (e *engine) dropStation(device string) {
	if vk := e.vk(); vk != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		e.hotDrop(ctx, vk, device)
		cancel()
		return
	}
	e.c.mu.Lock()
	delete(e.c.station, device)
	e.c.mu.Unlock()
}

// setOpen records the device's open session (the engine created or loaded it).
func (e *engine) setOpen(device, id, conn string) {
	e.c.mu.Lock()
	e.c.open[device] = openRef{id, conn}
	e.c.mu.Unlock()
}

// closeOpen forgets the open session; conn "" closes whatever is open.
func (e *engine) closeOpen(device, conn string) {
	e.c.mu.Lock()
	if r, ok := e.c.open[device]; ok && (conn == "" || r.conn == conn) {
		e.c.open[device] = openRef{}
	}
	e.c.mu.Unlock()
}

// openSessionID: the device's newest open session, "" if none.
func (e *engine) openSessionID(ctx context.Context, device string) string {
	if v, ok := e.st.(*vkState); ok { // several instances: the pointer lives in Valkey
		id, _ := v.c.HGet(ctx, oKey(device), "sid").Result()
		return id
	}
	e.c.mu.Lock()
	r, ok := e.c.open[device]
	e.c.mu.Unlock()
	if ok {
		return r.id
	}
	open, err := e.st.Open(ctx, device)
	if err != nil {
		return ""
	}
	r = openRef{}
	if len(open) > 0 {
		r = openRef{open[0].ID, open[0].Conn}
	}
	e.c.mu.Lock()
	if _, set := e.c.open[device]; !set { // the engine may have set it meanwhile
		e.c.open[device] = r
	}
	e.c.mu.Unlock()
	return r.id
}

// journal: write V2G messages and traces of this device?
func (e *engine) journal(device string) bool {
	if e.c.journalAll {
		return true
	}
	e.c.jmu.RLock()
	defer e.c.jmu.RUnlock()
	return e.c.journalOn[device]
}

// journalLoop refreshes the set of devices with debug_journal (flag mode).
func (e *engine) journalLoop() {
	if e.c.journalAll {
		return
	}
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		ids, err := e.db.JournalDevices(ctx)
		cancel()
		if err == nil {
			m := make(map[string]bool, len(ids))
			for _, id := range ids {
				m[id] = true
			}
			e.c.jmu.Lock()
			e.c.journalOn = m
			e.c.jmu.Unlock()
		} else {
			e.log.Warn("journal devices", "err", err)
		}
		time.Sleep(15 * time.Second)
	}
}

// telemetryBatcher writes metering reports in batches: up to 500 rows or
// every 200 ms, whichever comes first.
type telemetryBatcher struct {
	in chan store.TelemetryRow
}

func (e *engine) startTelemetryBatcher() *telemetryBatcher {
	b := &telemetryBatcher{in: make(chan store.TelemetryRow, 20000)}
	go func() {
		const maxRows = 500
		buf := make([]store.TelemetryRow, 0, maxRows)
		tick := time.NewTicker(200 * time.Millisecond)
		flush := func() {
			if len(buf) == 0 {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := e.bg.AddTelemetryBatch(ctx, buf); err != nil {
				e.log.Error("telemetry batch", "rows", len(buf), "err", err)
			}
			cancel()
			buf = buf[:0]
		}
		for {
			select {
			case r := <-b.in:
				buf = append(buf, r)
				if len(buf) >= maxRows {
					flush()
				}
			case <-tick.C:
				flush()
			}
		}
	}()
	return b
}
