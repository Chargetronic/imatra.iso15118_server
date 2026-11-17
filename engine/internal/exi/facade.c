// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

#include "facade.h"

#include <string.h>

#include <cbv2g/app_handshake/appHand_Datatypes.h>
#include <cbv2g/app_handshake/appHand_Decoder.h>
#include <cbv2g/app_handshake/appHand_Encoder.h>
#include <cbv2g/common/exi_bitstream.h>
#include <cbv2g/iso_2/iso2_msgDefDatatypes.h>
#include <cbv2g/iso_2/iso2_msgDefDecoder.h>
#include <cbv2g/iso_2/iso2_msgDefEncoder.h>
#include <cbv2g/din/din_msgDefDatatypes.h>
#include <cbv2g/din/din_msgDefDecoder.h>
#include <cbv2g/din/din_msgDefEncoder.h>

// The decoder takes a non-const buffer; it does not write to it, but copy
// anyway so callers can pass Go memory without surprises.
#define SP_MAX_EXI 8192

static sp_pv pv_from(const struct iso2_PhysicalValueType* v) {
    sp_pv o;
    o.value = v->Value;
    o.multiplier = v->Multiplier;
    o.unit = (int8_t)v->Unit;
    return o;
}

static void pv_to(struct iso2_PhysicalValueType* v, const sp_pv* o) {
    v->Value = o->value;
    v->Multiplier = o->multiplier;
    v->Unit = (iso2_unitSymbolType)o->unit;
}

// ---------------------------------------------------------------- handshake

int sp_decode_apphand(const uint8_t* data, size_t len, sp_apphand_req* out) {
    if (!data || !out || len == 0 || len > SP_MAX_EXI) return SP_ERR_ARG;
    uint8_t buf[SP_MAX_EXI];
    memcpy(buf, data, len);

    struct appHand_exiDocument doc;
    memset(&doc, 0, sizeof(doc));
    exi_bitstream_t s;
    exi_bitstream_init(&s, buf, len, 0, NULL);
    if (decode_appHand_exiDocument(&s, &doc) != 0) return SP_ERR_DECODE;
    if (!doc.supportedAppProtocolReq_isUsed) return SP_ERR_UNSUPPORTED;

    memset(out, 0, sizeof(*out));
    const struct appHand_supportedAppProtocolReq* r = &doc.supportedAppProtocolReq;
    int n = r->AppProtocol.arrayLen;
    if (n > 5) n = 5;
    out->n = n;
    for (int i = 0; i < n; i++) {
        const struct appHand_AppProtocolType* p = &r->AppProtocol.array[i];
        size_t l = p->ProtocolNamespace.charactersLen;
        if (l > sizeof(out->p[i].ns) - 1) l = sizeof(out->p[i].ns) - 1;
        memcpy(out->p[i].ns, p->ProtocolNamespace.characters, l);
        out->p[i].ns[l] = 0;
        out->p[i].major = p->VersionNumberMajor;
        out->p[i].minor = p->VersionNumberMinor;
        out->p[i].schema_id = p->SchemaID;
        out->p[i].priority = p->Priority;
    }
    return SP_OK;
}

int sp_encode_apphand_res(int response_code, int schema_id, uint8_t* out, size_t cap, size_t* out_len) {
    if (!out || !out_len) return SP_ERR_ARG;
    struct appHand_exiDocument doc;
    memset(&doc, 0, sizeof(doc));
    doc.supportedAppProtocolRes_isUsed = 1;
    doc.supportedAppProtocolRes.ResponseCode = (appHand_responseCodeType)response_code;
    if (schema_id >= 0) {
        doc.supportedAppProtocolRes.SchemaID = (uint8_t)schema_id;
        doc.supportedAppProtocolRes.SchemaID_isUsed = 1;
    }
    exi_bitstream_t s;
    exi_bitstream_init(&s, out, cap, 0, NULL);
    if (encode_appHand_exiDocument(&s, &doc) != 0) return SP_ERR_ENCODE;
    *out_len = exi_bitstream_get_length(&s);
    return SP_OK;
}

// ----------------------------------------------------------- ISO 15118-2

