// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

#include "ev.h"

#include <string.h>

#include <cbv2g/app_handshake/appHand_Datatypes.h>
#include <cbv2g/app_handshake/appHand_Encoder.h>
#include <cbv2g/common/exi_bitstream.h>
#include <cbv2g/iso_2/iso2_msgDefDatatypes.h>
#include <cbv2g/iso_2/iso2_msgDefDecoder.h>
#include <cbv2g/iso_2/iso2_msgDefEncoder.h>

#define EV_MAX_EXI 8192

static const char ISO2_NS[] = "urn:iso:15118:2:2013:MsgDef";

static void pv(struct iso2_PhysicalValueType* v, int32_t value, int unit) {
    int8_t mult = 0;
    while (value > 32767 || value < -32768) {
        value /= 10;
        mult++;
    }
    v->Value = (int16_t)value;
    v->Multiplier = mult;
    v->Unit = (iso2_unitSymbolType)unit;
}

static int32_t pv_da(const struct iso2_PhysicalValueType* v) {
    double x = v->Value;
    for (int i = 0; i < v->Multiplier; i++) x *= 10;
    for (int i = 0; i > v->Multiplier; i--) x /= 10;
    return (int32_t)(x * 10 + (x >= 0 ? 0.5 : -0.5));
}

int ev_encode_apphand_req(uint8_t* out, size_t cap, size_t* out_len) {
    if (!out || !out_len) return EV_ERR_ARG;
    struct appHand_exiDocument doc;
    memset(&doc, 0, sizeof(doc));
    doc.supportedAppProtocolReq_isUsed = 1;
    struct appHand_AppProtocolType* p = &doc.supportedAppProtocolReq.AppProtocol.array[0];
    doc.supportedAppProtocolReq.AppProtocol.arrayLen = 1;
    memcpy(p->ProtocolNamespace.characters, ISO2_NS, sizeof(ISO2_NS) - 1);
    p->ProtocolNamespace.charactersLen = sizeof(ISO2_NS) - 1;
    p->VersionNumberMajor = 2;
    p->VersionNumberMinor = 0;
    p->SchemaID = 1;
    p->Priority = 1;
    exi_bitstream_t s;
    exi_bitstream_init(&s, out, cap, 0, NULL);
    if (encode_appHand_exiDocument(&s, &doc) != 0) return EV_ERR_ENCODE;
    *out_len = exi_bitstream_get_length(&s);
    return EV_OK;
}

// supportedAppProtocolReq of an EV with ISO 15118-20 AC (SchemaID 2, priority
// 1) and ISO 15118-2 (SchemaID 1, priority 2).
int ev_encode_apphand_req20(uint8_t* out, size_t cap, size_t* out_len) {
    if (!out || !out_len) return EV_ERR_ARG;
    static const char ns20[] = "urn:iso:std:iso:15118:-20:AC";
    struct appHand_exiDocument doc;
    memset(&doc, 0, sizeof(doc));
    doc.supportedAppProtocolReq_isUsed = 1;
    struct appHand_AppProtocolType* p = &doc.supportedAppProtocolReq.AppProtocol.array[0];
    memcpy(p->ProtocolNamespace.characters, ns20, sizeof(ns20) - 1);
    p->ProtocolNamespace.charactersLen = sizeof(ns20) - 1;
    p->VersionNumberMajor = 1;
    p->SchemaID = 2;
    p->Priority = 1;
    p = &doc.supportedAppProtocolReq.AppProtocol.array[1];
    memcpy(p->ProtocolNamespace.characters, ISO2_NS, sizeof(ISO2_NS) - 1);
    p->ProtocolNamespace.charactersLen = sizeof(ISO2_NS) - 1;
    p->VersionNumberMajor = 2;
    p->SchemaID = 1;
    p->Priority = 2;
    doc.supportedAppProtocolReq.AppProtocol.arrayLen = 2;
    exi_bitstream_t s;
    exi_bitstream_init(&s, out, cap, 0, NULL);
    if (encode_appHand_exiDocument(&s, &doc) != 0) return EV_ERR_ENCODE;
    *out_len = exi_bitstream_get_length(&s);
    return EV_OK;
}

