# Load test

Runs a fleet of virtual devices (`fleetgen`) against an Imatra deployment over
mTLS, with the test service layer of the
[test harness](https://github.com/chargetronic/imatra.test_harness) issuing the
deployment's trust roots and publishing the device registry. Everything runs on
one host: the deployment, the test layer, the generator. Each run writes
`results/<label>/report.md` (targets against results, V2G latencies per
message, CPU and memory per container, data in Postgres).

The figures in the README of this repository come from this kit, with the
conditions stated there.

## Host

A Linux host with Docker; the reference measurement uses 2 dedicated vCPU and
8 GB. Clone this repository and the test harness side by side:

```sh
git clone https://github.com/chargetronic/imatra.iso15118_server
git clone https://github.com/chargetronic/imatra.test_harness
cd imatra.iso15118_server
sudo loadtest/host-prepare.sh      # Docker, limits for many connections, time sync
loadtest/server-up.sh              # test layer + deployment (PKI_MODE=layer, 2 engines)
loadtest/fleet-bind.sh 5000        # register and bind the fleet in the test layer
N=500 loadtest/gen-run.sh f0       # short check
N=5000 loadtest/gen-run.sh steady  # steady state and a stage of 2 000
```

Variables of `gen-run.sh`: `N`, `STAGE_N` (devices in the activation stage),
`STAGE_SPREAD` (s), `CONN_RATE` (MQTT connections per second at start, 200),
`SETTLE` (pause after connecting before plug-ins, e.g. `60s`: a fleet at rest
rather than a reconnect wave), `GEN_ARGS` (more `fleetgen` flags),
`ENGINE_BUILD` (a tag for the summary), `SERVER=user@host` (the generator on
another host).

## Files

| File | What it does |
|---|---|
| `host-prepare.sh` | Docker, kernel and file limits, time sync (Ubuntu) |
| `server-up.sh` | `.env` for the test (fresh secrets, `PKI_MODE=layer`), the test layer and the deployment through the harness's `compose/up.sh`, the deployment root of the test layer for the generator (`~/loadtest`, mode 700) |
| `fleet-bind.sh` | the fleet in the test layer at once (`layer fleet`), time until the deployment has applied every registry message |
| `gen-run.sh` | one scenario: collector, `fleetgen` on the deployment's network, report |
| `collect.py` | every 5 s: host and containers, Valkey, Postgres, broker, engine metrics; at the end the run's data from Postgres |
| `report.py` | the report in Markdown |
| `summary.py` | every run in `results/summary.csv` (one row per run: V2G latency and timeouts, stage delivery, FCR-D stages within 7.5 s, server CPU) and `results/timeline-<label>.csv` (server samples over time), for charts |

## Scenarios

| Name | What it checks |
|---|---|
| `f0` | the kit works (500 devices, 5 min) |
| `steady` | steady state; a stage of 2 000 from the server's pool (`POST /v1/activations`) and its cancel |
| `reconnect` | every connection dropped, back over mTLS within 60 s |

Targets: no V2G timeouts (EVCC 2 s, PowerDelivery 5 s, ISO 15118-2 table 109),
answer p99 < 1.5 s, the stage delivered within 2 s, every device back after a
reconnect, no orphan summaries, API data lag p95 ≤ 2 s.

Every root, key and certificate here is a test one, created by the test layer
on this host. Remove the host after the test.
