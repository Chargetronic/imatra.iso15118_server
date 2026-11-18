# SPDX-License-Identifier: Apache-2.0
# Copyright Contributors to the project. See NOTICE.

"""The operator API against a real Postgres: keys and rights, the audit log,
commands and activations, the event feed by cursor.

Needs an empty database the tests may fill (the migrations are applied at the
start): the API's settings POSTGRES_HOST, POSTGRES_DB, POSTGRES_USER,
POSTGRES_PASSWORD (and PGPORT if not 5432). Without them the tests are skipped.

    POSTGRES_HOST=localhost POSTGRES_DB=imatra_test POSTGRES_USER=imatra \\
    POSTGRES_PASSWORD=... python -m pytest -q tests      # from api/
"""
import json
import os
import uuid
from datetime import datetime, timezone

import pytest

if not os.environ.get("POSTGRES_DB"):
    pytest.skip("no test database (POSTGRES_DB)", allow_module_level=True)

ADMIN = "t-admin-" + "k" * 24
os.environ["OPERATOR_API_KEYS"] = f"admin:{ADMIN}"

from fastapi.testclient import TestClient  # noqa: E402

from app import auth, db, main  # noqa: E402

DEV = "dev-test-01"


@pytest.fixture(scope="module")
def client():
    with TestClient(main.app) as c:  # the lifespan applies the migrations, takes over the env key
        with db.connect() as conn:
            conn.execute("INSERT INTO devices (device_id) VALUES (%s) ON CONFLICT DO NOTHING", (DEV,))
        yield c


def h(key):
    return {"X-API-Key": key}


@pytest.fixture(scope="module")
def keys(client):
    """A key per set of rights, made like app.keys does."""
    out = {}
    with db.connect() as conn:
        for name, rights in (("t-read", ["read"]), ("t-control", ["read", "control"])):
            out[name] = auth.create(conn, f"{name}-{uuid.uuid4().hex[:6]}", rights)
    return out


def names_of(key):
    with db.connect() as conn:
        return conn.execute("SELECT name FROM api_keys WHERE hash = %s", (auth._hash(key),)).fetchone()[0]


# --- keys and rights ---

def test_no_key_and_unknown_key_401(client):
    assert client.get("/v1/devices").status_code == 401
    assert client.get("/v1/devices", headers=h("ct_" + "x" * 40)).status_code == 401


def test_env_key_taken_over_with_all_rights(client):
    assert client.get("/v1/devices", headers=h(ADMIN)).status_code == 200
    with db.connect() as conn:
        src, rights = conn.execute("SELECT source, rights FROM api_keys WHERE name = 'admin'").fetchone()
    assert src == "env" and set(rights) == set(auth.RIGHTS)


def test_keys_stored_as_hash_only(client, keys):
    with db.connect() as conn:
        rows = conn.execute("SELECT prefix, hash FROM api_keys").fetchall()
    for key in [ADMIN, *keys.values()]:
        assert any(r[1] == auth._hash(key) for r in rows)
        assert all(len(r[0]) <= 8 for r in rows)  # only the prefix is kept in clear


def test_read_key_reads_but_cannot_command(client, keys):
    assert client.get("/v1/devices", headers=h(keys["t-read"])).status_code == 200
    r = client.post(f"/v1/devices/{DEV}/limit", headers=h(keys["t-read"]), json={"limit_a": 10, "duration_s": 60})
    assert r.status_code == 403


def test_revoked_key_refused(client):
    with db.connect() as conn:
        key = auth.create(conn, f"t-rev-{uuid.uuid4().hex[:6]}", ["read"])
    assert client.get("/v1/devices", headers=h(key)).status_code == 200
    with db.connect() as conn:
        conn.execute("UPDATE api_keys SET revoked_at = now() WHERE hash = %s", (auth._hash(key),))
    auth._cache.clear()  # the process cache holds a key for KEY_CACHE_S
    assert client.get("/v1/devices", headers=h(key)).status_code == 401


