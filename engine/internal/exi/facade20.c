// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

#include "facade20.h"
#include "facade.h"

#include <string.h>

#include <cbv2g/common/exi_bitstream.h>
#include <cbv2g/iso_20/iso20_AC_Datatypes.h>
#include <cbv2g/iso_20/iso20_AC_Decoder.h>
#include <cbv2g/iso_20/iso20_AC_Encoder.h>
#include <cbv2g/iso_20/iso20_CommonMessages_Datatypes.h>
#include <cbv2g/iso_20/iso20_CommonMessages_Decoder.h>
#include <cbv2g/iso_20/iso20_CommonMessages_Encoder.h>

#define SP20_MAX_EXI 8192

// The CommonMessages document is ~320 kB (ScheduleExchangeRes can carry 3 x
// 1024 schedule entries): thread-local scratch, off the cgo stack.
static _Thread_local struct iso20_exiDocument cdoc;
static _Thread_local struct iso20_ac_exiDocument adoc;

// ------------------------------------------------------------------ helpers

static sp_rat rat_c(const struct iso20_RationalNumberType* r) {
    sp_rat o = {r->Value, r->Exponent};
    return o;
}
static void rat_c_to(struct iso20_RationalNumberType* r, sp_rat v) {
    r->Value = v.value;
    r->Exponent = v.exponent;
}
static sp_rat rat_a(const struct iso20_ac_RationalNumberType* r) {
    sp_rat o = {r->Value, r->Exponent};
    return o;
}
static void rat_a_to(struct iso20_ac_RationalNumberType* r, sp_rat v) {
    r->Value = v.value;
    r->Exponent = v.exponent;
}

#define HDR_GET(h, o)                                                              \
    do {                                                                           \
        (o)->session_id_len = (h)->SessionID.bytesLen > 8 ? 8 : (h)->SessionID.bytesLen; \
        memcpy((o)->session_id, (h)->SessionID.bytes, (o)->session_id_len);        \
        (o)->timestamp = (h)->TimeStamp;                                           \
    } while (0)

#define HDR_SET(h, i)                                                              \
    do {                                                                           \
        int n_ = (i)->session_id_len > 8 ? 8 : (i)->session_id_len;                \
        if (n_ < 0) n_ = 0;                                                        \
        memcpy((h)->SessionID.bytes, (i)->session_id, n_);                         \
        (h)->SessionID.bytesLen = (uint16_t)n_;                                    \
        (h)->TimeStamp = (i)->timestamp;                                           \
        (h)->Signature_isUsed = 0;                                                 \
    } while (0)

static void str_get(char* dst, size_t cap, const char* src, uint16_t len) {
    size_t l = len;
    if (l > cap - 1) l = cap - 1;
    memcpy(dst, src, l);
    dst[l] = 0;
}

static uint16_t str_set(char* dst, size_t cap, const char* src) {
    size_t l = strnlen(src, cap - 1);
    memcpy(dst, src, l);
    return (uint16_t)l;
}

static int decode_doc(int pt, const uint8_t* data, size_t len, uint8_t* buf) {
    if (!data || len == 0 || len > SP20_MAX_EXI) return SP_ERR_ARG;
    memcpy(buf, data, len);
    exi_bitstream_t s;
    exi_bitstream_init(&s, buf, len, 0, NULL);
    if (pt == SP20_PT_MAIN) {
        memset(&cdoc, 0, sizeof(cdoc));
        return decode_iso20_exiDocument(&s, &cdoc) == 0 ? SP_OK : SP_ERR_DECODE;
    }
    if (pt == SP20_PT_AC) {
        memset(&adoc, 0, sizeof(adoc));
        return decode_iso20_ac_exiDocument(&s, &adoc) == 0 ? SP_OK : SP_ERR_DECODE;
    }
    return SP_ERR_UNSUPPORTED;
}

static int encode_doc(int pt, uint8_t* out, size_t cap, size_t* out_len) {
    exi_bitstream_t s;
    exi_bitstream_init(&s, out, cap, 0, NULL);
    int rc = pt == SP20_PT_MAIN ? encode_iso20_exiDocument(&s, &cdoc) : encode_iso20_ac_exiDocument(&s, &adoc);
    if (rc != 0) return SP_ERR_ENCODE;
    *out_len = exi_bitstream_get_length(&s);
    return SP_OK;
}

// ------------------------------------------------------- server: requests

