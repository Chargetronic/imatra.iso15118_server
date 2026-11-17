// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

package exi

/*
#include <stdlib.h>
#include "facade.h"
*/
import "C"

import (
	"fmt"
	"math"
	"unsafe"
)

// DIN 70121: only the handshake that reads the EV's data, up to
// ChargeParameterDiscovery (see facade.h). No DC session, no power.

// DINKind is the message kind of a DIN 70121 request.
type DINKind int

const (
	DINUnknown DINKind = iota
	DINSessionSetup
	DINServiceDiscovery
	DINServicePaymentSelection
	DINContractAuthentication
	DINChargeParameterDiscovery
	DINSessionStop
	DINOther
)

var dinNames = []string{"Unknown", "SessionSetup", "ServiceDiscovery", "ServicePaymentSelection",
	"ContractAuthentication", "ChargeParameterDiscovery", "SessionStop", "Other"}

func (k DINKind) String() string {
	if int(k) >= 0 && int(k) < len(dinNames) {
		return dinNames[k]
	}
	return fmt.Sprintf("DINKind(%d)", int(k))
}

// DIN unit symbols (din_unitSymbolType) — differ from ISO 15118-2.
const (
	DINUnitA  int8 = 3
	DINUnitV  int8 = 5
	DINUnitW  int8 = 7
	DINUnitWh int8 = 9
)

// DIN status values used by the engine.
const (
	DINEVSEReady         = 1 // DC_EVSEStatusCode EVSE_Ready
	DINEVSEShutdown      = 2 // EVSE_Shutdown
	DINNotifNone         = 0
	DINNotifStopCharging = 1
	DINIsolationInvalid  = 0
	DINProcessFinished   = 0
	DINProcessOngoing    = 1
)

// DINRequest is a decoded DIN 70121 request.
type DINRequest struct {
	Kind      DINKind `json:"-"`
	Type      string  `json:"type"`
	SessionID []byte  `json:"-"`
	EVCCID    []byte  `json:"evcc_id,omitempty"`

	SelectedPaymentOption *int     `json:"selected_payment_option,omitempty"`
	SelectedServiceIDs    []uint16 `json:"selected_service_ids,omitempty"`

	RequestedEnergyTransfer *int   `json:"requested_energy_transfer,omitempty"`
	DC                      *DINDC `json:"dc,omitempty"`
}

// DINDC is DC_EVChargeParameter of ChargeParameterDiscoveryReq.
type DINDC struct {
	EVReady        bool  `json:"ev_ready"`
	ErrorCode      int   `json:"ev_error_code"`
	SoC            int8  `json:"soc"`
	MaxCurrent     PV    `json:"ev_max_current"`
	MaxVoltage     PV    `json:"ev_max_voltage"`
	MaxPower       *PV   `json:"ev_max_power,omitempty"`
	EnergyCapacity *PV   `json:"ev_energy_capacity,omitempty"`
	EnergyRequest  *PV   `json:"ev_energy_request,omitempty"`
	FullSoC        *int8 `json:"full_soc,omitempty"`
	BulkSoC        *int8 `json:"bulk_soc,omitempty"`
}

// DINResponse describes a response; EncodeDINRes builds it.
type DINResponse struct {
	Kind             DINKind `json:"-"`
	Type             string  `json:"type"`
	SessionID        []byte  `json:"-"`
	ResponseCode     int     `json:"response_code"`
	ResponseName     string  `json:"response_name"`
	Timestamp        *int64  `json:"timestamp,omitempty"`
	EVSEProcessing   int     `json:"evse_processing"`
	EVSEStatusCode   int     `json:"evse_status_code"`
	EVSENotification int     `json:"evse_notification"`
	Isolation        int     `json:"isolation"` // -1: not sent
	MaxCurrent       PV      `json:"evse_max_current"`
	MaxVoltage       PV      `json:"evse_max_voltage"`
	MinCurrent       PV      `json:"evse_min_current"`
	MinVoltage       PV      `json:"evse_min_voltage"`
	PeakRipple       PV      `json:"evse_peak_current_ripple"`
	MaxPower         PV      `json:"evse_max_power"`
	PMaxW            float64 `json:"pmax_w"`
}

func dinPV(p C.sp_pv) PV {
	return PV{Value: int16(p.value), Multiplier: int8(p.multiplier), Unit: int8(p.unit)}
}

