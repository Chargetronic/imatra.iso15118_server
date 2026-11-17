// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

// Flat facade over libcbv2g for the server engine.
//
// libcbv2g message bodies are C unions with bit-field "_isUsed" flags, which
// Go (cgo) cannot read or write. This facade turns a decoded request into a
// plain struct and builds a response from a plain struct, so all protocol
// logic stays in Go. No allocation, no globals: safe to call concurrently.
#ifndef SP_EXI_FACADE_H
#define SP_EXI_FACADE_H

#include <stddef.h>
#include <stdint.h>

#define SP_OK 0
#define SP_ERR_DECODE -1
#define SP_ERR_ENCODE -2
#define SP_ERR_UNSUPPORTED -3
#define SP_ERR_ARG -4

// ---------------------------------------------------------------- handshake
typedef struct {
    char ns[101];
    uint32_t major;
    uint32_t minor;
    uint8_t schema_id;
    uint8_t priority;
} sp_app_protocol;

typedef struct {
    int n;
    sp_app_protocol p[5];
} sp_apphand_req;

int sp_decode_apphand(const uint8_t* data, size_t len, sp_apphand_req* out);
// response_code: 0 OK_SuccessfulNegotiation, 1 ..WithMinorDeviation, 2 Failed_NoNegotiation
int sp_encode_apphand_res(int response_code, int schema_id, uint8_t* out, size_t cap, size_t* out_len);

// ----------------------------------------------------------- ISO 15118-2
typedef struct {
    int16_t value;
    int8_t multiplier;
    int8_t unit;  // iso2_unitSymbolType: 0 h, 1 m, 2 s, 3 A, 4 V, 5 W, 6 Wh
} sp_pv;

enum sp_iso2_kind {
    // EIM, AC only. Plug & Charge (ServiceDetail, PaymentDetails, MeteringReceipt,
    // Certificate*) and DC messages decode as UNKNOWN until they are needed.
    SP_ISO2_UNKNOWN = 0,
    SP_ISO2_SESSION_SETUP = 1,
    SP_ISO2_SERVICE_DISCOVERY = 2,
    SP_ISO2_PAYMENT_SERVICE_SELECTION = 4,
    SP_ISO2_AUTHORIZATION = 6,
    SP_ISO2_CHARGE_PARAMETER_DISCOVERY = 7,
    SP_ISO2_POWER_DELIVERY = 8,
    SP_ISO2_CHARGING_STATUS = 9,
    SP_ISO2_SESSION_STOP = 11,
};

typedef struct {
    int kind;  // enum sp_iso2_kind
    uint8_t session_id[8];
    int session_id_len;

    // SessionSetupReq
    uint8_t evcc_id[6];
    int evcc_id_len;


    // PaymentServiceSelectionReq
    int selected_payment_option;  // 0 Contract, 1 ExternalPayment
    int n_selected_services;
    uint16_t selected_service_ids[16];


    // ChargeParameterDiscoveryReq
    int requested_energy_transfer_mode;  // iso2_EnergyTransferModeType
    int has_ac;
    int has_departure_time;
    uint32_t departure_time;  // seconds from now
    sp_pv e_amount;
    sp_pv ev_max_voltage;
    sp_pv ev_max_current;
    sp_pv ev_min_current;

    // PowerDeliveryReq
    int charge_progress;  // 0 Start, 1 Stop, 2 Renegotiate
    uint8_t sa_schedule_tuple_id;
    int n_profile;
    struct {
        uint32_t start;
        sp_pv max_power;
        int has_phases;
        int8_t phases;
    } profile[24];

    // SessionStopReq
    int charging_session;  // 0 Terminate, 1 Pause
} sp_iso2_req;

typedef struct {
    uint32_t start;
    int has_duration;
    uint32_t duration;
    sp_pv pmax;
} sp_pmax_entry;