int sp20_decode_req(int pt, const uint8_t* data, size_t len, sp20_req* o) {
    if (!o) return SP_ERR_ARG;
    uint8_t buf[SP20_MAX_EXI];
    int rc = decode_doc(pt, data, len, buf);
    if (rc != SP_OK) return rc;
    memset(o, 0, sizeof(*o));

    if (pt == SP20_PT_MAIN) {
        struct iso20_exiDocument* d = &cdoc;
        if (d->SessionSetupReq_isUsed) {
            o->kind = SP20_SESSION_SETUP;
            HDR_GET(&d->SessionSetupReq.Header, o);
            str_get(o->evcc_id, sizeof(o->evcc_id), d->SessionSetupReq.EVCCID.characters,
                    d->SessionSetupReq.EVCCID.charactersLen);
        } else if (d->AuthorizationSetupReq_isUsed) {
            o->kind = SP20_AUTHORIZATION_SETUP;
            HDR_GET(&d->AuthorizationSetupReq.Header, o);
        } else if (d->AuthorizationReq_isUsed) {
            o->kind = SP20_AUTHORIZATION;
            HDR_GET(&d->AuthorizationReq.Header, o);
            o->selected_authorization = d->AuthorizationReq.SelectedAuthorizationService;
        } else if (d->ServiceDiscoveryReq_isUsed) {
            o->kind = SP20_SERVICE_DISCOVERY;
            HDR_GET(&d->ServiceDiscoveryReq.Header, o);
        } else if (d->ServiceDetailReq_isUsed) {
            o->kind = SP20_SERVICE_DETAIL;
            HDR_GET(&d->ServiceDetailReq.Header, o);
            o->service_id = d->ServiceDetailReq.ServiceID;
        } else if (d->ServiceSelectionReq_isUsed) {
            const struct iso20_ServiceSelectionReqType* r = &d->ServiceSelectionReq;
            o->kind = SP20_SERVICE_SELECTION;
            HDR_GET(&r->Header, o);
            o->selected_service_id = r->SelectedEnergyTransferService.ServiceID;
            o->selected_parameter_set_id = r->SelectedEnergyTransferService.ParameterSetID;
            o->n_selected_vas = r->SelectedVASList_isUsed ? r->SelectedVASList.SelectedService.arrayLen : 0;
        } else if (d->ScheduleExchangeReq_isUsed) {
            const struct iso20_ScheduleExchangeReqType* r = &d->ScheduleExchangeReq;
            o->kind = SP20_SCHEDULE_EXCHANGE;
            HDR_GET(&r->Header, o);
            o->max_supporting_points = r->MaximumSupportingPoints;
            if (r->Dynamic_SEReqControlMode_isUsed) {
                const struct iso20_Dynamic_SEReqControlModeType* m = &r->Dynamic_SEReqControlMode;
                o->se_mode = SP20_MODE_DYNAMIC;
                o->has_departure_time = 1;
                o->departure_time = m->DepartureTime;
                o->has_min_soc = m->MinimumSOC_isUsed;
                o->min_soc = m->MinimumSOC;
                o->has_target_soc = m->TargetSOC_isUsed;
                o->target_soc = m->TargetSOC;
                o->has_target_energy = o->has_max_energy = o->has_min_energy = 1;
                o->target_energy = rat_c(&m->EVTargetEnergyRequest);
                o->max_energy = rat_c(&m->EVMaximumEnergyRequest);
                o->min_energy = rat_c(&m->EVMinimumEnergyRequest);
            } else if (r->Scheduled_SEReqControlMode_isUsed) {
                const struct iso20_Scheduled_SEReqControlModeType* m = &r->Scheduled_SEReqControlMode;
                o->se_mode = SP20_MODE_SCHEDULED;
                o->has_departure_time = m->DepartureTime_isUsed;
                o->departure_time = m->DepartureTime;
                o->has_target_energy = m->EVTargetEnergyRequest_isUsed;
                o->target_energy = rat_c(&m->EVTargetEnergyRequest);
                o->has_max_energy = m->EVMaximumEnergyRequest_isUsed;
                o->max_energy = rat_c(&m->EVMaximumEnergyRequest);
                o->has_min_energy = m->EVMinimumEnergyRequest_isUsed;
                o->min_energy = rat_c(&m->EVMinimumEnergyRequest);
            }
        } else if (d->PowerDeliveryReq_isUsed) {
            const struct iso20_PowerDeliveryReqType* r = &d->PowerDeliveryReq;
            o->kind = SP20_POWER_DELIVERY;
            HDR_GET(&r->Header, o);
            o->ev_processing = r->EVProcessing;
            o->charge_progress = r->ChargeProgress;
            if (r->EVPowerProfile_isUsed) {
                o->has_power_profile = 1;
                if (r->EVPowerProfile.Scheduled_EVPPTControlMode_isUsed) {
                    o->profile_mode = SP20_MODE_SCHEDULED;
                    o->selected_schedule_tuple_id =
                        r->EVPowerProfile.Scheduled_EVPPTControlMode.SelectedScheduleTupleID;
                } else if (r->EVPowerProfile.Dynamic_EVPPTControlMode_isUsed) {
                    o->profile_mode = SP20_MODE_DYNAMIC;
                }
            }
            o->bpt = r->BPT_ChannelSelection_isUsed;
        } else if (d->SessionStopReq_isUsed) {
            const struct iso20_SessionStopReqType* r = &d->SessionStopReq;
            o->kind = SP20_SESSION_STOP;
            HDR_GET(&r->Header, o);
            o->charging_session = r->ChargingSession;
            if (r->EVTerminationCode_isUsed)
                str_get(o->termination_code, sizeof(o->termination_code), r->EVTerminationCode.characters,
                        r->EVTerminationCode.charactersLen);
        } else if (d->CertificateInstallationReq_isUsed) {
            o->kind = SP20_OTHER;
            HDR_GET(&d->CertificateInstallationReq.Header, o);
        } else if (d->MeteringConfirmationReq_isUsed) {
            o->kind = SP20_OTHER;
            HDR_GET(&d->MeteringConfirmationReq.Header, o);
        } else if (d->VehicleCheckInReq_isUsed) {
            o->kind = SP20_OTHER;
            HDR_GET(&d->VehicleCheckInReq.Header, o);
        } else if (d->VehicleCheckOutReq_isUsed) {
            o->kind = SP20_OTHER;
            HDR_GET(&d->VehicleCheckOutReq.Header, o);
        } else {
            o->kind = SP20_UNKNOWN;
        }
        return SP_OK;
    }

    struct iso20_ac_exiDocument* d = &adoc;
    if (d->AC_ChargeParameterDiscoveryReq_isUsed) {
        const struct iso20_ac_AC_ChargeParameterDiscoveryReqType* r = &d->AC_ChargeParameterDiscoveryReq;
        o->kind = SP20_AC_CHARGE_PARAMETER_DISCOVERY;
        HDR_GET(&r->Header, o);
        if (r->AC_CPDReqEnergyTransferMode_isUsed) {
            const struct iso20_ac_AC_CPDReqEnergyTransferModeType* m = &r->AC_CPDReqEnergyTransferMode;
            o->has_max_power = o->has_min_power = 1;
            o->max_power = rat_a(&m->EVMaximumChargePower);
            o->min_power = rat_a(&m->EVMinimumChargePower);
            o->has_max_power_l2 = m->EVMaximumChargePower_L2_isUsed;
            o->max_power_l2 = rat_a(&m->EVMaximumChargePower_L2);
            o->has_max_power_l3 = m->EVMaximumChargePower_L3_isUsed;
            o->max_power_l3 = rat_a(&m->EVMaximumChargePower_L3);
        } else if (r->BPT_AC_CPDReqEnergyTransferMode_isUsed) {
            o->bpt = 1;
        }
    } else if (d->AC_ChargeLoopReq_isUsed) {
        const struct iso20_ac_AC_ChargeLoopReqType* r = &d->AC_ChargeLoopReq;
        o->kind = SP20_AC_CHARGE_LOOP;
        HDR_GET(&r->Header, o);
        o->meter_info_requested = r->MeterInfoRequested;
        if (r->DisplayParameters_isUsed) {
            const struct iso20_ac_DisplayParametersType* p = &r->DisplayParameters;
            o->has_display = 1;
            o->has_present_soc = p->PresentSOC_isUsed;
            o->present_soc = p->PresentSOC;
            o->has_display_target_soc = p->TargetSOC_isUsed;
            o->display_target_soc = p->TargetSOC;
            o->has_display_min_soc = p->MinimumSOC_isUsed;
            o->display_min_soc = p->MinimumSOC;
            o->has_charging_complete = p->ChargingComplete_isUsed;
            o->charging_complete = p->ChargingComplete;
            o->has_battery_capacity = p->BatteryEnergyCapacity_isUsed;
            o->battery_capacity = rat_a(&p->BatteryEnergyCapacity);
        }
        if (r->Dynamic_AC_CLReqControlMode_isUsed) {
            const struct iso20_ac_Dynamic_AC_CLReqControlModeType* m = &r->Dynamic_AC_CLReqControlMode;
            o->cl_mode = SP20_MODE_DYNAMIC;
            o->has_departure_time = m->DepartureTime_isUsed;
            o->departure_time = m->DepartureTime;
            o->has_target_energy = o->has_max_energy = o->has_min_energy = 1;
            o->target_energy = rat_a(&m->EVTargetEnergyRequest);
            o->max_energy = rat_a(&m->EVMaximumEnergyRequest);
            o->min_energy = rat_a(&m->EVMinimumEnergyRequest);
            o->has_max_power = o->has_min_power = o->has_present_power = 1;
            o->max_power = rat_a(&m->EVMaximumChargePower);
            o->min_power = rat_a(&m->EVMinimumChargePower);
            o->present_power = rat_a(&m->EVPresentActivePower);
        } else if (r->Scheduled_AC_CLReqControlMode_isUsed) {
            const struct iso20_ac_Scheduled_AC_CLReqControlModeType* m = &r->Scheduled_AC_CLReqControlMode;
            o->cl_mode = SP20_MODE_SCHEDULED;
            o->has_target_energy = m->EVTargetEnergyRequest_isUsed;
            o->target_energy = rat_a(&m->EVTargetEnergyRequest);
            o->has_max_energy = m->EVMaximumEnergyRequest_isUsed;
            o->max_energy = rat_a(&m->EVMaximumEnergyRequest);
            o->has_min_energy = m->EVMinimumEnergyRequest_isUsed;
            o->min_energy = rat_a(&m->EVMinimumEnergyRequest);
            o->has_max_power = m->EVMaximumChargePower_isUsed;
            o->max_power = rat_a(&m->EVMaximumChargePower);
            o->has_min_power = m->EVMinimumChargePower_isUsed;
            o->min_power = rat_a(&m->EVMinimumChargePower);
            o->has_present_power = 1;
            o->present_power = rat_a(&m->EVPresentActivePower);
        } else {
            o->cl_mode = SP20_MODE_BPT;
            o->bpt = 1;
        }
    } else {
        o->kind = SP20_UNKNOWN;
    }
    return SP_OK;
}

