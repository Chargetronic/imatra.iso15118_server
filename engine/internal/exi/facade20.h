// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

// Flat facade over libcbv2g for ISO 15118-20, AC with external identification
// (EIM). Same rules as facade.h: plain structs in and out, no allocation on the
// caller's side, no globals except thread-local scratch documents.
//
// ISO 15118-20 splits the messages over schemas, and the V2GTP payload type
// tells which one a frame is encoded with:
//   0x8002 CommonMessages (SessionSetup ... ScheduleExchange, PowerDelivery, SessionStop)
//   0x8003 AC (AC_ChargeParameterDiscovery, AC_ChargeLoop)
#ifndef SP_EXI_FACADE20_H
#define SP_EXI_FACADE20_H

#include <stddef.h>
#include <stdint.h>

#define SP20_PT_MAIN 0x8002
#define SP20_PT_AC 0x8003

// RationalNumber: value * 10^exponent.
typedef struct {
    int16_t value;
    int8_t exponent;
} sp_rat;

enum sp20_kind {
    SP20_UNKNOWN = 0,
    SP20_SESSION_SETUP = 1,
    SP20_AUTHORIZATION_SETUP = 2,
    SP20_AUTHORIZATION = 3,
    SP20_SERVICE_DISCOVERY = 4,
    SP20_SERVICE_DETAIL = 5,
    SP20_SERVICE_SELECTION = 6,
    SP20_SCHEDULE_EXCHANGE = 7,
    SP20_POWER_DELIVERY = 8,
    SP20_SESSION_STOP = 9,
    SP20_AC_CHARGE_PARAMETER_DISCOVERY = 10,
    SP20_AC_CHARGE_LOOP = 11,
    // a valid message outside this edition (CertificateInstallation,
    // MeteringConfirmation, VehicleCheckIn/Out, BPT): answered with FAILED
    SP20_OTHER = 12,
};

// Control modes (ServiceDetail parameter "ControlMode").
#define SP20_MODE_NONE 0
#define SP20_MODE_SCHEDULED 1
#define SP20_MODE_DYNAMIC 2
#define SP20_MODE_BPT 3  // a BPT control mode (not offered in this edition)

typedef struct {
    int kind;  // enum sp20_kind
    uint8_t session_id[8];
    int session_id_len;
    uint64_t timestamp;

    // SessionSetupReq
    char evcc_id[256];

    // AuthorizationReq: 0 EIM, 1 PnC
    int selected_authorization;

    // ServiceDetailReq
    uint16_t service_id;

    // ServiceSelectionReq
    uint16_t selected_service_id;
    uint16_t selected_parameter_set_id;
    int n_selected_vas;

    // ScheduleExchangeReq (se_mode) and AC_ChargeLoopReq (cl_mode): SP20_MODE_*
    uint16_t max_supporting_points;
    int se_mode;
    int cl_mode;
    int has_departure_time;
    uint32_t departure_time;  // seconds from now
    int has_min_soc, has_target_soc;
    int8_t min_soc, target_soc;
    int has_target_energy, has_max_energy, has_min_energy;
    sp_rat target_energy, max_energy, min_energy;  // Wh

    // PowerDeliveryReq: processing 0 Finished 1 Ongoing; progress iso20_chargeProgressType
    int ev_processing;
    int charge_progress;
    int has_power_profile;
    int profile_mode;  // SP20_MODE_*
    uint32_t selected_schedule_tuple_id;

    // SessionStopReq: 0 Pause, 1 Terminate, 2 ServiceRenegotiation
    int charging_session;
    char termination_code[81];

    // AC_ChargeParameterDiscoveryReq / AC_ChargeLoopReq
    int bpt;  // the EV asked for the BPT variant
    int has_max_power, has_min_power;
    sp_rat max_power, min_power;  // W (L1, or the total for a symmetric EV)
    int has_max_power_l2, has_max_power_l3;
    sp_rat max_power_l2, max_power_l3;
    int has_present_power;
    sp_rat present_power;  // W

    // AC_ChargeLoopReq
    int meter_info_requested;
    int has_display;
    int has_present_soc, has_display_target_soc, has_display_min_soc;
    int8_t present_soc, display_target_soc, display_min_soc;
    int has_charging_complete;
    int charging_complete;
    int has_battery_capacity;
    sp_rat battery_capacity;  // Wh
} sp20_req;

typedef struct {
    uint32_t duration;  // s
    sp_rat power;       // W
} sp20_power_entry;

typedef struct {
    uint16_t id;
    int connector;  // 1 single-phase, 2 three-phase
    int control_mode;
    int nominal_voltage;
    int mobility_needs;  // 1 EV provides its needs, 2 EV or SECC
    int pricing;         // 0 none
} sp20_param_set;

typedef struct {
    int kind;  // enum sp20_kind: the response to this request kind
    uint8_t session_id[8];
    int session_id_len;
    uint64_t timestamp;
    int response_code;  // iso20_responseCodeType

    // SessionSetupRes
    char evse_id[256];

    // AuthorizationRes, ScheduleExchangeRes: 0 Finished, 1 Ongoing
    int evse_processing;

    // ServiceDiscoveryRes
    int n_services;
    uint16_t service_ids[4];
    int free_service[4];

    // ServiceDetailRes
    uint16_t service_id;
    int n_sets;
    sp20_param_set sets[4];

    // ScheduleExchangeRes
    int se_mode;  // SP20_MODE_SCHEDULED or SP20_MODE_DYNAMIC
    uint32_t schedule_tuple_id;
    uint64_t time_anchor;
    int n_entries;
    sp20_power_entry entries[24];

    // PowerDeliveryRes, AC_ChargeLoopRes: EVSEStatus (optional)
    int has_evse_status;
    uint16_t notification_max_delay;
    int evse_notification;  // iso20_evseNotificationType

    // AC_ChargeParameterDiscoveryRes
    sp_rat evse_max_power, evse_min_power, evse_nominal_frequency;

    // AC_ChargeLoopRes
    int cl_mode;  // SP20_MODE_SCHEDULED or SP20_MODE_DYNAMIC
    int has_target_power;  // mandatory in Dynamic, optional in Scheduled
    sp_rat target_power;   // W
} sp20_res;

// Server side: decode a request, encode a response. *pt_out: the payload type
// the response goes in.
int sp20_decode_req(int pt, const uint8_t* data, size_t len, sp20_req* out);
int sp20_encode_res(const sp20_res* in, uint8_t* out, size_t cap, size_t* out_len, int* pt_out);

// EV side (tests, the fleet generator): encode a request, decode a response.
int sp20_encode_req(const sp20_req* in, uint8_t* out, size_t cap, size_t* out_len, int* pt_out);
int sp20_decode_res(int pt, const uint8_t* data, size_t len, sp20_res* out);

// Bit offset of the header's TimeStamp in an encoded ISO 15118-20 message (any
// message of the two schemas, request or response): the message is decoded and
// encoded again with two time stamps of the same length; the offset is the
// first bit they differ in, less the continuation bit. Verified: the first
// *bit_out bits of the re-encoding equal those of the input (the prefix the
// device compares), and with verify_full the input equals the re-encoding with
// its own time stamp bit for bit (a response the engine encoded itself).
int sp20_ts_bit(int pt, const uint8_t* data, size_t len, int verify_full, int* bit_out);

#endif
