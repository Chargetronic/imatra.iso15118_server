// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

// Package secc is the ISO 15118-2 SECC state machine of the cloud engine.
//
// It is a pure function: Step(state, config, message) -> (new state, response).
// No I/O, no clocks except the injected Now, no goroutines. All session state
// lives in State, which the caller loads before and stores after every step,
// so any engine instance can serve any message (stateless engine).
//
// Scope: ISO 15118-2, AC, EIM (ExternalPayment); ISO 15118-20, AC, EIM
// (iso20.go); DIN 70121 only to read the EV's SoC (din.go).
package secc

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/chargetronic/imatra.iso15118_server/engine/internal/exi"
)

// Phase is the position of the session in the ISO 15118-2 message sequence
// (V2G2-538: any other request in a phase is a sequence error).
type Phase string

const (
	PhaseAppHand       Phase = "WaitSupportedAppProtocol"
	PhaseSessionSetup  Phase = "WaitSessionSetup"
	PhaseServiceDisc   Phase = "WaitServiceDiscovery"
	PhasePaymentSel    Phase = "WaitPaymentServiceSelection"
	PhaseAuthorization Phase = "WaitAuthorization"
	PhaseChargeParam   Phase = "WaitChargeParameterDiscovery"
	PhasePowerDelivery Phase = "WaitPowerDelivery"
	PhaseCharging      Phase = "Charging"
	PhaseStopped       Phase = "ChargingStopped" // after PowerDelivery(Stop)
	PhaseEnded         Phase = "Ended"
)

// Station-side limits and identity, from the device configuration.
type Config struct {
	EVSEID          string  `json:"evse_id"`
	MaxCurrentA     float64 `json:"max_current_a"`     // station limit (its PWM), never exceeded
	NominalVoltageV float64 `json:"nominal_voltage_v"` // no voltage measurement: nominal
	Phases          int     `json:"phases"`            // 1 or 3
	// LimitA is the active power limit from a partner / the UI, in amps per
	// phase. 0 = none. Never above MaxCurrentA.
	LimitA float64 `json:"limit_a"`
	// The operator command behind LimitA (engine bookkeeping, not encoded):
	// its id goes with the Charging Loop pair and comes back in the ack.
	CommandID   int64     `json:"-"`
	LimitSource string    `json:"-"`
	LimitUntil  time.Time `json:"-"`
	LimitPrio   int       `json:"-"`
	// ISO20: offer ISO 15118-20 AC in the handshake (ISO20_ENABLED; engine
	// setting, not stored). Offered only on a TLS 1.3 connection (ADR-004).
	ISO20 bool `json:"-"`
}

// EV data collected during the session; what partners and the UI care about.
type EVData struct {
	EVCCID         string   `json:"evcc_id,omitempty"`
	EnergyMode     *int     `json:"energy_transfer_mode,omitempty"`
	EAmountWh      *float64 `json:"e_amount_wh,omitempty"`
	DepartureTimeS *uint32  `json:"departure_time_s,omitempty"`
	EVMaxCurrentA  *float64 `json:"ev_max_current_a,omitempty"`
	EVMinCurrentA  *float64 `json:"ev_min_current_a,omitempty"`
	EVMaxVoltageV  *float64 `json:"ev_max_voltage_v,omitempty"`
	SoC            *int8    `json:"soc,omitempty"`
	// DIN 70121 (SoC read): DC_EVChargeParameter. Raw, each value in its own
	// field: the energy need is derived from them outside the engine (vehicle
	// history per EVCCID, decision 01.11.2026), never mixed with EAmount.
	EVEnergyRequestWh  *float64           `json:"ev_energy_request_wh,omitempty"`
	EVEnergyCapacityWh *float64           `json:"ev_energy_capacity_wh,omitempty"`
	EVMaxPowerW        *float64           `json:"ev_max_power_w,omitempty"`
	FullSoC            *int8              `json:"full_soc,omitempty"`
	BulkSoC            *int8              `json:"bulk_soc,omitempty"`
	ChargingProfile    []exi.ProfileEntry `json:"charging_profile,omitempty"`
	// ISO 15118-20 (ScheduleExchange, AC_ChargeLoop): the EV's energy
	// requests and limits; EAmountWh carries the target energy as well.
	EVMinPowerW      *float64 `json:"ev_min_power_w,omitempty"`
	EVTargetEnergyWh *float64 `json:"ev_target_energy_wh,omitempty"`
	EVMaxEnergyWh    *float64 `json:"ev_max_energy_wh,omitempty"`
	EVMinEnergyWh    *float64 `json:"ev_min_energy_wh,omitempty"`
	TargetSoC        *int8    `json:"target_soc,omitempty"`
	MinSoC           *int8    `json:"min_soc,omitempty"`
}