int sp_decode_iso2(const uint8_t* data, size_t len, sp_iso2_req* out) {
    if (!data || !out || len == 0 || len > SP_MAX_EXI) return SP_ERR_ARG;
    uint8_t buf[SP_MAX_EXI];
    memcpy(buf, data, len);

    // ~40 kB: keep it off the (small) cgo goroutine stack.
    static _Thread_local struct iso2_exiDocument doc;
    memset(&doc, 0, sizeof(doc));
    exi_bitstream_t s;
    exi_bitstream_init(&s, buf, len, 0, NULL);
    if (decode_iso2_exiDocument(&s, &doc) != 0) return SP_ERR_DECODE;

    memset(out, 0, sizeof(*out));
    const struct iso2_V2G_Message* m = &doc.V2G_Message;
    out->session_id_len = m->Header.SessionID.bytesLen > 8 ? 8 : m->Header.SessionID.bytesLen;
    memcpy(out->session_id, m->Header.SessionID.bytes, out->session_id_len);

    const struct iso2_BodyType* b = &m->Body;
    if (b->SessionSetupReq_isUsed) {
        out->kind = SP_ISO2_SESSION_SETUP;
        out->evcc_id_len = b->SessionSetupReq.EVCCID.bytesLen > 6 ? 6 : b->SessionSetupReq.EVCCID.bytesLen;
        memcpy(out->evcc_id, b->SessionSetupReq.EVCCID.bytes, out->evcc_id_len);
    } else if (b->ServiceDiscoveryReq_isUsed) {
        out->kind = SP_ISO2_SERVICE_DISCOVERY;
    } else if (b->PaymentServiceSelectionReq_isUsed) {
        const struct iso2_PaymentServiceSelectionReqType* r = &b->PaymentServiceSelectionReq;
        out->kind = SP_ISO2_PAYMENT_SERVICE_SELECTION;
        out->selected_payment_option = r->SelectedPaymentOption;
        int n = r->SelectedServiceList.SelectedService.arrayLen;
        if (n > 16) n = 16;
        out->n_selected_services = n;
        for (int i = 0; i < n; i++)
            out->selected_service_ids[i] = r->SelectedServiceList.SelectedService.array[i].ServiceID;
    } else if (b->AuthorizationReq_isUsed) {
        out->kind = SP_ISO2_AUTHORIZATION;
    } else if (b->ChargeParameterDiscoveryReq_isUsed) {
        const struct iso2_ChargeParameterDiscoveryReqType* r = &b->ChargeParameterDiscoveryReq;
        out->kind = SP_ISO2_CHARGE_PARAMETER_DISCOVERY;
        out->requested_energy_transfer_mode = r->RequestedEnergyTransferMode;
        if (r->AC_EVChargeParameter_isUsed) {
            const struct iso2_AC_EVChargeParameterType* a = &r->AC_EVChargeParameter;
            out->has_ac = 1;
            out->has_departure_time = a->DepartureTime_isUsed;
            out->departure_time = a->DepartureTime;
            out->e_amount = pv_from(&a->EAmount);
            out->ev_max_voltage = pv_from(&a->EVMaxVoltage);
            out->ev_max_current = pv_from(&a->EVMaxCurrent);
            out->ev_min_current = pv_from(&a->EVMinCurrent);
        }
    } else if (b->PowerDeliveryReq_isUsed) {
        const struct iso2_PowerDeliveryReqType* r = &b->PowerDeliveryReq;
        out->kind = SP_ISO2_POWER_DELIVERY;
        out->charge_progress = r->ChargeProgress;
        out->sa_schedule_tuple_id = r->SAScheduleTupleID;
        if (r->ChargingProfile_isUsed) {
            int n = r->ChargingProfile.ProfileEntry.arrayLen;
            if (n > 24) n = 24;
            out->n_profile = n;
            for (int i = 0; i < n; i++) {
                const struct iso2_ProfileEntryType* e = &r->ChargingProfile.ProfileEntry.array[i];
                out->profile[i].start = e->ChargingProfileEntryStart;
                out->profile[i].max_power = pv_from(&e->ChargingProfileEntryMaxPower);
                out->profile[i].has_phases = e->ChargingProfileEntryMaxNumberOfPhasesInUse_isUsed;
                out->profile[i].phases = e->ChargingProfileEntryMaxNumberOfPhasesInUse;
            }
        }
    } else if (b->ChargingStatusReq_isUsed) {
        out->kind = SP_ISO2_CHARGING_STATUS;
    } else if (b->SessionStopReq_isUsed) {
        out->kind = SP_ISO2_SESSION_STOP;
        out->charging_session = b->SessionStopReq.ChargingSession;
    } else {
        out->kind = SP_ISO2_UNKNOWN;
    }
    return SP_OK;
}

