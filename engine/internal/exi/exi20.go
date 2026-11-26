// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

package exi

/*
#include <stdlib.h>
#include "facade20.h"
*/
import "C"

import (
	"fmt"
	"math"
	"unsafe"
)

// ISO 15118-20, AC (supportedAppProtocol namespace) and its V2GTP payload
// types: the CommonMessages schema and the AC schema are separate EXI grammars.
const (
	NamespaceISO20AC = "urn:iso:std:iso:15118:-20:AC"
	PayloadISO20Main = 0x8002
	PayloadISO20AC   = 0x8003
)

// Kind20 values match enum sp20_kind (facade20.h).
type Kind20 int

const (
	K20Unknown            Kind20 = 0
	K20SessionSetup       Kind20 = 1
	K20AuthorizationSetup Kind20 = 2
	K20Authorization      Kind20 = 3
	K20ServiceDiscovery   Kind20 = 4
	K20ServiceDetail      Kind20 = 5
	K20ServiceSelection   Kind20 = 6
	K20ScheduleExchange   Kind20 = 7
	K20PowerDelivery      Kind20 = 8
	K20SessionStop        Kind20 = 9
	K20ACChargeParamDisc  Kind20 = 10
	K20ACChargeLoop       Kind20 = 11
	K20Other              Kind20 = 12
)

var kind20Names = map[Kind20]string{K20Unknown: "Unknown", K20SessionSetup: "SessionSetup",
	K20AuthorizationSetup: "AuthorizationSetup", K20Authorization: "Authorization",
	K20ServiceDiscovery: "ServiceDiscovery", K20ServiceDetail: "ServiceDetail",
	K20ServiceSelection: "ServiceSelection", K20ScheduleExchange: "ScheduleExchange",
	K20PowerDelivery: "PowerDelivery", K20SessionStop: "SessionStop",
	K20ACChargeParamDisc: "AC_ChargeParameterDiscovery", K20ACChargeLoop: "AC_ChargeLoop",
	K20Other: "Other"}

func (k Kind20) String() string {
	if n, ok := kind20Names[k]; ok {
		return n
	}
	return fmt.Sprintf("Kind20(%d)", int(k))
}

// PayloadType is the V2GTP payload type a message of this kind travels in.
func (k Kind20) PayloadType() uint16 {
	if k == K20ACChargeParamDisc || k == K20ACChargeLoop {
		return PayloadISO20AC
	}
	return PayloadISO20Main
}

// Control modes (ServiceDetail parameter "ControlMode", ISO 15118-20 Table 205).
const (
	ModeNone      = 0
	ModeScheduled = 1
	ModeDynamic   = 2
	ModeBPT       = 3 // a BPT control mode: not offered in this edition
)

// Energy transfer service IDs (ISO 15118-20 Table 204); this edition offers AC only.
const ServiceAC = 1

// PayloadISO20Reneg: the multiplexed side stream of schedule renegotiation
// (V2G20-1045): AC_ChargeParameterDiscovery (AC schema), ScheduleExchange and
// PowerDelivery (CommonMessages) next to the running AC_ChargeLoop.
const PayloadISO20Reneg = 0x8101

// ChargeProgress (PowerDeliveryReq).
const (
	Progress20Start                 = 0
	Progress20Stop                  = 1
	Progress20Standby               = 2
	Progress20ScheduleRenegotiation = 3
)

// ChargingSession (SessionStopReq).
const (
	Session20Pause                = 0
	Session20Terminate            = 1
	Session20ServiceRenegotiation = 2
)

// EVSENotification (EVSEStatus). There is no "None": no notification = no EVSEStatus.
const (
	Notif20Pause                 = 0
	Notif20ExitStandby           = 1
	Notif20Terminate             = 2
	Notif20ScheduleRenegotiation = 3
)