// State is everything the engine needs to continue a V2G session.
type State struct {
	Phase    Phase  `json:"phase"`
	Protocol string `json:"protocol,omitempty"` // namespace agreed in the handshake
	SchemaID int    `json:"schema_id"`
	// OffersISO2: the EV offered ISO 15118-2 in the handshake. After a DIN
	// session (the SoC read) such an EV is restarted for AC; one that offers
	// only DIN 70121 goes to basic charging on the station's PWM.
	OffersISO2 bool   `json:"offers_iso2,omitempty"`
	SessionID  string `json:"session_id,omitempty"` // hex, 8 bytes
	// Plug: the device's plug-in id this session runs in (ADR-003); set by
	// the engine from the device's messages, not used by the state machine.
	Plug                string    `json:"plug,omitempty"`
	TupleID             uint8     `json:"sa_schedule_tuple_id,omitempty"`
	EV                  EVData    `json:"ev"`
	StartedAt           time.Time `json:"started_at"`
	EndReason           string    `json:"end_reason,omitempty"`
	ChargingStatusCount int       `json:"charging_status_count"`
	// Charging Loop cache of the device: the EV's ChargingStatusReq as
	// received (hex; constant within a session) and the response last given
	// to the device for it. The device compares bytes, it decodes nothing.
	CLReq string `json:"cl_req,omitempty"`
	CLRes string `json:"cl_res,omitempty"`
	// EVSEMaxCurrent in the device's pair: lets the journal show what a
	// cached answer carried without decoding it.
	CLMaxCurrent *exi.PV `json:"cl_evse_max_current,omitempty"`
	// ISO 15118-20 (ADR-004, ADR-005). TLS: the device's transport to the EV
	// as reported when the connection opened ("" = TCP). CLReqBits: the
	// leading bits of CLReq the device compares (message type, session ID).
	// ControlMode: the selected one; SchedPowerW: the power in the schedule
	// given to the EV (Scheduled mode).
	TLS         string  `json:"tls,omitempty"`
	CLReqBits   int     `json:"cl_req_bits,omitempty"`
	ControlMode int     `json:"control_mode,omitempty"`
	SchedPowerW float64 `json:"sched_power_w,omitempty"`
}

func NewState(now time.Time) *State {
	return &State{Phase: PhaseAppHand, SchemaID: -1, StartedAt: now}
}

// Resume prepares the state of a connection the server has no session for
// (its state was lost with Valkey, ADR-002) when its first request is not a
// supportedAppProtocolReq: the protocol is told by the message itself, and
// the session goes on from SessionSetup. Then the normal step answers
//   - SessionSetupReq: a new session, OK_NewSessionEstablished (V2G2-756);
//   - any other request: FAILED_UnknownSession (V2G2-460) and the
//     connection is closed (V2G2-539); the EV starts a new session.
//
// False: the payload is a supportedAppProtocolReq (a new connection) or
// cannot be decoded at all; the state is left as it is.
func Resume(st *State, payload []byte) bool { return ResumePT(st, exi.PayloadEXI, payload) }

