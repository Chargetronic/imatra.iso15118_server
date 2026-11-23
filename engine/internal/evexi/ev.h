// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

// EV side of ISO 15118-2 (EIM, AC) over libcbv2g, for the fleet generator
// (cmd/fleetgen) only: the engine never encodes requests. Kept out of the
// engine's exi package so the engine binary does not carry it.
#ifndef EV_EXI_H
#define EV_EXI_H

#include <stddef.h>
#include <stdint.h>

#define EV_OK 0
#define EV_ERR_DECODE -1
#define EV_ERR_ENCODE -2
#define EV_ERR_UNSUPPORTED -3
#define EV_ERR_ARG -4

// Message kinds: same numbers as the engine's sp_iso2_kind.
enum ev_kind {
    EV_UNKNOWN = 0,
    EV_SESSION_SETUP = 1,
    EV_SERVICE_DISCOVERY = 2,
    EV_PAYMENT_SERVICE_SELECTION = 4,
    EV_AUTHORIZATION = 6,
    EV_CHARGE_PARAMETER_DISCOVERY = 7,
    EV_POWER_DELIVERY = 8,
    EV_CHARGING_STATUS = 9,
    EV_SESSION_STOP = 11,
};

typedef struct {
    int kind;
    uint8_t session_id[8];
    int session_id_len;
    uint8_t evcc_id[6];        // SessionSetupReq
    uint32_t departure_time;   // CPD: seconds from now
    int32_t e_amount_wh;       // CPD
    int32_t max_current_a;     // CPD, per phase
    int charge_progress;       // PowerDeliveryReq: 0 Start, 1 Stop
    uint8_t sa_tuple_id;       // PowerDeliveryReq
    int pause;                 // SessionStopReq: 0 Terminate, 1 Pause
} ev_req;

typedef struct {
    int kind;                  // which response it is
    uint8_t session_id[8];
    int session_id_len;
    int response_code;
    int evse_processing;       // Authorization, CPD: 0 Finished, 1 Ongoing
    int evse_notification;     // AC_EVSEStatus: 0 None, 1 StopCharging, 2 ReNegotiation
    int sa_tuple_id;           // first tuple of CPD, or ChargingStatusRes
    int has_max_current;
    int32_t max_current_da;    // EVSEMaxCurrent, in 0.1 A
} ev_res;

// supportedAppProtocolReq offering ISO 15118-2 only (SchemaID 1).
int ev_encode_apphand_req(uint8_t* out, size_t cap, size_t* out_len);
int ev_encode_apphand_req20(uint8_t* out, size_t cap, size_t* out_len);
int ev_encode_req(const ev_req* in, uint8_t* out, size_t cap, size_t* out_len);
int ev_decode_res(const uint8_t* data, size_t len, ev_res* out);

#endif
