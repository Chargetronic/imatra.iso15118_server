// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

// Package exi is the EXI codec of the engine: libcbv2g behind a flat C facade.
//
// Build: libcbv2g installed where the compiler finds it, or CGO_CFLAGS /
// CGO_LDFLAGS pointing at it (see the engine Dockerfile).
package exi

/*
#cgo LDFLAGS: -lcbv2g_iso2 -lcbv2g_din -lcbv2g_exi_codec
#include <stdlib.h>
#include "facade.h"
*/
import "C"

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"unsafe"
)

// Namespaces of supportedAppProtocol.
const (
	NamespaceISO2 = "urn:iso:15118:2:2013:MsgDef"
	NamespaceDIN  = "urn:din:70121:2012:MsgDef"
)

// V2GTP payload types.
const (
	PayloadEXI    = 0x8001
	PayloadSDPReq = 0x9000
	PayloadSDPRes = 0x9001
)

// Units (iso2_unitSymbolType).
const (
	UnitH  = 0
	UnitM  = 1
	UnitS  = 2
	UnitA  = 3
	UnitV  = 4
	UnitW  = 5
	UnitWh = 6
)

var unitNames = []string{"h", "m", "s", "A", "V", "W", "Wh"}

// HexBytes marshals to JSON as a hex string (session IDs, EVCCID).
type HexBytes []byte

func (h HexBytes) MarshalJSON() ([]byte, error) {
	return []byte(`"` + hex.EncodeToString(h) + `"`), nil
}

// PV is an ISO 15118 PhysicalValue: Value * 10^Multiplier [Unit].
type PV struct {
	Value      int16 `json:"value"`
	Multiplier int8  `json:"multiplier"`
	Unit       int8  `json:"unit"`
}

// Float returns the value in base units.
func (p PV) Float() float64 { return float64(p.Value) * math.Pow10(int(p.Multiplier)) }

func (p PV) String() string {
	u := "?"
	if int(p.Unit) >= 0 && int(p.Unit) < len(unitNames) {
		u = unitNames[p.Unit]
	}
	return fmt.Sprintf("%g %s", p.Float(), u)
}

// MakePV picks the smallest multiplier that keeps v within int16 at the given
// precision (decimals after the point, e.g. 1 for 0.1 A).
func MakePV(v float64, unit int8, decimals int) PV {
	mult := -decimals
	for {
		scaled := math.Round(v / math.Pow10(mult))
		if scaled >= math.MinInt16 && scaled <= math.MaxInt16 {
			return PV{Value: int16(scaled), Multiplier: int8(mult), Unit: unit}
		}
		mult++
	}
}

var (
	ErrDecode      = errors.New("exi: decode failed")
	ErrEncode      = errors.New("exi: encode failed")
	ErrUnsupported = errors.New("exi: unsupported message")
)

func errOf(rc C.int) error {
	switch rc {
	case C.SP_OK:
		return nil
	case C.SP_ERR_DECODE:
		return ErrDecode
	case C.SP_ERR_ENCODE:
		return ErrEncode
	case C.SP_ERR_UNSUPPORTED:
		return ErrUnsupported
	default:
		return fmt.Errorf("exi: error %d", int(rc))
	}
}

// ------------------------------------------------------------------ handshake

type AppProtocol struct {
	Namespace string `json:"namespace"`
	Major     uint32 `json:"major"`
	Minor     uint32 `json:"minor"`
	SchemaID  uint8  `json:"schema_id"`
	Priority  uint8  `json:"priority"`
}

func DecodeAppHand(data []byte) ([]AppProtocol, error) {
	if len(data) == 0 {
		return nil, ErrDecode
	}
	var r C.sp_apphand_req
	if err := errOf(C.sp_decode_apphand((*C.uint8_t)(unsafe.Pointer(&data[0])), C.size_t(len(data)), &r)); err != nil {
		return nil, err
	}
	out := make([]AppProtocol, int(r.n))
	for i := range out {
		p := r.p[i]
		out[i] = AppProtocol{
			Namespace: C.GoString(&p.ns[0]),
			Major:     uint32(p.major), Minor: uint32(p.minor),
			SchemaID: uint8(p.schema_id), Priority: uint8(p.priority),
		}
	}
	return out, nil
}

