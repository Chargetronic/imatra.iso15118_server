// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

package secc

// ISO 15118-20, AC, EIM (ADR-004). The same pure step as ISO 15118-2: state
// in, response out. Message sequence after the handshake (V2G20, clause 8):
//
//	SessionSetup -> AuthorizationSetup -> Authorization -> ServiceDiscovery ->
//	ServiceDetail (one or more) -> ServiceSelection -> AC_ChargeParameterDiscovery ->
//	ScheduleExchange -> PowerDelivery(Start) -> AC_ChargeLoop ... ->
//	PowerDelivery(Stop) -> SessionStop
//
// SessionStop is accepted from AuthorizationSetup on. Schedule renegotiation
// runs on the multiplexed side stream 0x8101 next to the loop (V2G20-1045 to
// -1050): AC_ChargeParameterDiscovery -> ScheduleExchange -> PowerDelivery.
// Time stamps are microseconds of SECC time, rising with every message
// (V2G20-1534, -1537). Out of this edition: Plug & Charge
// (CertificateInstallation, PnC authorization, MeteringConfirmation), BPT
// (V2G), DC, value-added services, service renegotiation, resuming a paused
// session (a new one is set up instead, V2G20-2547).

import (
	"encoding/hex"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/chargetronic/imatra.iso15118_server/engine/internal/exi"
)

// Phases of ISO 15118-20 not shared with ISO 15118-2.
const (
	Phase20AuthSetup        Phase = "WaitAuthorizationSetup"
	Phase20Authorization    Phase = "WaitAuthorization20"
	Phase20ServiceDetail    Phase = "WaitServiceDetail"
	Phase20ServiceSelection Phase = "WaitServiceSelection"
	Phase20ScheduleExchange Phase = "WaitScheduleExchange"
)

// Parameter set IDs of the AC service: Dynamic first (an EV that takes the
// first set gets the mode in which the operator sets the power directly).
const (
	paramSetDynamic   = 1
	paramSetScheduled = 2
	scheduleTupleID   = 1
	minCurrentA       = 6  // IEC 61851-1: the lowest current a station offers
	notifyMaxDelayS   = 60 // EVSEStatus: time the EV has to react to a notification
)

var allowed20 = map[Phase][]exi.Kind20{
	PhaseSessionSetup:       {exi.K20SessionSetup},
	Phase20AuthSetup:        {exi.K20AuthorizationSetup, exi.K20SessionStop},
	Phase20Authorization:    {exi.K20Authorization, exi.K20SessionStop},
	PhaseServiceDisc:        {exi.K20ServiceDiscovery, exi.K20SessionStop},
	Phase20ServiceDetail:    {exi.K20ServiceDetail, exi.K20SessionStop},
	Phase20ServiceSelection: {exi.K20ServiceDetail, exi.K20ServiceSelection, exi.K20SessionStop},
	PhaseChargeParam:        {exi.K20ACChargeParamDisc, exi.K20SessionStop},
	Phase20ScheduleExchange: {exi.K20ScheduleExchange, exi.K20SessionStop},
	PhasePowerDelivery:      {exi.K20ScheduleExchange, exi.K20PowerDelivery, exi.K20SessionStop},
	PhaseCharging:           {exi.K20ACChargeLoop, exi.K20PowerDelivery, exi.K20SessionStop},
	PhaseStopped:            {exi.K20SessionStop}, // V2G20-1623
}

func isAllowed20(p Phase, k exi.Kind20) bool {
	for _, a := range allowed20[p] {
		if a == k {
			return true
		}
	}
	return false
}

// TLS13 reports whether the device's connection to the EV runs TLS 1.3: the
// only transport ISO 15118-20 allows, and the sign that the device's firmware
// can carry it (ADR-004: no separate capability flag).
func TLS13(tls string) bool {
	t := strings.ToLower(strings.ReplaceAll(tls, " ", ""))
	return strings.Contains(t, "tlsv1.3") || strings.Contains(t, "tls1.3") || strings.HasPrefix(t, "1.3")
}

