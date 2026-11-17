# SPDX-License-Identifier: Apache-2.0
# Copyright Contributors to the project. See NOTICE.

"""Activations: a group limit in stages, for compensation and
flexibility services. The API records the request; the engines' leader
schedules the stages, selects the devices when a stage starts and has the
limits sent at a controlled rate (engine: activations.go).

    POST   /v1/activations          create (a repeat with the same id and body is safe)
    GET    /v1/activations          list, newest first
    GET    /v1/activations/{id}     summary per stage: selected, sent, confirmed, estimated reduction
    PATCH  /v1/activations/{id}     add stages (compensation), extend, new level for the next stages
    DELETE /v1/activations/{id}     end now: the limits in force before apply again

Rights: control. Every write is audited.
"""
import json
import re
import uuid
from datetime import datetime, timedelta, timezone

from fastapi import APIRouter, Depends, HTTPException, Query, Response
from pydantic import BaseModel, Field, model_validator

from . import db
from .auth import audit, need
from .v1_routes import MIN_A, _json

router = APIRouter(prefix="/v1", tags=["activations"])
MAX_STAGES = 20
MAX_DURATION_S = 24 * 3600
ID_RE = re.compile(r"^[A-Za-z0-9._:-]{1,64}$")


class Selection(BaseModel):
    devices: list[str] | None = Field(None, max_length=20000, description="explicit list")
    online: bool = Field(True, description="only devices online at the stage start")
    charging: bool | None = Field(None, description="true: only charging; false: only not charging")
    mode: str | None = Field(None, pattern="^(iso|din|pwm)$", description="control mode of the plug-in")


class Level(BaseModel):
    current_a: float | None = Field(None, ge=MIN_A, description="per phase, per device")
    power_kw: float | None = Field(None, gt=0, description="per device")
    group_power_kw: float | None = Field(None, gt=0, description="per stage, shared evenly by its devices")

    @model_validator(mode="after")
    def one(self):
        if sum(v is not None for v in (self.current_a, self.power_kw, self.group_power_kw)) != 1:
            raise ValueError("give exactly one of current_a, power_kw, group_power_kw")
        return self


class Stage(BaseModel):
    at: datetime | None = Field(None, description="start time; default: now + offset_s")
    offset_s: float = Field(0, ge=0, le=MAX_DURATION_S, description="start after the activation is created")
    count: int | None = Field(None, ge=1, le=100000, description="devices in this stage, or")
    share: float | None = Field(None, gt=0, le=1, description="share of the devices eligible at the start")
    spread_s: float = Field(0, ge=0, le=600, description="sends spread evenly over this time")

    @model_validator(mode="after")
    def one(self):
        if (self.count is None) == (self.share is None):
            raise ValueError("give exactly one of count, share")
        return self


class Activation(BaseModel):
    id: str | None = Field(None, description="your id; a repeated POST with the same id and body is safe")
    selection: Selection
    level: Level
    stages: list[Stage] = Field(..., min_length=1, max_length=MAX_STAGES)
    duration_s: int = Field(..., ge=60, le=MAX_DURATION_S, description="limits end this long after creation")
    priority: int = Field(50, ge=1, le=100, description="the higher wins on a device (direct limits are 0)")


class Patch(BaseModel):
    add_stages: list[Stage] | None = Field(None, max_length=MAX_STAGES, description="compensation stages")
    extend_s: int | None = Field(None, ge=1, le=MAX_DURATION_S, description="end later by this much")
    level: Level | None = Field(None, description="level for stages not started yet")


def _stage_rows(base: datetime, stages: list[Stage], first: int):
    for i, s in enumerate(stages):
        at = s.at or base + timedelta(seconds=s.offset_s)
        if at.tzinfo is None:
            at = at.replace(tzinfo=timezone.utc)
        yield first + i, at, s.count, s.share, int(s.spread_s * 1000)


def _notify(conn, aid: str, what: str) -> None:
    conn.execute("SELECT pg_notify('activations', %s)", (f"{what}:{aid}",))