// ------------------------------------------------------ server: responses

static void set_param_int(struct iso20_ParameterType* p, const char* name, int v) {
    p->Name.charactersLen = str_set(p->Name.characters, sizeof(p->Name.characters), name);
    p->intValue = v;
    p->intValue_isUsed = 1;
}

int sp20_encode_res(const sp20_res* in, uint8_t* out, size_t cap, size_t* out_len, int* pt_out) {
    if (!in || !out || !out_len || !pt_out) return SP_ERR_ARG;
    int pt = SP20_PT_MAIN;
    if (in->kind == SP20_AC_CHARGE_PARAMETER_DISCOVERY || in->kind == SP20_AC_CHARGE_LOOP) pt = SP20_PT_AC;

    if (pt == SP20_PT_MAIN) {
        memset(&cdoc, 0, sizeof(cdoc));
        struct iso20_exiDocument* d = &cdoc;
        iso20_responseCodeType rc = (iso20_responseCodeType)in->response_code;
        switch (in->kind) {
        case SP20_SESSION_SETUP: {
            struct iso20_SessionSetupResType* r = &d->SessionSetupRes;
            d->SessionSetupRes_isUsed = 1;
            HDR_SET(&r->Header, in);
            r->ResponseCode = rc;
            r->EVSEID.charactersLen = str_set(r->EVSEID.characters, sizeof(r->EVSEID.characters), in->evse_id);
            break;
        }
        case SP20_AUTHORIZATION_SETUP: {
            struct iso20_AuthorizationSetupResType* r = &d->AuthorizationSetupRes;
            d->AuthorizationSetupRes_isUsed = 1;
            HDR_SET(&r->Header, in);
            r->ResponseCode = rc;
            r->AuthorizationServices.array[0] = iso20_authorizationType_EIM;
            r->AuthorizationServices.arrayLen = 1;
            r->CertificateInstallationService = 0;
            r->EIM_ASResAuthorizationMode_isUsed = 1;
            break;
        }
        case SP20_AUTHORIZATION: {
            struct iso20_AuthorizationResType* r = &d->AuthorizationRes;
            d->AuthorizationRes_isUsed = 1;
            HDR_SET(&r->Header, in);
            r->ResponseCode = rc;
            r->EVSEProcessing = (iso20_processingType)in->evse_processing;
            break;
        }
        case SP20_SERVICE_DISCOVERY: {
            struct iso20_ServiceDiscoveryResType* r = &d->ServiceDiscoveryRes;
            d->ServiceDiscoveryRes_isUsed = 1;
            HDR_SET(&r->Header, in);
            r->ResponseCode = rc;
            r->ServiceRenegotiationSupported = 0;
            int n = in->n_services > 4 ? 4 : in->n_services;
            for (int i = 0; i < n; i++) {
                r->EnergyTransferServiceList.Service.array[i].ServiceID = in->service_ids[i];
                r->EnergyTransferServiceList.Service.array[i].FreeService = in->free_service[i];
            }
            r->EnergyTransferServiceList.Service.arrayLen = (uint16_t)n;
            break;
        }
        case SP20_SERVICE_DETAIL: {
            struct iso20_ServiceDetailResType* r = &d->ServiceDetailRes;
            d->ServiceDetailRes_isUsed = 1;
            HDR_SET(&r->Header, in);
            r->ResponseCode = rc;
            r->ServiceID = in->service_id;
            int n = in->n_sets > 4 ? 4 : in->n_sets;
            for (int i = 0; i < n; i++) {
                const sp20_param_set* s = &in->sets[i];
                struct iso20_ParameterSetType* ps = &r->ServiceParameterList.ParameterSet.array[i];
                ps->ParameterSetID = s->id;
                set_param_int(&ps->Parameter.array[0], "Connector", s->connector);
                set_param_int(&ps->Parameter.array[1], "ControlMode", s->control_mode);
                set_param_int(&ps->Parameter.array[2], "EVSENominalVoltage", s->nominal_voltage);
                set_param_int(&ps->Parameter.array[3], "MobilityNeedsMode", s->mobility_needs);
                set_param_int(&ps->Parameter.array[4], "Pricing", s->pricing);
                ps->Parameter.arrayLen = 5;
            }
            r->ServiceParameterList.ParameterSet.arrayLen = (uint16_t)n;
            break;
        }
        case SP20_SERVICE_SELECTION:
            d->ServiceSelectionRes_isUsed = 1;
            HDR_SET(&d->ServiceSelectionRes.Header, in);
            d->ServiceSelectionRes.ResponseCode = rc;
            break;
        case SP20_SCHEDULE_EXCHANGE: {
            struct iso20_ScheduleExchangeResType* r = &d->ScheduleExchangeRes;
            d->ScheduleExchangeRes_isUsed = 1;
            HDR_SET(&r->Header, in);
            r->ResponseCode = rc;
            r->EVSEProcessing = (iso20_processingType)in->evse_processing;
            if (in->se_mode == SP20_MODE_SCHEDULED) {
                r->Scheduled_SEResControlMode_isUsed = 1;
                struct iso20_ScheduleTupleType* t = &r->Scheduled_SEResControlMode.ScheduleTuple.array[0];
                r->Scheduled_SEResControlMode.ScheduleTuple.arrayLen = 1;
                t->ScheduleTupleID = in->schedule_tuple_id;
                struct iso20_PowerScheduleType* p = &t->ChargingSchedule.PowerSchedule;
                p->TimeAnchor = in->time_anchor;
                int n = in->n_entries > 24 ? 24 : in->n_entries;
                for (int i = 0; i < n; i++) {
                    p->PowerScheduleEntries.PowerScheduleEntry.array[i].Duration = in->entries[i].duration;
                    rat_c_to(&p->PowerScheduleEntries.PowerScheduleEntry.array[i].Power, in->entries[i].power);
                }
                p->PowerScheduleEntries.PowerScheduleEntry.arrayLen = (uint16_t)n;
            } else {
                r->Dynamic_SEResControlMode_isUsed = 1;  // all of its elements are optional
            }
            break;
        }
        case SP20_POWER_DELIVERY: {
            struct iso20_PowerDeliveryResType* r = &d->PowerDeliveryRes;
            d->PowerDeliveryRes_isUsed = 1;
            HDR_SET(&r->Header, in);
            r->ResponseCode = rc;
            if (in->has_evse_status) {
                r->EVSEStatus_isUsed = 1;
                r->EVSEStatus.NotificationMaxDelay = in->notification_max_delay;
                r->EVSEStatus.EVSENotification = (iso20_evseNotificationType)in->evse_notification;
            }
            break;
        }
        case SP20_SESSION_STOP:
            d->SessionStopRes_isUsed = 1;
            HDR_SET(&d->SessionStopRes.Header, in);
            d->SessionStopRes.ResponseCode = rc;
            break;
        default:
            return SP_ERR_UNSUPPORTED;
        }
    } else {
        memset(&adoc, 0, sizeof(adoc));
        struct iso20_ac_exiDocument* d = &adoc;
        iso20_ac_responseCodeType rc = (iso20_ac_responseCodeType)in->response_code;
        if (in->kind == SP20_AC_CHARGE_PARAMETER_DISCOVERY) {
            struct iso20_ac_AC_ChargeParameterDiscoveryResType* r = &d->AC_ChargeParameterDiscoveryRes;
            d->AC_ChargeParameterDiscoveryRes_isUsed = 1;
            HDR_SET(&r->Header, in);
            r->ResponseCode = rc;
            r->AC_CPDResEnergyTransferMode_isUsed = 1;
            rat_a_to(&r->AC_CPDResEnergyTransferMode.EVSEMaximumChargePower, in->evse_max_power);
            rat_a_to(&r->AC_CPDResEnergyTransferMode.EVSEMinimumChargePower, in->evse_min_power);
            rat_a_to(&r->AC_CPDResEnergyTransferMode.EVSENominalFrequency, in->evse_nominal_frequency);
        } else {
            struct iso20_ac_AC_ChargeLoopResType* r = &d->AC_ChargeLoopRes;
            d->AC_ChargeLoopRes_isUsed = 1;
            HDR_SET(&r->Header, in);
            r->ResponseCode = rc;
            if (in->has_evse_status) {
                r->EVSEStatus_isUsed = 1;
                r->EVSEStatus.NotificationMaxDelay = in->notification_max_delay;
                r->EVSEStatus.EVSENotification = (iso20_ac_evseNotificationType)in->evse_notification;
            }
            if (in->cl_mode == SP20_MODE_DYNAMIC) {
                r->Dynamic_AC_CLResControlMode_isUsed = 1;
                rat_a_to(&r->Dynamic_AC_CLResControlMode.EVSETargetActivePower, in->target_power);
            } else {
                r->Scheduled_AC_CLResControlMode_isUsed = 1;
                if (in->has_target_power) {
                    r->Scheduled_AC_CLResControlMode.EVSETargetActivePower_isUsed = 1;
                    rat_a_to(&r->Scheduled_AC_CLResControlMode.EVSETargetActivePower, in->target_power);
                }
            }
        }
    }
    *pt_out = pt;
    return encode_doc(pt, out, cap, out_len);
}