// AppHand response codes.
const (
	AppHandOK                  = 0
	AppHandOKMinorDev          = 1
	AppHandFailedNoNegotiation = 2
)

func EncodeAppHandRes(code int, schemaID int) ([]byte, error) {
	buf := make([]byte, 64)
	var n C.size_t
	if err := errOf(C.sp_encode_apphand_res(C.int(code), C.int(schemaID), (*C.uint8_t)(unsafe.Pointer(&buf[0])), C.size_t(len(buf)), &n)); err != nil {
		return nil, err
	}
	return buf[:n], nil
}

// ------------------------------------------------------------- ISO 15118-2

type Kind int

// Values match enum sp_iso2_kind (facade.h). EIM and AC only: Plug & Charge
// messages (ServiceDetail, PaymentDetails, MeteringReceipt, Certificate*) and
// the DC messages decode as Unknown; the EV's SoC is read through DIN 70121.
const (
	KindUnknown                  Kind = 0
	KindSessionSetup             Kind = 1
	KindServiceDiscovery         Kind = 2
	KindPaymentServiceSelection  Kind = 4
	KindAuthorization            Kind = 6
	KindChargeParameterDiscovery Kind = 7
	KindPowerDelivery            Kind = 8
	KindChargingStatus           Kind = 9
	KindSessionStop              Kind = 11
)

var kindNames = map[Kind]string{KindUnknown: "Unknown", KindSessionSetup: "SessionSetup",
	KindServiceDiscovery: "ServiceDiscovery", KindPaymentServiceSelection: "PaymentServiceSelection",
	KindAuthorization: "Authorization", KindChargeParameterDiscovery: "ChargeParameterDiscovery",
	KindPowerDelivery: "PowerDelivery", KindChargingStatus: "ChargingStatus", KindSessionStop: "SessionStop"}

func (k Kind) String() string {
	if n, ok := kindNames[k]; ok {
		return n
	}
	return fmt.Sprintf("Kind(%d)", int(k))
}

// Energy transfer modes.
const (
	ModeACSingle = 0
	ModeACThree  = 1
	// 2..5 are the DC modes: not offered (ISO 15118-2 is AC only here)
)

// ChargeProgress.
const (
	ProgressStart       = 0
	ProgressStop        = 1
	ProgressRenegotiate = 2
)

// Response codes (subset).
const (
	RCOK                            = 0
	RCOKNewSession                  = 1
	RCOKOldSessionJoined            = 2
	RCFailed                        = 4
	RCFailedSequenceError           = 5
	RCFailedServiceIDInvalid        = 6
	RCFailedUnknownSession          = 7
	RCFailedPaymentSelection        = 9
	RCFailedWrongChargeParameter    = 16
	RCFailedWrongEnergyTransferMode = 22
)

var rcNames = map[int]string{0: "OK", 1: "OK_NewSessionEstablished", 2: "OK_OldSessionJoined",
	4: "FAILED", 5: "FAILED_SequenceError", 6: "FAILED_ServiceIDInvalid", 7: "FAILED_UnknownSession",
	8: "FAILED_ServiceSelectionInvalid", 9: "FAILED_PaymentSelectionInvalid",
	16: "FAILED_WrongChargeParameter", 21: "FAILED_NoChargeServiceSelected",
	22: "FAILED_WrongEnergyTransferMode"}

func RCName(rc int) string {
	if s, ok := rcNames[rc]; ok {
		return s
	}
	return fmt.Sprintf("RC(%d)", rc)
}

type ProfileEntry struct {
	Start    uint32 `json:"start_s"`
	MaxPower PV     `json:"max_power"`
	Phases   *int8  `json:"phases,omitempty"`
}

// Request is a decoded ISO 15118-2 request, flattened. Only fields relevant to
// the request kind are set.
type Request struct {
	Kind      Kind     `json:"-"`
	Type      string   `json:"type"`
	SessionID HexBytes `json:"session_id"`

	EVCCID HexBytes `json:"evcc_id,omitempty"`

	SelectedPaymentOption *int     `json:"selected_payment_option,omitempty"`
	SelectedServiceIDs    []uint16 `json:"selected_service_ids,omitempty"`

	RequestedEnergyTransferMode *int      `json:"requested_energy_transfer_mode,omitempty"`
	AC                          *ACParams `json:"ac,omitempty"`
	DepartureTime               *uint32   `json:"departure_time_s,omitempty"`

	ChargeProgress    *int           `json:"charge_progress,omitempty"`
	SAScheduleTupleID *uint8         `json:"sa_schedule_tuple_id,omitempty"`
	ChargingProfile   []ProfileEntry `json:"charging_profile,omitempty"`

	ChargingSession *int `json:"charging_session,omitempty"` // 0 Terminate, 1 Pause
}

