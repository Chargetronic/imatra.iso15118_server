# SPDX-License-Identifier: Apache-2.0
# Copyright Contributors to the project. See NOTICE.

"""Operator API: the operator (VPP, aggregator) calls the deployment.

Keys: header X-API-Key; keys and their rights in api_keys (app/auth.py,
python -m app.keys). Commands need the "control" right and are audited.
Execution is asynchronous: 202 with a command id; the engine applies the
limit (new Charging Loop answer in a digital session, power/set on the PWM
side) and the device confirms it (cmd/ack): GET /v1/devices/{id} shows
confirmed_at / reaction_ms, the feed has limit.applied.
"""
import os
from datetime import datetime, timedelta, timezone

from fastapi import APIRouter, Depends, Header, HTTPException, Response
from fastapi.responses import JSONResponse
from pydantic import BaseModel, Field

from . import db
from .auth import audit, need

router = APIRouter(prefix="/v1")
MIN_A = 6.0           # IEC 61851-1: no PWM below 6 A
MAX_DURATION_S = 86400




def _rule_a(rules: dict, key: str) -> float | None:
    """A current limit of the business rules (docs/rules.md), or None (absent or unusable)."""
    v = (rules or {}).get(key)
    return float(v) if isinstance(v, (int, float)) and not isinstance(v, bool) and 6 <= v <= 1000 else None


def _station_max(conn, device_id: str) -> float:
    """The station limit as the engine uses it: the settings, lowered by the device's
    rating and the installation's limit (business rules)."""
    row = conn.execute(
        "SELECT (config->'station'->>'max_current_a')::float FROM device_configs "
        "WHERE device_id = %s ORDER BY version DESC LIMIT 1", (device_id,)).fetchone()
    c = row[0] if row and row[0] else float(os.environ.get("EVSE_MAX_CURRENT_A", "32"))
    r = conn.execute("SELECT rules FROM devices WHERE device_id = %s", (device_id,)).fetchone()
    for k in ("rated_current_a", "install_max_current_a"):
        lim = _rule_a(r[0] if r else {}, k)
        if lim is not None and lim < c:
            c = lim
    return c


def _device(conn, device_id: str):
    if not conn.execute("SELECT 1 FROM devices WHERE device_id = %s",
                        (device_id,)).fetchone():
        raise HTTPException(404, "unknown device")


def _active(conn, device_id: str):
    r = conn.execute(
        "SELECT command_id, limit_a, source, starts_at, expires_at, applied_at, delivered_at, acked_at, "
        "ack_via, reaction_ms, priority, activation_id FROM limits "
        "WHERE device_id = %s AND cancelled_at IS NULL AND starts_at <= now() AND expires_at > now() "
        "ORDER BY priority DESC, command_id DESC LIMIT 1", (device_id,)).fetchone()  # as the engine
    if not r:
        return None
    return {"command_id": r[0], "limit_a": float(r[1]), "source": r[2], "priority": r[10], "activation_id": r[11],
            "starts_at": r[3].isoformat(), "expires_at": r[4].isoformat(),
            "applied_at": r[5].isoformat() if r[5] else None,
            # the device's own confirmation (cmd/ack, or its CP report)
            "delivered_at": r[6].isoformat() if r[6] else None,
            "confirmed_at": r[7].isoformat() if r[7] else None,
            "confirmed_via": r[8], "reaction_ms": r[9]}


class Limit(BaseModel):
    limit_a: float = Field(..., description="per phase, A")
    duration_s: int = Field(..., description="how long the limit holds, s")


def apply_limit(device_id: str, limit_a: float, duration_s: int, source: str,
                max_duration_s: int = MAX_DURATION_S) -> dict:
    """A current limit for a device: checks, replaces the active one, wakes the engine.
    Source = the operator key's name."""
    with db.connect() as conn:
        _device(conn, device_id)
        station = _station_max(conn, device_id)
        if limit_a < MIN_A:
            raise HTTPException(422, f"limit_a below {MIN_A} A (IEC 61851-1)")
        if limit_a > station:
            raise HTTPException(422, f"limit_a above the station limit {station} A")
        if not 0 < duration_s <= max_duration_s:
            raise HTTPException(422, f"duration_s must be 1..{max_duration_s}")
        expires = datetime.now(timezone.utc) + timedelta(seconds=duration_s)
        with conn.transaction():
            # replaces the previous direct limit only; an activation's limit
            # (higher priority) stays and ends with its activation
            conn.execute("UPDATE limits SET cancelled_at = now() WHERE device_id = %s "
                         "AND cancelled_at IS NULL AND activation_id IS NULL", (device_id,))
            cid = conn.execute(
                "INSERT INTO limits (device_id, limit_a, expires_at, source) VALUES (%s, %s, %s, %s) "
                "RETURNING command_id", (device_id, limit_a, expires, source)).fetchone()[0]
            conn.execute("SELECT pg_notify('limits', %s)", (device_id,))
    return {"command_id": cid, "device_id": device_id, "limit_a": limit_a,
            "expires_at": expires.isoformat(), "status": "accepted"}