func iso20Offered(st *State, cfg Config) bool { return cfg.ISO20 && TLS13(st.TLS) }

// IsISO20 reports whether the payload type is one of ISO 15118-20 (the
// CommonMessages or the AC stream).
func IsISO20(pt uint16) bool {
	return pt == exi.PayloadISO20Main || pt == exi.PayloadISO20AC || pt == exi.PayloadISO20Reneg
}

// decode20: the side stream carries messages of both schemas; which one is
// told by the renegotiation step expected (reneg) or, without a state, tried.
func decode20(pt uint16, reneg string, known bool, payload []byte) (*exi.Request20, error) {
	if pt != exi.PayloadISO20Reneg {
		return exi.DecodeISO20(pt, payload)
	}
	if known {
		if reneg == "" {
			return exi.DecodeISO20(exi.PayloadISO20AC, payload)
		}
		return exi.DecodeISO20(exi.PayloadISO20Main, payload)
	}
	if q, err := exi.DecodeISO20(exi.PayloadISO20AC, payload); err == nil && q.Kind == exi.K20ACChargeParamDisc {
		return q, nil
	}
	return exi.DecodeISO20(exi.PayloadISO20Main, payload)
}

// ts20: the next header time stamp of the session, microseconds of SECC time,
// higher than every one before (V2G20-1534, -1537).
func ts20(st *State, env Env) uint64 {
	t := uint64(env.Now().UnixMicro())
	if t <= st.LastTS {
		t = st.LastTS + 1
	}
	st.LastTS = t
	return t
}

// phases20: the station's phase count for the -20 power values.
func (cfg Config) phases20() int {
	if cfg.Phases == 3 {
		return 3
	}
	return 1
}

// powerW is the power the EV may take: effective current at the nominal voltage.
func (cfg Config) powerW(currentA float64) float64 {
	return currentA * cfg.NominalVoltageV * float64(cfg.phases20())
}

// targetPowerW: the station's effective power, not above what the EV said it
// can take (Dynamic: the EV follows the target).
func targetPowerW(st *State, cfg Config) float64 {
	p := cfg.powerW(cfg.effectiveCurrent())
	if st.EV.EVMaxPowerW != nil && *st.EV.EVMaxPowerW > 0 && *st.EV.EVMaxPowerW < p {
		p = *st.EV.EVMaxPowerW
	}
	return math.Max(0, p)
}

func rat(v float64) exi.Rat { return exi.MakeRat(v) }

func fptr(v float64) *float64 { return &v }

func stepISO20(st *State, cfg Config, env Env, pt uint16, payload []byte) (*Result, error) {
	if pt == exi.PayloadISO20Reneg {
		return stepReneg20(st, cfg, env, payload)
	}
	q, err := exi.DecodeISO20(pt, payload)
	if err != nil {
		return nil, err
	}
	if q.Kind == exi.K20Unknown || q.Kind == exi.K20Other {
		// No response type of this edition fits: the connection is closed.
		return nil, fmt.Errorf("ISO 15118-20 %s not supported", q.Type)
	}
	res, events := step20(st, cfg, env, q)
	if q.Kind == exi.K20ACChargeLoop && res.ResponseCode == exi.RC20OK {
		// The cache pair's request: its prefix (message type + session ID)
		// is what the device compares (ADR-005). Not cachable: no pair, the
		// EV's loop goes to the server as every other request.
		if bits, err := exi.TimeStampBit(pt, payload, false); err == nil {
			st.CLReq, st.CLReqBits = hex.EncodeToString(payload), bits
		}
	}
	out, rpt, err := exi.EncodeISO20Res(res)
	if err != nil {
		return nil, fmt.Errorf("encode %v: %w", res.Kind, err)
	}
	return &Result{
		EXI: out, PT: rpt, Request: q, Response: res,
		RequestType: q.Type, ResponseType: res.Type,
		Close: closeAfter20(st, res), Events: events,
	}, nil
}