int ev_encode_req(const ev_req* in, uint8_t* out, size_t cap, size_t* out_len) {
    if (!in || !out || !out_len) return EV_ERR_ARG;
    static _Thread_local struct iso2_exiDocument doc;
    memset(&doc, 0, sizeof(doc));
    struct iso2_V2G_Message* m = &doc.V2G_Message;
    int sl = in->session_id_len > 8 ? 8 : in->session_id_len;
    if (sl <= 0) {  // before SessionSetupRes: SessionID 0x00
        m->Header.SessionID.bytes[0] = 0;
        m->Header.SessionID.bytesLen = 1;
    } else {
        memcpy(m->Header.SessionID.bytes, in->session_id, sl);
        m->Header.SessionID.bytesLen = (uint16_t)sl;
    }
    struct iso2_BodyType* b = &m->Body;
    switch (in->kind) {
    case EV_SESSION_SETUP:
        b->SessionSetupReq_isUsed = 1;
        memcpy(b->SessionSetupReq.EVCCID.bytes, in->evcc_id, 6);
        b->SessionSetupReq.EVCCID.bytesLen = 6;
        break;
    case EV_SERVICE_DISCOVERY:
        b->ServiceDiscoveryReq_isUsed = 1;
        break;
    case EV_PAYMENT_SERVICE_SELECTION:
        b->PaymentServiceSelectionReq_isUsed = 1;
        b->PaymentServiceSelectionReq.SelectedPaymentOption = iso2_paymentOptionType_ExternalPayment;
        b->PaymentServiceSelectionReq.SelectedServiceList.SelectedService.array[0].ServiceID = 1;
        b->PaymentServiceSelectionReq.SelectedServiceList.SelectedService.arrayLen = 1;
        break;
    case EV_AUTHORIZATION:
        b->AuthorizationReq_isUsed = 1;
        break;
    case EV_CHARGE_PARAMETER_DISCOVERY: {
        b->ChargeParameterDiscoveryReq_isUsed = 1;
        struct iso2_ChargeParameterDiscoveryReqType* r = &b->ChargeParameterDiscoveryReq;
        r->RequestedEnergyTransferMode = iso2_EnergyTransferModeType_AC_three_phase_core;
        r->AC_EVChargeParameter_isUsed = 1;
        struct iso2_AC_EVChargeParameterType* a = &r->AC_EVChargeParameter;
        if (in->departure_time) {
            a->DepartureTime = in->departure_time;
            a->DepartureTime_isUsed = 1;
        }
        pv(&a->EAmount, in->e_amount_wh, iso2_unitSymbolType_Wh);
        pv(&a->EVMaxVoltage, 400, iso2_unitSymbolType_V);
        pv(&a->EVMaxCurrent, in->max_current_a, iso2_unitSymbolType_A);
        pv(&a->EVMinCurrent, 6, iso2_unitSymbolType_A);
        break;
    }
    case EV_POWER_DELIVERY:
        b->PowerDeliveryReq_isUsed = 1;
        b->PowerDeliveryReq.ChargeProgress = (iso2_chargeProgressType)in->charge_progress;
        b->PowerDeliveryReq.SAScheduleTupleID = in->sa_tuple_id;
        break;
    case EV_CHARGING_STATUS:
        b->ChargingStatusReq_isUsed = 1;
        break;
    case EV_SESSION_STOP:
        b->SessionStopReq_isUsed = 1;
        b->SessionStopReq.ChargingSession =
            in->pause ? iso2_chargingSessionType_Pause : iso2_chargingSessionType_Terminate;
        break;
    default:
        return EV_ERR_UNSUPPORTED;
    }
    exi_bitstream_t s;
    exi_bitstream_init(&s, out, cap, 0, NULL);
    if (encode_iso2_exiDocument(&s, &doc) != 0) return EV_ERR_ENCODE;
    *out_len = exi_bitstream_get_length(&s);
    return EV_OK;
}

