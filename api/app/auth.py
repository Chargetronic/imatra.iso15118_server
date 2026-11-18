# SPDX-License-Identifier: Apache-2.0
# Copyright Contributors to the project. See NOTICE.

"""Operator API keys: header X-API-Key, rights per key, audit.

    need("read")   -> dependency: the key's name, or 401 / 403
    audit(...)     -> one row in audit_log

Keys live in api_keys as SHA-256. A key found is cached in
the process for KEY_CACHE_S (30 s), so a revoked key stops working within
that time; last_used_at is written at most once a minute per key.
Keys in OPERATOR_API_KEYS (.env, "name:key ...") are taken over at start with
all rights (a first key for a new installation); a key taken over this way and
no longer in OPERATOR_API_KEYS is revoked at the next start.
"""
import hashlib
import json
import logging
import os
import secrets
import time

from fastapi import Header, HTTPException

from . import db

log = logging.getLogger("auth")
RIGHTS = ("read", "control", "debug")
KEY_CACHE_S = 30.0
_cache: dict[bytes, tuple[float, str | None, tuple[str, ...]]] = {}  # hash -> (until, name, rights)
_used: dict[str, float] = {}


def _hash(key: str) -> bytes:
    return hashlib.sha256(key.encode()).digest()


def new_key() -> str:
    return "ct_" + secrets.token_urlsafe(32)


def create(conn, name: str, rights: list[str], source: str = "cli", key: str | None = None) -> str:
    bad = [r for r in rights if r not in RIGHTS]
    if bad or not rights:
        raise ValueError(f"rights must be some of {', '.join(RIGHTS)}")
    key = key or new_key()
    conn.execute("INSERT INTO api_keys (name, prefix, hash, rights, source) VALUES (%s, %s, %s, %s, %s)",
                 (name, key[:8], _hash(key), rights, source))
    return key


def take_over_env_keys() -> int:
    """OPERATOR_API_KEYS from .env -> api_keys (all rights), once per key; the
    keys taken over earlier and no longer listed there are revoked."""
    n = 0
    with db.connect() as conn:
        listed = []
        for pair in os.environ.get("OPERATOR_API_KEYS", "").split():
            name, _, key = pair.partition(":")
            if not name or len(key) < 16:
                continue
            listed.append(_hash(key))
            # Several API processes start at once: whichever inserts first wins.
            r = conn.execute("INSERT INTO api_keys (name, prefix, hash, rights, source) "
                             "VALUES (%s, %s, %s, %s, 'env') ON CONFLICT DO NOTHING",
                             (name, key[:8], _hash(key), list(RIGHTS)))
            if r.rowcount:
                n += 1
            elif not conn.execute("SELECT 1 FROM api_keys WHERE hash = %s", (_hash(key),)).fetchone():
                log.warning("OPERATOR_API_KEYS: a key named %s exists already with another value; skipped", name)
        for (name,) in conn.execute("UPDATE api_keys SET revoked_at = now() WHERE source = 'env' "
                                    "AND revoked_at IS NULL AND NOT (hash = ANY(%s::bytea[])) RETURNING name", (listed,)):
            log.warning("operator key %s revoked: no longer in OPERATOR_API_KEYS", name)
    return n


def _lookup(key: str) -> tuple[str | None, tuple[str, ...]]:
    h = _hash(key)
    now = time.monotonic()
    hit = _cache.get(h)
    if hit and hit[0] > now:
        return hit[1], hit[2]
    with db.connect() as conn:
        r = conn.execute("SELECT name, rights FROM api_keys WHERE hash = %s AND revoked_at IS NULL", (h,)).fetchone()
        name, rights = (r[0], tuple(r[1])) if r else (None, ())
        if name and now - _used.get(name, 0) > 60:
            _used[name] = now
            conn.execute("UPDATE api_keys SET last_used_at = now() WHERE name = %s", (name,))
    _cache[h] = (now + KEY_CACHE_S, name, rights)
    return name, rights


def need(right: str):
    """FastAPI dependency: a valid key with this right; returns the key's name."""
    def dep(x_api_key: str = Header(default="", description="operator API key")) -> str:
        if not x_api_key:
            raise HTTPException(401, "missing X-API-Key")
        name, rights = _lookup(x_api_key)
        if not name:
            raise HTTPException(401, "invalid or revoked X-API-Key")
        if right not in rights:
            raise HTTPException(403, f"this key has no '{right}' right")
        return name
    dep.__name__ = f"need_{right}"
    return dep


def audit(key_name: str, action: str, status: int, device_id: str | None = None,
          command_id: int | None = None, request: dict | None = None) -> None:
    try:
        with db.connect() as conn:
            conn.execute("INSERT INTO audit_log (key_name, action, device_id, command_id, request, status) "
                         "VALUES (%s, %s, %s, %s, %s, %s)",
                         (key_name, action, device_id, command_id, json.dumps(request) if request else None, status))
    except Exception:  # the command itself is done; the audit gap shows in the log
        log.exception("audit %s %s %s", key_name, action, device_id)
