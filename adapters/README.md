# Adapters

An adapter connects a deployment to something outside it — a flexibility market, a
VPP's dispatch system, an OpenADR server, a message bus. Adapters are
**separate processes**: they never link the engine's code and need no change
to it. They use the published interfaces of the deployment:

| Interface | Where | For |
|---|---|---|
| Operator API `/v1` | `GET /v1/openapi.json` (OpenAPI 3.1) | reading devices and sessions, limits, activations, device settings, local block |
| Event feed `GET /v1/events?cursor=` | `api/schemas/events.v1.json` | everything that happened, in order, by cursor; at least once, ids for de-duplication |

An adapter gets an operator key with the rights it needs (`read`, `control`,
...) and is audited like any other caller.

This server ships no adapters: OpenADR, OCPP and other protocol clients are out
of its scope. They are built on these interfaces as separate projects; this
directory only describes how to connect them.