// ResumePT is Resume for a request of the given V2GTP payload type: an
// ISO 15118-20 request goes on from SessionSetup the same way (a new session,
// or FAILED_UnknownSession).
func ResumePT(st *State, pt uint16, payload []byte) bool {
	if IsISO20(pt) {
		if _, err := exi.DecodeISO20(pt, payload); err == nil {
			st.Protocol, st.Phase = exi.NamespaceISO20AC, PhaseSessionSetup
			return true
		}
		return false
	}
	if _, err := exi.DecodeAppHand(payload); err == nil {
		return false
	}
	if _, err := exi.DecodeISO2(payload); err == nil {
		st.Protocol, st.Phase = exi.NamespaceISO2, PhaseSessionSetup
		return true
	}
	if _, err := exi.DecodeDIN(payload); err == nil {
		st.Protocol, st.Phase = exi.NamespaceDIN, PhaseSessionSetup
		return true
	}
	return false
}

// Result of one step.
type Result struct {
	// EXI to send back to the EV. Nil when nothing must be sent.
	EXI []byte
	// PT: the V2GTP payload type of EXI (0x8001; ISO 15118-20: 0x8002 or 0x8003).
	PT uint16
	// Response, decoded form of EXI (for the message journal / UI).
	Response any
	// Request, decoded form of the input.
	Request any
	// RequestType / ResponseType names, e.g. "ChargingStatusReq".
	RequestType, ResponseType string
	// Close: the session is over; the transport may close the TCP connection
	// after sending EXI.
	Close bool
	// Events worth publishing (session.started, ev data known, ...).
	Events []Event
}

type Event struct {
	Type string `json:"type"`
	Data any    `json:"data,omitempty"`
}

// Env is what Step needs from the outside world.
type Env struct {
	Now    func() time.Time
	Random func([]byte) // fills session IDs
}

func DefaultEnv() Env {
	return Env{Now: time.Now, Random: func(b []byte) { _, _ = rand.Read(b) }}
}

const chargeServiceID = 1

// Step processes one EXI message (V2GTP payload type 0x8001) of the session.
func Step(st *State, cfg Config, env Env, payload []byte) (*Result, error) {
	return StepPT(st, cfg, env, exi.PayloadEXI, payload)
}

// StepPT processes one message of the given V2GTP payload type.
func StepPT(st *State, cfg Config, env Env, pt uint16, payload []byte) (*Result, error) {
	r, err := stepPT(st, cfg, env, pt, payload)
	if r != nil && r.PT == 0 {
		r.PT = exi.PayloadEXI
	}
	return r, err
}

func stepPT(st *State, cfg Config, env Env, pt uint16, payload []byte) (*Result, error) {
	if st.Phase == PhaseAppHand {
		if pt != exi.PayloadEXI {
			return nil, fmt.Errorf("payload type 0x%04x before the handshake", pt)
		}
		return stepAppHand(st, cfg, payload)
	}
	if st.Phase == PhaseEnded {
		return nil, fmt.Errorf("session already ended (%s)", st.EndReason)
	}
	if st.Protocol == exi.NamespaceISO20AC {
		if !IsISO20(pt) {
			return nil, fmt.Errorf("payload type 0x%04x in an ISO 15118-20 session", pt)
		}
		return stepISO20(st, cfg, env, pt, payload)
	}
	if pt != exi.PayloadEXI {
		return nil, fmt.Errorf("payload type 0x%04x in a %s session", pt, st.Protocol)
	}
	if st.Protocol == exi.NamespaceDIN {
		return stepDIN(st, cfg, env, payload)
	}
	if st.Protocol != exi.NamespaceISO2 {
		return nil, fmt.Errorf("protocol %q not supported by the engine yet", st.Protocol)
	}
	req, err := exi.DecodeISO2(payload)
	if err != nil {
		return nil, err
	}
	res, events := stepISO2(st, cfg, env, req)
	if req.Kind == exi.KindChargingStatus && res.ResponseCode == exi.RCOK {
		st.CLReq = hex.EncodeToString(payload)
	}
	out, err := exi.EncodeISO2Res(res)
	if err != nil {
		return nil, fmt.Errorf("encode %v: %w", res.Kind, err)
	}
	return &Result{
		EXI: out, Request: req, Response: res,
		RequestType: req.Type, ResponseType: res.Type,
		Close: st.Phase == PhaseEnded, Events: events,
	}, nil
}