// Response codes (iso20_responseCodeType, subset).
const (
	RC20OK                             = 0
	RC20OKNewSession                   = 2
	RC20OKOldSessionJoined             = 3
	RC20WarnAuthorizationSelection     = 5
	RC20Failed                         = 21
	RC20FailedNoEnergyTransferService  = 27
	RC20FailedScheduleSelectionInvalid = 33
	RC20FailedSequenceError            = 34
	RC20FailedServiceIDInvalid         = 35
	RC20FailedServiceSelectionInvalid  = 36
	RC20FailedUnknownSession           = 38
	RC20FailedWrongChargeParameter     = 39
)

var rc20Names = map[int]string{0: "OK", 2: "OK_NewSessionEstablished", 3: "OK_OldSessionJoined",
	5: "WARNING_AuthorizationSelectionInvalid", 21: "FAILED", 27: "FAILED_NoEnergyTransferServiceSelected",
	33: "FAILED_ScheduleSelectionInvalid", 34: "FAILED_SequenceError", 35: "FAILED_ServiceIDInvalid",
	36: "FAILED_ServiceSelectionInvalid", 38: "FAILED_UnknownSession", 39: "FAILED_WrongChargeParameter"}

func RC20Name(rc int) string {
	if s, ok := rc20Names[rc]; ok {
		return s
	}
	return fmt.Sprintf("RC20(%d)", rc)
}

// Failed20 reports whether a response code ends the session (FAILED*).
func Failed20(rc int) bool { return rc >= RC20Failed }

// Rat is an ISO 15118-20 RationalNumber: Value * 10^Exponent.
type Rat struct {
	Value    int16 `json:"value"`
	Exponent int8  `json:"exponent"`
}

func (r Rat) Float() float64 { return float64(r.Value) * math.Pow10(int(r.Exponent)) }

// MakeRat picks the smallest exponent that keeps v within int16 (whole units
// and coarser: power in W, energy in Wh).
func MakeRat(v float64) Rat {
	for e := 0; e < 10; e++ {
		s := math.Round(v / math.Pow10(e))
		if s >= math.MinInt16 && s <= math.MaxInt16 {
			return Rat{Value: int16(s), Exponent: int8(e)}
		}
	}
	return Rat{Value: math.MaxInt16, Exponent: 9}
}

func ratFrom(r C.sp_rat) Rat { return Rat{Value: int16(r.value), Exponent: int8(r.exponent)} }
func ratTo(r Rat) C.sp_rat {
	return C.sp_rat{value: C.int16_t(r.Value), exponent: C.int8_t(r.Exponent)}
}

// Request20 is a decoded ISO 15118-20 request, flattened; only the fields of
// its kind are set.
type Request20 struct {
	Kind      Kind20   `json:"-"`
	Type      string   `json:"type"`
	SessionID HexBytes `json:"session_id"`
	TimeStamp uint64   `json:"timestamp"`

	EVCCID string `json:"evcc_id,omitempty"`

	SelectedAuthorization *int `json:"selected_authorization,omitempty"` // 0 EIM, 1 PnC

	ServiceID *uint16 `json:"service_id,omitempty"` // ServiceDetailReq

	SelectedServiceID      *uint16 `json:"selected_service_id,omitempty"`
	SelectedParameterSetID *uint16 `json:"selected_parameter_set_id,omitempty"`
	SelectedVAS            int     `json:"selected_vas,omitempty"`

	MaxSupportingPoints *uint16 `json:"max_supporting_points,omitempty"`
	ControlMode         int     `json:"control_mode,omitempty"` // ScheduleExchangeReq or AC_ChargeLoopReq
	DepartureTime       *uint32 `json:"departure_time_s,omitempty"`
	MinSoC              *int8   `json:"min_soc,omitempty"`
	TargetSoC           *int8   `json:"target_soc,omitempty"`
	TargetEnergy        *Rat    `json:"ev_target_energy_request,omitempty"`
	MaxEnergy           *Rat    `json:"ev_max_energy_request,omitempty"`
	MinEnergy           *Rat    `json:"ev_min_energy_request,omitempty"`

	EVProcessing          *int    `json:"ev_processing,omitempty"`
	ChargeProgress        *int    `json:"charge_progress,omitempty"`
	ProfileMode           int     `json:"profile_mode,omitempty"`
	SelectedScheduleTuple *uint32 `json:"selected_schedule_tuple_id,omitempty"`
	ChargingSession       *int    `json:"charging_session,omitempty"`
	TerminationCode       string  `json:"ev_termination_code,omitempty"`

	BPT          bool `json:"bpt,omitempty"`
	MaxPower     *Rat `json:"ev_max_charge_power,omitempty"`
	MinPower     *Rat `json:"ev_min_charge_power,omitempty"`
	MaxPowerL2   *Rat `json:"ev_max_charge_power_l2,omitempty"`
	MaxPowerL3   *Rat `json:"ev_max_charge_power_l3,omitempty"`
	PresentPower *Rat `json:"ev_present_active_power,omitempty"`

	MeterInfoRequested bool  `json:"meter_info_requested,omitempty"`
	PresentSoC         *int8 `json:"present_soc,omitempty"`
	DisplayTargetSoC   *int8 `json:"display_target_soc,omitempty"`
	DisplayMinSoC      *int8 `json:"display_min_soc,omitempty"`
	ChargingComplete   *bool `json:"charging_complete,omitempty"`
	BatteryCapacity    *Rat  `json:"battery_energy_capacity,omitempty"`
}