static void ac_status_to(struct iso2_AC_EVSEStatusType* st, const sp_iso2_res* in) {
    st->NotificationMaxDelay = in->notification_max_delay;
    st->EVSENotification = (iso2_EVSENotificationType)in->evse_notification;
    st->RCD = in->rcd;
}

static void copy_evse_id(char* dst, uint16_t* dst_len, const char* src) {
    size_t l = strnlen(src, 37);
    memcpy(dst, src, l);
    *dst_len = (uint16_t)l;
}

int sp_encode_iso2_res(const sp_iso2_res* in, uint8_t* out, size_t cap, size_t* out_len) {
    if (!in || !out || !out_len) return SP_ERR_ARG;

    static _Thread_local struct iso2_exiDocument doc;
    memset(&doc, 0, sizeof(doc));
    struct iso2_V2G_Message* m = &doc.V2G_Message;
    int sl = in->session_id_len > 8 ? 8 : in->session_id_len;
    memcpy(m->Header.SessionID.bytes, in->session_id, sl);
    m->Header.SessionID.bytesLen = (uint16_t)sl;

    struct iso2_BodyType* b = &m->Body;
    iso2_responseCodeType rc = (iso2_responseCodeType)in->response_code;

    switch (in->kind) {
    case SP_ISO2_SESSION_SETUP: {
        b->SessionSetupRes_isUsed = 1;
        struct iso2_SessionSetupResType* r = &b->SessionSetupRes;
        r->ResponseCode = rc;
        copy_evse_id(r->EVSEID.characters, &r->EVSEID.charactersLen, in->evse_id);
        if (in->has_timestamp) {
            r->EVSETimeStamp = in->timestamp;
            r->EVSETimeStamp_isUsed = 1;
        }
        break;
    }
    case SP_ISO2_SERVICE_DISCOVERY: {
        b->ServiceDiscoveryRes_isUsed = 1;
        struct iso2_ServiceDiscoveryResType* r = &b->ServiceDiscoveryRes;
        r->ResponseCode = rc;
        int np = in->n_payment_options > 2 ? 2 : in->n_payment_options;
        r->PaymentOptionList.PaymentOption.arrayLen = (uint16_t)np;
        for (int i = 0; i < np; i++)
            r->PaymentOptionList.PaymentOption.array[i] = (iso2_paymentOptionType)in->payment_options[i];
        r->ChargeService.ServiceID = in->service_id;
        r->ChargeService.ServiceCategory = iso2_serviceCategoryType_EVCharging;
        r->ChargeService.FreeService = in->free_service;
        int nm = in->n_energy_modes > 6 ? 6 : in->n_energy_modes;
        r->ChargeService.SupportedEnergyTransferMode.EnergyTransferMode.arrayLen = (uint16_t)nm;
        for (int i = 0; i < nm; i++)
            r->ChargeService.SupportedEnergyTransferMode.EnergyTransferMode.array[i] =
                (iso2_EnergyTransferModeType)in->energy_modes[i];
        break;
    }
    case SP_ISO2_PAYMENT_SERVICE_SELECTION:
        b->PaymentServiceSelectionRes_isUsed = 1;
        b->PaymentServiceSelectionRes.ResponseCode = rc;
        break;
    case SP_ISO2_AUTHORIZATION:
        b->AuthorizationRes_isUsed = 1;
        b->AuthorizationRes.ResponseCode = rc;
        b->AuthorizationRes.EVSEProcessing = (iso2_EVSEProcessingType)in->evse_processing;
        break;
    case SP_ISO2_CHARGE_PARAMETER_DISCOVERY: {
        b->ChargeParameterDiscoveryRes_isUsed = 1;
        struct iso2_ChargeParameterDiscoveryResType* r = &b->ChargeParameterDiscoveryRes;
        r->ResponseCode = rc;
        r->EVSEProcessing = (iso2_EVSEProcessingType)in->evse_processing;
        if (in->n_tuples > 0) {
            r->SAScheduleList_isUsed = 1;
            int nt = in->n_tuples > 3 ? 3 : in->n_tuples;
            r->SAScheduleList.SAScheduleTuple.arrayLen = (uint16_t)nt;
            for (int t = 0; t < nt; t++) {
                struct iso2_SAScheduleTupleType* tu = &r->SAScheduleList.SAScheduleTuple.array[t];
                tu->SAScheduleTupleID = in->tuples[t].id;
                int ne = in->tuples[t].n_entries > 12 ? 12 : in->tuples[t].n_entries;
                tu->PMaxSchedule.PMaxScheduleEntry.arrayLen = (uint16_t)ne;
                for (int e = 0; e < ne; e++) {
                    struct iso2_PMaxScheduleEntryType* en = &tu->PMaxSchedule.PMaxScheduleEntry.array[e];
                    en->RelativeTimeInterval_isUsed = 1;
                    en->RelativeTimeInterval.start = in->tuples[t].entries[e].start;
                    if (in->tuples[t].entries[e].has_duration) {
                        en->RelativeTimeInterval.duration = in->tuples[t].entries[e].duration;
                        en->RelativeTimeInterval.duration_isUsed = 1;
                    }
                    pv_to(&en->PMax, &in->tuples[t].entries[e].pmax);
                }
            }
        }
        r->AC_EVSEChargeParameter_isUsed = 1;
        ac_status_to(&r->AC_EVSEChargeParameter.AC_EVSEStatus, in);
        pv_to(&r->AC_EVSEChargeParameter.EVSENominalVoltage, &in->evse_nominal_voltage);
        pv_to(&r->AC_EVSEChargeParameter.EVSEMaxCurrent, &in->evse_max_current);
        break;
    }
    case SP_ISO2_POWER_DELIVERY:
        b->PowerDeliveryRes_isUsed = 1;
        b->PowerDeliveryRes.ResponseCode = rc;
        b->PowerDeliveryRes.AC_EVSEStatus_isUsed = 1;
        ac_status_to(&b->PowerDeliveryRes.AC_EVSEStatus, in);
        break;
    case SP_ISO2_CHARGING_STATUS: {
        b->ChargingStatusRes_isUsed = 1;
        struct iso2_ChargingStatusResType* r = &b->ChargingStatusRes;
        r->ResponseCode = rc;
        copy_evse_id(r->EVSEID.characters, &r->EVSEID.charactersLen, in->evse_id);
        r->SAScheduleTupleID = in->sa_schedule_tuple_id;
        if (in->has_evse_max_current) {
            pv_to(&r->EVSEMaxCurrent, &in->evse_max_current);
            r->EVSEMaxCurrent_isUsed = 1;
        }
        if (in->has_receipt_required) {
            r->ReceiptRequired = in->receipt_required;
            r->ReceiptRequired_isUsed = 1;
        }
        ac_status_to(&r->AC_EVSEStatus, in);
        break;
    }
    case SP_ISO2_SESSION_STOP:
        b->SessionStopRes_isUsed = 1;
        b->SessionStopRes.ResponseCode = rc;
        break;
    default:
        return SP_ERR_UNSUPPORTED;
    }

    exi_bitstream_t s;
    exi_bitstream_init(&s, out, cap, 0, NULL);
    if (encode_iso2_exiDocument(&s, &doc) != 0) return SP_ERR_ENCODE;
    *out_len = exi_bitstream_get_length(&s);
    return SP_OK;
}