int ev_decode_res(const uint8_t* data, size_t len, ev_res* out) {
    if (!data || !out || len == 0 || len > EV_MAX_EXI) return EV_ERR_ARG;
    uint8_t buf[EV_MAX_EXI];
    memcpy(buf, data, len);
    static _Thread_local struct iso2_exiDocument doc;
    memset(&doc, 0, sizeof(doc));
    exi_bitstream_t s;
    exi_bitstream_init(&s, buf, len, 0, NULL);
    if (decode_iso2_exiDocument(&s, &doc) != 0) return EV_ERR_DECODE;

    memset(out, 0, sizeof(*out));
    const struct iso2_V2G_Message* m = &doc.V2G_Message;
    out->session_id_len = m->Header.SessionID.bytesLen > 8 ? 8 : m->Header.SessionID.bytesLen;
    memcpy(out->session_id, m->Header.SessionID.bytes, out->session_id_len);
    const struct iso2_BodyType* b = &m->Body;
    if (b->SessionSetupRes_isUsed) {
        out->kind = EV_SESSION_SETUP;
        out->response_code = b->SessionSetupRes.ResponseCode;
    } else if (b->ServiceDiscoveryRes_isUsed) {
        out->kind = EV_SERVICE_DISCOVERY;
        out->response_code = b->ServiceDiscoveryRes.ResponseCode;
    } else if (b->PaymentServiceSelectionRes_isUsed) {
        out->kind = EV_PAYMENT_SERVICE_SELECTION;
        out->response_code = b->PaymentServiceSelectionRes.ResponseCode;
    } else if (b->AuthorizationRes_isUsed) {
        out->kind = EV_AUTHORIZATION;
        out->response_code = b->AuthorizationRes.ResponseCode;
        out->evse_processing = b->AuthorizationRes.EVSEProcessing;
    } else if (b->ChargeParameterDiscoveryRes_isUsed) {
        const struct iso2_ChargeParameterDiscoveryResType* r = &b->ChargeParameterDiscoveryRes;
        out->kind = EV_CHARGE_PARAMETER_DISCOVERY;
        out->response_code = r->ResponseCode;
        out->evse_processing = r->EVSEProcessing;
        if (r->SAScheduleList_isUsed && r->SAScheduleList.SAScheduleTuple.arrayLen > 0)
            out->sa_tuple_id = r->SAScheduleList.SAScheduleTuple.array[0].SAScheduleTupleID;
        if (r->AC_EVSEChargeParameter_isUsed) {
            out->evse_notification = r->AC_EVSEChargeParameter.AC_EVSEStatus.EVSENotification;
            out->has_max_current = 1;
            out->max_current_da = pv_da(&r->AC_EVSEChargeParameter.EVSEMaxCurrent);
        }
    } else if (b->PowerDeliveryRes_isUsed) {
        out->kind = EV_POWER_DELIVERY;
        out->response_code = b->PowerDeliveryRes.ResponseCode;
        if (b->PowerDeliveryRes.AC_EVSEStatus_isUsed)
            out->evse_notification = b->PowerDeliveryRes.AC_EVSEStatus.EVSENotification;
    } else if (b->ChargingStatusRes_isUsed) {
        const struct iso2_ChargingStatusResType* r = &b->ChargingStatusRes;
        out->kind = EV_CHARGING_STATUS;
        out->response_code = r->ResponseCode;
        out->sa_tuple_id = r->SAScheduleTupleID;
        out->evse_notification = r->AC_EVSEStatus.EVSENotification;
        if (r->EVSEMaxCurrent_isUsed) {
            out->has_max_current = 1;
            out->max_current_da = pv_da(&r->EVSEMaxCurrent);
        }
    } else if (b->SessionStopRes_isUsed) {
        out->kind = EV_SESSION_STOP;
        out->response_code = b->SessionStopRes.ResponseCode;
    } else {
        out->kind = EV_UNKNOWN;
    }
    return EV_OK;
}
