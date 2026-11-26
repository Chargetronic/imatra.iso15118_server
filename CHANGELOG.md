# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
Changes to a contract (device protocol, registry and rules, database schema,
`/v1`, event types, settings, metrics) are marked **contract**.

## [Unreleased]

### Fixed

- Session summaries: a limit applied after the device's plug-in id changed
  without an unplug was not counted (`limits_applied` 0), and the earlier
  plug-in ended "no unplug seen" only when the next one started
  ([#15](https://github.com/Chargetronic/imatra.iso15118_server/issues/15)).
  - Engine: a `plugged` with another plug-in id within `PLUG_REPEAT_S` is a
    new plug-in, not a repeat; its `session.started` carries `supersedes`
    (**contract**: event data, `api/schemas/events.v1.json`).
  - Projector: an event of a plug-in id without a summary opens it
    implicitly at the time of that event, the device's open plug-in ends
    `superseded`; a late `session.started` of the id joins the summary and
    takes the events waiting for it. A V2G session is not closed by an id
    change, only by the unplug.
  - Database: `session_summaries.implicit_start`, migration
    `003_implicit_plug_in.sql`; `/v1/sessions` returns it (**contract**).

## [0.1.0] - 2026-11-18

First public release: the server of one deployment for ISO 15118-2 AC
charging over a cloud-connected device, for fleets in the thousands.

### Breaking changes

None (first release).

### Known issues

- `PKI_MODE=own` keeps no certificate revocation list: a device is cut off by
  the block list (`PUT /v1/devices/{id}/block`), its certificate stays valid
  until it expires (`DEVICE_CERT_DAYS`, 365).
- The rate limits are per gateway process; a deployment runs one gateway.
- DIN 70121 is used only to read the state of charge; DC charging and Plug and
  Charge are not part of this release.
- No container images are published: build from the sources (`docker compose
  up -d --build`).
- Tested on Ubuntu 24.04 and 26.04 with Docker Engine 29.

### Key changes

- Engine: ISO 15118-2 AC (EIM), DIN 70121 state-of-charge read-out, IEC 61851-1
  PWM control; Charging Loop cache pairs on the device; command confirmation;
  staged group activations; several engine instances sharing the devices.
- Working state in Valkey; event stream to Postgres (relay) and operator tables
  (projector), rebuildable from the feed.
- Operator API `/v1` (OpenAPI 3.1): devices, sessions, limits, activations,
  device settings, local block, event feed with cursor; keys with rights and audit.
- Device admission by client certificate (CN = device id); own CA or the CA of
  a provisioning service; device registry from the service layer with the
  default behaviour on change of operator.
- Stated capacity measured: 5 000 devices with 3 200 sessions at once on
  2 dedicated vCPU (conditions in `docs/architecture.md`).
- Security, before the release (`docs/security-review.md`):
  - 26 vulnerabilities of the Go 1.24 standard library (govulncheck): the
    toolchain is Go 1.27; Caddy 2.10 → 2.11 with golang.org/x/net 0.62.0;
    alpine 3.20 → 3.24 and the base packages of the images;
  - the rate limits for requests with an operator key (per key and per
    address) did not apply; fixed in the gateway, checked by the negative tests;
  - a first key removed from `OPERATOR_API_KEYS` stayed active; it is now
    revoked at the next start of the API;
  - the deployment does not start with empty, example or short secrets.

### Other changes

- Business rules of devices (open set): rated and installation current,
  phases, meter quality, supply window.
- Gateway (Caddy) with TLS and rate limits; metrics and probes; fleet generator
  for load tests; export and import of a deployment.
- History kept to a retention period: telemetry, the twin journal and the V2G
  journal 30 days (`HISTORY_RETENTION_DAYS`, **contract**: settings).
- Load test kit (`loadtest/`): a generator on another host, fault injection,
  reports and tables of every run.
- Documentation: conceptual design (`docs/concept.md`) and the C4 model in
  draw.io (`docs/architecture/c4-model.drawio`: context, containers, engine
  components, deployment).
- Tests: Go unit tests against Valkey and Postgres; API tests against
  Postgres (`api/tests`); end-to-end scenarios T1–T7 in the test environment.
- Documentation: deployment guide (`docs/deployment.md`), operator's
  responsibilities (`docs/operator.md`), security review.

[Unreleased]: https://github.com/chargetronic/imatra.iso15118_server/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/chargetronic/imatra.iso15118_server/releases/tag/v0.1.0