func optRat(has C.int, r C.sp_rat) *Rat {
	if has == 0 {
		return nil
	}
	v := ratFrom(r)
	return &v
}

func optI8(has C.int, v C.int8_t) *int8 {
	if has == 0 {
		return nil
	}
	x := int8(v)
	return &x
}

func DecodeISO20(pt uint16, data []byte) (*Request20, error) {
	if len(data) == 0 {
		return nil, ErrDecode
	}
	r := (*C.sp20_req)(C.malloc(C.size_t(unsafe.Sizeof(C.sp20_req{}))))
	defer C.free(unsafe.Pointer(r))
	if err := errOf(C.sp20_decode_req(C.int(pt), (*C.uint8_t)(unsafe.Pointer(&data[0])), C.size_t(len(data)), r)); err != nil {
		return nil, err
	}
	q := &Request20{Kind: Kind20(r.kind), TimeStamp: uint64(r.timestamp)}
	q.Type = q.Kind.String() + "Req"
	q.SessionID = C.GoBytes(unsafe.Pointer(&r.session_id[0]), r.session_id_len)
	switch q.Kind {
	case K20SessionSetup:
		q.EVCCID = C.GoString(&r.evcc_id[0])
	case K20Authorization:
		q.SelectedAuthorization = ptr(int(r.selected_authorization))
	case K20ServiceDetail:
		q.ServiceID = ptr(uint16(r.service_id))
	case K20ServiceSelection:
		q.SelectedServiceID = ptr(uint16(r.selected_service_id))
		q.SelectedParameterSetID = ptr(uint16(r.selected_parameter_set_id))
		q.SelectedVAS = int(r.n_selected_vas)
	case K20ScheduleExchange:
		q.MaxSupportingPoints = ptr(uint16(r.max_supporting_points))
		q.ControlMode = int(r.se_mode)
	case K20PowerDelivery:
		q.EVProcessing = ptr(int(r.ev_processing))
		q.ChargeProgress = ptr(int(r.charge_progress))
		if r.has_power_profile != 0 {
			q.ProfileMode = int(r.profile_mode)
			if q.ProfileMode == ModeScheduled {
				q.SelectedScheduleTuple = ptr(uint32(r.selected_schedule_tuple_id))
			}
		}
		q.BPT = r.bpt != 0
	case K20SessionStop:
		q.ChargingSession = ptr(int(r.charging_session))
		q.TerminationCode = C.GoString(&r.termination_code[0])
	case K20ACChargeParamDisc:
		q.BPT = r.bpt != 0
	case K20ACChargeLoop:
		q.ControlMode = int(r.cl_mode)
		q.BPT = r.bpt != 0
		q.MeterInfoRequested = r.meter_info_requested != 0
		if r.has_display != 0 {
			q.PresentSoC = optI8(r.has_present_soc, r.present_soc)
			q.DisplayTargetSoC = optI8(r.has_display_target_soc, r.display_target_soc)
			q.DisplayMinSoC = optI8(r.has_display_min_soc, r.display_min_soc)
			if r.has_charging_complete != 0 {
				q.ChargingComplete = ptr(r.charging_complete != 0)
			}
			q.BatteryCapacity = optRat(r.has_battery_capacity, r.battery_capacity)
		}
	}
	if q.Kind == K20ScheduleExchange || q.Kind == K20ACChargeLoop {
		if r.has_departure_time != 0 {
			q.DepartureTime = ptr(uint32(r.departure_time))
		}
		q.MinSoC = optI8(r.has_min_soc, r.min_soc)
		q.TargetSoC = optI8(r.has_target_soc, r.target_soc)
		q.TargetEnergy = optRat(r.has_target_energy, r.target_energy)
		q.MaxEnergy = optRat(r.has_max_energy, r.max_energy)
		q.MinEnergy = optRat(r.has_min_energy, r.min_energy)
	}
	if q.Kind == K20ACChargeParamDisc || q.Kind == K20ACChargeLoop {
		q.MaxPower = optRat(r.has_max_power, r.max_power)
		q.MinPower = optRat(r.has_min_power, r.min_power)
		q.MaxPowerL2 = optRat(r.has_max_power_l2, r.max_power_l2)
		q.MaxPowerL3 = optRat(r.has_max_power_l3, r.max_power_l3)
		q.PresentPower = optRat(r.has_present_power, r.present_power)
	}
	return q, nil
}

