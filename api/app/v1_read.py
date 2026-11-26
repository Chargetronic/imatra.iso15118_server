# SPDX-License-Identifier: Apache-2.0
# Copyright Contributors to the project. See NOTICE.

"""Operator API, reading: devices, plug-in sessions, debug.

Data: the operator tables the projector fills (device_live,
session_summaries, device_attributes). Every answer carries X-Data-Lag-Ms
(how far the tables trail the engines) and every row its as_of.

Lists are keyset-paged: pass next_cursor back as cursor until it is null.
A list read while data changes may show a device or a session in its newer
state on a later page, never twice and never skip one that existed before
the first page (devices by id; sessions by start, newest first).
"""
import base64
from datetime import datetime
from typing import Literal

from fastapi import APIRouter, Body, Depends, HTTPException, Query, Response
from pydantic import BaseModel, Field

from . import db
from .auth import audit, need
from .events_routes import tables_lag_ms
from .v1_routes import LIVE_COLS, _json

router = APIRouter(prefix="/v1")
ATTR_COLS = ("firmware", "connectivity", "attrs")
SUMMARY_COLS = ("summary_id", "device_id", "started_at", "ended_at", "mode", "v2g_sessions", "has_eamount",
                "has_soc", "e_amount_wh", "soc_start", "departure_time_s", "charging_s", "limits_applied",
                "energy_wh", "energy_quality", "duration_s", "end_reason", "implicit_start", "data_gap", "updated_at")


def _lag(conn, response: Response) -> None:
    response.headers["X-Data-Lag-Ms"] = str(tables_lag_ms(conn))


def _device_row(r) -> dict:
    live = dict(zip(LIVE_COLS, r[1:1 + len(LIVE_COLS)]))
    attrs = dict(zip(ATTR_COLS, r[1 + len(LIVE_COLS):1 + len(LIVE_COLS) + len(ATTR_COLS)]))
    lat, lon, blocked, binding, btype, ver, seen, rules = r[-8:]
    extra = attrs.pop("attrs") or {}
    return {"device_id": r[0], **{k: _json(v) for k, v in live.items()}, "blocked": blocked is not None,
            "binding": binding, "binding_ver": ver, "type": btype, "rules": rules or {},
            "connected_ever": seen is not None,
            "location": {"lat": float(lat), "lon": float(lon)} if lat is not None else None,
            "attributes": {**{k: v for k, v in attrs.items()}, **extra}}


@router.get("/devices", tags=["devices"], summary="Devices with live state and attributes, by id, 1000 a page")
def devices(response: Response,
            online: bool | None = None, charging: bool | None = None, blocked: bool | None = None,
            binding: Literal["bound", "unbound", "revoked", "none"] | None = Query(
                None, description="registry state of the service layer; none: not in the registry"),
            mode: Literal["iso", "din", "pwm"] | None = Query(None, description="control mode of the plug-in"),
            type: str | None = Query(None, description="device type from the service layer's registry (for reports only)"),
            cursor: str = Query("", description="next_cursor of the previous page"),
            limit: int = Query(1000, ge=1, le=1000),
            who: str = Depends(need("read"))):
    where, args = ["d.device_id > %s"], [cursor]
    for col, val in (("l.online", online), ("l.charging", charging), ("l.control_mode", mode)):
        if val is not None:
            where.append(f"{col} = %s")
            args.append(val)
    if blocked is not None:
        where.append("d.blocked_at IS NOT NULL" if blocked else "d.blocked_at IS NULL")
    if type is not None:
        where.append("d.device_type = %s")
        args.append(type)
    if binding == "none":
        where.append("d.bind_state IS NULL")
    elif binding is not None:
        where.append("d.bind_state = %s")
        args.append(binding)
    cols = ", ".join([f"l.{c}" for c in LIVE_COLS] + [f"a.{c}" for c in ATTR_COLS] + ["d.lat", "d.lon", "d.blocked_at", "d.bind_state",
                                                                                "d.device_type", "d.binding_ver", "d.last_seen_at", "d.rules"])
    with db.connect() as conn:
        rows = conn.execute(
            f"SELECT d.device_id, {cols} FROM devices d "  # nosec B608: built from constant fragments; values are parameters
            "LEFT JOIN device_live l USING (device_id) LEFT JOIN device_attributes a USING (device_id) "
            f"WHERE {' AND '.join(where)} ORDER BY d.device_id LIMIT %s", (*args, limit)).fetchall()
        _lag(conn, response)
    out = [_device_row(r) for r in rows]
    return {"devices": out, "next_cursor": out[-1]["device_id"] if len(out) == limit else None}