func stepAppHand(st *State, cfg Config, payload []byte) (*Result, error) {
	protos, err := exi.DecodeAppHand(payload)
	if err != nil {
		return nil, err
	}
	// ISO 15118-20 AC first when it is enabled and the connection runs
	// TLS 1.3 (ADR-004), then ISO 15118-2 (AC), highest priority first; DIN
	// 70121 only when the EV offers neither (a DC-only EV: the SoC read, then
	// basic charging). A digital session is never left for a SoC read.
	best := -1
	order := []string{exi.NamespaceISO2, exi.NamespaceDIN}
	if iso20Offered(st, cfg) {
		order = append([]string{exi.NamespaceISO20AC}, order...)
	}
	major := map[string]uint32{exi.NamespaceISO20AC: 1, exi.NamespaceISO2: 2, exi.NamespaceDIN: 2}
	for _, ns := range order {
		for i, p := range protos {
			if p.Namespace == ns && p.Major == major[ns] {
				if best < 0 || p.Priority < protos[best].Priority {
					best = i
				}
			}
		}
		if best >= 0 {
			break
		}
	}
	type appHandRes struct {
		Type         string `json:"type"`
		ResponseCode string `json:"response_code"`
		SchemaID     *int   `json:"schema_id,omitempty"`
	}
	r := &Result{Request: map[string]any{"type": "supportedAppProtocolReq", "app_protocols": protos},
		RequestType: "supportedAppProtocolReq", ResponseType: "supportedAppProtocolRes"}
	if best < 0 {
		r.EXI, err = exi.EncodeAppHandRes(exi.AppHandFailedNoNegotiation, -1)
		r.Response = appHandRes{Type: r.ResponseType, ResponseCode: "Failed_NoNegotiation"}
		st.Phase, st.EndReason = PhaseEnded, "no supported protocol offered"
		r.Close = true
		return r, err
	}
	for _, q := range protos {
		if (q.Namespace == exi.NamespaceISO2 && q.Major == 2) || (q.Namespace == exi.NamespaceISO20AC && q.Major == 1) {
			st.OffersISO2 = true // an AC digital session exists for this EV
		}
	}
	p := protos[best]
	code, codeName := exi.AppHandOK, "OK_SuccessfulNegotiation"
	if p.Minor != 0 {
		code, codeName = exi.AppHandOKMinorDev, "OK_SuccessfulNegotiationWithMinorDeviation"
	}
	r.EXI, err = exi.EncodeAppHandRes(code, int(p.SchemaID))
	sid := int(p.SchemaID)
	r.Response = appHandRes{Type: r.ResponseType, ResponseCode: codeName, SchemaID: &sid}
	st.Protocol, st.SchemaID, st.Phase = p.Namespace, sid, PhaseSessionSetup
	return r, err
}

// allowed lists the requests accepted in each phase.
var allowed = map[Phase][]exi.Kind{
	PhaseSessionSetup:  {exi.KindSessionSetup},
	PhaseServiceDisc:   {exi.KindServiceDiscovery},
	PhasePaymentSel:    {exi.KindPaymentServiceSelection},
	PhaseAuthorization: {exi.KindAuthorization},
	PhaseChargeParam:   {exi.KindChargeParameterDiscovery},
	PhasePowerDelivery: {exi.KindChargeParameterDiscovery, exi.KindPowerDelivery},
	PhaseCharging:      {exi.KindChargingStatus, exi.KindPowerDelivery},
	PhaseStopped:       {exi.KindSessionStop, exi.KindChargeParameterDiscovery, exi.KindPowerDelivery},
}

