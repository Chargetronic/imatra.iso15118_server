# Roadmap

What the project intends to do, and not to do, over the next twelve months.
The order is the planned order; items carry no committed dates. The roadmap
is reviewed by the committers at every release and changes are made by pull
request to this file.

## Planned

| # | Item | What it gives |
|---|---|---|
| 1 | **Charging schedule per device** | one schedule built from the commands of the operator's systems, the device's business rules and its limits; pause (0 A) as a schedule entry; the supply window as a dated entry ([#18](https://github.com/chargetronic/imatra.iso15118_server/issues/18): the OCPP use case) |
| 2 | **Extension points** | documented interfaces for the state store, an event-bus adapter, the activation allocator and operator-defined attributes, with an example adapter |
| 3 | **Telemetry profiles** | less traffic for large fleets; per-device reporting rate |
| 4 | **Interoperability testing** with hardware devices and production vehicles | a public list of tested vehicles, supported use cases and known limitations |
| 5 | **Project maturity** | OpenSSF Best Practices badge (passing), a second maintainer |

## Done

* **First public release, 0.1.0** (18 November 2026): see [CHANGELOG.md](CHANGELOG.md).

## Not planned for this edition

These belong to the larger, commercial editions (see the README, "This
edition and what we commit to"), or are out of scope altogether.

* ISO 15118-20, DC charging, bidirectional power transfer (V2G), frequency
  containment reserve (it needs a local reaction to grid frequency).
* High availability of single components (broker cluster, Valkey replica,
  Postgres failover), several deployments under one management, automatic
  scaling, Kubernetes manifests.
* A streaming (push) API, a user interface.
* Protocol clients and adapters — OpenADR, OCPP, market and bus connectors.
  External systems connect through `/v1` and the event feed; such adapters
  are separate projects.

## Open

* **Plug & Charge.** The functions that can move to the server side are known
  and the architecture leaves room for them; they have to be built together
  with the certificate authorities. With the focus on residential charging it
  is not yet essential, so it stays open and may be added later as an
  extension.
