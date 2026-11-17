# SPDX-License-Identifier: Apache-2.0
# Copyright Contributors to the project. See NOTICE.

"""Cloud ISO 15118 server of one deployment — operator API.

/v1/*    operator API: devices, settings, limits, activations, the event feed; keys with rights
/health  for the host and the compose healthcheck
There is no UI: metrics and the feed go to the operator's own systems.
"""
import logging
import os
import socket
from contextlib import asynccontextmanager

from fastapi import FastAPI
from fastapi.responses import JSONResponse

from . import db
from .v1_routes import router as v1_router
from .v1_read import router as v1_read_router
from .activations import router as activations_router
from . import auth
from .events_routes import router as events_router, tables_lag_ms, LAG_DEGRADED_MS
from .device_config import router as config_router

log = logging.getLogger("api")
MQTT_HOST = os.environ.get("MQTT_HOST", "mqtt")
MQTT_PORT = int(os.environ.get("MQTT_PORT", "1883"))


@asynccontextmanager
async def lifespan(_app: FastAPI):
    applied = db.migrate()
    log.warning("migrations applied: %s", applied or "none (schema up to date)")
    if n := auth.take_over_env_keys():
        log.warning("operator keys taken over from OPERATOR_API_KEYS: %d (all rights)", n)
    yield


# The name of this server edition (the only place it appears in the code): the
# contracts — device protocol, registry, rules, schema, /v1 — are the same in
# every edition built on them.
EDITION = "imatra"

app = FastAPI(title="Imatra — cloud ISO 15118 server, one deployment", lifespan=lifespan)
app.include_router(v1_router)
app.include_router(v1_read_router)
app.include_router(activations_router)
app.include_router(events_router)
app.include_router(config_router)


def _check_db() -> str:
    try:
        with db.connect() as conn:
            conn.execute("SELECT 1")
        return "ok"
    except Exception as e:  # reported, not raised: /health must answer
        return f"error: {type(e).__name__}"


def _check_mqtt() -> str:
    try:
        with socket.create_connection((MQTT_HOST, MQTT_PORT), timeout=2):
            return "ok"
    except OSError as e:
        return f"error: {type(e).__name__}"


def _events_lag() -> int | None:
    try:
        with db.connect() as conn:
            return tables_lag_ms(conn)  # engines -> feed -> operator tables
    except Exception:  # table not there yet, database down: reported by "db"
        return None


@app.get("/health")
def health():
    checks = {"db": _check_db(), "mqtt": _check_mqtt()}
    ok = all(v == "ok" for v in checks.values())
    lag = _events_lag()
    # Events trailing by more than 5 s: data still served, but stale.
    # 200 with "degraded": the service works, the operator sees the lag.
    status = "ok" if ok else "error"
    if ok and lag is not None and lag > LAG_DEGRADED_MS:
        status = "degraded"
    return JSONResponse({"status": status, "edition": EDITION, **checks, "events_lag_ms": lag},
                        status_code=200 if ok else 503)


# The published operator contract: /v1 only.
_V1_DESC = """Operator API of the cloud ISO 15118 server for the devices of the architecture.

Header `X-API-Key`; rights per key: `read`, `control`, `debug`. Device registry data (binding, type,
coarse location, business rules) comes from the service layer through the broker, not over HTTP.
Device settings: `GET|PUT /v1/devices/{id}/config` (fields: `GET /v1/config/fields`).
Reading: devices, plug-in sessions and the event feed come from tables filled
asynchronously from the engines; every answer has `X-Data-Lag-Ms`, every row
its `as_of`. Lists page by `cursor` = previous `next_cursor` until it is null.
Commands are asynchronous (202 + `command_id`); the device confirms them and
the feed carries `limit.applied`. Event schema: `GET /v1/events/schema`."""


@app.get("/v1/openapi.json", include_in_schema=False)
def v1_openapi():
    from fastapi.openapi.utils import get_openapi
    if not getattr(app.state, "v1_openapi", None):
        # From the whole app, then /v1 paths only: since FastAPI 0.16x included
        # routers sit in app.routes as lazy wrappers without a path, so
        # filtering the routes first gave an empty document.
        doc = get_openapi(title="Imatra — operator API", version="1.0.0",
                          openapi_version="3.1.0", description=_V1_DESC, routes=app.routes)
        doc["paths"] = {p: v for p, v in doc.get("paths", {}).items() if p.startswith("/v1/")}
        if not doc["paths"]:
            raise RuntimeError("operator API contract without /v1 paths")
        doc["info"]["license"] = {"name": "Apache 2.0", "identifier": "Apache-2.0"}
        doc["servers"] = [{"url": "/", "description": "the deployment's gateway (SITE_ADDRESS)"}]
        doc.setdefault("components", {})["securitySchemes"] = {"apiKey": {
            "type": "apiKey", "in": "header", "name": "X-API-Key",
            "description": "Operator key with rights read, control and/or debug (python -m app.keys create)"}}
        doc["security"] = [{"apiKey": []}]
        app.state.v1_openapi = doc
    return app.state.v1_openapi