def cancel_limit(device_id: str) -> dict:
    """Cancels the direct limit (any source); an activation's limit stays until its activation ends
    (DELETE /v1/activations/{id}). Without either, the station limit applies."""
    with db.connect() as conn:
        _device(conn, device_id)
        with conn.transaction():
            n = conn.execute("UPDATE limits SET cancelled_at = now() WHERE device_id = %s "
                             "AND cancelled_at IS NULL AND expires_at > now() AND activation_id IS NULL",
                             (device_id,)).rowcount
            conn.execute("SELECT pg_notify('limits', %s)", (device_id,))
    return {"device_id": device_id, "cancelled": n, "status": "accepted"}


@router.post("/devices/{device_id}/limit", status_code=202, tags=["commands"],
             summary="Set a current limit (asynchronous; confirmed by the device)")
def set_limit(device_id: str, body: Limit, who: str = Depends(need("control"))):
    req = {"limit_a": body.limit_a, "duration_s": body.duration_s}
    try:
        r = apply_limit(device_id, body.limit_a, body.duration_s, who)
    except HTTPException as e:
        audit(who, "limit.set", e.status_code, device_id, request=req)
        raise
    audit(who, "limit.set", 202, device_id, r["command_id"], req)
    return r


@router.delete("/devices/{device_id}/limit", status_code=202, tags=["commands"],
               summary="Cancel the limit in force; the station limit applies again")
def clear_limit(device_id: str, who: str = Depends(need("control"))):
    try:
        r = cancel_limit(device_id)
    except HTTPException as e:
        audit(who, "limit.clear", e.status_code, device_id)
        raise
    audit(who, "limit.clear", 202, device_id)
    return JSONResponse(r, 202)


class Block(BaseModel):
    reason: str = Field("", max_length=200, description="why, for the audit and the device view")


@router.put("/devices/{device_id}/block", tags=["commands"],
            summary="Block the device locally: no V2G answers, no commands, broker denies its topics")
def block(device_id: str, body: Block, who: str = Depends(need("control"))):
    """Immediate cut-off of one device in this deployment (the deployment keeps no revocation list:
    a certificate revoked by the service layer stays valid until it expires).
    Charging goes on without the cloud (basic charging); unblock with DELETE."""
    req = {"reason": body.reason}
    with db.connect() as conn:
        try:
            _device(conn, device_id)
        except HTTPException as e:
            audit(who, "device.block", e.status_code, device_id, request=req)
            raise
        with conn.transaction():
            r = conn.execute("UPDATE devices SET blocked_at = COALESCE(blocked_at, now()), blocked_reason = %s, "
                             "blocked_by = %s WHERE device_id = %s RETURNING blocked_at",
                             (body.reason or None, who, device_id)).fetchone()
            conn.execute("SELECT pg_notify('blocked', %s)", (device_id,))
    audit(who, "device.block", 200, device_id, request=req)
    return {"device_id": device_id, "blocked": True, "blocked_at": r[0].isoformat(), "reason": body.reason or None}


@router.delete("/devices/{device_id}/block", tags=["commands"], summary="Unblock the device")
def unblock(device_id: str, who: str = Depends(need("control"))):
    with db.connect() as conn:
        try:
            _device(conn, device_id)
        except HTTPException as e:
            audit(who, "device.unblock", e.status_code, device_id)
            raise
        with conn.transaction():
            n = conn.execute("UPDATE devices SET blocked_at = NULL, blocked_reason = NULL, blocked_by = NULL "
                             "WHERE device_id = %s AND blocked_at IS NOT NULL", (device_id,)).rowcount
            conn.execute("SELECT pg_notify('blocked', %s)", (device_id,))
    audit(who, "device.unblock", 200, device_id)
    return {"device_id": device_id, "blocked": False, "changed": bool(n)}


LIVE_COLS = ("online", "cp", "charging", "control_mode", "plugged_since", "v2g_session_id", "protocol",
             "limit_a", "limit_source", "limit_until", "command_id", "limit_confirmed_at", "power_w",
             "power_quality", "e_amount_wh", "soc", "departure_time_s", "as_of")


