# SPDX-License-Identifier: Apache-2.0
# Copyright Contributors to the project. See NOTICE.

"""Operator event feed /v1/events.

Reads domain_events directly; independent of the projector.
- cursor: the last `seq` the operator has; the answer holds the next one.
- No event is skipped: every writer of domain_events holds an advisory lock
  from its insert to its commit (store.feedLock), so seq follows commit
  order and a later seq is never visible before an earlier one.
- Order within a device: `dseq` (ADR-001), not `seq`.
- X-Data-Lag-Ms: how far the stored events trail the engines (relay lag);
  `system.events_gap` in the feed: an interval with possibly lost events.
Schema: GET /v1/events/schema (schemas/events.v1.json).
"""
import json
import pathlib

from fastapi import APIRouter, Depends, Query, Response
from fastapi.responses import JSONResponse

from . import db
from .auth import need

router = APIRouter(prefix="/v1")
SCHEMA = pathlib.Path(__file__).resolve().parent.parent / "schemas" / "events.v1.json"
LAG_DEGRADED_MS = 5000


def data_lag_ms(conn) -> int:
    """Lag of the event store behind the engines, ms. No relay row: a single
    engine on Postgres writes the events itself, no lag. A relay that stopped
    reporting adds the time since its last report."""
    r = conn.execute("SELECT lag_ms, extract(epoch FROM now() - updated_at) * 1000 FROM relay_state "
                     "WHERE name = 'events'").fetchone()
    if not r:
        return 0
    lag, silent = int(r[0] or 0), int(r[1] or 0)
    return lag + (silent if silent > 3000 else 0)


def projector_lag_ms(conn) -> int | None:
    """How far the operator tables trail the stored feed, ms: age of the
    oldest event the projector has not applied. None: no projector running
    (projector_state empty) — the tables are then not filled at all."""
    r = conn.execute("SELECT cursor FROM projector_state WHERE name = 'operator'").fetchone()
    if not r:
        return None
    n = conn.execute("SELECT extract(epoch FROM now() - recorded_at) * 1000 FROM domain_events "
                     "WHERE seq > %s ORDER BY seq LIMIT 1", (r[0],)).fetchone()
    return int(n[0]) if n else 0


def tables_lag_ms(conn) -> int:
    """Data lag of the operator tables: engines -> feed (relay) -> tables (projector)."""
    return data_lag_ms(conn) + (projector_lag_ms(conn) or 0)


@router.get("/events", tags=["feed"], summary="Event feed by cursor (seq)")
def feed(response: Response, cursor: int = Query(0, ge=0), limit: int = Query(500, ge=1, le=1000),
         types: str = Query("", description="comma-separated; a trailing * matches a prefix, e.g. session.*"),
         device: str = Query(""), who: str = Depends(need("read"))):
    where, args = ["seq > %s"], [cursor]
    if device:
        where.append("device_id = %s")
        args.append(device)
    exact, prefix = [], []
    for t in filter(None, (x.strip() for x in types.split(","))):
        (prefix if t.endswith("*") else exact).append(t.rstrip("*"))
    if exact or prefix:
        ors = []
        if exact:
            ors.append("type = ANY(%s)")
            args.append(exact)
        for p in prefix:
            ors.append("starts_with(type, %s)")
            args.append(p)
        where.append("(" + " OR ".join(ors) + ")")
    with db.connect() as conn:
        rows = conn.execute(
            "SELECT seq, id, type, schema_ver, device_id, session_id, dseq, time, data, recorded_at "  # nosec B608: built from constant fragments; values are parameters
            f"FROM domain_events WHERE {' AND '.join(where)} ORDER BY seq LIMIT %s", (*args, limit)).fetchall()
        lag = data_lag_ms(conn)
    response.headers["X-Data-Lag-Ms"] = str(lag)
    out = [{"seq": r[0], "id": r[1], "type": r[2], "schema_ver": r[3], "device_id": r[4],
            "session_id": str(r[5]) if r[5] else None, "dseq": r[6], "time": r[7].isoformat(),
            "data": r[8], "recorded_at": r[9].isoformat()} for r in rows]
    return {"events": out, "next_cursor": out[-1]["seq"] if out else cursor, "data_lag_ms": lag}


@router.get("/events/schema", tags=["feed"], summary="JSON Schema of the feed events")
def schema(who: str = Depends(need("read"))):
    return JSONResponse(json.loads(SCHEMA.read_text()))