def test_env_key_removed_from_env_is_revoked(client):
    extra = "t-env2-" + "e" * 20
    os.environ["OPERATOR_API_KEYS"] = f"admin:{ADMIN} env2:{extra}"
    auth.take_over_env_keys()
    os.environ["OPERATOR_API_KEYS"] = f"admin:{ADMIN}"
    auth.take_over_env_keys()
    auth._cache.clear()
    assert client.get("/v1/devices", headers=h(extra)).status_code == 401
    assert client.get("/v1/devices", headers=h(ADMIN)).status_code == 200


# --- commands and the audit log ---

def audit_rows(action, device=None):
    with db.connect() as conn:
        return conn.execute("SELECT key_name, status FROM audit_log WHERE action = %s "
                            "AND (%s::text IS NULL OR device_id = %s) ORDER BY id", (action, device, device)).fetchall()


def test_limit_accepted_stored_and_audited(client, keys):
    key = keys["t-control"]
    r = client.post(f"/v1/devices/{DEV}/limit", headers=h(key), json={"limit_a": 10, "duration_s": 120})
    assert r.status_code == 202, r.text
    cid = r.json()["command_id"]
    with db.connect() as conn:
        row = conn.execute("SELECT device_id, limit_a, source FROM limits WHERE command_id = %s", (cid,)).fetchone()
    assert row[0] == DEV and float(row[1]) == 10 and row[2] == names_of(key)
    assert (names_of(key), 202) in audit_rows("limit.set", DEV)
    d = client.get(f"/v1/devices/{DEV}", headers=h(key)).json()
    assert d["limit"]["limit_a"] == 10 and d["limit"]["command_id"] == cid


def test_limit_refusals_audited(client, keys):
    key = keys["t-control"]
    assert client.post(f"/v1/devices/{DEV}/limit", headers=h(key),
                       json={"limit_a": 3, "duration_s": 60}).status_code == 422   # below 6 A
    assert client.post(f"/v1/devices/{DEV}/limit", headers=h(key),
                       json={"limit_a": 10, "duration_s": 0}).status_code == 422
    assert client.post("/v1/devices/no-such-device/limit", headers=h(key),
                       json={"limit_a": 10, "duration_s": 60}).status_code == 404
    statuses = [s for _, s in audit_rows("limit.set")]
    assert 422 in statuses and 404 in statuses


def test_new_limit_replaces_the_previous_and_cancel(client, keys):
    key = keys["t-control"]
    a = client.post(f"/v1/devices/{DEV}/limit", headers=h(key), json={"limit_a": 12, "duration_s": 120}).json()
    b = client.post(f"/v1/devices/{DEV}/limit", headers=h(key), json={"limit_a": 14, "duration_s": 120}).json()
    with db.connect() as conn:
        live = conn.execute("SELECT command_id FROM limits WHERE device_id = %s AND cancelled_at IS NULL "
                            "AND expires_at > now() AND activation_id IS NULL", (DEV,)).fetchall()
    assert [r[0] for r in live] == [b["command_id"]] and a["command_id"] != b["command_id"]
    r = client.delete(f"/v1/devices/{DEV}/limit", headers=h(key))
    assert r.status_code == 202 and r.json()["cancelled"] == 1


def test_block_and_unblock_audited(client, keys):
    key = keys["t-control"]
    assert client.put(f"/v1/devices/{DEV}/block", headers=h(key), json={"reason": "test"}).status_code == 200
    assert client.get(f"/v1/devices/{DEV}", headers=h(key)).json()["blocked"] is not None
    assert client.delete(f"/v1/devices/{DEV}/block", headers=h(key)).status_code == 200
    assert client.get(f"/v1/devices/{DEV}", headers=h(key)).json()["blocked"] is None
    assert (names_of(key), 200) in audit_rows("device.block", DEV)


# --- activations ---

