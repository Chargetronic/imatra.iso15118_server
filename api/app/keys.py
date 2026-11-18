# SPDX-License-Identifier: Apache-2.0
# Copyright Contributors to the project. See NOTICE.

"""Manage operator API keys. In the api container:

    python -m app.keys create NAME --rights read,control   prints the key once
    python -m app.keys list
    python -m app.keys revoke NAME

Rights: read (devices, sessions, feed), control (limits), debug (twin
journal).
"""
import argparse
import sys

from . import auth, db


def main(argv=None) -> int:
    p = argparse.ArgumentParser(prog="python -m app.keys")
    sub = p.add_subparsers(dest="cmd", required=True)
    c = sub.add_parser("create")
    c.add_argument("name")
    c.add_argument("--rights", default="read", help="comma-separated: " + ",".join(auth.RIGHTS))
    sub.add_parser("list")
    r = sub.add_parser("revoke")
    r.add_argument("name")
    a = p.parse_args(argv)
    db.migrate()
    with db.connect() as conn:
        if a.cmd == "create":
            try:
                key = auth.create(conn, a.name, [x.strip() for x in a.rights.split(",") if x.strip()])
            except ValueError as e:
                print(e, file=sys.stderr)
                return 2
            auth.audit("cli", "key.create", 201, request={"name": a.name, "rights": a.rights})
            print("Store it now: only its hash is kept.", file=sys.stderr, flush=True)
            print(key, flush=True)
        elif a.cmd == "list":
            for name, prefix, rights, source, created, revoked, used in conn.execute(
                    "SELECT name, prefix, rights, source, created_at, revoked_at, last_used_at "
                    "FROM api_keys ORDER BY name"):
                state = f"revoked {revoked:%Y-%m-%d}" if revoked else "active"
                print(f"{name:20} {prefix}…  {','.join(rights):28} {source:4} {state:18} "
                      f"last used {used:%Y-%m-%d %H:%M}" if used else
                      f"{name:20} {prefix}…  {','.join(rights):28} {source:4} {state:18} never used")
        else:
            n = conn.execute("UPDATE api_keys SET revoked_at = now() WHERE name = %s AND revoked_at IS NULL",
                             (a.name,)).rowcount
            if not n:
                print(f"no active key named {a.name}", file=sys.stderr)
                return 1
            auth.audit("cli", "key.revoke", 200, request={"name": a.name})
            print(f"revoked {a.name} (API processes stop accepting it within {int(auth.KEY_CACHE_S)} s)")
    return 0


if __name__ == "__main__":
    try:
        rc = main()
    finally:
        db.close()
    sys.exit(rc)