type ACParams struct {
	EAmount      PV `json:"e_amount"`
	EVMaxVoltage PV `json:"ev_max_voltage"`
	EVMaxCurrent PV `json:"ev_max_current"`
	EVMinCurrent PV `json:"ev_min_current"`
}

func pvFrom(p C.sp_pv) PV {
	return PV{Value: int16(p.value), Multiplier: int8(p.multiplier), Unit: int8(p.unit)}
}

func pvTo(p PV) C.sp_pv {
	return C.sp_pv{value: C.int16_t(p.Value), multiplier: C.int8_t(p.Multiplier), unit: C.int8_t(p.Unit)}
}

func ptr[T any](v T) *T { return &v }

func DecodeISO2(data []byte) (*Request, error) {
	if len(data) == 0 {
		return nil, ErrDecode
	}
	r := (*C.sp_iso2_req)(C.malloc(C.size_t(unsafe.Sizeof(C.sp_iso2_req{}))))
	defer C.free(unsafe.Pointer(r))
	if err := errOf(C.sp_decode_iso2((*C.uint8_t)(unsafe.Pointer(&data[0])), C.size_t(len(data)), r)); err != nil {
		return nil, err
	}
	q := &Request{Kind: Kind(r.kind)}
	q.Type = q.Kind.String() + "Req"
	q.SessionID = C.GoBytes(unsafe.Pointer(&r.session_id[0]), r.session_id_len)

	switch q.Kind {
	case KindSessionSetup:
		q.EVCCID = C.GoBytes(unsafe.Pointer(&r.evcc_id[0]), r.evcc_id_len)
	case KindPaymentServiceSelection:
		q.SelectedPaymentOption = ptr(int(r.selected_payment_option))
		for i := 0; i < int(r.n_selected_services); i++ {
			q.SelectedServiceIDs = append(q.SelectedServiceIDs, uint16(r.selected_service_ids[i]))
		}
	case KindChargeParameterDiscovery:
		q.RequestedEnergyTransferMode = ptr(int(r.requested_energy_transfer_mode))
		if r.has_departure_time != 0 {
			q.DepartureTime = ptr(uint32(r.departure_time))
		}
		if r.has_ac != 0 {
			q.AC = &ACParams{EAmount: pvFrom(r.e_amount), EVMaxVoltage: pvFrom(r.ev_max_voltage),
				EVMaxCurrent: pvFrom(r.ev_max_current), EVMinCurrent: pvFrom(r.ev_min_current)}
		}
	case KindPowerDelivery:
		q.ChargeProgress = ptr(int(r.charge_progress))
		q.SAScheduleTupleID = ptr(uint8(r.sa_schedule_tuple_id))
		for i := 0; i < int(r.n_profile); i++ {
			e := r.profile[i]
			pe := ProfileEntry{Start: uint32(e.start), MaxPower: pvFrom(e.max_power)}
			if e.has_phases != 0 {
				pe.Phases = ptr(int8(e.phases))
			}
			q.ChargingProfile = append(q.ChargingProfile, pe)
		}
	case KindSessionStop:
		q.ChargingSession = ptr(int(r.charging_session))
	}
	return q, nil
}

type PMaxEntry struct {
	Start    uint32  `json:"start_s"`
	Duration *uint32 `json:"duration_s,omitempty"`
	PMax     PV      `json:"pmax"`
}

type ScheduleTuple struct {
	ID      uint8       `json:"id"`
	Entries []PMaxEntry `json:"entries"`
}