// ------------------------------------------------- EV side: requests

int sp20_encode_req(const sp20_req* in, uint8_t* out, size_t cap, size_t* out_len, int* pt_out) {
    if (!in || !out || !out_len || !pt_out) return SP_ERR_ARG;
    int pt = SP20_PT_MAIN;
    if (in->kind == SP20_AC_CHARGE_PARAMETER_DISCOVERY || in->kind == SP20_AC_CHARGE_LOOP) pt = SP20_PT_AC;

    if (pt == SP20_PT_MAIN) {
        memset(&cdoc, 0, sizeof(cdoc));
        struct iso20_exiDocument* d = &cdoc;
        switch (in->kind) {
        case SP20_SESSION_SETUP:
            d->SessionSetupReq_isUsed = 1;
            HDR_SET(&d->SessionSetupReq.Header, in);
            d->SessionSetupReq.EVCCID.charactersLen =
                str_set(d->SessionSetupReq.EVCCID.characters, sizeof(d->SessionSetupReq.EVCCID.characters), in->evcc_id);
            break;
        case SP20_AUTHORIZATION_SETUP:
            d->AuthorizationSetupReq_isUsed = 1;
            HDR_SET(&d->AuthorizationSetupReq.Header, in);
            break;
        case SP20_AUTHORIZATION:
            d->AuthorizationReq_isUsed = 1;
            HDR_SET(&d->AuthorizationReq.Header, in);
            d->AuthorizationReq.SelectedAuthorizationService = (iso20_authorizationType)in->selected_authorization;
            if (in->selected_authorization == 0) d->AuthorizationReq.EIM_AReqAuthorizationMode_isUsed = 1;
            break;
        case SP20_SERVICE_DISCOVERY:
            d->ServiceDiscoveryReq_isUsed = 1;
            HDR_SET(&d->ServiceDiscoveryReq.Header, in);
            break;
        case SP20_SERVICE_DETAIL:
            d->ServiceDetailReq_isUsed = 1;
            HDR_SET(&d->ServiceDetailReq.Header, in);
            d->ServiceDetailReq.ServiceID = in->service_id;
            break;
        case SP20_SERVICE_SELECTION:
            d->ServiceSelectionReq_isUsed = 1;
            HDR_SET(&d->ServiceSelectionReq.Header, in);
            d->ServiceSelectionReq.SelectedEnergyTransferService.ServiceID = in->selected_service_id;
            d->ServiceSelectionReq.SelectedEnergyTransferService.ParameterSetID = in->selected_parameter_set_id;
            break;
        case SP20_SCHEDULE_EXCHANGE: {
            struct iso20_ScheduleExchangeReqType* r = &d->ScheduleExchangeReq;
            d->ScheduleExchangeReq_isUsed = 1;
            HDR_SET(&r->Header, in);
            r->MaximumSupportingPoints = in->max_supporting_points;
            if (in->se_mode == SP20_MODE_DYNAMIC) {
                struct iso20_Dynamic_SEReqControlModeType* m = &r->Dynamic_SEReqControlMode;
                r->Dynamic_SEReqControlMode_isUsed = 1;
                m->DepartureTime = in->departure_time;
                m->MinimumSOC_isUsed = in->has_min_soc;
                m->MinimumSOC = in->min_soc;
                m->TargetSOC_isUsed = in->has_target_soc;
                m->TargetSOC = in->target_soc;
                rat_c_to(&m->EVTargetEnergyRequest, in->target_energy);
                rat_c_to(&m->EVMaximumEnergyRequest, in->max_energy);
                rat_c_to(&m->EVMinimumEnergyRequest, in->min_energy);
            } else {
                struct iso20_Scheduled_SEReqControlModeType* m = &r->Scheduled_SEReqControlMode;
                r->Scheduled_SEReqControlMode_isUsed = 1;
                m->DepartureTime_isUsed = in->has_departure_time;
                m->DepartureTime = in->departure_time;
                m->EVTargetEnergyRequest_isUsed = in->has_target_energy;
                rat_c_to(&m->EVTargetEnergyRequest, in->target_energy);
                m->EVMaximumEnergyRequest_isUsed = in->has_max_energy;
                rat_c_to(&m->EVMaximumEnergyRequest, in->max_energy);
                m->EVMinimumEnergyRequest_isUsed = in->has_min_energy;
                rat_c_to(&m->EVMinimumEnergyRequest, in->min_energy);
            }
            break;
        }
        case SP20_POWER_DELIVERY: {
            struct iso20_PowerDeliveryReqType* r = &d->PowerDeliveryReq;
            d->PowerDeliveryReq_isUsed = 1;
            HDR_SET(&r->Header, in);
            r->EVProcessing = (iso20_processingType)in->ev_processing;
            r->ChargeProgress = (iso20_chargeProgressType)in->charge_progress;
            if (in->has_power_profile) {
                r->EVPowerProfile_isUsed = 1;
                r->EVPowerProfile.TimeAnchor = in->timestamp;
                if (in->profile_mode == SP20_MODE_SCHEDULED) {
                    r->EVPowerProfile.Scheduled_EVPPTControlMode_isUsed = 1;
                    r->EVPowerProfile.Scheduled_EVPPTControlMode.SelectedScheduleTupleID = in->selected_schedule_tuple_id;
                } else {
                    r->EVPowerProfile.Dynamic_EVPPTControlMode_isUsed = 1;
                }
                struct iso20_PowerScheduleEntryType* e = &r->EVPowerProfile.EVPowerProfileEntries.EVPowerProfileEntry.array[0];
                e->Duration = 3600;
                e->Power.Value = 11;
                e->Power.Exponent = 3;
                r->EVPowerProfile.EVPowerProfileEntries.EVPowerProfileEntry.arrayLen = 1;
            }
            break;
        }
        case SP20_SESSION_STOP:
            d->SessionStopReq_isUsed = 1;
            HDR_SET(&d->SessionStopReq.Header, in);
            d->SessionStopReq.ChargingSession = (iso20_chargingSessionType)in->charging_session;
            break;
        default:
            return SP_ERR_UNSUPPORTED;
        }
    } else {
        memset(&adoc, 0, sizeof(adoc));
        struct iso20_ac_exiDocument* d = &adoc;
        if (in->kind == SP20_AC_CHARGE_PARAMETER_DISCOVERY) {
            struct iso20_ac_AC_ChargeParameterDiscoveryReqType* r = &d->AC_ChargeParameterDiscoveryReq;
            d->AC_ChargeParameterDiscoveryReq_isUsed = 1;
            HDR_SET(&r->Header, in);
            r->AC_CPDReqEnergyTransferMode_isUsed = 1;
            rat_a_to(&r->AC_CPDReqEnergyTransferMode.EVMaximumChargePower, in->max_power);
            rat_a_to(&r->AC_CPDReqEnergyTransferMode.EVMinimumChargePower, in->min_power);
        } else {
            struct iso20_ac_AC_ChargeLoopReqType* r = &d->AC_ChargeLoopReq;
            d->AC_ChargeLoopReq_isUsed = 1;
            HDR_SET(&r->Header, in);
            r->MeterInfoRequested = in->meter_info_requested;
            if (in->has_display) {
                r->DisplayParameters_isUsed = 1;
                r->DisplayParameters.PresentSOC_isUsed = in->has_present_soc;
                r->DisplayParameters.PresentSOC = in->present_soc;
            }
            if (in->cl_mode == SP20_MODE_DYNAMIC) {
                struct iso20_ac_Dynamic_AC_CLReqControlModeType* m = &r->Dynamic_AC_CLReqControlMode;
                r->Dynamic_AC_CLReqControlMode_isUsed = 1;
                m->DepartureTime_isUsed = in->has_departure_time;
                m->DepartureTime = in->departure_time;
                rat_a_to(&m->EVTargetEnergyRequest, in->target_energy);
                rat_a_to(&m->EVMaximumEnergyRequest, in->max_energy);
                rat_a_to(&m->EVMinimumEnergyRequest, in->min_energy);
                rat_a_to(&m->EVMaximumChargePower, in->max_power);
                rat_a_to(&m->EVMinimumChargePower, in->min_power);
                rat_a_to(&m->EVPresentActivePower, in->present_power);
            } else {
                struct iso20_ac_Scheduled_AC_CLReqControlModeType* m = &r->Scheduled_AC_CLReqControlMode;
                r->Scheduled_AC_CLReqControlMode_isUsed = 1;
                m->EVTargetEnergyRequest_isUsed = in->has_target_energy;
                rat_a_to(&m->EVTargetEnergyRequest, in->target_energy);
                rat_a_to(&m->EVPresentActivePower, in->present_power);
            }
        }
    }
    *pt_out = pt;
    return encode_doc(pt, out, cap, out_len);
}