// PowerEntry is one entry of a PowerSchedule.
type PowerEntry struct {
	DurationS uint32 `json:"duration_s"`
	Power     Rat    `json:"power"`
}

// ParamSet is one parameter set of the AC service (ServiceDetailRes).
type ParamSet struct {
	ID             uint16 `json:"id"`
	Connector      int    `json:"connector"` // 1 single-phase, 2 three-phase
	ControlMode    int    `json:"control_mode"`
	NominalVoltage int    `json:"evse_nominal_voltage"`
	MobilityNeeds  int    `json:"mobility_needs_mode"`
	Pricing        int    `json:"pricing"`
}

// Response20 is an ISO 15118-20 response, flattened. Kind: the request kind it answers.
type Response20 struct {
	Kind         Kind20   `json:"-"`
	Type         string   `json:"type"`
	SessionID    HexBytes `json:"session_id"`
	TimeStamp    uint64   `json:"timestamp"`
	ResponseCode int      `json:"-"`
	ResponseName string   `json:"response_code"`

	EVSEID         string       `json:"evse_id,omitempty"`
	EVSEProcessing int          `json:"evse_processing,omitempty"`
	Services       []uint16     `json:"energy_transfer_services,omitempty"`
	ServiceID      uint16       `json:"service_id,omitempty"`
	ParamSets      []ParamSet   `json:"parameter_sets,omitempty"`
	ControlMode    int          `json:"control_mode,omitempty"`
	ScheduleTuple  uint32       `json:"schedule_tuple_id,omitempty"`
	TimeAnchor     uint64       `json:"time_anchor,omitempty"`
	Schedule       []PowerEntry `json:"power_schedule,omitempty"`

	Notification         *int   `json:"evse_notification,omitempty"` // nil: no EVSEStatus
	NotificationMaxDelay uint16 `json:"notification_max_delay,omitempty"`

	EVSEMaxPower         *Rat `json:"evse_max_charge_power,omitempty"`
	EVSEMinPower         *Rat `json:"evse_min_charge_power,omitempty"`
	EVSENominalFrequency *Rat `json:"evse_nominal_frequency,omitempty"`
	TargetPower          *Rat `json:"evse_target_active_power,omitempty"`

	// ScheduleExchangeRes: no power available (V2G20-2121)
	GoToPause bool `json:"go_to_pause,omitempty"`
	// AC_ChargeLoopRes: when the EV asked for it (V2G20-1082)
	Meter *MeterInfo20 `json:"meter_info,omitempty"`
}

