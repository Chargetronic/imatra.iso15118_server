// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

// Package events: the operator's event feed (domain events).
// Published contract: schemas/events.v1.json.
//
// Domain events are few and meaningful (about 20-30 per plug-in); the twin's
// own timeline (table events: every CP report, every pair push) stays where
// it is and is not part of the feed.
//
// Order (ADR-001): engine instances share the device topics, so arrival order
// is not the device's order. Every event carries a per-device sequence number
// (dseq) taken when the event is written; a device's events are ordered by
// dseq, not by the feed cursor.
//
// Time (ADR-003): the server's clock only, taken when the message reached the
// engine (before its worker queue). A device's clock is never an event time.
// What belongs together is told by identifiers the device gives: the plug-in
// (data "plug", "<boot>-<n>"), the TCP connection to the EV and the V2G
// session id; durations the device measures itself (plugged_ms).
package events

import (
	"encoding/json"
	"time"

	"github.com/chargetronic/imatra.iso15118_server/engine/internal/secc"
)

// SchemaVer of the event payloads; bumped on an incompatible change.
const SchemaVer = 1

// Types in the feed.
const (
	DeviceOnline    = "device.online"
	DeviceOffline   = "device.offline"
	CPChanged       = "cp.changed"
	SessionStarted  = "session.started" // EV plugged in (CP B): start of the plug-in session
	SessionEnded    = "session.ended"   // EV unplugged (CP A): energy, duration
	V2GStarted      = "v2g.started"     // digital session (ISO 15118-2 / DIN 70121) set up
	V2GEnded        = "v2g.ended"       // digital session over (SessionStop, error, connection closed)
	EVParameters    = "ev.parameters"
	ChargingStarted = "charging.started"
	ChargingStopped = "charging.stopped"
	LimitChanged    = "limit.changed"
	LimitApplied    = "limit.applied" // the device confirmed it: via, reaction_ms
	PowerChanged    = "power.changed"
	// Activations: schema now, emitted by the executor later.
	ActivationStageStarted   = "activation.stage.started"
	ActivationStageDelivered = "activation.stage.delivered"
	ActivationEnded          = "activation.ended"
	// Written by the relay, not by the engine.
	EventsGap = "system.events_gap"

	// Device registry of the service layer (block D): what the layer told
	// this operator, applied once per binding version.
	DeviceBound      = "device.bound"
	DeviceUnbound    = "device.unbound"
	DeviceRevoked    = "device.revoked"
	RegistryMismatch = "system.registry_mismatch"

	// Business rule "supply": a charging window for the device's own battery.
	SupplyWindow = "device.supply_window"
)

// Event before it gets its place in the feed (dseq, id).
type Event struct {
	Type      string
	Device    string
	SessionID string    // V2G session, "" for device and plug-in level events
	Time      time.Time // server time: when the message reached the engine (ADR-003)
	Plug      string    // the device's plug-in id ("<boot>-<n>"), "" when unknown
	Data      any
}

// Payload: the data as written to the feed, with the plug-in id ("plug").
func (e Event) Payload() any {
	if e.Plug == "" {
		return e.Data
	}
	m, ok := e.Data.(map[string]any)
	if !ok {
		m = map[string]any{}
		if raw, err := json.Marshal(e.Data); err == nil && e.Data != nil {
			_ = json.Unmarshal(raw, &m)
		}
	}
	out := make(map[string]any, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	out["plug"] = e.Plug
	return out
}

// FromStep maps what the V2G state machine reported in one step to the feed;
// the twin keeps the full list. The result is written together with the
// session state (one Lua step with Valkey, one transaction with Postgres).
func FromStep(device, sessionID string, st *secc.State, evs []secc.Event, now time.Time) []Event {
	var out []Event
	add := func(t string, d any) {
		out = append(out, Event{Type: t, Device: device, SessionID: sessionID, Time: now, Plug: st.Plug, Data: d})
	}
	for _, ev := range evs {
		switch ev.Type {
		case "session.started":
			d := map[string]any{"protocol": protocolName(st.Protocol)}
			if st.EV.EVCCID != "" {
				d["evcc_id"] = st.EV.EVCCID
			}
			if p, ok := ev.Data.(map[string]any); ok && p["purpose"] != nil {
				d["purpose"] = p["purpose"]
			}
			add(V2GStarted, d)
		case "session.ev_data":
			add(EVParameters, evParameters(st))
		case "charging.started":
			add(ChargingStarted, map[string]any{})
		case "charging.stopped":
			add(ChargingStopped, map[string]any{})
		case "session.ended":
			add(V2GEnded, map[string]any{"reason": st.EndReason})
		case "session.failed":
			d := map[string]any{"reason": "failed"}
			if p, ok := ev.Data.(map[string]any); ok {
				d["reason"], d["request"] = p["reason"], p["request"]
			}
			d["failed"] = true
			add(V2GEnded, d)
		}
	}
	return out
}

// Ends reports whether the events close the digital session (the Valkey
// store then marks it, so a later "connection closed" adds no second one).
func Ends(evs []Event) bool {
	for _, e := range evs {
		if e.Type == V2GEnded {
			return true
		}
	}
	return false
}

func protocolName(ns string) string {
	switch ns {
	case "":
		return ""
	case "urn:din:70121:2012:MsgDef":
		return "din70121"
	case "urn:iso:std:iso:15118:-20:AC":
		return "iso15118-20"
	default:
		return "iso15118-2"
	}
}

// evParameters: what the EV told about its need. Raw values, each in its own
// field and with its source; nothing derived (decision 01.11.2026).
func evParameters(st *secc.State) map[string]any {
	ev := st.EV
	d := map[string]any{"source": protocolName(st.Protocol)}
	f := func(k string, v *float64) {
		if v != nil {
			d[k] = *v
		}
	}
	f("e_amount_wh", ev.EAmountWh)
	f("ev_max_current_a", ev.EVMaxCurrentA)
	f("ev_min_current_a", ev.EVMinCurrentA)
	f("ev_max_voltage_v", ev.EVMaxVoltageV)
	f("ev_max_power_w", ev.EVMaxPowerW)
	f("ev_energy_request_wh", ev.EVEnergyRequestWh)
	f("ev_energy_capacity_wh", ev.EVEnergyCapacityWh)
	f("ev_min_power_w", ev.EVMinPowerW)
	f("ev_target_energy_wh", ev.EVTargetEnergyWh)
	f("ev_max_energy_wh", ev.EVMaxEnergyWh)
	f("ev_min_energy_wh", ev.EVMinEnergyWh)
	if ev.TargetSoC != nil {
		d["target_soc"] = int(*ev.TargetSoC)
	}
	if ev.MinSoC != nil {
		d["min_soc"] = int(*ev.MinSoC)
	}
	if st.ControlMode != 0 {
		d["control_mode"] = map[int]string{1: "scheduled", 2: "dynamic"}[st.ControlMode]
	}
	if ev.DepartureTimeS != nil {
		d["departure_time_s"] = *ev.DepartureTimeS
	}
	if ev.SoC != nil {
		d["soc"] = int(*ev.SoC)
	}
	if ev.EnergyMode != nil {
		d["energy_transfer_mode"] = *ev.EnergyMode
	}
	return d
}
