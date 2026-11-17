#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
# Copyright Contributors to the project. See NOTICE.

"""Export the operator API contract (/v1) to spec/openapi-operator.json.

The file in the repository is the published contract; the running API serves
the same document at /v1/openapi.json. The edition name stays in packaging
only (scripts/check-names.sh), so the exported title is functional.

    python3 scripts/export-openapi.py           write the file
    python3 scripts/export-openapi.py --check   exit 1 if the file is out of date (CI)

Needs the API's requirements (api/requirements.txt); no database.
"""
import json
import pathlib
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
OUT = ROOT / "spec" / "openapi-operator.json"
sys.path.insert(0, str(ROOT / "api"))

from app.main import v1_openapi  # noqa: E402

doc = json.loads(json.dumps(v1_openapi()))  # a copy: the app caches its own
doc["info"]["title"] = "Cloud ISO 15118 server: operator API"
text = json.dumps(doc, indent=2, ensure_ascii=False, sort_keys=False) + "\n"

if "--check" in sys.argv:
    if not OUT.exists() or OUT.read_text(encoding="utf-8") != text:
        print(f"{OUT.relative_to(ROOT)} is out of date: run scripts/export-openapi.py", file=sys.stderr)
        sys.exit(1)
    print("openapi: ok")
else:
    OUT.write_text(text, encoding="utf-8")
    print(f"wrote {OUT.relative_to(ROOT)}")