// closeAfter20: after a FAILED response the SECC closes the connection
// (V2G20-2222); after SessionStopRes OK it waits for the EV to close it (at
// least 5 s, V2G20-1633), which the device does on the EV's side.
func closeAfter20(st *State, res *exi.Response20) bool {
	return st.Phase == PhaseEnded && exi.Failed20(res.ResponseCode)
}

func step20(st *State, cfg Config, env Env, q *exi.Request20) (*exi.Response20, []Event) {
	res := &exi.Response20{Kind: q.Kind, SessionID: st.sessionIDBytes(), TimeStamp: ts20(st, env),
		ResponseCode: exi.RC20OK}
	var events []Event

	fail := func(rc int, reason string) (*exi.Response20, []Event) {
		res.ResponseCode = rc
		st.Phase, st.EndReason = PhaseEnded, reason
		fill20(res, cfg, st)
		return res, append(events, Event{Type: "session.failed", Data: map[string]any{"reason": reason, "request": q.Type}})
	}

	if q.Kind != exi.K20SessionSetup && hex.EncodeToString(q.SessionID) != st.SessionID {
		return fail(exi.RC20FailedUnknownSession, "unknown session id")
	}
	if !isAllowed20(st.Phase, q.Kind) {
		return fail(exi.RC20FailedSequenceError, fmt.Sprintf("%s not allowed in %s", q.Type, st.Phase))
	}

	switch q.Kind {
	case exi.K20SessionSetup:
		// A new session (no resumption in this edition): an ID that is
		// neither zero nor the one the EV sent (V2G20-2547, -2627).
		sid := make([]byte, 8)
		for {
			env.Random(sid)
			if !allZero(sid) && hex.EncodeToString(sid) != hex.EncodeToString(q.SessionID) {
				break
			}
		}
		st.SessionID = hex.EncodeToString(sid)
		res.SessionID = sid
		res.ResponseCode = exi.RC20OKNewSession
		res.EVSEID = cfg.EVSEID
		st.EV.EVCCID = q.EVCCID
		st.Phase = Phase20AuthSetup
		events = append(events, Event{Type: "session.started", Data: map[string]any{"evcc_id": st.EV.EVCCID}})

	case exi.K20AuthorizationSetup:
		st.Phase = Phase20Authorization // EIM only (filled by fill20)

	case exi.K20Authorization:
		if q.SelectedAuthorization == nil || *q.SelectedAuthorization != 0 {
			return fail(exi.RC20Failed, "authorization other than EIM")
		}
		res.EVSEProcessing = 0 // Finished: home charger, EIM, no operator
		st.Phase = PhaseServiceDisc

	case exi.K20ServiceDiscovery:
		res.Services = []uint16{exi.ServiceAC}
		st.Phase = Phase20ServiceDetail

	case exi.K20ServiceDetail:
		if q.ServiceID == nil || *q.ServiceID != exi.ServiceAC {
			return fail(exi.RC20FailedServiceIDInvalid, "service detail of a service not offered")
		}
		res.ServiceID = exi.ServiceAC
		res.ParamSets = paramSets(cfg)
		st.Phase = Phase20ServiceSelection

	case exi.K20ServiceSelection:
		if q.SelectedServiceID == nil || *q.SelectedServiceID != exi.ServiceAC {
			return fail(exi.RC20FailedNoEnergyTransferService, "energy transfer service other than AC")
		}
		switch *q.SelectedParameterSetID {
		case paramSetDynamic:
			st.ControlMode = exi.ModeDynamic
		case paramSetScheduled:
			st.ControlMode = exi.ModeScheduled
		default:
			return fail(exi.RC20FailedServiceSelectionInvalid, "parameter set not offered")
		}
		if q.SelectedVAS > 0 {
			return fail(exi.RC20FailedServiceSelectionInvalid, "value-added service not offered")
		}
		st.Phase = PhaseChargeParam

	case exi.K20ACChargeParamDisc:
		if q.BPT || q.MaxPower == nil {
			return fail(exi.RC20FailedWrongChargeParameter, "BPT or no AC charge parameters (AC only)")
		}
		st.EV.EVMaxPowerW = fptr(q.MaxPower.Float())
		if q.MinPower != nil {
			st.EV.EVMinPowerW = fptr(q.MinPower.Float())
		}
		st.Phase = Phase20ScheduleExchange

	case exi.K20ScheduleExchange:
		if q.ControlMode != st.ControlMode {
			return fail(exi.RC20Failed, "control mode other than the selected one")
		}
		ev20Needs(&st.EV, q)
		res.ControlMode = st.ControlMode
		res.EVSEProcessing = 0 // Finished
		if p, scheduled := schedule20(res, cfg, st); scheduled {
			st.SchedPowerW = p
		}
		st.Phase = PhasePowerDelivery
		events = append(events, Event{Type: "session.ev_data", Data: st.EV})

	case exi.K20PowerDelivery:
		if q.EVProcessing != nil && *q.EVProcessing != 0 {
			break // the EV is still preparing its power profile: answer, stay
		}
		switch *q.ChargeProgress {
		case exi.Progress20Start:
			if st.Phase == PhaseCharging {
				return fail(exi.RC20FailedSequenceError, "PowerDelivery(Start) while charging")
			}
			if st.ControlMode == exi.ModeScheduled && (q.SelectedScheduleTuple == nil || *q.SelectedScheduleTuple != scheduleTupleID) {
				return fail(exi.RC20FailedScheduleSelectionInvalid, "schedule tuple not offered")
			}
			st.Phase = PhaseCharging
			events = append(events, Event{Type: "charging.started"})
		case exi.Progress20Stop:
			st.Phase = PhaseStopped
			events = append(events, Event{Type: "charging.stopped"})
		case exi.Progress20Standby:
			// The EV stops drawing current and keeps the loop: the next
			// request is AC_ChargeLoopReq (V2G20-1194, -2192).
			if st.Phase != PhaseCharging && st.Phase != PhasePowerDelivery {
				return fail(exi.RC20FailedSequenceError, "Standby outside the charge loop")
			}
			st.Phase = PhaseCharging
		case exi.Progress20ScheduleRenegotiation:
			// Renegotiation runs on the side stream 0x8101, not on the main
			// stream (V2G20-1045, -2018).
			return fail(exi.RC20FailedSequenceError, "ScheduleRenegotiation on the main stream")
		}

	case exi.K20ACChargeLoop:
		st.ChargingStatusCount++
		ev20Loop(&st.EV, q)
		if q.MeterInfoRequested {
			st.MeterRequested = true // from now on in every answer (V2G20-1082)
		}

	case exi.K20SessionStop:
		switch {
		case q.ChargingSession != nil && *q.ChargingSession == exi.Session20Pause:
			st.EndReason = "paused by EV"
		default:
			st.EndReason = "terminated by EV"
		}
		st.Phase = PhaseEnded
		events = append(events, Event{Type: "session.ended", Data: map[string]any{"reason": st.EndReason}})
	}
	fill20(res, cfg, st)
	return res, events
}