// ------------------------------------------------ EV side: responses

int sp20_decode_res(int pt, const uint8_t* data, size_t len, sp20_res* o) {
    if (!o) return SP_ERR_ARG;
    uint8_t buf[SP20_MAX_EXI];
    int rc = decode_doc(pt, data, len, buf);
    if (rc != SP_OK) return rc;
    memset(o, 0, sizeof(*o));
    if (pt == SP20_PT_MAIN) {
        struct iso20_exiDocument* d = &cdoc;
        if (d->SessionSetupRes_isUsed) {
            o->kind = SP20_SESSION_SETUP;
            HDR_GET(&d->SessionSetupRes.Header, o);
            o->response_code = d->SessionSetupRes.ResponseCode;
            str_get(o->evse_id, sizeof(o->evse_id), d->SessionSetupRes.EVSEID.characters,
                    d->SessionSetupRes.EVSEID.charactersLen);
        } else if (d->AuthorizationSetupRes_isUsed) {
            o->kind = SP20_AUTHORIZATION_SETUP;
            HDR_GET(&d->AuthorizationSetupRes.Header, o);
            o->response_code = d->AuthorizationSetupRes.ResponseCode;
        } else if (d->AuthorizationRes_isUsed) {
            o->kind = SP20_AUTHORIZATION;
            HDR_GET(&d->AuthorizationRes.Header, o);
            o->response_code = d->AuthorizationRes.ResponseCode;
            o->evse_processing = d->AuthorizationRes.EVSEProcessing;
        } else if (d->ServiceDiscoveryRes_isUsed) {
            const struct iso20_ServiceDiscoveryResType* r = &d->ServiceDiscoveryRes;
            o->kind = SP20_SERVICE_DISCOVERY;
            HDR_GET(&r->Header, o);
            o->response_code = r->ResponseCode;
            int n = r->EnergyTransferServiceList.Service.arrayLen > 4 ? 4 : r->EnergyTransferServiceList.Service.arrayLen;
            o->n_services = n;
            for (int i = 0; i < n; i++) {
                o->service_ids[i] = r->EnergyTransferServiceList.Service.array[i].ServiceID;
                o->free_service[i] = r->EnergyTransferServiceList.Service.array[i].FreeService;
            }
        } else if (d->ServiceDetailRes_isUsed) {
            const struct iso20_ServiceDetailResType* r = &d->ServiceDetailRes;
            o->kind = SP20_SERVICE_DETAIL;
            HDR_GET(&r->Header, o);
            o->response_code = r->ResponseCode;
            o->service_id = r->ServiceID;
            int n = r->ServiceParameterList.ParameterSet.arrayLen > 4 ? 4 : r->ServiceParameterList.ParameterSet.arrayLen;
            o->n_sets = n;
            for (int i = 0; i < n; i++) {
                const struct iso20_ParameterSetType* ps = &r->ServiceParameterList.ParameterSet.array[i];
                o->sets[i].id = ps->ParameterSetID;
                for (int k = 0; k < ps->Parameter.arrayLen; k++) {
                    const struct iso20_ParameterType* p = &ps->Parameter.array[k];
                    char nm[82];
                    str_get(nm, sizeof(nm), p->Name.characters, p->Name.charactersLen);
                    if (!strcmp(nm, "Connector")) o->sets[i].connector = p->intValue;
                    else if (!strcmp(nm, "ControlMode")) o->sets[i].control_mode = p->intValue;
                    else if (!strcmp(nm, "EVSENominalVoltage")) o->sets[i].nominal_voltage = p->intValue;
                    else if (!strcmp(nm, "MobilityNeedsMode")) o->sets[i].mobility_needs = p->intValue;
                    else if (!strcmp(nm, "Pricing")) o->sets[i].pricing = p->intValue;
                }
            }
        } else if (d->ServiceSelectionRes_isUsed) {
            o->kind = SP20_SERVICE_SELECTION;
            HDR_GET(&d->ServiceSelectionRes.Header, o);
            o->response_code = d->ServiceSelectionRes.ResponseCode;
        } else if (d->ScheduleExchangeRes_isUsed) {
            const struct iso20_ScheduleExchangeResType* r = &d->ScheduleExchangeRes;
            o->kind = SP20_SCHEDULE_EXCHANGE;
            HDR_GET(&r->Header, o);
            o->response_code = r->ResponseCode;
            o->evse_processing = r->EVSEProcessing;
            if (r->Scheduled_SEResControlMode_isUsed && r->Scheduled_SEResControlMode.ScheduleTuple.arrayLen > 0) {
                const struct iso20_ScheduleTupleType* t = &r->Scheduled_SEResControlMode.ScheduleTuple.array[0];
                o->se_mode = SP20_MODE_SCHEDULED;
                o->schedule_tuple_id = t->ScheduleTupleID;
                o->time_anchor = t->ChargingSchedule.PowerSchedule.TimeAnchor;
                int n = t->ChargingSchedule.PowerSchedule.PowerScheduleEntries.PowerScheduleEntry.arrayLen;
                if (n > 24) n = 24;
                o->n_entries = n;
                for (int i = 0; i < n; i++) {
                    const struct iso20_PowerScheduleEntryType* e =
                        &t->ChargingSchedule.PowerSchedule.PowerScheduleEntries.PowerScheduleEntry.array[i];
                    o->entries[i].duration = e->Duration;
                    o->entries[i].power = rat_c(&e->Power);
                }
            } else if (r->Dynamic_SEResControlMode_isUsed) {
                o->se_mode = SP20_MODE_DYNAMIC;
            }
        } else if (d->PowerDeliveryRes_isUsed) {
            const struct iso20_PowerDeliveryResType* r = &d->PowerDeliveryRes;
            o->kind = SP20_POWER_DELIVERY;
            HDR_GET(&r->Header, o);
            o->response_code = r->ResponseCode;
            o->has_evse_status = r->EVSEStatus_isUsed;
            o->notification_max_delay = r->EVSEStatus.NotificationMaxDelay;
            o->evse_notification = r->EVSEStatus.EVSENotification;
        } else if (d->SessionStopRes_isUsed) {
            o->kind = SP20_SESSION_STOP;
            HDR_GET(&d->SessionStopRes.Header, o);
            o->response_code = d->SessionStopRes.ResponseCode;
        } else {
            o->kind = SP20_UNKNOWN;
        }
        return SP_OK;
    }
    struct iso20_ac_exiDocument* d = &adoc;
    if (d->AC_ChargeParameterDiscoveryRes_isUsed) {
        const struct iso20_ac_AC_ChargeParameterDiscoveryResType* r = &d->AC_ChargeParameterDiscoveryRes;
        o->kind = SP20_AC_CHARGE_PARAMETER_DISCOVERY;
        HDR_GET(&r->Header, o);
        o->response_code = r->ResponseCode;
        o->evse_max_power = rat_a(&r->AC_CPDResEnergyTransferMode.EVSEMaximumChargePower);
        o->evse_min_power = rat_a(&r->AC_CPDResEnergyTransferMode.EVSEMinimumChargePower);
        o->evse_nominal_frequency = rat_a(&r->AC_CPDResEnergyTransferMode.EVSENominalFrequency);
    } else if (d->AC_ChargeLoopRes_isUsed) {
        const struct iso20_ac_AC_ChargeLoopResType* r = &d->AC_ChargeLoopRes;
        o->kind = SP20_AC_CHARGE_LOOP;
        HDR_GET(&r->Header, o);
        o->response_code = r->ResponseCode;
        o->has_evse_status = r->EVSEStatus_isUsed;
        o->notification_max_delay = r->EVSEStatus.NotificationMaxDelay;
        o->evse_notification = r->EVSEStatus.EVSENotification;
        if (r->Dynamic_AC_CLResControlMode_isUsed) {
            o->cl_mode = SP20_MODE_DYNAMIC;
            o->has_target_power = 1;
            o->target_power = rat_a(&r->Dynamic_AC_CLResControlMode.EVSETargetActivePower);
        } else if (r->Scheduled_AC_CLResControlMode_isUsed) {
            o->cl_mode = SP20_MODE_SCHEDULED;
            o->has_target_power = r->Scheduled_AC_CLResControlMode.EVSETargetActivePower_isUsed;
            o->target_power = rat_a(&r->Scheduled_AC_CLResControlMode.EVSETargetActivePower);
        }
    } else {
        o->kind = SP20_UNKNOWN;
    }
    return SP_OK;
}