func isAllowed(p Phase, k exi.Kind) bool {
	for _, a := range allowed[p] {
		if a == k {
			return true
		}
	}
	return false
}

func (st *State) sessionIDBytes() []byte {
	b, _ := hex.DecodeString(st.SessionID)
	return b
}

func (cfg Config) effectiveCurrent() float64 {
	c := cfg.MaxCurrentA
	if cfg.LimitA > 0 && cfg.LimitA < c {
		c = cfg.LimitA
	}
	return c
}

// EffectiveCurrentA is the current the EV is offered: station limit, lowered
// by an active limit.
func (cfg Config) EffectiveCurrentA() float64 { return cfg.effectiveCurrent() }

func (cfg Config) phasesFor(mode *int) int {
	if mode != nil && *mode == exi.ModeACSingle {
		return 1
	}
	if cfg.Phases <= 0 {
		return 1
	}
	return cfg.Phases
}

func acStatus(r *exi.Response) {
	r.NotificationMaxDelay = 0
	r.EVSENotification = 0
	r.RCD = false
}

func stepISO2(st *State, cfg Config, env Env, q *exi.Request) (*exi.Response, []Event) {
	res := &exi.Response{Kind: q.Kind, SessionID: st.sessionIDBytes(), ResponseCode: exi.RCOK}
	var events []Event

	fail := func(rc int, reason string) (*exi.Response, []Event) {
		res.ResponseCode = rc
		st.Phase, st.EndReason = PhaseEnded, reason
		fillMandatory(res, cfg, st)
		return res, append(events, Event{Type: "session.failed", Data: map[string]any{"reason": reason, "request": q.Type}})
	}

	// Session ID check (V2G2-753 ff.): after SessionSetup every request must
	// carry the session's ID.
	if q.Kind != exi.KindSessionSetup && hex.EncodeToString(q.SessionID) != st.SessionID {
		return fail(exi.RCFailedUnknownSession, "unknown session id")
	}
	if !isAllowed(st.Phase, q.Kind) {
		if q.Kind == exi.KindUnknown {
			return fail(exi.RCFailed, "unknown message")
		}
		return fail(exi.RCFailedSequenceError, fmt.Sprintf("%s not allowed in %s", q.Type, st.Phase))
	}

	switch q.Kind {
	case exi.KindSessionSetup:
		sid := make([]byte, 8)
		env.Random(sid)
		st.SessionID = hex.EncodeToString(sid)
		res.SessionID = sid
		res.ResponseCode = exi.RCOKNewSession
		res.EVSEID = cfg.EVSEID
		ts := env.Now().Unix()
		res.Timestamp = &ts
		st.EV.EVCCID = hex.EncodeToString(q.EVCCID)
		st.Phase = PhaseServiceDisc
		events = append(events, Event{Type: "session.started", Data: map[string]any{"evcc_id": st.EV.EVCCID}})

	case exi.KindServiceDiscovery:
		res.PaymentOptions = []int{1} // ExternalPayment (EIM)
		res.ServiceID = chargeServiceID
		res.FreeService = true
		res.EnergyModes = []int{exi.ModeACSingle}
		if cfg.Phases == 3 {
			res.EnergyModes = append(res.EnergyModes, exi.ModeACThree)
		}
		st.Phase = PhasePaymentSel

	case exi.KindPaymentServiceSelection:
		if q.SelectedPaymentOption == nil || *q.SelectedPaymentOption != 1 {
			return fail(exi.RCFailedPaymentSelection, "payment option other than ExternalPayment")
		}
		found := false
		for _, id := range q.SelectedServiceIDs {
			if id == chargeServiceID {
				found = true
			}
		}
		if !found {
			return fail(8, "charge service not selected") // FAILED_ServiceSelectionInvalid
		}
		st.Phase = PhaseAuthorization

	case exi.KindAuthorization:
		// Home charger, EIM, no operator: authorised at once.
		res.EVSEProcessing = 0 // Finished
		st.Phase = PhaseChargeParam

	case exi.KindChargeParameterDiscovery:
		if q.AC == nil || q.RequestedEnergyTransferMode == nil ||
			(*q.RequestedEnergyTransferMode != exi.ModeACSingle && *q.RequestedEnergyTransferMode != exi.ModeACThree) {
			return fail(exi.RCFailedWrongEnergyTransferMode, "DC or unknown energy transfer mode (AC only)")
		}
		if *q.RequestedEnergyTransferMode == exi.ModeACThree && cfg.Phases != 3 {
			return fail(exi.RCFailedWrongEnergyTransferMode, "three-phase requested on a single-phase station")
		}
		m := *q.RequestedEnergyTransferMode
		st.EV.EnergyMode = &m
		ea := q.AC.EAmount.Float()
		mx := q.AC.EVMaxCurrent.Float()
		mn := q.AC.EVMinCurrent.Float()
		mv := q.AC.EVMaxVoltage.Float()
		st.EV.EAmountWh, st.EV.EVMaxCurrentA, st.EV.EVMinCurrentA, st.EV.EVMaxVoltageV = &ea, &mx, &mn, &mv
		st.EV.DepartureTimeS = q.DepartureTime
		res.EVSEProcessing = 0
		st.TupleID = 1
		res.Schedules = []exi.ScheduleTuple{schedule(cfg, st)}
		st.Phase = PhasePowerDelivery
		events = append(events, Event{Type: "session.ev_data", Data: st.EV})

	case exi.KindPowerDelivery:
		switch *q.ChargeProgress {
		case exi.ProgressStart:
			if st.Phase == PhaseCharging {
				return fail(exi.RCFailedSequenceError, "PowerDelivery(Start) while charging")
			}
			if *q.SAScheduleTupleID != st.TupleID {
				return fail(18, "unknown SAScheduleTupleID") // FAILED_TariffSelectionInvalid
			}
			st.EV.ChargingProfile = q.ChargingProfile
			st.Phase = PhaseCharging
			events = append(events, Event{Type: "charging.started"})
		case exi.ProgressStop:
			st.Phase = PhaseStopped
			events = append(events, Event{Type: "charging.stopped"})
		case exi.ProgressRenegotiate:
			st.Phase = PhaseChargeParam
		}

	case exi.KindChargingStatus:
		st.ChargingStatusCount++
		res.EVSEID = cfg.EVSEID
		res.SAScheduleTupleID = st.TupleID

	case exi.KindSessionStop:
		if q.ChargingSession != nil && *q.ChargingSession == 1 {
			st.EndReason = "paused by EV"
		} else {
			st.EndReason = "terminated by EV"
		}
		st.Phase = PhaseEnded
		events = append(events, Event{Type: "session.ended", Data: map[string]any{"reason": st.EndReason}})
	}
	fillMandatory(res, cfg, st)
	return res, events
}

