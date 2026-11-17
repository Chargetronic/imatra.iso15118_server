# SPDX-License-Identifier: Apache-2.0
# Copyright Contributors to the project. See NOTICE.

"""Check of the feed cursor under concurrent writers.

Writers insert events into domain_events in parallel transactions that commit
late (as an engine on Postgres does: the event and the session state in one
transaction). seq is given at insert; every writer holds the feed lock
(pg_advisory_xact_lock(15118), as store.feedLock) from its insert to its
commit, so seq follows commit order and the cursor never passes an event
still to commit, however slow the commit. The writers here serialize on the
lock, so keep the commit delays and counts small.

Usage (against a running API and its database; env as for the API):
    python tests/feed_cursor_check.py --writers 16 --events 400 --max-commit-ms 100
    python tests/feed_cursor_check.py --writers 8 --events 32 --max-commit-ms 2500
Exit 0: the reader got every event exactly once.
"""
import argparse
import json
import os
import random
import threading
import time
import urllib.request
import uuid

import psycopg


def dsn() -> str:
    return (f"host={os.environ.get('POSTGRES_HOST', '127.0.0.1')} dbname={os.environ['POSTGRES_DB']} "
            f"user={os.environ['POSTGRES_USER']} password={os.environ['POSTGRES_PASSWORD']}")


def main() -> int:
    p = argparse.ArgumentParser()
    p.add_argument("--api", default="http://127.0.0.1:8100")
    p.add_argument("--key", default=os.environ.get("CHECK_API_KEY", ""))
    p.add_argument("--writers", type=int, default=16)
    p.add_argument("--events", type=int, default=3000)
    p.add_argument("--max-commit-ms", type=int, default=800)
    a = p.parse_args()
    run = uuid.uuid4().hex[:8]
    device = f"feedcheck-{run}"
    written: set[str] = set()
    lock = threading.Lock()
    with psycopg.connect(dsn(), autocommit=True) as c:
        start = c.execute("SELECT COALESCE(max(seq), 0) FROM domain_events").fetchone()[0]

    def writer(n: int) -> None:
        with psycopg.connect(dsn()) as c:
            for _ in range(n):
                eid = f"check:{run}:{uuid.uuid4().hex}"
                with c.transaction():
                    c.execute("SELECT 1")  # the transaction's own work comes first
                    c.execute("SELECT pg_advisory_xact_lock(15118)")
                    c.execute("INSERT INTO domain_events (id, type, device_id, time, data) "
                              "VALUES (%s, 'cp.changed', %s, now(), '{}')", (eid, device))
                    time.sleep(random.uniform(0, a.max_commit_ms / 1000))  # a slow commit
                with lock:
                    written.add(eid)

    per = a.events // a.writers
    threads = [threading.Thread(target=writer, args=(per,)) for _ in range(a.writers)]
    for t in threads:
        t.start()
    got: list[str] = []
    cursor = start
    deadline = None
    while True:
        req = urllib.request.Request(f"{a.api}/v1/events?cursor={cursor}&limit=1000&device={device}",
                                     headers={"X-API-Key": a.key})
        d = json.load(urllib.request.urlopen(req))
        got += [e["id"] for e in d["events"]]
        cursor = d["next_cursor"]
        if all(not t.is_alive() for t in threads):
            deadline = deadline or time.monotonic() + 3  # settle time after the last commit
            if time.monotonic() > deadline and not d["events"]:
                break
        time.sleep(0.1)
    missing = written - set(got)
    dupes = len(got) - len(set(got))
    print(f"written {len(written)}, read {len(got)}, missing {len(missing)}, duplicates {dupes}")
    with psycopg.connect(dsn(), autocommit=True) as c:
        c.execute("DELETE FROM domain_events WHERE device_id = %s", (device,))
    return 0 if not missing and not dupes and len(written) == per * a.writers else 1


if __name__ == "__main__":
    raise SystemExit(main())
