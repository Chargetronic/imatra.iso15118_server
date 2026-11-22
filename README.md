# Imatra

[![CI](https://github.com/chargetronic/imatra.iso15118_server/actions/workflows/ci.yml/badge.svg)](https://github.com/chargetronic/imatra.iso15118_server/actions/workflows/ci.yml)
[![License: Apache-2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
<!-- OpenSSF Best Practices badge: added once the project is registered at bestpractices.dev -->

Imatra is a cloud-side ISO 15118 server for residential AC charging. It runs
the vehicle protocol (ISO 15118-2 AC, DIN 70121 state-of-charge read-out, PWM
control) in the cloud for many charging devices at once, so that a charger
needs only a PLC modem and a small controller. Operators read what each car
needs — energy, departure time, state of charge — and shift or limit charging
through an open API.

**Status: version 0.1.0, released 18 November 2026**
([release notes](https://github.com/chargetronic/imatra.iso15118_server/releases/tag/v0.1.0),
[CHANGELOG.md](CHANGELOG.md), [ROADMAP.md](ROADMAP.md)).

## How it works

The device at the charger is a thin proxy: it tunnels the EV's messages over
MQTT and answers the repetitive Charging Loop from a cache the server gives
it, so the server is not on the 2-second timing path of every loop message.
One installation is one **deployment**: one operator, one MQTT broker, one
Valkey, one Postgres, one or more engine instances, the operator API (`/v1`)
and an event feed. Everything a deployment needs is in this repository and
works without any proprietary module.

| Part | What it does |
|---|---|
| engine (`engine/cmd/engine`) | ISO 15118-2 AC / DIN 70121 / PWM sessions, the Charging Loop cache pairs, command confirmation, the activation executor; several instances share the devices |
| relay (`engine/cmd/relay`) | event stream (Valkey) → `domain_events` (Postgres), at least once, no duplicates |
| projector (`engine/cmd/projector`) | operator tables from the feed (devices, plug-in sessions, attributes); can rebuild them from scratch |
| API (`api/`) | `/v1`: reading, limits, activations, device settings, local block; keys with rights, audit; OpenAPI 3.1 |
| broker, PKI, gateway (`mqtt/`, `pki/`, `gateway/`) | Mosquitto with device admission by client certificate (CN = device id); the deployment's CA or the one of a provisioning service; Caddy in front of the API |
| fleetgen (`engine/cmd/fleetgen`) | virtual fleet for load tests (ISO 15118-2, DIN, PWM devices with certificates) |

Architecture, limits and assumptions: [docs/architecture.md](docs/architecture.md).

## Quick start

Requirements: a Linux host with Docker Engine and the Compose plugin; for a
public deployment, a DNS name pointing to it and ports 80, 443 and 8883 open.
The full guide: [docs/deployment.md](docs/deployment.md).

```sh
cp .env.example .env        # fill in the passwords, BROKER_DNS, SITE_ADDRESS, an operator key
docker compose up -d --build                      # one engine
docker compose up -d --build --scale engine=2     # reference: 5 000 devices on 2 vCPU
docker compose run --rm -T pki-init device dev-01 > dev-01.pem   # a device certificate (own CA)
curl -s https://$SITE_ADDRESS/health              # {"status":"ok", ...}
```

The gateway serves `/v1` and `/health` over HTTPS for `SITE_ADDRESS` (its own
Let's Encrypt certificate, ports 80/443), with a rate limit per operator key.
Behind a TLS proxy of your own: add `-f compose.behind-proxy.yml`. To try the
API, read `GET /v1/openapi.json` with the operator key from `.env` in the
header `X-API-Key` (`curl -H "X-API-Key: $KEY" https://<SITE_ADDRESS>/v1/openapi.json`); the device
protocol is in `engine/internal/tunnel`.

## Documentation

| Topic | Where |
|---|---|
| Install, run, update, back up, move | [docs/deployment.md](docs/deployment.md) |
| The operator's responsibilities | [docs/operator.md](docs/operator.md) |
| Purpose, design principles, boundaries | [docs/concept.md](docs/concept.md) |
| Architecture, limits, assumptions | [docs/architecture.md](docs/architecture.md) |
| C4 diagrams (draw.io) and data flows | [docs/architecture/](docs/architecture/README.md) |
| Architecture decisions | [docs/adr/](docs/adr/) |
| Device registry and change of operator | [docs/registry.md](docs/registry.md) |
| Business rules of devices | [docs/rules.md](docs/rules.md) |
| Metrics and probes | [docs/metrics.md](docs/metrics.md) |
| Operator API | `GET /v1/openapi.json`; event feed schema `api/schemas/events.v1.json` |
| Connecting external systems | [adapters/README.md](adapters/README.md) |

## Devices and their rules

Any device of the architecture connects the same way: the device protocol on
`dev/<id>/…` (`engine/internal/tunnel`), admission by its certificate. What a
device is and what it allows comes from the service layer's registry as an
open set of **business rules** ([docs/rules.md](docs/rules.md)); the server's
behaviour depends on the rules, never on a device type or a product.

## This edition and what we commit to

Imatra is the open base edition of a server line: one complete deployment for
one operator, with nothing held back inside it. Larger editions — redundancy
of single components, several deployments under one management, automatic
scaling, streaming delivery, market adapters, support with SLAs — are
commercial products of Chargetronic and are not published here.

For this repository the maintainers commit to:

* fixes of bugs and security vulnerabilities ([SECURITY.md](SECURITY.md));
* stable contracts: the device protocol, the registry and its rules, `/v1`
  and the event feed stay compatible within a major version, so a deployment
  can move to a larger edition, or back, without changes to devices or to
  the operator's systems;
* published capacity figures with the conditions they were measured under
  ([docs/architecture.md](docs/architecture.md)), and the kit to repeat the
  measurement (`fleetgen`, `loadtest/`).

There is no commitment to add new functions to this edition. Apache 2.0 lets
anyone build on it, including commercially.

## Names

The **contracts** — device protocol, registry
and rules, database schema, `/v1`, event types, settings, metrics — use
functional names only and are the same in every edition: a device or the
service layer does not know which edition it talks to. The edition's name
appears only in packaging: this README, the API title, `edition` in
`/health`, image and project names. `scripts/check-names.sh` (CI) keeps it so.

## Layout

```
engine/     Go module: cmd/ (engine, relay, projector, fleetgen), internal/ (secc state machine,
            exi codec on libcbv2g, store, events, tunnel, metrics)
api/        operator API (Python): app/, migrations/, schemas/ (the event feed's JSON Schema);
            the OpenAPI document is served at /v1/openapi.json
mqtt/       broker of the deployment (Mosquitto: mTLS for devices, ACL, local block)
pki/        the deployment's CA or the files from the service layer (pki-init.sh)
gateway/    Caddy: /v1 and /health
scripts/    move a deployment to another host (export.sh, import.sh); repository checks
adapters/   how to connect a deployment to the outside (separate processes on the published interfaces)
docs/       concept, architecture, decisions (adr/), C4 and data-flow diagrams (architecture/), metrics,
            device registry, business rules, security review
spec/       machine-readable contracts: device protocol and registry (AsyncAPI 3.0),
            operator API (OpenAPI)
```

## Contributing

Contributions are welcome once the repository is public: bug reports, fixes,
tests, documentation, adapters. Every commit carries a Developer Certificate
of Origin sign-off (`git commit -s`). How to propose a change, the checks a
pull request must pass and the coding conventions: [CONTRIBUTING.md](CONTRIBUTING.md).

## Getting help and discussion

Questions, ideas and bug reports: see [SUPPORT.md](SUPPORT.md). Bugs and
feature requests go to the [issue tracker](https://github.com/chargetronic/imatra.iso15118_server/issues).
Community channels (mailing list, Slack channel) and a public technical call
are listed in SUPPORT.md as they open.

## Meetings

There are no public meetings yet. A monthly public technical call is planned
from the first public release; its schedule, joining instructions, agendas
and notes will be published in [GOVERNANCE.md](GOVERNANCE.md#meetings).

## Reporting security issues

Please do not open a public issue for a vulnerability. Follow
[SECURITY.md](SECURITY.md).

## Governance

How decisions are made, the roles in the project and how to take one on:
[GOVERNANCE.md](GOVERNANCE.md). Current maintainers: [COMMITTERS.csv](COMMITTERS.csv).
The project is stewarded by Chargetronic (Salute Solutions Oy). It is not
hosted by a foundation; hosting may be considered later.

## Acknowledgements

Imatra was started and is maintained by [Chargetronic](https://chargetronic.fi)
(Salute Solutions Oy, Finland). It uses the EXI codec
[libcbv2g](https://github.com/EVerest/libcbv2g) of the EVerest project
(LF Energy), and EVerest's EV-side modules are its main interoperability test
peer.

## License

[Apache License 2.0](LICENSE); notices: [NOTICE](NOTICE); third-party
components and their licences: [THIRD_PARTY.md](THIRD_PARTY.md). Patents: the
server is free to use; equipment on this architecture is licensed separately,
see [PATENTS.md](PATENTS.md).