@router.post("/activations", status_code=201, summary="Create an activation (group limit in stages)")
def create(body: Activation, response: Response, who: str = Depends(need("control"))):
    aid = body.id or uuid.uuid4().hex
    if not ID_RE.match(aid):
        raise HTTPException(422, "id: 1-64 characters of A-Z a-z 0-9 . _ : -")
    req = body.model_dump(mode="json", exclude={"id"})
    now = datetime.now(timezone.utc)
    with db.connect() as conn:
        with conn.transaction():
            r = conn.execute("INSERT INTO activations (activation_id, key_name, priority, selection, level, ends_at, request) "
                             "VALUES (%s, %s, %s, %s, %s, %s, %s) ON CONFLICT (activation_id) DO NOTHING",
                             (aid, who, body.priority, json.dumps(req["selection"]), json.dumps(req["level"]),
                              now + timedelta(seconds=body.duration_s), json.dumps(req)))
            if not r.rowcount:  # a repeat
                prev = conn.execute("SELECT request, key_name FROM activations WHERE activation_id = %s",
                                    (aid,)).fetchone()
                if prev[0] != req or prev[1] != who:
                    audit(who, "activation.create", 409, request={"id": aid})
                    raise HTTPException(409, "an activation with this id exists with another request")
                response.status_code = 200
                return summary_of(conn, aid)
            for row in _stage_rows(now, body.stages, 1):
                conn.execute("INSERT INTO activation_stages (activation_id, stage, starts_at, count, share, spread_ms) "
                             "VALUES (%s, %s, %s, %s, %s, %s)", (aid, *row))
            _notify(conn, aid, "new")
        out = summary_of(conn, aid)
    audit(who, "activation.create", 201, request={"id": aid, **req})
    return out


@router.get("/activations", summary="Activations, newest first")
def list_(state: str | None = Query(None, pattern="^(active|ended|cancelled)$"), limit: int = Query(100, ge=1, le=1000),
          who: str = Depends(need("read"))):
    with db.connect() as conn:
        rows = conn.execute("SELECT activation_id FROM activations "  # nosec B608: built from constant fragments; values are parameters
                            f"{'WHERE state = %s' if state else ''} ORDER BY created_at DESC LIMIT %s",
                            ((state, limit) if state else (limit,))).fetchall()
        return {"activations": [summary_of(conn, r[0], stages=False) for r in rows]}


@router.get("/activations/{aid}", summary="Summary per stage: selected, sent, confirmed, estimated reduction")
def get(aid: str, who: str = Depends(need("read"))):
    with db.connect() as conn:
        return summary_of(conn, aid)


@router.patch("/activations/{aid}", summary="Add stages, extend, or change the level of the next stages")
def patch(aid: str, body: Patch, who: str = Depends(need("control"))):
    if not (body.add_stages or body.extend_s or body.level):
        raise HTTPException(422, "nothing to change")
    with db.connect() as conn:
        with conn.transaction():
            a = conn.execute("SELECT state, ends_at FROM activations WHERE activation_id = %s FOR UPDATE",
                             (aid,)).fetchone()
            if not a:
                raise HTTPException(404, "no such activation")
            if a[0] != "active" or a[1] <= datetime.now(timezone.utc):
                audit(who, "activation.patch", 409, request={"id": aid})
                raise HTTPException(409, f"activation is {a[0] if a[0] != 'active' else 'over'}")
            if body.level:
                conn.execute("UPDATE activations SET level = %s WHERE activation_id = %s",
                             (json.dumps(body.level.model_dump(mode="json")), aid))
            if body.add_stages:
                last = conn.execute("SELECT max(stage) FROM activation_stages WHERE activation_id = %s",
                                    (aid,)).fetchone()[0] or 0
                for row in _stage_rows(datetime.now(timezone.utc), body.add_stages, last + 1):
                    conn.execute("INSERT INTO activation_stages (activation_id, stage, starts_at, count, share, spread_ms) "
                                 "VALUES (%s, %s, %s, %s, %s, %s)", (aid, *row))
            if body.extend_s:
                conn.execute("UPDATE activations SET ends_at = ends_at + make_interval(secs => %s) "
                             "WHERE activation_id = %s", (body.extend_s, aid))
                conn.execute("UPDATE limits SET expires_at = expires_at + make_interval(secs => %s) "
                             "WHERE activation_id = %s AND cancelled_at IS NULL", (body.extend_s, aid))
                _notify(conn, aid, "extend")  # devices get the new duration
            if body.add_stages:
                _notify(conn, aid, "new")
        out = summary_of(conn, aid)
    audit(who, "activation.patch", 200, request={"id": aid, **body.model_dump(mode="json", exclude_none=True)})
    return out