func paramSets(cfg Config) []exi.ParamSet {
	conn := 1
	if cfg.Phases == 3 {
		conn = 2
	}
	v := int(math.Round(cfg.NominalVoltageV))
	return []exi.ParamSet{
		{ID: paramSetDynamic, Connector: conn, ControlMode: exi.ModeDynamic, NominalVoltage: v, MobilityNeeds: 1},
		{ID: paramSetScheduled, Connector: conn, ControlMode: exi.ModeScheduled, NominalVoltage: v, MobilityNeeds: 1},
	}
}

// fill20 sets what every response of that type must carry, also on failure.
func fill20(res *exi.Response20, cfg Config, st *State) {
	switch res.Kind {
	case exi.K20SessionSetup:
		if res.EVSEID == "" {
			res.EVSEID = cfg.EVSEID
		}
	case exi.K20ServiceDiscovery:
		if len(res.Services) == 0 {
			res.Services = []uint16{exi.ServiceAC}
		}
	case exi.K20ServiceDetail:
		res.ServiceID = exi.ServiceAC
		if len(res.ParamSets) == 0 {
			res.ParamSets = paramSets(cfg)
		}
	case exi.K20ACChargeParamDisc:
		mx := rat(cfg.powerW(cfg.effectiveCurrent()))
		mn := rat(cfg.powerW(math.Min(minCurrentA, cfg.MaxCurrentA)))
		fq := exi.Rat{Value: 50}
		res.EVSEMaxPower, res.EVSEMinPower, res.EVSENominalFrequency = &mx, &mn, &fq
	case exi.K20ScheduleExchange:
		if res.ControlMode == exi.ModeNone {
			res.ControlMode = st.ControlMode
			if res.ControlMode == exi.ModeNone {
				res.ControlMode = exi.ModeDynamic
			}
		}
		if res.ControlMode == exi.ModeScheduled && len(res.Schedule) == 0 {
			schedule20(res, cfg, st)
		}
	case exi.K20ACChargeLoop:
		loopRes20(res, cfg, st, -1)
	}
}

