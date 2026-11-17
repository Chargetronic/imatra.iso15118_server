// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

// Package evexi is the EV side of ISO 15118-2 (EIM, AC) for the fleet
// generator: it encodes the requests a real EV sends and reads the engine's
// responses. The engine itself does not use it.
package evexi

/*
#cgo LDFLAGS: -lcbv2g_iso2 -lcbv2g_exi_codec
#include <stdlib.h>
#include "ev.h"
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// Kind of an ISO 15118-2 message pair (numbers as in the engine's exi.Kind);
// 0 is also used for the supportedAppProtocol handshake in reports.
type Kind int

const (
	Unknown                  Kind = 0
	SessionSetup             Kind = 1
	ServiceDiscovery         Kind = 2
	PaymentServiceSelection  Kind = 4
	Authorization            Kind = 6
	ChargeParameterDiscovery Kind = 7
	PowerDelivery            Kind = 8
	ChargingStatus           Kind = 9
	SessionStop              Kind = 11
)

var kindNames = map[Kind]string{
	Unknown:      "AppHand",
	SessionSetup: "SessionSetup", ServiceDiscovery: "ServiceDiscovery",
	PaymentServiceSelection: "PaymentServiceSelection", Authorization: "Authorization",
	ChargeParameterDiscovery: "ChargeParameterDiscovery", PowerDelivery: "PowerDelivery",
	ChargingStatus: "ChargingStatus", SessionStop: "SessionStop",
}

func (k Kind) String() string {
	if s, ok := kindNames[k]; ok {
		return s
	}
	return fmt.Sprintf("Kind(%d)", int(k))
}

// Req is one EV request.
type Req struct {
	Kind           Kind
	SessionID      []byte // empty before SessionSetupRes
	EVCCID         [6]byte
	DepartureS     uint32 // CPD
	EAmountWh      int32  // CPD
	MaxCurrentA    int32  // CPD, per phase
	ChargeProgress int    // PowerDelivery: 0 Start, 1 Stop
	SATupleID      uint8  // PowerDelivery
	Pause          bool   // SessionStop
}

// Res is what the generator needs from a response.
type Res struct {
	Kind          Kind
	SessionID     []byte
	ResponseCode  int
	Ongoing       bool // EVSEProcessing Ongoing (Authorization, CPD)
	Notification  int  // 0 None, 1 StopCharging, 2 ReNegotiation
	SATupleID     int
	MaxCurrentA   float64
	HasMaxCurrent bool
}

const maxEXI = 1024

// EncodeAppHandReq: supportedAppProtocolReq offering ISO 15118-2 (SchemaID 1).
func EncodeAppHandReq() ([]byte, error) {
	buf := make([]byte, maxEXI)
	var n C.size_t
	if rc := C.ev_encode_apphand_req((*C.uint8_t)(unsafe.Pointer(&buf[0])), C.size_t(len(buf)), &n); rc != C.EV_OK {
		return nil, fmt.Errorf("evexi: apphand encode: %d", int(rc))
	}
	return buf[:n], nil
}

// EncodeReq encodes an ISO 15118-2 request.
func EncodeReq(r *Req) ([]byte, error) {
	var in C.ev_req
	in.kind = C.int(r.Kind)
	n := len(r.SessionID)
	if n > 8 {
		n = 8
	}
	for i := 0; i < n; i++ {
		in.session_id[i] = C.uint8_t(r.SessionID[i])
	}
	in.session_id_len = C.int(n)
	for i := 0; i < 6; i++ {
		in.evcc_id[i] = C.uint8_t(r.EVCCID[i])
	}
	in.departure_time = C.uint32_t(r.DepartureS)
	in.e_amount_wh = C.int32_t(r.EAmountWh)
	in.max_current_a = C.int32_t(r.MaxCurrentA)
	in.charge_progress = C.int(r.ChargeProgress)
	in.sa_tuple_id = C.uint8_t(r.SATupleID)
	if r.Pause {
		in.pause = 1
	}
	buf := make([]byte, maxEXI)
	var out C.size_t
	if rc := C.ev_encode_req(&in, (*C.uint8_t)(unsafe.Pointer(&buf[0])), C.size_t(len(buf)), &out); rc != C.EV_OK {
		return nil, fmt.Errorf("evexi: encode %v: %d", r.Kind, int(rc))
	}
	return buf[:out], nil
}

// DecodeRes reads an ISO 15118-2 response.
func DecodeRes(b []byte) (*Res, error) {
	if len(b) == 0 {
		return nil, fmt.Errorf("evexi: empty response")
	}
	var out C.ev_res
	if rc := C.ev_decode_res((*C.uint8_t)(unsafe.Pointer(&b[0])), C.size_t(len(b)), &out); rc != C.EV_OK {
		return nil, fmt.Errorf("evexi: decode: %d", int(rc))
	}
	r := &Res{
		Kind:          Kind(out.kind),
		ResponseCode:  int(out.response_code),
		Ongoing:       out.evse_processing == 1,
		Notification:  int(out.evse_notification),
		SATupleID:     int(out.sa_tuple_id),
		HasMaxCurrent: out.has_max_current != 0,
		MaxCurrentA:   float64(out.max_current_da) / 10,
	}
	n := int(out.session_id_len)
	r.SessionID = make([]byte, n)
	for i := 0; i < n; i++ {
		r.SessionID[i] = byte(out.session_id[i])
	}
	return r, nil
}
