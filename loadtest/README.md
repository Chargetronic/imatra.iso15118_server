# Load test

Runs a fleet of virtual devices (`fleetgen`) against an Imatra deployment over
mTLS: every device has its own client certificate and real ISO 15118-2 or PWM
sessions. Each run writes `results/<label>/report.md` (targets against results,
V2G latencies per message, CPU and memory per container, data in Postgres).

The figures in [docs/architecture.md](../docs/architecture.md) come from this
kit, with the conditions stated there.

## Two modes

| | `LT_MODE=own` (default) | `LT_MODE=layer` |
|---|---|---|
| What runs | this repository alone | this repository and the test service layer of the test harness (`imatra.test_harness`, cloned beside it) |
| Admission | `PKI_MODE=own`: the deployment's own CA signs the generator's device certificates | `PKI_MODE=layer`: the test layer issues the trust roots and publishes the registry |
| Extra scenarios | — | the registry under load (`fleet-bind.sh`), the layer away (`layerdown`) |

The measurement in `docs/architecture.md` was made in `LT_MODE=layer`; the
server does the same work per device in both modes (the registry only adds the
messages of binding).

## Host

A Linux host with Docker; the reference measurement uses 2 dedicated vCPU and
8 GB for the deployment and the generator on a second host.

```sh
git clone https://github.com/chargetronic/imatra.iso15118_server
cd imatra.iso15118_server
sudo loadtest/host-prepare.sh      # Docker, limits for many connections, time sync
loadtest/server-up.sh              # deployment (PKI_MODE=own, 2 engines), test CA files in ~/loadtest
N=500 loadtest/gen-run.sh f0       # short check
N=5000 loadtest/gen-run.sh steady  # steady state and a stage of 2 000
```

With the test layer: clone `imatra.test_harness` beside this repository, then
`LT_MODE=layer loadtest/server-up.sh` and `loadtest/fleet-bind.sh 5000` before
the runs.

The generator on a second host (as in the measurement): on the deployment's
host `LT_BIND=<its private IP> loadtest/server-up.sh`; on the generator's host,
a clone of this repository and `SERVER=deploy@<that IP> N=5000 loadtest/gen-run.sh stage`.

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
| `lib.sh` | the mode, the compose project and command, shared by the scripts |
| `server-up.sh` | `.env` for the test (fresh secrets, the mode's `PKI_MODE`), the deployment (in `LT_MODE=layer` with the test layer through the harness's `compose/up.sh`), the CA files for the generator (`~/loadtest`, mode 700) |
| `fleet-bind.sh` | `LT_MODE=layer`: the fleet in the test layer at once (`layer fleet`), time until the deployment has applied every registry message |
| `inject.sh` | one failure (an engine killed, Postgres or relay stopped, Valkey killed or flushed; in `LT_MODE=layer` the test layer stopped) |
| `gen-run.sh` | one scenario: collector, `fleetgen` on the deployment's network, report |
| `collect.py` | every 5 s: host and containers, Valkey, Postgres, broker, engine metrics; at the end the run's data from Postgres |
| `report.py` | the report in Markdown |
| `summary.py` | every run in `results/summary.csv` (one row per run: V2G latency and timeouts, stage delivery, FCR-D stages within 7.5 s, server CPU) and `results/timeline-<label>.csv` (server samples over time), for charts |

## Scenarios

| Name | What it checks |
|---|---|
| `smoke` | the kit end to end in a minute (20 devices; CI) |
| `f0` | the kit works (500 devices, 5 min) |
| `steady` | steady state; a stage of 2 000 from the server's pool (`POST /v1/activations`) and its cancel |
| `reconnect` | every connection dropped, back over mTLS within 60 s |

Targets: no V2G timeouts (EVCC 2 s, PowerDelivery 5 s, ISO 15118-2 table 109),
answer p99 < 1.5 s, the stage delivered within 2 s, every device back after a
reconnect, no orphan summaries, API data lag p95 ≤ 2 s.

Every root, key and certificate here is a test one, created on this host (by
the deployment in `LT_MODE=own`, by the test layer in `LT_MODE=layer`): the
CA key is handed to the generator. Never run the kit on a deployment that
serves real devices; remove the host after the test.