// fillMandatory sets the fields every response of that type must carry, also
// on failure responses (V2G2-539: a FAILED response is still a complete one).
func fillMandatory(res *exi.Response, cfg Config, st *State) {
	switch res.Kind {
	case exi.KindSessionSetup:
		if res.EVSEID == "" {
			res.EVSEID = cfg.EVSEID
		}
	case exi.KindServiceDiscovery:
		if len(res.PaymentOptions) == 0 {
			res.PaymentOptions = []int{1}
			res.ServiceID = chargeServiceID
			res.EnergyModes = []int{exi.ModeACSingle}
		}
	case exi.KindChargeParameterDiscovery:
		v := exi.MakePV(cfg.NominalVoltageV, exi.UnitV, 0)
		c := exi.MakePV(cfg.effectiveCurrent(), exi.UnitA, 1)
		res.EVSENominalVoltage, res.EVSEMaxCurrent = &v, &c
		acStatus(res)
	case exi.KindPowerDelivery:
		acStatus(res)
	case exi.KindChargingStatus:
		if res.EVSEID == "" {
			res.EVSEID = cfg.EVSEID
		}
		res.SAScheduleTupleID = st.TupleID
		c := exi.MakePV(cfg.effectiveCurrent(), exi.UnitA, 1)
		res.EVSEMaxCurrent = &c
		acStatus(res)
	}
}

