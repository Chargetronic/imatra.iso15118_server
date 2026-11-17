# Architecture: data flows and contracts

Diagrams are Mermaid in Markdown (GitHub renders them); the machine-readable
contracts in [`spec/`](../../spec/) are the source of truth for messages,
fields and enumerations. When a contract changes, the diagrams change in the
same pull request.

| Boundary | Diagrams | Contract |
|---|---|---|
| EV ↔ device ↔ deployment | [dataflow-device.md](dataflow-device.md) | [`spec/asyncapi-device.yaml`](../../spec/asyncapi-device.yaml) (AsyncAPI 3.0) |
| Inside a deployment | [dataflow-internal.md](dataflow-internal.md) | implementation; feed schema [`api/schemas/events.v1.json`](../../api/schemas/events.v1.json) |
| Service layer → deployment | [dataflow-service.md](dataflow-service.md) | [`spec/asyncapi-registry.yaml`](../../spec/asyncapi-registry.yaml) (AsyncAPI 3.0) |
| Device ↔ service layer (certificate, TLS signature) | [dataflow-service.md](dataflow-service.md) §1–2 | [`spec/asyncapi-layer.yaml`](../../spec/asyncapi-layer.yaml) (AsyncAPI 3.0) |
| Operator systems → deployment | [dataflow-internal.md](dataflow-internal.md) §3–5 | [`spec/openapi-operator.json`](../../spec/openapi-operator.json) (OpenAPI 3.1, exported by `scripts/export-openapi.py`) |

C4 model — system context, containers, engine components (two views) and
deployment on one host — in one draw.io file with a page per view:
[c4-model.drawio](c4-model.drawio) (open in draw.io / diagrams.net or the
draw.io editor extension). Purpose and principles: [concept.md](../concept.md).

Decisions: [`docs/adr/`](../adr/). Reading the contracts as HTML:
`npx @asyncapi/cli start studio spec/asyncapi-device.yaml`,
`npx @redocly/cli preview-docs spec/openapi-operator.json`.

CI checks: the AsyncAPI documents validate; `spec/openapi-operator.json`
matches the API code.