// Response is an ISO 15118-2 response, flattened. Kind is the request kind it
// answers.
type Response struct {
	Kind         Kind     `json:"-"`
	Type         string   `json:"type"`
	SessionID    HexBytes `json:"session_id"`
	ResponseCode int      `json:"-"`
	ResponseName string   `json:"response_code"`

	EVSEID    string `json:"evse_id,omitempty"`
	Timestamp *int64 `json:"evse_timestamp,omitempty"`

	PaymentOptions []int  `json:"payment_options,omitempty"`
	ServiceID      uint16 `json:"service_id,omitempty"`
	FreeService    bool   `json:"free_service,omitempty"`
	EnergyModes    []int  `json:"energy_modes,omitempty"`

	EVSEProcessing int `json:"evse_processing,omitempty"`

	Schedules          []ScheduleTuple `json:"sa_schedules,omitempty"`
	EVSENominalVoltage *PV             `json:"evse_nominal_voltage,omitempty"`
	EVSEMaxCurrent     *PV             `json:"evse_max_current,omitempty"`

	NotificationMaxDelay uint16 `json:"notification_max_delay,omitempty"`
	EVSENotification     int    `json:"evse_notification,omitempty"`
	RCD                  bool   `json:"rcd,omitempty"`

	SAScheduleTupleID uint8 `json:"sa_schedule_tuple_id,omitempty"`
	ReceiptRequired   *bool `json:"receipt_required,omitempty"`
}

func b2i(b bool) C.int {
	if b {
		return 1
	}
	return 0
}

func EncodeISO2Res(s *Response) ([]byte, error) {
	in := (*C.sp_iso2_res)(C.malloc(C.size_t(unsafe.Sizeof(C.sp_iso2_res{}))))
	defer C.free(unsafe.Pointer(in))
	*in = C.sp_iso2_res{}

	in.kind = C.int(s.Kind)
	n := copy(unsafe.Slice((*byte)(unsafe.Pointer(&in.session_id[0])), 8), s.SessionID)
	in.session_id_len = C.int(n)
	in.response_code = C.int(s.ResponseCode)

	id := s.EVSEID
	if len(id) > 37 {
		id = id[:37]
	}
	copy(unsafe.Slice((*byte)(unsafe.Pointer(&in.evse_id[0])), 38), id)
	if s.Timestamp != nil {
		in.has_timestamp = 1
		in.timestamp = C.int64_t(*s.Timestamp)
	}
	for i, p := range s.PaymentOptions {
		if i >= 2 {
			break
		}
		in.payment_options[i] = C.int(p)
		in.n_payment_options = C.int(i + 1)
	}
	in.service_id = C.uint16_t(s.ServiceID)
	in.free_service = b2i(s.FreeService)
	for i, m := range s.EnergyModes {
		if i >= 6 {
			break
		}
		in.energy_modes[i] = C.int(m)
		in.n_energy_modes = C.int(i + 1)
	}
	in.evse_processing = C.int(s.EVSEProcessing)
	for t, tu := range s.Schedules {
		if t >= 3 {
			break
		}
		in.tuples[t].id = C.uint8_t(tu.ID)
		for e, en := range tu.Entries {
			if e >= 12 {
				break
			}
			ce := &in.tuples[t].entries[e]
			ce.start = C.uint32_t(en.Start)
			if en.Duration != nil {
				ce.has_duration = 1
				ce.duration = C.uint32_t(*en.Duration)
			}
			ce.pmax = pvTo(en.PMax)
			in.tuples[t].n_entries = C.int(e + 1)
		}
		in.n_tuples = C.int(t + 1)
	}
	if s.EVSENominalVoltage != nil {
		in.evse_nominal_voltage = pvTo(*s.EVSENominalVoltage)
	}
	if s.EVSEMaxCurrent != nil {
		in.has_evse_max_current = 1
		in.evse_max_current = pvTo(*s.EVSEMaxCurrent)
	}
	in.notification_max_delay = C.uint16_t(s.NotificationMaxDelay)
	in.evse_notification = C.int(s.EVSENotification)
	in.rcd = b2i(s.RCD)
	in.sa_schedule_tuple_id = C.uint8_t(s.SAScheduleTupleID)
	if s.ReceiptRequired != nil {
		in.has_receipt_required = 1
		in.receipt_required = b2i(*s.ReceiptRequired)
	}

	buf := make([]byte, 4096)
	var outLen C.size_t
	if err := errOf(C.sp_encode_iso2_res(in, (*C.uint8_t)(unsafe.Pointer(&buf[0])), C.size_t(len(buf)), &outLen)); err != nil {
		return nil, err
	}
	s.Type = s.Kind.String() + "Res"
	s.ResponseName = RCName(s.ResponseCode)
	return buf[:outLen], nil
}