def _scursor(s: str) -> tuple[datetime, str] | None:
    if not s:
        return None
    try:
        ts, _, sid = base64.urlsafe_b64decode(s.encode()).decode().partition("|")
        return datetime.fromisoformat(ts), sid
    except Exception:
        raise HTTPException(422, "bad cursor")


@router.get("/sessions", tags=["sessions"], summary="Plug-in sessions (summaries), newest first, 1000 a page")
def sessions(response: Response,
             device: str | None = None,
             since: datetime | None = Query(None, alias="from", description="started at or after"),
             until: datetime | None = Query(None, alias="to", description="started before"),
             open: bool | None = Query(None, description="true: still plugged in; false: ended"),
             cursor: str = "", limit: int = Query(1000, ge=1, le=1000),
             who: str = Depends(need("read"))):
    where, args = [], []
    if device:
        where.append("device_id = %s")
        args.append(device)
    if since:
        where.append("started_at >= %s")
        args.append(since)
    if until:
        where.append("started_at < %s")
        args.append(until)
    if open is not None:
        where.append("ended_at IS NULL" if open else "ended_at IS NOT NULL")
    c = _scursor(cursor)
    if c:
        where.append("(started_at, summary_id) < (%s, %s)")
        args += [c[0], c[1]]
    with db.connect() as conn:
        rows = conn.execute(
            f"SELECT {', '.join(SUMMARY_COLS)} FROM session_summaries "  # nosec B608: built from constant fragments; values are parameters
            f"{'WHERE ' + ' AND '.join(where) if where else ''} "
            "ORDER BY started_at DESC, summary_id DESC LIMIT %s", (*args, limit)).fetchall()
        _lag(conn, response)
    out = [{k: _json(v) for k, v in zip(SUMMARY_COLS, r)} for r in rows]
    nxt = None
    if len(rows) == limit:
        nxt = base64.urlsafe_b64encode(f"{rows[-1][2].isoformat()}|{rows[-1][0]}".encode()).decode()
    return {"sessions": out, "next_cursor": nxt}


@router.get("/sessions/{summary_id}", tags=["sessions"], summary="One plug-in session")
def session(summary_id: str, response: Response, who: str = Depends(need("read"))):
    with db.connect() as conn:
        r = conn.execute(f"SELECT {', '.join(SUMMARY_COLS)} FROM session_summaries WHERE summary_id = %s",  # nosec B608: built from constant fragments; values are parameters
                         (summary_id,)).fetchone()
        _lag(conn, response)
    if not r:
        raise HTTPException(404, "no such session")
    return {k: _json(v) for k, v in zip(SUMMARY_COLS, r)}


@router.get("/debug/sessions/{session_id}/messages", tags=["debug"],
            summary="Twin journal of a digital (V2G) session: the decoded messages, when the journal was on")
def debug_messages(session_id: str, limit: int = Query(500, ge=1, le=5000), who: str = Depends(need("debug"))):
    with db.connect() as conn:
        s = conn.execute("SELECT device_id FROM sessions WHERE session_id::text = %s", (session_id,)).fetchone()
        if not s:
            raise HTTPException(404, "no such digital session")
        rows = conn.execute(
            "SELECT seq, direction, source, msg_type, decoded, payload_bytes, device_ts, server_ts FROM messages "
            "WHERE session_id::text = %s ORDER BY id LIMIT %s", (session_id, limit)).fetchall()
    cols = ("seq", "direction", "source", "msg_type", "decoded", "payload_bytes", "device_ts", "server_ts")
    return {"session_id": session_id, "device_id": s[0],
            "journal": "empty: the full journal is off for this device (JOURNAL_MODE, debug flag)" if not rows else None,
            "messages": [{k: _json(v) for k, v in zip(cols, r)} for r in rows]}