// schedule: one tuple, 24 h at the effective current. Partner schedules plug
// in here later (several PMaxScheduleEntries).
func schedule(cfg Config, st *State) exi.ScheduleTuple {
	day := uint32(86400)
	p := cfg.effectiveCurrent() * cfg.NominalVoltageV * float64(cfg.phasesFor(st.EV.EnergyMode))
	return exi.ScheduleTuple{ID: st.TupleID, Entries: []exi.PMaxEntry{
		{Start: 0, Duration: &day, PMax: exi.MakePV(p, exi.UnitW, 0)},
	}}
}

// EVSENotification values (ISO 15118-2, EVSENotificationType).
const (
	NotificationNone         = 0
	NotificationStopCharging = 1
)

// CachePair is what the device needs to answer the Charging Loop itself:
// the response to the session's ChargingStatusReq under the current limits,
// and the local termination scenario (three responses given in the order of
// the next cache misses: ChargingStatusRes with StopCharging, then
// PowerDeliveryRes, then SessionStopRes). Nil when the loop has not started.
func CachePair(st *State, cfg Config) (res []byte, term [][]byte, err error) {
	if st.CLReq == "" || st.Phase != PhaseCharging || st.Protocol == exi.NamespaceISO20AC {
		return nil, nil, nil
	}
	if res, err = chargingStatusRes(st, cfg, NotificationNone); err != nil {
		return nil, nil, err
	}
	pv := exi.MakePV(cfg.effectiveCurrent(), exi.UnitA, 1)
	st.CLMaxCurrent = &pv
	stop, err := chargingStatusRes(st, cfg, NotificationStopCharging)
	if err != nil {
		return nil, nil, err
	}
	term = [][]byte{stop}
	for _, k := range []exi.Kind{exi.KindPowerDelivery, exi.KindSessionStop} {
		r := &exi.Response{Kind: k, SessionID: st.sessionIDBytes(), ResponseCode: exi.RCOK}
		fillMandatory(r, cfg, st)
		b, err := exi.EncodeISO2Res(r)
		if err != nil {
			return nil, nil, err
		}
		term = append(term, b)
	}
	return res, term, nil
}

// Pair is the Charging Loop pair the device gets (ADR-005), for either
// protocol version. ISO 15118-2: ReqBits 0, the whole request is compared,
// the answers are given as they are. ISO 15118-20: only the first ReqBits bits
// of the request are compared, and the device writes the current time at the
// bit offsets ResTS / TermTS before it sends an answer. Key identifies the
// answer without its time stamp (a new pair is sent only when it changes).
type Pair struct {
	PT      uint16
	Res     []byte
	ResTS   int
	ReqBits int
	Term    [][]byte
	TermPT  []uint16
	TermTS  []int
	Key     string
}

// CachePairOf returns the session's pair, nil when the loop has not started
// (or the -20 loop request could not be made cachable).
func CachePairOf(st *State, cfg Config, now time.Time) (*Pair, error) {
	if st.CLReq == "" || st.Phase != PhaseCharging {
		return nil, nil
	}
	if st.Protocol == exi.NamespaceISO20AC {
		return pair20(st, cfg, now)
	}
	res, term, err := CachePair(st, cfg)
	if err != nil || res == nil {
		return nil, err
	}
	return &Pair{PT: exi.PayloadEXI, Res: res, Term: term, Key: hex.EncodeToString(res)}, nil
}

func chargingStatusRes(st *State, cfg Config, notification int) ([]byte, error) {
	r := &exi.Response{Kind: exi.KindChargingStatus, SessionID: st.sessionIDBytes(), ResponseCode: exi.RCOK}
	fillMandatory(r, cfg, st)
	r.EVSENotification = notification
	return exi.EncodeISO2Res(r)
}
