# SPDX-License-Identifier: Apache-2.0
# Copyright Contributors to the project. See NOTICE.

"""Device settings for the operator: /v1/devices/{id}/config (OSS scope, 13.11.2026).

Two sections, stored in versions in device_configs (a change is a new row,
never an update; the latest row is in force):
  device     the device's own settings: the engine is woken by NOTIFY config
             and publishes dev/<id>/config (retained) with rev = that version;
             the device reports the rev it applied in its heartbeat
             (config_rev): "applied" or "pending".
  station    the host charger behind it: max current, phases (in force in the
             cloud at once, every answer to the EV uses them); nominal
             voltage and EVSEID are server settings, shown read-only.

Defaults come from the same environment variables the engine uses.
Reading: right "read"; saving: right "control", audited.
"""
import json
import os

from fastapi import APIRouter, Depends, HTTPException
from pydantic import BaseModel

from . import db
from .auth import audit, need

router = APIRouter(prefix="/v1", tags=["config"])


def _env(name, default):
    v = os.environ.get(name)
    if v is None or v == "":
        return default
    if isinstance(default, bool):
        return v.lower() == "true"
    if isinstance(default, str):
        return v
    return int(float(v))


def F(name, type_, default, label, help_="", **kw):
    """One setting: type int | bool | choice | time | readonly."""
    return {"name": name, "type": type_, "default": default, "label": label, "help": help_, **kw}


DEVICE = [
    F("loop_sample_s", "int", _env("DEVICE_LOOP_SAMPLE_S", 30), "Loop sample period T, s",
      "A cache hit is forwarded to the cloud at most once per T s (liveness, latency). 0 = every one (debugging).",
      min=0, max=600),
    F("ttl_s", "int", _env("DEVICE_TTL_S", 1200), "TTL, s",
      "How long the cache and limit commands hold without word from the cloud. On expiry: local termination (digital) or charger PWM (basic).",
      min=60, max=3600),
    F("heartbeat_s", "int", _env("DEVICE_HEARTBEAT_S", 60), "Heartbeat period, s",
      "Status message period; also the window of the loop-active flag.", min=10, max=600),
    F("term_backup_s", "int", _env("DEVICE_TERM_BACKUP_S", 5), "Termination backup timer, s",
      "If the EV does not follow the stop scenario within this time, the connection is dropped and the CP interruption recovers the EV.",
      min=2, max=30),
    F("trace", "bool", _env("DEVICE_TRACE", True), "Per-frame trace",
      "Timestamps of every frame (diagnostics). Off in production: only sampled requests reach the cloud."),
    F("cache_enabled", "bool", _env("DEVICE_CACHE", True), "Charging Loop cache",
      "Off: every loop request goes to the cloud (comparison runs)."),
    F("digital_mode", "bool", _env("DEVICE_DIGITAL_MODE", True), "Digital mode (5 % to the EV)",
      "On: the device masks the charger PWM to 5 %, the EV starts ISO 15118 / DIN. "
      "Off: transit, basic charging on the charger PWM."),
    F("hlc_wait_s", "int", _env("DEVICE_HLC_WAIT_S", 30), "Wait for HLC, s",
      "Digital mode: an EV that starts no ISO 15118 / DIN session within this time after plug-in "
      "is charged in basic mode on the charger PWM (with the operator limit, if any). 0 = wait forever.",
      min=0, max=120),
    F("telemetry_s", "int", _env("DEVICE_TELEMETRY_S", 60), "Metering period, s",
      "Current and energy report while current flows; a change of 1 A or more is reported at once.",
      min=1, max=600),
    F("soc_final", "bool", _env("DEVICE_SOC_FINAL", True), "Final SoC read",
      "An EV without ISO 15118-2 (its SoC read over DIN 70121 at plug-in) is read again when it stops charging by itself: "
      "5 % for one more DIN handshake, then the charger PWM again. Two readings and the energy between them give the battery capacity.",
      applies="cloud"),
    F("soc_final_wait_s", "int", _env("DEVICE_SOC_FINAL_WAIT_S", 20), "Final SoC read after, s",
      "How long the EV must stay in B (stopped charging) before the final SoC read.",
      min=5, max=600, applies="cloud"),
]