// ------------------------------------------------- time stamp position

// The TimeStamp of the message's header in the decoded document, or NULL.
static uint64_t* ts_field(int pt) {
    if (pt == SP20_PT_MAIN) {
        struct iso20_exiDocument* d = &cdoc;
#define TS(m) if (d->m##_isUsed) return &d->m.Header.TimeStamp
        TS(SessionSetupReq); TS(SessionSetupRes);
        TS(AuthorizationSetupReq); TS(AuthorizationSetupRes);
        TS(AuthorizationReq); TS(AuthorizationRes);
        TS(ServiceDiscoveryReq); TS(ServiceDiscoveryRes);
        TS(ServiceDetailReq); TS(ServiceDetailRes);
        TS(ServiceSelectionReq); TS(ServiceSelectionRes);
        TS(ScheduleExchangeReq); TS(ScheduleExchangeRes);
        TS(PowerDeliveryReq); TS(PowerDeliveryRes);
        TS(SessionStopReq); TS(SessionStopRes);
#undef TS
        return NULL;
    }
    struct iso20_ac_exiDocument* d = &adoc;
    if (d->AC_ChargeParameterDiscoveryReq_isUsed) return &d->AC_ChargeParameterDiscoveryReq.Header.TimeStamp;
    if (d->AC_ChargeParameterDiscoveryRes_isUsed) return &d->AC_ChargeParameterDiscoveryRes.Header.TimeStamp;
    if (d->AC_ChargeLoopReq_isUsed) return &d->AC_ChargeLoopReq.Header.TimeStamp;
    if (d->AC_ChargeLoopRes_isUsed) return &d->AC_ChargeLoopRes.Header.TimeStamp;
    return NULL;
}

