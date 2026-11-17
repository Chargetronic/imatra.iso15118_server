// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

package main

// Business rules of a device (internal/rules, docs/rules.md): the open set
// the service layer gives in the registry. Handlers here:
//
//	rated_current_a, install_max_current_a  lower the station limit (stationDefaults)
//	phases                                   phases of the power calculation (stationDefaults)
//	meter                                    quality label of the energy in session.ended
//	supply                                   charging windows for the device's own battery
//
// A device with no rules behaves as before rules existed.

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/chargetronic/imatra.iso15118_server/engine/internal/events"
	"github.com/chargetronic/imatra.iso15118_server/engine/internal/rules"
	"github.com/chargetronic/imatra.iso15118_server/engine/internal/secc"
	"github.com/chargetronic/imatra.iso15118_server/engine/internal/tunnel"
)

var (
	mRulesInvalid = reg.CounterVec("engine_rules_invalid_total",
		"Business rules of a known key with a value the server cannot use (the key is skipped).", "key")
	mSupplyWindows = reg.Counter("engine_supply_windows_total",
		"Charging windows opened for a device's own battery (rule supply).")
)

// deviceRules: the device's rules as stored (defaults on any error).
func (e *engine) deviceRules(ctx context.Context, device string) rules.Set {
	b, err := e.db.DeviceRules(ctx, device)
	if err != nil {
		e.log.Warn("rules", "device", device, "err", err)
	}
	s := rules.Parse(b)
	for _, k := range s.Invalid {
		mRulesInvalid.With(k).Inc()
	}
	return s
}

// applyRules: the station limit and phases as the rules bound them.
func applyRules(cfg secc.Config, r rules.Set) secc.Config {
	cfg.MaxCurrentA = r.MaxCurrent(cfg.MaxCurrentA)
	if r.Phases != nil && *r.Phases < max(cfg.Phases, 1) {
		cfg.Phases = *r.Phases
	}
	return cfg
}

// energyQuality: how the energy of a session was measured.
func energyQuality(r rules.Set) string {
	if r.Meter == "certified" {
		return "certified"
	}
	return "measured"
}

// supplyState: when a device last got a window (one per three window lengths).
type supplyState struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// batteryOf: the device's own battery, percent, from its heartbeat
// ("battery_pct"); ok=false when the device reports none.
func batteryOf(status []byte) (float64, bool) {
	var s struct {
		BatteryPct *float64 `json:"battery_pct"`
	}
	if json.Unmarshal(status, &s) != nil || s.BatteryPct == nil {
		return 0, false
	}
	return *s.BatteryPct, true
}

// supplyCheck (on a heartbeat): a device whose battery is below its rule's
// threshold, with an EV plugged in that draws no current, gets a window: a
// limit of the rule's current for its minutes and a CP restart pulse, after
// which the EV starts a new session and the device charges its battery from
// the line. An EV that does not charge (full) gives nothing; the next window
// comes after three window lengths at the earliest.
func (e *engine) supplyCheck(ctx context.Context, device string, status []byte) {
	soc, ok := batteryOf(status)
	if !ok {
		return
	}
	r := e.deviceRules(ctx, device)
	if !r.SupplyNeeded(soc) {
		return
	}
	window := time.Duration(r.Supply.Minutes) * time.Minute
	e.supply.mu.Lock()
	if t, ok := e.supply.last[device]; ok && time.Since(t) < 3*window {
		e.supply.mu.Unlock()
		return
	}
	e.supply.mu.Unlock()
	idle, err := e.db.PluggedIdle(ctx, device)
	if err != nil || !idle {
		return
	}
	id, err := e.db.AddSupplyLimit(ctx, device, r.Supply.MinCurrentA, window)
	if err != nil {
		e.log.Warn("supply window", "device", device, "err", err)
		return
	}
	e.supply.mu.Lock()
	e.supply.last[device] = time.Now()
	e.supply.mu.Unlock()
	b, _ := json.Marshal(tunnel.CPSet{V: tunnel.Version, Cmd: "restart", Via: "X1", HoldS: 3})
	e.client.Publish(tunnel.CPSetTopic(device), 1, false, b)
	mSupplyWindows.Inc()
	e.emit(ctx, events.Event{Type: events.SupplyWindow, Device: device, Time: time.Now().UTC(), Data: map[string]any{
		"battery_pct": soc, "below_pct": r.Supply.BelowPct, "limit_a": r.Supply.MinCurrentA,
		"minutes": r.Supply.Minutes, "command_id": id}})
	e.log.Info("supply window", "device", device, "battery_pct", soc, "limit_a", r.Supply.MinCurrentA, "minutes", r.Supply.Minutes)
}