def _json(v):
    """A database value as JSON: times as ISO 8601, numerics as numbers, jsonb as is."""
    if v is None or isinstance(v, (bool, int, float, str, dict, list)):
        return v
    if hasattr(v, "isoformat"):
        return v.isoformat()
    if type(v).__name__ == "Decimal":
        return float(v)
    return str(v)  # uuid


def operator_view(conn, device_id: str) -> dict:
    """Operator tables (projector): live state and attributes."""
    live = conn.execute(f"SELECT {', '.join(LIVE_COLS)} FROM device_live WHERE device_id = %s",  # nosec B608: built from constant fragments; values are parameters
                        (device_id,)).fetchone()
    attrs = conn.execute("SELECT firmware, connectivity, attrs, updated_at "
                         "FROM device_attributes WHERE device_id = %s", (device_id,)).fetchone()
    loc = conn.execute("SELECT lat, lon, bind_state, binding_ver, device_type, bound_at, unbound_at, bound_tz, "
                       "last_seen_at, rules FROM devices WHERE device_id = %s", (device_id,)).fetchone()
    return {"live": {k: _json(v) for k, v in zip(LIVE_COLS, live)} if live else None,
            # reported by the device itself
            "attributes": {"firmware": attrs[0], "connectivity": attrs[1],
                           **(attrs[2] or {}), "updated_at": _json(attrs[3])} if attrs else None,
            "location": {"lat": float(loc[0]), "lon": float(loc[1])} if loc and loc[0] is not None else None,
            # the service layer's registry (block D); state None: not in the registry; business rules as given
            "registry": {"state": loc[2], "binding_ver": loc[3], "type": loc[4], "bound_at": _json(loc[5]),
                         "unbound_at": _json(loc[6]), "tz": loc[7], "rules": loc[9] or {}} if loc else None,
            "connected_ever": bool(loc and loc[8]),
            "as_of": _json(live[-1]) if live else None}


@router.get("/devices/{device_id}", tags=["devices"], summary="One device: live state, attributes, limit in force")
def device(device_id: str, response: Response, who: str = Depends(need("read"))):
    from .events_routes import tables_lag_ms
    with db.connect() as conn:
        _device(conn, device_id)
        response.headers["X-Data-Lag-Ms"] = str(tables_lag_ms(conn))
        view = operator_view(conn, device_id)
        on = conn.execute("SELECT online, last_seen_at, blocked_at, blocked_reason FROM devices WHERE device_id = %s",
                          (device_id,)).fetchone()
        s = conn.execute(
            "SELECT session_id, phase, ev_data, started_at FROM sessions WHERE device_id = %s "
            "AND ended_at IS NULL ORDER BY started_at DESC LIMIT 1", (device_id,)).fetchone()
        m = conn.execute(
            "SELECT i_a, p_w, e_wh, server_ts FROM telemetry WHERE device_id = %s "
            "ORDER BY server_ts DESC LIMIT 1", (device_id,)).fetchone()
        plug = conn.execute(
            "SELECT p.t, telemetry_energy(%s, p.t, now()) FROM (SELECT max(server_ts) AS t FROM events "
            "WHERE device_id = %s AND kind = 'ev.plugged') p WHERE p.t IS NOT NULL AND NOT EXISTS ("
            "SELECT 1 FROM events WHERE device_id = %s AND kind = 'ev.unplugged' AND server_ts > p.t)",
            (device_id, device_id, device_id)).fetchone()
        soc = conn.execute(
            "SELECT ev_soc_start, ended_at, mode FROM sessions WHERE device_id = %s AND ev_soc_start IS NOT NULL "
            "ORDER BY started_at DESC LIMIT 1", (device_id,)).fetchone()
        return {"device_id": device_id, "online": on[0], **view,
                "blocked": {"at": on[2].isoformat(), "reason": on[3]} if on[2] else None,
                "last_soc": {"soc": float(soc[0]), "at": soc[1].isoformat() if soc[1] else None,
                             "source": soc[2]} if soc else None,
                "last_seen_at": on[1].isoformat() if on[1] else None,
                "station_max_a": _station_max(conn, device_id), "limit": _active(conn, device_id),
                # Device metering: current by phase (clamps), power and energy at the nominal voltage
                "metering": {"i_a": [float(x) for x in m[0]], "p_w": float(m[1]), "at": m[3].isoformat()}
                if m else None,
                # the EV plugged in now: since when and the energy it has taken (digital and basic charging)
                "charge": {"plugged_at": plug[0].isoformat(), "energy_wh": round(float(plug[1]), 1)}
                if plug else None,
                "session": {"session_id": str(s[0]), "phase": s[1], "ev": s[2],
                            "started_at": s[3].isoformat()} if s else None}