typedef struct {
    int kind;  // enum sp_iso2_kind (response for this request kind)
    uint8_t session_id[8];
    int session_id_len;
    int response_code;  // iso2_responseCodeType

    // SessionSetupRes, ChargingStatusRes
    char evse_id[38];
    int has_timestamp;
    int64_t timestamp;

    // ServiceDiscoveryRes
    int n_payment_options;
    int payment_options[2];
    uint16_t service_id;
    int free_service;
    int n_energy_modes;
    int energy_modes[6];

    // AuthorizationRes, ChargeParameterDiscoveryRes
    int evse_processing;  // 0 Finished, 1 Ongoing

    // ChargeParameterDiscoveryRes
    int n_tuples;
    struct {
        uint8_t id;
        int n_entries;
        sp_pmax_entry entries[12];
    } tuples[3];
    sp_pv evse_nominal_voltage;

    // AC_EVSEStatus (CPD, PowerDelivery, ChargingStatus)
    uint16_t notification_max_delay;
    int evse_notification;  // 0 None, 1 StopCharging, 2 ReNegotiation
    int rcd;

    // CPD (mandatory there), ChargingStatus (optional)
    int has_evse_max_current;
    sp_pv evse_max_current;

    // ChargingStatusRes
    uint8_t sa_schedule_tuple_id;
    int has_receipt_required;
    int receipt_required;
} sp_iso2_res;

int sp_decode_iso2(const uint8_t* data, size_t len, sp_iso2_req* out);
int sp_encode_iso2_res(const sp_iso2_res* in, uint8_t* out, size_t cap, size_t* out_len);

// ----------------------------------------------------------- DIN 70121
// Only the handshake that reads the EV's data (SoC, DC limits): the requests up
// to and including ChargeParameterDiscovery, plus SessionStop. No DC session:
// the engine stops the EV there and never delivers power.
enum sp_din_kind {
    SP_DIN_UNKNOWN = 0,
    SP_DIN_SESSION_SETUP = 1,
    SP_DIN_SERVICE_DISCOVERY = 2,
    SP_DIN_SERVICE_PAYMENT_SELECTION = 3,
    SP_DIN_CONTRACT_AUTHENTICATION = 4,
    SP_DIN_CHARGE_PARAMETER_DISCOVERY = 5,
    SP_DIN_SESSION_STOP = 6,
    SP_DIN_OTHER = 7,  // any other DIN message (CableCheck, PreCharge, ...): not answered
};

typedef struct {
    int kind;  // enum sp_din_kind
    uint8_t session_id[8];
    int session_id_len;
    uint8_t evcc_id[8];
    int evcc_id_len;
    int selected_payment_option;
    int n_selected_services;
    uint16_t selected_service_ids[16];
    int requested_energy_transfer;  // din_EVRequestedEnergyTransferType
    int has_dc;
    int dc_ev_ready;
    int dc_ev_error_code;
    int8_t dc_soc;
    sp_pv dc_max_current;
    sp_pv dc_max_voltage;
    int has_dc_max_power;
    sp_pv dc_max_power;
    int has_dc_energy_capacity;
    sp_pv dc_energy_capacity;
    int has_dc_energy_request;
    sp_pv dc_energy_request;
    int has_dc_full_soc;
    int8_t dc_full_soc;
    int has_dc_bulk_soc;
    int8_t dc_bulk_soc;
} sp_din_req;

typedef struct {
    int kind;  // enum sp_din_kind (response for this request kind)
    uint8_t session_id[8];
    int session_id_len;
    int response_code;  // din_responseCodeType
    int has_timestamp;
    int64_t timestamp;
    int evse_processing;
    // ChargeParameterDiscoveryRes: DC_EVSEStatus
    int evse_status_code;   // din_DC_EVSEStatusCodeType
    int evse_notification;  // din_EVSENotificationType
    int isolation;          // din_isolationLevelType, -1 = not sent
    // ChargeParameterDiscoveryRes: DC_EVSEChargeParameter and one PMax entry
    sp_pv max_current, max_voltage, min_current, min_voltage, peak_ripple, max_power;
    int16_t pmax_w_div;     // PMax in the unit of the schema (W, multiplier 0 -> capped to int16)
} sp_din_res;

int sp_decode_din(const uint8_t* data, size_t len, sp_din_req* out);
int sp_encode_din_res(const sp_din_res* in, uint8_t* out, size_t cap, size_t* out_len);

#endif
