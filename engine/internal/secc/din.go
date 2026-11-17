// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

package secc

import (
	"encoding/hex"
	"fmt"
	"math"

	"github.com/chargetronic/imatra.iso15118_server/engine/internal/exi"
)

// DIN 70121 branch: only the handshake, whose only purpose is the EV's SoC
// (design doc, section 4: the EV did not accept AC HLC or gave no usable
// EAmount / departure time). The engine walks the EV up to
// ChargeParameterDiscovery, keeps SoC and the EV's DC parameters, answers
// with EVSENotification = StopCharging and closes the connection. Power is
// never delivered: there is no DC on an AC station. The device then
// interrupts CP (F towards the EV) and the EV continues in basic AC mode.
//
// DIN 70121 has no graceful SECC-initiated stop before power delivery, so the
// end is the TCP close plus the CP interruption.

var dinAllowed = map[Phase][]exi.DINKind{
	PhaseSessionSetup:  {exi.DINSessionSetup},
	PhaseServiceDisc:   {exi.DINServiceDiscovery},
	PhasePaymentSel:    {exi.DINServicePaymentSelection},
	PhaseAuthorization: {exi.DINContractAuthentication},
	PhaseChargeParam:   {exi.DINChargeParameterDiscovery, exi.DINContractAuthentication},
}

const dinRCWrongEnergyTransferType = 22 // DIN 70121 FAILED_WrongEnergyTransferType

// DC values offered to the EV. They only have to look sane: no DC power is
// ever switched on.
const (
	dinMaxVoltageV = 500
	dinMinVoltageV = 150
	dinMinCurrentA = 0
	dinRippleA     = 1
)

func dinPV(v float64, unit int8, decimals int) exi.PV {
	p := exi.MakePV(v, unit, decimals)
	return p
}

func stepDIN(st *State, cfg Config, env Env, payload []byte) (*Result, error) {
	q, err := exi.DecodeDIN(payload)
	if err != nil {
		return nil, err
	}
	res := &exi.DINResponse{Kind: q.Kind, SessionID: st.sessionIDBytes(), ResponseCode: exi.RCOK, Isolation: -1,
		EVSEStatusCode: exi.DINEVSEReady}
	var events []Event
	r := &Result{Request: q, Response: res, RequestType: q.Type}

	fail := func(rc int, reason string) (*Result, error) {
		res.ResponseCode = rc
		st.Phase, st.EndReason = PhaseEnded, reason
		events = append(events, Event{Type: "session.failed", Data: map[string]any{"reason": reason, "request": q.Type}})
		return finishDIN(r, res, events, true)
	}

	if q.Kind == exi.DINSessionSetup {
		// new session ID below
	} else if hex.EncodeToString(q.SessionID) != st.SessionID {
		return fail(exi.RCFailedUnknownSession, "unknown session id")
	}
	if !dinIsAllowed(st.Phase, q.Kind) {
		if q.Kind == exi.DINSessionStop {
			st.Phase, st.EndReason = PhaseEnded, "terminated by EV"
			return finishDIN(r, res, append(events, Event{Type: "session.ended", Data: map[string]any{"reason": st.EndReason}}), true)
		}
		if q.Kind == exi.DINOther || q.Kind == exi.DINUnknown {
			return fail(exi.RCFailed, "unknown message")
		}
		return fail(exi.RCFailedSequenceError, fmt.Sprintf("%s not allowed in %s", q.Type, st.Phase))
	}

	switch q.Kind {
	case exi.DINSessionSetup:
		sid := make([]byte, 8)
		env.Random(sid)
		st.SessionID = hex.EncodeToString(sid)
		res.SessionID = sid
		res.ResponseCode = exi.RCOKNewSession
		ts := env.Now().Unix()
		res.Timestamp = &ts
		st.EV.EVCCID = hex.EncodeToString(q.EVCCID)
		st.Phase = PhaseServiceDisc
		events = append(events, Event{Type: "session.started", Data: map[string]any{"evcc_id": st.EV.EVCCID, "protocol": "din70121", "purpose": "soc"}})

	case exi.DINServiceDiscovery:
		st.Phase = PhasePaymentSel

	case exi.DINServicePaymentSelection:
		if q.SelectedPaymentOption == nil || *q.SelectedPaymentOption != 1 {
			return fail(exi.RCFailedPaymentSelection, "payment option other than ExternalPayment")
		}
		st.Phase = PhaseAuthorization

	case exi.DINContractAuthentication:
		res.EVSEProcessing = exi.DINProcessFinished
		st.Phase = PhaseChargeParam

	case exi.DINChargeParameterDiscovery:
		if q.DC == nil {
			return fail(dinRCWrongEnergyTransferType, "no DC_EVChargeParameter")
		}
		d := q.DC
		soc := d.SoC
		st.EV.SoC = &soc
		mx, mv := d.MaxCurrent.Float(), d.MaxVoltage.Float()
		st.EV.EVMaxCurrentA, st.EV.EVMaxVoltageV = &mx, &mv
		if d.EnergyCapacity != nil {
			v := d.EnergyCapacity.Float()
			st.EV.EVEnergyCapacityWh = &v
		}
		if d.EnergyRequest != nil { // EVEnergyRequest, not EAmount: kept apart
			v := d.EnergyRequest.Float()
			st.EV.EVEnergyRequestWh = &v
		}
		if d.MaxPower != nil {
			v := d.MaxPower.Float()
			st.EV.EVMaxPowerW = &v
		}
		st.EV.FullSoC, st.EV.BulkSoC = d.FullSoC, d.BulkSoC
		// Offer sane DC limits, tell the EV to stop, and end the session.
		res.EVSEProcessing = exi.DINProcessFinished
		res.EVSENotification = exi.DINNotifStopCharging
		res.MaxCurrent = dinPV(cfg.MaxCurrentA, exi.DINUnitA, 1)
		res.MaxVoltage = dinPV(dinMaxVoltageV, exi.DINUnitV, 0)
		res.MinCurrent = dinPV(dinMinCurrentA, exi.DINUnitA, 0)
		res.MinVoltage = dinPV(dinMinVoltageV, exi.DINUnitV, 0)
		res.PeakRipple = dinPV(dinRippleA, exi.DINUnitA, 0)
		pw := cfg.MaxCurrentA * cfg.NominalVoltageV * float64(max(cfg.Phases, 1))
		res.PMaxW = math.Min(pw, math.MaxInt16)
		res.MaxPower = dinPV(pw, exi.DINUnitW, 0)
		st.Phase, st.EndReason = PhaseEnded, "SoC read (DIN 70121)"
		events = append(events,
			Event{Type: "session.ev_data", Data: st.EV},
			Event{Type: "soc.read", Data: map[string]any{"soc": soc, "next": "ac_or_basic"}},
			Event{Type: "session.ended", Data: map[string]any{"reason": st.EndReason}})
		return finishDIN(r, res, events, true)
	}
	return finishDIN(r, res, events, false)
}

func dinIsAllowed(p Phase, k exi.DINKind) bool {
	for _, a := range dinAllowed[p] {
		if a == k {
			return true
		}
	}
	return false
}

func finishDIN(r *Result, res *exi.DINResponse, events []Event, closeConn bool) (*Result, error) {
	b, err := exi.EncodeDINRes(res)
	if err != nil {
		return nil, fmt.Errorf("encode DIN %v: %w", res.Kind, err)
	}
	r.EXI, r.ResponseType, r.Close, r.Events = b, res.Type, closeConn, events
	return r, nil
}