ACT = {"selection": {"devices": [DEV], "online": False}, "level": {"current_a": 8},
       "stages": [{"count": 1}], "duration_s": 300}


def test_activation_create_repeat_conflict_cancel(client, keys):
    key = keys["t-control"]
    aid = "t-act-" + uuid.uuid4().hex[:8]
    r = client.post("/v1/activations", headers=h(key), json={"id": aid, **ACT})
    assert r.status_code == 201, r.text
    # the same id and body: safe, 200 with the same activation
    r2 = client.post("/v1/activations", headers=h(key), json={"id": aid, **ACT})
    assert r2.status_code == 200 and r2.json()["id"] == aid
    # the same id, another body: 409
    other = {**ACT, "level": {"current_a": 10}}
    assert client.post("/v1/activations", headers=h(key), json={"id": aid, **other}).status_code == 409
    assert client.get(f"/v1/activations/{aid}", headers=h(keys["t-read"])).status_code == 200
    assert client.post("/v1/activations", headers=h(keys["t-read"]), json=ACT).status_code == 403
    assert client.delete(f"/v1/activations/{aid}", headers=h(key)).status_code == 200
    with db.connect() as conn:
        state = conn.execute("SELECT state FROM activations WHERE activation_id = %s", (aid,)).fetchone()[0]
    assert state == "cancelled"
    statuses = [s for _, s in audit_rows("activation.create")]
    assert 201 in statuses and 409 in statuses


def test_activation_bad_id_and_body(client, keys):
    key = keys["t-control"]
    assert client.post("/v1/activations", headers=h(key), json={"id": "bad id!", **ACT}).status_code == 422
    assert client.post("/v1/activations", headers=h(key), json={**ACT, "duration_s": 10}).status_code == 422
    assert client.post("/v1/activations", headers=h(key), json={**ACT, "stages": []}).status_code == 422


# --- event feed by cursor ---

def add_events(n, typ="session.started", device=DEV):
    with db.connect() as conn:
        with conn.transaction():
            for _ in range(n):
                conn.execute("INSERT INTO domain_events (id, type, device_id, time, data) VALUES (%s, %s, %s, %s, %s)",
                             (uuid.uuid4().hex, typ, device, datetime.now(timezone.utc), json.dumps({"t": 1})))


def test_feed_by_cursor_every_event_once(client, keys):
    key = keys["t-read"]
    start = client.get("/v1/events", headers=h(key), params={"cursor": 0, "limit": 1000}).json()
    cur = start["next_cursor"]
    while True:  # to the end of what is there
        page = client.get("/v1/events", headers=h(key), params={"cursor": cur, "limit": 1000}).json()
        if not page["events"]:
            break
        cur = page["next_cursor"]
    add_events(25)
    got, c = [], cur
    while True:
        page = client.get("/v1/events", headers=h(key), params={"cursor": c, "limit": 10}).json()
        if not page["events"]:
            assert page["next_cursor"] == c  # an empty page keeps the cursor
            break
        got += [e["seq"] for e in page["events"]]
        c = page["next_cursor"]
    assert len(got) == 25 and got == sorted(got) and len(set(got)) == 25


def test_feed_filters(client, keys):
    key = keys["t-read"]
    cur = client.get("/v1/events", headers=h(key), params={"cursor": 0, "limit": 1}).json()
    add_events(2, "limit.confirmed")
    add_events(3, "session.ended", device="dev-test-02")
    r = client.get("/v1/events", headers=h(key), params={"types": "session.*", "device": "dev-test-02"}).json()
    assert r["events"] and all(e["type"].startswith("session.") and e["device_id"] == "dev-test-02" for e in r["events"])
    r = client.get("/v1/events", headers=h(key), params={"types": "limit.confirmed"}).json()
    assert r["events"] and all(e["type"] == "limit.confirmed" for e in r["events"])
    assert cur is not None
    assert client.get("/v1/events", params={"cursor": 0}).status_code == 401