@router.delete("/activations/{aid}", summary="End now: the limits in force before apply again")
def end(aid: str, who: str = Depends(need("control"))):
    with db.connect() as conn:
        with conn.transaction():
            n = conn.execute("UPDATE activations SET state = 'cancelled', ended_at = now() "
                             "WHERE activation_id = %s AND state = 'active'", (aid,)).rowcount
            if not n:
                if not conn.execute("SELECT 1 FROM activations WHERE activation_id = %s", (aid,)).fetchone():
                    raise HTTPException(404, "no such activation")
            else:
                conn.execute("UPDATE activation_stages SET state = 'skipped' WHERE activation_id = %s "
                             "AND state = 'scheduled'", (aid,))
                conn.execute("UPDATE limits SET cancelled_at = now() WHERE activation_id = %s AND cancelled_at IS NULL",
                             (aid,))
                _notify(conn, aid, "end")
        out = summary_of(conn, aid)
    audit(who, "activation.end", 200 if n else 409, request={"id": aid})
    return out


def summary_of(conn, aid: str, stages: bool = True) -> dict:
    a = conn.execute("SELECT activation_id, key_name, priority, selection, level, ends_at, state, created_at, ended_at "
                     "FROM activations WHERE activation_id = %s", (aid,)).fetchone()
    if not a:
        raise HTTPException(404, "no such activation")
    out = {"id": a[0], "source": a[1], "priority": a[2], "selection": a[3], "level": a[4],
           "ends_at": _json(a[5]), "state": "ended" if a[6] == "active" and a[5] <= datetime.now(timezone.utc) else a[6],
           "created_at": _json(a[7]), "ended_at": _json(a[8])}
    rows = conn.execute("""
        SELECT s.stage, s.starts_at, s.count, s.share, s.spread_ms, s.state, s.selected, s.started_at, s.dispatched_at,
               count(t.device_id), count(COALESCE(l.applied_at, l.acked_at)), count(l.acked_at),
               count(*) FILTER (WHERE t.device_id IS NOT NULL AND l.acked_at IS NULL AND t.send_at < now() - interval '30 seconds'),
               COALESCE(sum(GREATEST(0, t.power_w_before - LEAST(t.power_w_before, t.limit_w))) FILTER (WHERE l.acked_at IS NOT NULL), 0),
               percentile_disc(0.95) WITHIN GROUP (ORDER BY extract(epoch FROM l.acked_at - t.send_at) * 1000)
        FROM activation_stages s
        LEFT JOIN activation_targets t ON t.activation_id = s.activation_id AND t.stage = s.stage
        LEFT JOIN limits l ON l.command_id = t.command_id
        WHERE s.activation_id = %s GROUP BY s.activation_id, s.stage ORDER BY s.stage""", (aid,)).fetchall()
    tot = {"selected": 0, "sent": 0, "confirmed": 0, "unconfirmed_30s": 0, "reduction_kw_est": 0.0}
    st = []
    for r in rows:
        d = {"stage": r[0], "starts_at": _json(r[1]), "count": r[2], "share": _json(r[3]), "spread_s": r[4] / 1000,
             "state": r[5], "eligible_selected": r[6], "started_at": _json(r[7]), "dispatched_at": _json(r[8]),
             "selected": r[9], "sent": r[10], "confirmed": r[11], "unconfirmed_30s": r[12],
             "reduction_kw_est": round(float(r[13]) / 1000, 1),
             "confirm_p95_ms": int(r[14]) if r[14] is not None else None}
        for k in ("selected", "sent", "confirmed", "unconfirmed_30s"):
            tot[k] += d[k]
        tot["reduction_kw_est"] = round(tot["reduction_kw_est"] + d["reduction_kw_est"], 1)
        st.append(d)
    out["totals"] = tot
    if stages:
        out["stages"] = st
    return out