STATION = [
    F("max_current_a", "int", _env("EVSE_MAX_CURRENT_A", 32), "Max current, A",
      "The charger limit, i.e. its PWM. The cloud never offers the EV more (EVSEMaxCurrent) and rejects operator limits above it. "
      "",
      min=6, max=80, applies="cloud"),
    F("phases", "choice", _env("EVSE_PHASES", 3), "Phases",
      "A three-phase request from the EV is refused on a single-phase charger. Allowed power = current x 230 V x phases.",
      choices=[[1, "1 phase"], [3, "3 phases"]], applies="cloud"),
    F("nominal_voltage_v", "readonly", _env("EVSE_NOMINAL_VOLTAGE_V", 230), "Nominal voltage, V",
      "No voltage measurement: power is computed at nominal voltage. Server setting (EVSE_NOMINAL_VOLTAGE_V)."),
    F("evse_id", "readonly", None, "EVSEID",
      "Sent in SessionSetupRes. Derived from the device name unless EVSE_ID is set on the server."),
]

SECTIONS = {
    "station": {"title": "Charger", "fields": STATION},
    "device": {"title": "Device", "fields": DEVICE},
}
RENEW_S = _env("DEVICE_TTL_RENEW_S", 300)


def _evse_id(device_id: str) -> str:
    # same rule as the engine (stationConfig)
    return os.environ.get("EVSE_ID") or "FI*CTR*E" + device_id.replace("-", "").upper()


def defaults(section: str, device_id: str = "") -> dict:
    d = {f["name"]: f["default"] for f in SECTIONS[section]["fields"]}
    if section == "station":
        d["evse_id"] = _evse_id(device_id)
    return d


def effective(device_id: str, config: dict | None) -> dict:
    """Every section of a device: stored values over defaults (read-only fields always from the server)."""
    out = {}
    for s in SECTIONS:
        d = defaults(s, device_id)
        stored = (config or {}).get(s) or {}
        out[s] = {**d, **{k: v for k, v in stored.items() if k in d}}
        for f in SECTIONS[s]["fields"]:
            if f["type"] == "readonly":
                out[s][f["name"]] = d[f["name"]]
    return out


def _check(f, v):
    """Validated value or raise ValueError with the message for the user."""
    n, t = f["name"], f["type"]
    if t == "readonly":
        raise ValueError(f"{n} is set on the server, not here")
    if v is None or v == "":
        if f.get("nullable"):
            return None
        raise ValueError(f"{n} is mandatory")
    if t == "bool":
        if not isinstance(v, bool):
            raise ValueError(f"{n} must be true or false")
        return v
    if t == "choice":
        allowed = [c[0] for c in f["choices"]]
        if v not in allowed:
            raise ValueError(f"{n} must be one of {', '.join(map(str, allowed))}")
        return v
    if t == "time":
        try:
            h, m = str(v).split(":")
            if not (0 <= int(h) <= 23 and 0 <= int(m) <= 59 and len(m) == 2):
                raise ValueError
        except ValueError:
            raise ValueError(f"{n} must be hh:mm") from None
        return f"{int(h):02d}:{m}"
    if not isinstance(v, (int, float)) or isinstance(v, bool) or not (f["min"] <= v <= f["max"]):
        raise ValueError(f"{n} must be {f['min']}..{f['max']}")
    return int(v)


def _cross_checks(section: str, vals: dict) -> list[str]:
    errs = []
    if section == "device" and vals["ttl_s"] < 2 * RENEW_S:
        errs.append(f"ttl_s must be at least twice the cloud renewal period ({RENEW_S} s), "
                    f"or the TTL may expire while the cloud is fine")
    return errs


def _latest(conn, device_id):
    return conn.execute("SELECT version, config FROM device_configs WHERE device_id = %s "
                        "ORDER BY version DESC LIMIT 1", (device_id,)).fetchone()