// loopRes20 fills AC_ChargeLoopRes: Dynamic carries the target power;
// Scheduled asks the EV to renegotiate when the station's power is no longer
// the one in the schedule it follows. notification >= 0 forces one (the
// termination scenario: Terminate).
func loopRes20(res *exi.Response20, cfg Config, st *State, notification int) {
	res.ControlMode = st.ControlMode
	if res.ControlMode == exi.ModeNone {
		res.ControlMode = exi.ModeDynamic
	}
	res.Notification, res.NotificationMaxDelay, res.TargetPower, res.Meter = nil, 0, nil, nil
	if res.ControlMode == exi.ModeDynamic {
		tp := rat(targetPowerW(st, cfg))
		if notification == exi.Notif20Terminate {
			tp = exi.Rat{} // Terminate while decreasing the power (V2G20-1959)
		}
		res.TargetPower = &tp
	} else if st.Reneg == "" && math.Abs(cfg.powerW(cfg.effectiveCurrent())-st.SchedPowerW) >= 1 {
		// The station's power is no longer the schedule's: the EV renegotiates
		// on the side stream (V2G20-1845); not asked again while it does.
		n := exi.Notif20ScheduleRenegotiation
		res.Notification, res.NotificationMaxDelay = &n, notifyMaxDelayS
	}
	if notification >= 0 {
		n := notification
		res.Notification, res.NotificationMaxDelay = &n, 0
	}
	if st.MeterRequested {
		res.Meter = &exi.MeterInfo20{MeterID: meterID(cfg), ChargedWh: uint64(math.Max(0, math.Round(st.MeterWh)))}
	}
}

// meterID: the station's identity. There is no certified meter: the energy
// comes from the device's current readings at the nominal voltage.
func meterID(cfg Config) string {
	id := cfg.EVSEID
	if id == "" {
		id = "imatra"
	}
	if len(id) > 32 {
		id = id[:32]
	}
	return id
}

// schedule20 fills the ScheduleExchangeRes. Scheduled: one tuple, one entry
// at the station's power for a day from now (TimeAnchor: microseconds of SECC
// time, V2G20-1861). No power: GoToPause and the entry at 0 W (V2G20-2121).
// Returns the schedule's power and whether a schedule was given.
func schedule20(res *exi.Response20, cfg Config, st *State) (float64, bool) {
	p := math.Max(0, cfg.powerW(cfg.effectiveCurrent()))
	res.GoToPause = p < 1
	if res.ControlMode != exi.ModeScheduled {
		return p, false
	}
	res.ScheduleTuple, res.TimeAnchor = scheduleTupleID, res.TimeStamp
	res.Schedule = []exi.PowerEntry{{DurationS: 86400, Power: rat(p)}}
	return p, true
}

func allZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