// DecodeDIN decodes one DIN 70121 V2G message.
func DecodeDIN(data []byte) (*DINRequest, error) {
	if len(data) == 0 {
		return nil, ErrDecode
	}
	var r C.sp_din_req
	cdata := C.CBytes(data)
	defer C.free(cdata)
	if err := errOf(C.sp_decode_din((*C.uint8_t)(cdata), C.size_t(len(data)), &r)); err != nil {
		return nil, err
	}
	q := &DINRequest{Kind: DINKind(r.kind)}
	q.Type = q.Kind.String() + "Req"
	q.SessionID = C.GoBytes(unsafe.Pointer(&r.session_id[0]), r.session_id_len)
	switch q.Kind {
	case DINSessionSetup:
		q.EVCCID = C.GoBytes(unsafe.Pointer(&r.evcc_id[0]), r.evcc_id_len)
	case DINServicePaymentSelection:
		q.SelectedPaymentOption = ptr(int(r.selected_payment_option))
		for i := 0; i < int(r.n_selected_services); i++ {
			q.SelectedServiceIDs = append(q.SelectedServiceIDs, uint16(r.selected_service_ids[i]))
		}
	case DINChargeParameterDiscovery:
		q.RequestedEnergyTransfer = ptr(int(r.requested_energy_transfer))
		if r.has_dc != 0 {
			d := &DINDC{EVReady: r.dc_ev_ready != 0, ErrorCode: int(r.dc_ev_error_code), SoC: int8(r.dc_soc),
				MaxCurrent: dinPV(r.dc_max_current), MaxVoltage: dinPV(r.dc_max_voltage)}
			if r.has_dc_max_power != 0 {
				d.MaxPower = ptr(dinPV(r.dc_max_power))
			}
			if r.has_dc_energy_capacity != 0 {
				d.EnergyCapacity = ptr(dinPV(r.dc_energy_capacity))
			}
			if r.has_dc_energy_request != 0 {
				d.EnergyRequest = ptr(dinPV(r.dc_energy_request))
			}
			if r.has_dc_full_soc != 0 {
				d.FullSoC = ptr(int8(r.dc_full_soc))
			}
			if r.has_dc_bulk_soc != 0 {
				d.BulkSoC = ptr(int8(r.dc_bulk_soc))
			}
			q.DC = d
		}
	}
	return q, nil
}

// EncodeDINRes encodes a response.
func EncodeDINRes(s *DINResponse) ([]byte, error) {
	in := (*C.sp_din_res)(C.malloc(C.size_t(unsafe.Sizeof(C.sp_din_res{}))))
	defer C.free(unsafe.Pointer(in))
	*in = C.sp_din_res{}
	in.kind = C.int(s.Kind)
	n := copy(unsafe.Slice((*byte)(unsafe.Pointer(&in.session_id[0])), 8), s.SessionID)
	in.session_id_len = C.int(n)
	in.response_code = C.int(s.ResponseCode)
	if s.Timestamp != nil {
		in.has_timestamp = 1
		in.timestamp = C.int64_t(*s.Timestamp)
	}
	in.evse_processing = C.int(s.EVSEProcessing)
	in.evse_status_code = C.int(s.EVSEStatusCode)
	in.evse_notification = C.int(s.EVSENotification)
	in.isolation = C.int(s.Isolation)
	in.max_current, in.max_voltage = pvTo(s.MaxCurrent), pvTo(s.MaxVoltage)
	in.min_current, in.min_voltage, in.peak_ripple = pvTo(s.MinCurrent), pvTo(s.MinVoltage), pvTo(s.PeakRipple)
	in.max_power = pvTo(s.MaxPower)
	p := s.PMaxW
	if p > math.MaxInt16 {
		p = math.MaxInt16 // PMax is a short in DIN 70121 (W)
	}
	in.pmax_w_div = C.int16_t(p)

	buf := make([]byte, 2048)
	var outLen C.size_t
	if err := errOf(C.sp_encode_din_res(in, (*C.uint8_t)(unsafe.Pointer(&buf[0])), C.size_t(len(buf)), &outLen)); err != nil {
		return nil, err
	}
	s.Type = s.Kind.String() + "Res"
	s.ResponseName = RCName(s.ResponseCode)
	return buf[:outLen], nil
}