// ----------------------------------------------------------- DIN 70121

static sp_pv din_pv_from(const struct din_PhysicalValueType* v) {
    sp_pv o;
    o.value = v->Value;
    o.multiplier = v->Multiplier;
    o.unit = v->Unit_isUsed ? (int8_t)v->Unit : -1;
    return o;
}

static void din_pv_to(struct din_PhysicalValueType* v, const sp_pv* o) {
    v->Value = o->value;
    v->Multiplier = o->multiplier;
    if (o->unit >= 0) {
        v->Unit = (din_unitSymbolType)o->unit;
        v->Unit_isUsed = 1;
    }
}

int sp_decode_din(const uint8_t* data, size_t len, sp_din_req* out) {
    if (!data || !out || len == 0 || len > SP_MAX_EXI) return SP_ERR_ARG;
    uint8_t buf[SP_MAX_EXI];
    memcpy(buf, data, len);

    static _Thread_local struct din_exiDocument doc;
    memset(&doc, 0, sizeof(doc));
    exi_bitstream_t s;
    exi_bitstream_init(&s, buf, len, 0, NULL);
    if (decode_din_exiDocument(&s, &doc) != 0) return SP_ERR_DECODE;

    memset(out, 0, sizeof(*out));
    const struct din_V2G_Message* m = &doc.V2G_Message;
    out->session_id_len = m->Header.SessionID.bytesLen > 8 ? 8 : m->Header.SessionID.bytesLen;
    memcpy(out->session_id, m->Header.SessionID.bytes, out->session_id_len);

    const struct din_BodyType* b = &m->Body;
    if (b->SessionSetupReq_isUsed) {
        out->kind = SP_DIN_SESSION_SETUP;
        out->evcc_id_len = b->SessionSetupReq.EVCCID.bytesLen > 8 ? 8 : b->SessionSetupReq.EVCCID.bytesLen;
        memcpy(out->evcc_id, b->SessionSetupReq.EVCCID.bytes, out->evcc_id_len);
    } else if (b->ServiceDiscoveryReq_isUsed) {
        out->kind = SP_DIN_SERVICE_DISCOVERY;
    } else if (b->ServicePaymentSelectionReq_isUsed) {
        const struct din_ServicePaymentSelectionReqType* r = &b->ServicePaymentSelectionReq;
        out->kind = SP_DIN_SERVICE_PAYMENT_SELECTION;
        out->selected_payment_option = r->SelectedPaymentOption;
        int n = r->SelectedServiceList.SelectedService.arrayLen;
        if (n > 16) n = 16;
        out->n_selected_services = n;
        for (int i = 0; i < n; i++)
            out->selected_service_ids[i] = r->SelectedServiceList.SelectedService.array[i].ServiceID;
    } else if (b->ContractAuthenticationReq_isUsed) {
        out->kind = SP_DIN_CONTRACT_AUTHENTICATION;
    } else if (b->ChargeParameterDiscoveryReq_isUsed) {
        const struct din_ChargeParameterDiscoveryReqType* r = &b->ChargeParameterDiscoveryReq;
        out->kind = SP_DIN_CHARGE_PARAMETER_DISCOVERY;
        out->requested_energy_transfer = r->EVRequestedEnergyTransferType;
        if (r->DC_EVChargeParameter_isUsed) {
            const struct din_DC_EVChargeParameterType* d = &r->DC_EVChargeParameter;
            out->has_dc = 1;
            out->dc_ev_ready = d->DC_EVStatus.EVReady;
            out->dc_ev_error_code = d->DC_EVStatus.EVErrorCode;
            out->dc_soc = d->DC_EVStatus.EVRESSSOC;
            out->dc_max_current = din_pv_from(&d->EVMaximumCurrentLimit);
            out->dc_max_voltage = din_pv_from(&d->EVMaximumVoltageLimit);
            if ((out->has_dc_max_power = d->EVMaximumPowerLimit_isUsed)) out->dc_max_power = din_pv_from(&d->EVMaximumPowerLimit);
            if ((out->has_dc_energy_capacity = d->EVEnergyCapacity_isUsed)) out->dc_energy_capacity = din_pv_from(&d->EVEnergyCapacity);
            if ((out->has_dc_energy_request = d->EVEnergyRequest_isUsed)) out->dc_energy_request = din_pv_from(&d->EVEnergyRequest);
            if ((out->has_dc_full_soc = d->FullSOC_isUsed)) out->dc_full_soc = d->FullSOC;
            if ((out->has_dc_bulk_soc = d->BulkSOC_isUsed)) out->dc_bulk_soc = d->BulkSOC;
        }
    } else if (b->SessionStopReq_isUsed) {
        out->kind = SP_DIN_SESSION_STOP;
    } else {
        out->kind = SP_DIN_OTHER;
    }
    return SP_OK;
}

