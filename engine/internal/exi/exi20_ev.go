// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

package exi

/*
#include <stdlib.h>
#include "facade20.h"
*/
import "C"

import "unsafe"

// The EV side of ISO 15118-20 (tests, the fleet generator): encode a request,
// decode a response. Same flat types as the server side.

func setRat(has *C.int, dst *C.sp_rat, r *Rat) {
	if r != nil {
		*has = 1
		*dst = ratTo(*r)
	}
}

// EncodeISO20Req encodes an EV request; the second value is its payload type.
func EncodeISO20Req(q *Request20) ([]byte, uint16, error) {
	in := (*C.sp20_req)(C.malloc(C.size_t(unsafe.Sizeof(C.sp20_req{}))))
	defer C.free(unsafe.Pointer(in))
	*in = C.sp20_req{}
	in.kind = C.int(q.Kind)
	n := copy(unsafe.Slice((*byte)(unsafe.Pointer(&in.session_id[0])), 8), q.SessionID)
	in.session_id_len = C.int(n)
	in.timestamp = C.uint64_t(q.TimeStamp)
	id := q.EVCCID
	if len(id) > 255 {
		id = id[:255]
	}
	copy(unsafe.Slice((*byte)(unsafe.Pointer(&in.evcc_id[0])), 256), id)
	if q.SelectedAuthorization != nil {
		in.selected_authorization = C.int(*q.SelectedAuthorization)
	}
	if q.ServiceID != nil {
		in.service_id = C.uint16_t(*q.ServiceID)
	}
	if q.SelectedServiceID != nil {
		in.selected_service_id = C.uint16_t(*q.SelectedServiceID)
	}
	if q.SelectedParameterSetID != nil {
		in.selected_parameter_set_id = C.uint16_t(*q.SelectedParameterSetID)
	}
	if q.MaxSupportingPoints != nil {
		in.max_supporting_points = C.uint16_t(*q.MaxSupportingPoints)
	}
	in.se_mode = C.int(q.ControlMode)
	in.cl_mode = C.int(q.ControlMode)
	if q.DepartureTime != nil {
		in.has_departure_time, in.departure_time = 1, C.uint32_t(*q.DepartureTime)
	}
	if q.MinSoC != nil {
		in.has_min_soc, in.min_soc = 1, C.int8_t(*q.MinSoC)
	}
	if q.TargetSoC != nil {
		in.has_target_soc, in.target_soc = 1, C.int8_t(*q.TargetSoC)
	}
	setRat(&in.has_target_energy, &in.target_energy, q.TargetEnergy)
	setRat(&in.has_max_energy, &in.max_energy, q.MaxEnergy)
	setRat(&in.has_min_energy, &in.min_energy, q.MinEnergy)
	if q.EVProcessing != nil {
		in.ev_processing = C.int(*q.EVProcessing)
	}
	if q.ChargeProgress != nil {
		in.charge_progress = C.int(*q.ChargeProgress)
	}
	if q.ProfileMode != ModeNone {
		in.has_power_profile, in.profile_mode = 1, C.int(q.ProfileMode)
		if q.SelectedScheduleTuple != nil {
			in.selected_schedule_tuple_id = C.uint32_t(*q.SelectedScheduleTuple)
		}
	}
	if q.ChargingSession != nil {
		in.charging_session = C.int(*q.ChargingSession)
	}
	setRat(&in.has_max_power, &in.max_power, q.MaxPower)
	setRat(&in.has_min_power, &in.min_power, q.MinPower)
	setRat(&in.has_present_power, &in.present_power, q.PresentPower)
	if q.MeterInfoRequested {
		in.meter_info_requested = 1
	}
	if q.PresentSoC != nil {
		in.has_display, in.has_present_soc, in.present_soc = 1, 1, C.int8_t(*q.PresentSoC)
	}
	buf := make([]byte, 4096)
	var outLen C.size_t
	var pt C.int
	if err := errOf(C.sp20_encode_req(in, (*C.uint8_t)(unsafe.Pointer(&buf[0])), C.size_t(len(buf)), &outLen, &pt)); err != nil {
		return nil, 0, err
	}
	return buf[:outLen], uint16(pt), nil
}

// DecodeISO20Res decodes a response as the EV receives it.
func DecodeISO20Res(pt uint16, data []byte) (*Response20, error) {
	if len(data) == 0 {
		return nil, ErrDecode
	}
	r := (*C.sp20_res)(C.malloc(C.size_t(unsafe.Sizeof(C.sp20_res{}))))
	defer C.free(unsafe.Pointer(r))
	if err := errOf(C.sp20_decode_res(C.int(pt), (*C.uint8_t)(unsafe.Pointer(&data[0])), C.size_t(len(data)), r)); err != nil {
		return nil, err
	}
	s := &Response20{Kind: Kind20(r.kind), TimeStamp: uint64(r.timestamp), ResponseCode: int(r.response_code)}
	s.Type, s.ResponseName = s.Kind.String()+"Res", RC20Name(s.ResponseCode)
	s.SessionID = C.GoBytes(unsafe.Pointer(&r.session_id[0]), r.session_id_len)
	s.EVSEID = C.GoString(&r.evse_id[0])
	s.EVSEProcessing = int(r.evse_processing)
	for i := 0; i < int(r.n_services); i++ {
		s.Services = append(s.Services, uint16(r.service_ids[i]))
	}
	s.ServiceID = uint16(r.service_id)
	for i := 0; i < int(r.n_sets); i++ {
		p := r.sets[i]
		s.ParamSets = append(s.ParamSets, ParamSet{ID: uint16(p.id), Connector: int(p.connector),
			ControlMode: int(p.control_mode), NominalVoltage: int(p.nominal_voltage),
			MobilityNeeds: int(p.mobility_needs), Pricing: int(p.pricing)})
	}
	switch s.Kind {
	case K20ScheduleExchange:
		s.ControlMode = int(r.se_mode)
	case K20ACChargeLoop:
		s.ControlMode = int(r.cl_mode)
	}
	s.ScheduleTuple, s.TimeAnchor = uint32(r.schedule_tuple_id), uint64(r.time_anchor)
	for i := 0; i < int(r.n_entries); i++ {
		s.Schedule = append(s.Schedule, PowerEntry{DurationS: uint32(r.entries[i].duration), Power: ratFrom(r.entries[i].power)})
	}
	if r.has_evse_status != 0 {
		s.Notification = ptr(int(r.evse_notification))
		s.NotificationMaxDelay = uint16(r.notification_max_delay)
	}
	if s.Kind == K20ACChargeParamDisc {
		s.EVSEMaxPower = ptr(ratFrom(r.evse_max_power))
		s.EVSEMinPower = ptr(ratFrom(r.evse_min_power))
		s.EVSENominalFrequency = ptr(ratFrom(r.evse_nominal_frequency))
	}
	if r.has_target_power != 0 {
		s.TargetPower = ptr(ratFrom(r.target_power))
	}
	return s, nil
}