// MeterInfo20: the energy charged during the current service session (V2G20-902).
type MeterInfo20 struct {
	MeterID   string `json:"meter_id"`
	ChargedWh uint64 `json:"charged_energy_wh"`
}

func fillRes20(s *Response20, in *C.sp20_res) {
	in.kind = C.int(s.Kind)
	n := copy(unsafe.Slice((*byte)(unsafe.Pointer(&in.session_id[0])), 8), s.SessionID)
	in.session_id_len = C.int(n)
	in.timestamp = C.uint64_t(s.TimeStamp)
	in.response_code = C.int(s.ResponseCode)
	id := s.EVSEID
	if len(id) > 255 {
		id = id[:255]
	}
	copy(unsafe.Slice((*byte)(unsafe.Pointer(&in.evse_id[0])), 256), id)
	in.evse_processing = C.int(s.EVSEProcessing)
	for i, sid := range s.Services {
		if i >= 4 {
			break
		}
		in.service_ids[i] = C.uint16_t(sid)
		in.free_service[i] = 1
		in.n_services = C.int(i + 1)
	}
	in.service_id = C.uint16_t(s.ServiceID)
	for i, p := range s.ParamSets {
		if i >= 4 {
			break
		}
		in.sets[i] = C.sp20_param_set{id: C.uint16_t(p.ID), connector: C.int(p.Connector),
			control_mode: C.int(p.ControlMode), nominal_voltage: C.int(p.NominalVoltage),
			mobility_needs: C.int(p.MobilityNeeds), pricing: C.int(p.Pricing)}
		in.n_sets = C.int(i + 1)
	}
	in.se_mode = C.int(s.ControlMode)
	in.cl_mode = C.int(s.ControlMode)
	in.schedule_tuple_id = C.uint32_t(s.ScheduleTuple)
	in.time_anchor = C.uint64_t(s.TimeAnchor)
	for i, e := range s.Schedule {
		if i >= 24 {
			break
		}
		in.entries[i] = C.sp20_power_entry{duration: C.uint32_t(e.DurationS), power: ratTo(e.Power)}
		in.n_entries = C.int(i + 1)
	}
	if s.Notification != nil {
		in.has_evse_status = 1
		in.evse_notification = C.int(*s.Notification)
		in.notification_max_delay = C.uint16_t(s.NotificationMaxDelay)
	}
	if s.EVSEMaxPower != nil {
		in.evse_max_power = ratTo(*s.EVSEMaxPower)
	}
	if s.EVSEMinPower != nil {
		in.evse_min_power = ratTo(*s.EVSEMinPower)
	}
	if s.EVSENominalFrequency != nil {
		in.evse_nominal_frequency = ratTo(*s.EVSENominalFrequency)
	}
	if s.TargetPower != nil {
		in.has_target_power = 1
		in.target_power = ratTo(*s.TargetPower)
	}
	if s.GoToPause {
		in.go_to_pause = 1
	}
	if s.Meter != nil {
		in.has_meter_info = 1
		id := s.Meter.MeterID
		if len(id) > 32 {
			id = id[:32]
		}
		copy(unsafe.Slice((*byte)(unsafe.Pointer(&in.meter_id[0])), 33), id)
		in.charged_energy_wh = C.uint64_t(s.Meter.ChargedWh)
	}
}

// EncodeISO20Res encodes the response; the second value is its payload type.
func EncodeISO20Res(s *Response20) ([]byte, uint16, error) {
	in := (*C.sp20_res)(C.malloc(C.size_t(unsafe.Sizeof(C.sp20_res{}))))
	defer C.free(unsafe.Pointer(in))
	*in = C.sp20_res{}
	fillRes20(s, in)
	buf := make([]byte, 4096)
	var n C.size_t
	var pt C.int
	if err := errOf(C.sp20_encode_res(in, (*C.uint8_t)(unsafe.Pointer(&buf[0])), C.size_t(len(buf)), &n, &pt)); err != nil {
		return nil, 0, err
	}
	s.Type = s.Kind.String() + "Res"
	s.ResponseName = RC20Name(s.ResponseCode)
	return buf[:n], uint16(pt), nil
}