// stepReneg20: schedule renegotiation on the side stream 0x8101 while the
// loop goes on (V2G20-1045 to -1050): AC_ChargeParameterDiscovery ->
// ScheduleExchange -> PowerDelivery; the new schedule is in force from the
// PowerDelivery on. Answers go on the same payload type. The phase stays
// Charging. A renegotiation the station wants is started by the EV
// (EVSENotification ScheduleRenegotiation in the loop answer).
func stepReneg20(st *State, cfg Config, env Env, payload []byte) (*Result, error) {
	q, err := decode20(exi.PayloadISO20Reneg, st.Reneg, true, payload)
	if err != nil {
		return nil, err
	}
	if q.Kind == exi.K20Unknown || q.Kind == exi.K20Other {
		return nil, fmt.Errorf("ISO 15118-20 %s on the renegotiation stream not supported", q.Type)
	}
	res := &exi.Response20{Kind: q.Kind, SessionID: st.sessionIDBytes(), TimeStamp: ts20(st, env),
		ResponseCode: exi.RC20OK}
	var events []Event
	fail := func(rc int, reason string) {
		res.ResponseCode = rc
		st.Reneg = ""
		st.Phase, st.EndReason = PhaseEnded, reason
		events = append(events, Event{Type: "session.failed", Data: map[string]any{"reason": reason, "request": q.Type}})
	}
	expect := map[string]exi.Kind20{"": exi.K20ACChargeParamDisc, "schedule": exi.K20ScheduleExchange,
		"delivery": exi.K20PowerDelivery}[st.Reneg]
	switch {
	case hex.EncodeToString(q.SessionID) != st.SessionID:
		fail(exi.RC20FailedUnknownSession, "unknown session id")
	case st.Phase != PhaseCharging || q.Kind != expect:
		fail(exi.RC20FailedSequenceError, fmt.Sprintf("%s on the renegotiation stream in %s/%q", q.Type, st.Phase, st.Reneg))
	case q.Kind == exi.K20ACChargeParamDisc:
		if q.BPT || q.MaxPower == nil {
			fail(exi.RC20FailedWrongChargeParameter, "BPT or no AC charge parameters (AC only)")
			break
		}
		st.EV.EVMaxPowerW = fptr(q.MaxPower.Float())
		if q.MinPower != nil {
			st.EV.EVMinPowerW = fptr(q.MinPower.Float())
		}
		st.Reneg = "schedule"
	case q.Kind == exi.K20ScheduleExchange:
		if q.ControlMode != st.ControlMode {
			fail(exi.RC20Failed, "control mode other than the selected one")
			break
		}
		ev20Needs(&st.EV, q)
		res.ControlMode = st.ControlMode
		st.RenegPowerW, _ = schedule20(res, cfg, st)
		st.Reneg = "delivery"
		events = append(events, Event{Type: "session.ev_data", Data: st.EV})
	case q.Kind == exi.K20PowerDelivery:
		if q.EVProcessing != nil && *q.EVProcessing != 0 {
			break // still preparing its profile: answer, stay
		}
		if st.ControlMode == exi.ModeScheduled && (q.SelectedScheduleTuple == nil || *q.SelectedScheduleTuple != scheduleTupleID) {
			fail(exi.RC20FailedScheduleSelectionInvalid, "schedule tuple not offered")
			break
		}
		st.SchedPowerW, st.Reneg = st.RenegPowerW, ""
		events = append(events, Event{Type: "schedule.renegotiated", Data: map[string]any{"power_w": st.SchedPowerW}})
	}
	fill20(res, cfg, st)
	out, _, err := exi.EncodeISO20Res(res)
	if err != nil {
		return nil, fmt.Errorf("encode %v: %w", res.Kind, err)
	}
	return &Result{
		EXI: out, PT: exi.PayloadISO20Reneg, Request: q, Response: res,
		RequestType: q.Type, ResponseType: res.Type,
		Close: closeAfter20(st, res), Events: events,
	}, nil
}

