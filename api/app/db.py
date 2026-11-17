# SPDX-License-Identifier: Apache-2.0
# Copyright Contributors to the project. See NOTICE.

"""Database connection and migrations."""
import os
import pathlib

import psycopg
from psycopg_pool import ConnectionPool

MIGRATIONS = pathlib.Path(__file__).resolve().parent.parent / "migrations"


def dsn() -> str:
    return (
        f"host={os.environ.get('POSTGRES_HOST', 'postgres')} "
        f"dbname={os.environ['POSTGRES_DB']} "
        f"user={os.environ['POSTGRES_USER']} "
        f"password={os.environ['POSTGRES_PASSWORD']} "
        "connect_timeout=5"
    )


_pool = None


def connect():
    """A pooled connection: `with db.connect() as conn:` returns it to the pool.

    One pool per API process, API_DB_POOL connections at most (default 10).
    Before 08.11.2026 every request opened its own connection; under a burst
    of operator calls (activation stage, load test) that ran Postgres out of
    connections next to the engines' pools.
    """
    global _pool
    if _pool is None:
        # check: a connection is tested when taken from the pool, so a
        # Postgres restart gives no errors to the callers (found 09.11.2026:
        # 18 of 100 operator calls failed with stale connections after one).
        _pool = ConnectionPool(dsn(), min_size=1, max_size=int(os.environ.get("API_DB_POOL", "10")),
                               kwargs={"autocommit": True}, open=True,
                               check=ConnectionPool.check_connection)
    return _pool.connection()


def migrate() -> list[str]:
    """Apply every migrations/NNN_*.sql not applied yet, in order.

    Safe to run on every start and from several processes: an advisory lock
    serialises concurrent runs, and each file is applied in its own
    transaction together with its bookkeeping row.
    """
    applied_now: list[str] = []
    with connect() as conn:
        conn.execute("SELECT pg_advisory_lock(15118)")
        try:
            conn.execute(
                "CREATE TABLE IF NOT EXISTS schema_migrations ("
                " name text PRIMARY KEY,"
                " applied_at timestamptz NOT NULL DEFAULT now())"
            )
            done = {r[0] for r in conn.execute("SELECT name FROM schema_migrations")}
            for path in sorted(MIGRATIONS.glob("*.sql")):
                if path.name in done:
                    continue
                with conn.transaction():
                    conn.execute(path.read_text())
                    conn.execute(
                        "INSERT INTO schema_migrations (name) VALUES (%s)", (path.name,)
                    )
                applied_now.append(path.name)
        finally:
            conn.execute("SELECT pg_advisory_unlock(15118)")
    return applied_now


def close() -> None:
    """Close the pool (command-line tools, before exit)."""
    global _pool
    if _pool is not None:
        _pool.close()
        _pool = None