// TimeStampBit is the bit offset of the header's TimeStamp in an encoded
// ISO 15118-20 message. A device compares the first TimeStampBit bits of an
// EV request with the cached request (message type and session ID), and
// writes the current time there into a cached response (WriteTimeStamp).
// full: the message must also re-encode bit for bit (responses the engine
// encoded itself); for an EV's request only the prefix is checked.
func TimeStampBit(pt uint16, data []byte, full bool) (int, error) {
	if len(data) == 0 {
		return 0, ErrDecode
	}
	var bit C.int
	f := C.int(0)
	if full {
		f = 1
	}
	if err := errOf(C.sp20_ts_bit(C.int(pt), (*C.uint8_t)(unsafe.Pointer(&data[0])), C.size_t(len(data)), f, &bit)); err != nil {
		return 0, err
	}
	return int(bit), nil
}

// TimeStampBits is the length of the time stamp field the device writes. The
// header's TimeStamp is microseconds of SECC time (V2G20-1534): an EXI
// unsigned integer of eight 7-bit groups holds every value from 2^49 (1987) to
// 2^56-1 (year 4253).
const TimeStampBits = 64

const tsGroups = TimeStampBits / 8

// TimeStampMin, TimeStampMax: the values the field holds.
const (
	TimeStampMin = uint64(1) << 49
	TimeStampMax = uint64(1)<<56 - 1
)

// WriteTimeStamp is the device's algorithm, here as the reference: write t
// (microseconds) as an EXI unsigned integer of eight 7-bit groups, least
// significant group first, each octet with its continuation bit (1, the last
// 0), MSB first into the bit stream at bit offset at. The message keeps its
// length. Errors: t outside [2^49, 2^56) or the field past the end.
func WriteTimeStamp(msg []byte, at int, t uint64) error {
	if t < TimeStampMin || t > TimeStampMax {
		return fmt.Errorf("exi: time stamp %d needs other than %d groups", t, tsGroups)
	}
	if at < 0 || at+TimeStampBits > len(msg)*8 {
		return fmt.Errorf("exi: time stamp field at bit %d outside %d bytes", at, len(msg))
	}
	bit := at
	for g := 0; g < tsGroups; g++ {
		octet := byte(t>>(7*uint(g))) & 0x7f
		if g < tsGroups-1 {
			octet |= 0x80
		}
		for i := 7; i >= 0; i-- {
			m := byte(1) << (7 - uint(bit&7))
			if octet>>uint(i)&1 == 1 {
				msg[bit>>3] |= m
			} else {
				msg[bit>>3] &^= m
			}
			bit++
		}
	}
	return nil
}

// ReadTimeStamp reads the field WriteTimeStamp writes (tests, the device's
// "last time stamp").
func ReadTimeStamp(msg []byte, at int) (uint64, error) {
	if at < 0 || at+TimeStampBits > len(msg)*8 {
		return 0, fmt.Errorf("exi: time stamp field at bit %d outside %d bytes", at, len(msg))
	}
	var t uint64
	for g := 0; g < tsGroups; g++ {
		var octet byte
		for i := 0; i < 8; i++ {
			b := at + 8*g + i
			octet = octet<<1 | (msg[b>>3]>>(7-uint(b&7)))&1
		}
		t |= uint64(octet&0x7f) << (7 * uint(g))
	}
	return t, nil
}

// PrefixMatch is the device's comparison, as the reference: the first bits of
// req equal those of the cached request.
func PrefixMatch(cached, req []byte, bits int) bool {
	if bits < 0 || len(cached)*8 < bits || len(req)*8 < bits {
		return false
	}
	full := bits / 8
	for i := 0; i < full; i++ {
		if cached[i] != req[i] {
			return false
		}
	}
	if r := bits % 8; r != 0 {
		m := byte(0xff) << uint(8-r)
		return cached[full]&m == req[full]&m
	}
	return true
}