// ev20Needs: the EV's mobility needs from ScheduleExchangeReq. EAmountWh is
// the target energy request, so the activation pool reads one field for both
// protocol versions; the -20 values are kept apart as well.
func ev20Needs(ev *EVData, q *exi.Request20) {
	if q.TargetEnergy != nil {
		ev.EVTargetEnergyWh = fptr(q.TargetEnergy.Float())
		ev.EAmountWh = fptr(q.TargetEnergy.Float())
	}
	if q.MaxEnergy != nil {
		ev.EVMaxEnergyWh = fptr(q.MaxEnergy.Float())
	}
	if q.MinEnergy != nil {
		ev.EVMinEnergyWh = fptr(q.MinEnergy.Float())
	}
	if q.DepartureTime != nil {
		d := *q.DepartureTime
		ev.DepartureTimeS = &d
	}
	if q.TargetSoC != nil {
		v := *q.TargetSoC
		ev.TargetSoC = &v
	}
	if q.MinSoC != nil {
		v := *q.MinSoC
		ev.MinSoC = &v
	}
}

// ev20Loop: what the EV reports in the loop (SoC if it shows it; in Dynamic
// mode its needs in every request).
func ev20Loop(ev *EVData, q *exi.Request20) {
	ev20Needs(ev, q)
	if q.PresentSoC != nil {
		v := *q.PresentSoC
		ev.SoC = &v
	}
	if q.MaxPower != nil {
		ev.EVMaxPowerW = fptr(q.MaxPower.Float())
	}
	if q.BatteryCapacity != nil {
		ev.EVEnergyCapacityWh = fptr(q.BatteryCapacity.Float())
	}
}

// LoopSample20 decodes a -20 loop request the device answered from its cache
// and forwarded as a sample: the EV's current report, without a step.
func LoopSample20(pt uint16, payload []byte) (*exi.Request20, error) {
	q, err := exi.DecodeISO20(pt, payload)
	if err != nil {
		return nil, err
	}
	if q.Kind != exi.K20ACChargeLoop {
		return nil, fmt.Errorf("not a loop request: %s", q.Type)
	}
	return q, nil
}

// pair20 builds the -20 Charging Loop pair (ADR-005): the response to the
// session's loop under the current limits and the termination scenario, each
// with the bit offset of its time stamp, which the device overwrites with the
// current time when it answers.
func pair20(st *State, cfg Config, now time.Time) (*Pair, error) {
	// Higher than every time stamp the EV has seen; the device writes a
	// higher one again each time it answers from the pair (V2G20-1537).
	ts := uint64(now.UnixMicro())
	if ts <= st.LastTS {
		ts = st.LastTS + 1
	}
	st.LastTS = ts
	enc := func(r *exi.Response20) ([]byte, uint16, int, error) {
		b, pt, err := exi.EncodeISO20Res(r)
		if err != nil {
			return nil, 0, 0, err
		}
		at, err := exi.TimeStampBit(pt, b, true)
		return b, pt, at, err
	}
	loop := func(notification int) *exi.Response20 {
		r := &exi.Response20{Kind: exi.K20ACChargeLoop, SessionID: st.sessionIDBytes(), TimeStamp: ts, ResponseCode: exi.RC20OK}
		loopRes20(r, cfg, st, notification)
		return r
	}
	p := &Pair{TS: ts, ReqBits: st.CLReqBits}
	var err error
	if p.Res, p.PT, p.ResTS, err = enc(loop(-1)); err != nil {
		return nil, err
	}
	for _, r := range []*exi.Response20{
		loop(exi.Notif20Terminate),
		{Kind: exi.K20PowerDelivery, SessionID: st.sessionIDBytes(), TimeStamp: ts, ResponseCode: exi.RC20OK},
		{Kind: exi.K20SessionStop, SessionID: st.sessionIDBytes(), TimeStamp: ts, ResponseCode: exi.RC20OK},
	} {
		b, pt, at, err := enc(r)
		if err != nil {
			return nil, err
		}
		p.Term, p.TermPT, p.TermTS = append(p.Term, b), append(p.TermPT, pt), append(p.TermTS, at)
	}
	// The key ignores the time stamp: a new pair only when the answer changes.
	k := append([]byte(nil), p.Res...)
	if err := exi.WriteTimeStamp(k, p.ResTS, exi.TimeStampMin); err != nil {
		return nil, err
	}
	p.Key = hex.EncodeToString(k)
	return p, nil
}
