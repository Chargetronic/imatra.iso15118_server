# Third-party components

Imatra is licensed under the Apache License 2.0. It uses the components below;
each keeps its own licence. Versions: `engine/go.mod`, `api/requirements.txt`, the
Dockerfiles and `docker-compose.yml`.

## Linked into the binaries

| Component | Used by | Licence |
|---|---|---|
| [libcbv2g](https://github.com/EVerest/libcbv2g) (EVerest, LF Energy) | engine: EXI codec of ISO 15118-2 / DIN 70121 (static, built in `engine/Dockerfile`) | Apache-2.0 |
| github.com/eclipse/paho.mqtt.golang | engine, fleetgen | EPL-2.0 or EDL-1.0 (BSD-3-Clause); used under EDL-1.0 |
| github.com/redis/go-redis/v9 | engine, relay | BSD-2-Clause |
| github.com/lib/pq | engine, relay, projector | MIT |
| github.com/cespare/xxhash/v2, go.uber.org/atomic | go-redis | MIT |
| github.com/gorilla/websocket | paho | BSD-2-Clause |
| golang.org/x/net, golang.org/x/sync, golang.org/x/sys | dependencies | BSD-3-Clause |

## Python packages of the API

| Component | Licence |
|---|---|
| FastAPI (with Starlette, Pydantic) | MIT (Starlette: BSD-3-Clause) |
| Uvicorn | BSD-3-Clause |
| psycopg 3, psycopg-pool | LGPL-3.0 — used unmodified as a library, installed from PyPI |
| Their dependencies: anyio, h11, annotated-types, annotated-doc, pydantic-core, typing-inspection; click, idna; typing-extensions; opentelemetry-api | MIT; BSD-3-Clause; PSF-2.0; Apache-2.0 |

## Run as separate containers (not distributed in this repository)

| Component | Image | Licence |
|---|---|---|
| Eclipse Mosquitto | `eclipse-mosquitto:2` | EPL-2.0 or EDL-1.0 |
| Valkey | `valkey/valkey:8-alpine` | BSD-3-Clause |
| PostgreSQL | `postgres:16-alpine` | PostgreSQL Licence |
| Caddy, built with the module [caddy-ratelimit](https://github.com/mholt/caddy-ratelimit) (`gateway/Dockerfile`) | `caddy:2.11-builder` → `caddy:2.11-alpine` | Apache-2.0 (both) |
| Base images | `debian:bookworm-slim`, `python:3.12-slim`, `alpine:3.24`, `golang:1.27-bookworm` (build only) | their distributions' licences |

No component is under a licence incompatible with Apache-2.0 for this use;
all are OSI-approved (ADR-001). Checked by CI on every change
(`.github/workflows/security.yml`: `go-licenses check`, `pip-licenses`).