def _device(conn, device_id):
    return conn.execute("SELECT device_id, status, online FROM devices WHERE device_id = %s", (device_id,)).fetchone()


@router.get("/config/fields", summary="The device settings: sections, fields, limits, defaults")
def fields(who: str = Depends(need("read"))):
    return {"sections": {s: {"title": d["title"], "fields": d["fields"]} for s, d in SECTIONS.items()},
            "ttl_renew_s": RENEW_S}


@router.get("/devices/{device_id}/config", summary="The device's settings in force, defaults, history")
def get_config(device_id: str, who: str = Depends(need("read"))):
    with db.connect() as conn:
        dev = _device(conn, device_id)
        if not dev:
            raise HTTPException(404, "unknown device")
        rows = conn.execute("SELECT version, created_at, created_by, note, config FROM device_configs "
                            "WHERE device_id = %s ORDER BY version DESC LIMIT 21", (device_id,)).fetchall()
    status = dev[1] or {}
    secs = list(SECTIONS)
    history = []
    for i, r in enumerate(rows[:20]):
        older = rows[i + 1][4] if i + 1 < len(rows) else ({} if r[0] == 1 else None)
        changes = {}
        for s in secs:
            new = (r[4] or {}).get(s) or {}
            if older is None:          # the version before is outside the window
                changes[s] = None
                continue
            old = older.get(s) or {}
            diff = {k: v for k, v in new.items() if old.get(k, object()) != v}
            if diff:
                changes[s] = diff
        history.append({"rev": r[0], "created_at": r[1].isoformat(), "by": r[2], "note": r[3], "changes": changes})
    return {"device_id": device_id, "rev": rows[0][0] if rows else 0,
            "sections": effective(device_id, rows[0][4] if rows else None),
            "defaults": {s: defaults(s, device_id) for s in secs},
            "applied_rev": status.get("config_rev"), "device_online": dev[2],
            "history": history}


class ConfigIn(BaseModel):
    device: dict | None = None
    station: dict | None = None
    note: str | None = None


@router.put("/devices/{device_id}/config", summary="Save a new version of the device's settings")
def put_config(device_id: str, body: ConfigIn, who: str = Depends(need("control"))):
    given = {s: getattr(body, s) for s in SECTIONS if getattr(body, s)}
    req = {**given, "note": body.note}
    if not given:
        audit(who, "device.config", 422, device_id, request=req)
        raise HTTPException(422, "nothing to save")
    with db.connect() as conn:
        dev = _device(conn, device_id)
        if not dev:
            audit(who, "device.config", 404, device_id, request=req)
            raise HTTPException(404, "unknown device")
        with conn.transaction():
            r = _latest(conn, device_id)
            cfg = dict(r[1]) if r else {}
            eff = effective(device_id, cfg)
            errors = []
            for s, changes in given.items():
                byname = {f["name"]: f for f in SECTIONS[s]["fields"]}
                clean = {}
                for k, v in changes.items():
                    f = byname.get(k)
                    if f is None:
                        errors.append(f"unknown setting {s}.{k}")
                        continue
                    try:
                        clean[k] = _check(f, v)
                    except ValueError as e:
                        errors.append(str(e))
                errors += _cross_checks(s, {**eff[s], **clean})
                cfg[s] = {**(cfg.get(s) or {}), **clean}
            if errors:
                audit(who, "device.config", 422, device_id, request=req)
                raise HTTPException(422, "; ".join(errors))
            rev = (r[0] if r else 0) + 1
            conn.execute("INSERT INTO device_configs (device_id, version, config, created_by, note) "
                         "VALUES (%s, %s, %s, %s, %s)", (device_id, rev, json.dumps(cfg), who, body.note))
            # the engine republishes dev/<id>/config and refreshes the cache pair
            conn.execute("SELECT pg_notify('config', %s)", (device_id,))
    audit(who, "device.config", 200, device_id, request=req)
    return {"device_id": device_id, "rev": rev, "sections": effective(device_id, cfg)}