static void din_evse_status(struct din_DC_EVSEStatusType* st, const sp_din_res* in) {
    st->EVSEStatusCode = (din_DC_EVSEStatusCodeType)in->evse_status_code;
    st->EVSENotification = (din_EVSENotificationType)in->evse_notification;
    st->NotificationMaxDelay = 0;
    if (in->isolation >= 0) {
        st->EVSEIsolationStatus = (din_isolationLevelType)in->isolation;
        st->EVSEIsolationStatus_isUsed = 1;
    }
}

int sp_encode_din_res(const sp_din_res* in, uint8_t* out, size_t cap, size_t* out_len) {
    if (!in || !out || !out_len) return SP_ERR_ARG;
    static _Thread_local struct din_exiDocument doc;
    memset(&doc, 0, sizeof(doc));
    struct din_V2G_Message* m = &doc.V2G_Message;
    int n = in->session_id_len > 8 ? 8 : in->session_id_len;
    memcpy(m->Header.SessionID.bytes, in->session_id, n);
    m->Header.SessionID.bytesLen = n;
    struct din_BodyType* b = &m->Body;
    din_responseCodeType rc = (din_responseCodeType)in->response_code;

    switch (in->kind) {
    case SP_DIN_SESSION_SETUP:
        b->SessionSetupRes_isUsed = 1;
        b->SessionSetupRes.ResponseCode = rc;
        b->SessionSetupRes.EVSEID.bytes[0] = 0x00;  // DIN 70121: EVSEID 0x00 when not used
        b->SessionSetupRes.EVSEID.bytesLen = 1;
        if (in->has_timestamp) {
            b->SessionSetupRes.DateTimeNow = in->timestamp;
            b->SessionSetupRes.DateTimeNow_isUsed = 1;
        }
        break;
    case SP_DIN_SERVICE_DISCOVERY: {
        b->ServiceDiscoveryRes_isUsed = 1;
        struct din_ServiceDiscoveryResType* r = &b->ServiceDiscoveryRes;
        r->ResponseCode = rc;
        r->PaymentOptions.PaymentOption.array[0] = din_paymentOptionType_ExternalPayment;
        r->PaymentOptions.PaymentOption.arrayLen = 1;
        r->ChargeService.ServiceTag.ServiceID = 1;
        r->ChargeService.ServiceTag.ServiceCategory = din_serviceCategoryType_EVCharging;
        r->ChargeService.FreeService = 0;
        r->ChargeService.EnergyTransferType = din_EVSESupportedEnergyTransferType_DC_extended;
        break;
    }
    case SP_DIN_SERVICE_PAYMENT_SELECTION:
        b->ServicePaymentSelectionRes_isUsed = 1;
        b->ServicePaymentSelectionRes.ResponseCode = rc;
        break;
    case SP_DIN_CONTRACT_AUTHENTICATION:
        b->ContractAuthenticationRes_isUsed = 1;
        b->ContractAuthenticationRes.ResponseCode = rc;
        b->ContractAuthenticationRes.EVSEProcessing = (din_EVSEProcessingType)in->evse_processing;
        break;
    case SP_DIN_CHARGE_PARAMETER_DISCOVERY: {
        b->ChargeParameterDiscoveryRes_isUsed = 1;
        struct din_ChargeParameterDiscoveryResType* r = &b->ChargeParameterDiscoveryRes;
        r->ResponseCode = rc;
        r->EVSEProcessing = (din_EVSEProcessingType)in->evse_processing;
        r->SAScheduleList_isUsed = 1;
        struct din_SAScheduleTupleType* t = &r->SAScheduleList.SAScheduleTuple.array[0];
        r->SAScheduleList.SAScheduleTuple.arrayLen = 1;
        t->SAScheduleTupleID = 1;
        t->PMaxSchedule.PMaxScheduleID = 1;
        t->PMaxSchedule.PMaxScheduleEntry.arrayLen = 1;
        t->PMaxSchedule.PMaxScheduleEntry.array[0].RelativeTimeInterval_isUsed = 1;
        t->PMaxSchedule.PMaxScheduleEntry.array[0].RelativeTimeInterval.start = 0;
        t->PMaxSchedule.PMaxScheduleEntry.array[0].RelativeTimeInterval.duration = 86400;
        t->PMaxSchedule.PMaxScheduleEntry.array[0].RelativeTimeInterval.duration_isUsed = 1;
        t->PMaxSchedule.PMaxScheduleEntry.array[0].PMax = in->pmax_w_div;
        r->DC_EVSEChargeParameter_isUsed = 1;
        struct din_DC_EVSEChargeParameterType* d = &r->DC_EVSEChargeParameter;
        din_evse_status(&d->DC_EVSEStatus, in);
        din_pv_to(&d->EVSEMaximumCurrentLimit, &in->max_current);
        din_pv_to(&d->EVSEMaximumVoltageLimit, &in->max_voltage);
        din_pv_to(&d->EVSEMinimumCurrentLimit, &in->min_current);
        din_pv_to(&d->EVSEMinimumVoltageLimit, &in->min_voltage);
        din_pv_to(&d->EVSEPeakCurrentRipple, &in->peak_ripple);
        // Optional in the schema, expected by EVs (and Josev) in practice.
        din_pv_to(&d->EVSEMaximumPowerLimit, &in->max_power);
        d->EVSEMaximumPowerLimit_isUsed = 1;
        break;
    }
    case SP_DIN_SESSION_STOP:
        b->SessionStopRes_isUsed = 1;
        b->SessionStopRes.ResponseCode = rc;
        break;
    default:
        return SP_ERR_UNSUPPORTED;
    }
    exi_bitstream_t s;
    exi_bitstream_init(&s, out, cap, 0, NULL);
    if (encode_din_exiDocument(&s, &doc) != 0) return SP_ERR_ENCODE;
    *out_len = exi_bitstream_get_length(&s);
    return SP_OK;
}