static int bit_at(const uint8_t* b, int i) { return (b[i >> 3] >> (7 - (i & 7))) & 1; }

int sp20_ts_bit(int pt, const uint8_t* data, size_t len, int verify_full, int* bit_out) {
    if (!bit_out) return SP_ERR_ARG;
    uint8_t buf[SP20_MAX_EXI];
    int rc = decode_doc(pt, data, len, buf);
    if (rc != SP_OK) return rc;
    uint64_t* ts = ts_field(pt);
    if (!ts) return SP_ERR_UNSUPPORTED;
    uint64_t own = *ts;

    // 2^28 and 2^35-1: both five 7-bit groups; every value bit differs but
    // the lowest one of the last group, the continuation bits are the same.
    static _Thread_local uint8_t a[SP20_MAX_EXI], b[SP20_MAX_EXI], c[SP20_MAX_EXI];
    size_t la, lb, lc;
    *ts = (uint64_t)1 << 28;
    if (encode_doc(pt, a, sizeof(a), &la) != SP_OK) return SP_ERR_ENCODE;
    *ts = ((uint64_t)1 << 35) - 1;
    if (encode_doc(pt, b, sizeof(b), &lb) != SP_OK) return SP_ERR_ENCODE;
    if (la != lb) return SP_ERR_ENCODE;
    int first = -1;
    for (int i = 0; i < (int)la * 8; i++) {
        if (bit_at(a, i) != bit_at(b, i)) {
            first = i;
            break;
        }
    }
    if (first < 1) return SP_ERR_ENCODE;
    int p = first - 1;  // the first group's continuation bit
    // nothing outside [p, p+40) may differ
    for (int i = 0; i < (int)la * 8; i++) {
        if ((i < p || i >= p + 40) && bit_at(a, i) != bit_at(b, i)) return SP_ERR_ENCODE;
    }
    // the input's prefix before the time stamp is the re-encoding's
    if ((size_t)(p + 7) / 8 > len) return SP_ERR_DECODE;
    for (int i = 0; i < p; i++) {
        if (bit_at(a, i) != bit_at(data, i)) return SP_ERR_DECODE;
    }
    if (verify_full) {
        *ts = own;
        if (encode_doc(pt, c, sizeof(c), &lc) != SP_OK) return SP_ERR_ENCODE;
        if (lc != len || memcmp(c, data, len) != 0) return SP_ERR_DECODE;
    }
    *bit_out = p;
    return SP_OK;
}
