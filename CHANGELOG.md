# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
Changes to a contract (device protocol, registry and rules, database schema,
`/v1`, event types, settings, metrics) are marked **contract**.

## [Unreleased]

First public release in preparation.

### Added

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
- Business rules of devices (open set): rated and installation current,
  phases, meter quality, supply window.
- Gateway (Caddy) with TLS and rate limits; metrics and probes; fleet generator
  for load tests; export and import of a deployment.
- History kept to a retention period: telemetry, the twin journal and the V2G
  journal 30 days (`HISTORY_RETENTION_DAYS`, **contract**: settings).
- Load test kit (`loadtest/`): a generator on another host, fault injection,
  reports and tables of every run.
- Stated capacity measured: 5 000 devices with 3 200 sessions at once on
  2 dedicated vCPU (conditions in `docs/architecture.md`).
- Documentation: conceptual design (`docs/concept.md`) and the C4 model in
  draw.io (`docs/architecture/c4-model.drawio`: context, containers, engine
  components, deployment).
