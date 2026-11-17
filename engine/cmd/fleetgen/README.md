# fleetgen — virtual device fleet for load tests

`fleetgen` connects N virtual devices to the server and makes their EVs
charge. Each device speaks the same MQTT protocol as a real device (the device protocol, `engine/internal/tunnel`)
(`status` with LWT, `cp`, `v2g/up`/`v2g/down`, Charging Loop cache with
`sampled` forwarding, `telemetry/energy`, `cl/push`, `power/set`, `config`,
`ctrl`). EVs with ISO 15118-2 run a real AC EIM session: requests are encoded
with libcbv2g (`internal/evexi`), so every session has its own SessionID and
the engine answers it like a real one. All latencies are measured on the
generator's clock only: no clock sync between hosts is needed.


## What it measures

| Scenario | Flags | Report |
|---|---|---|
| Fleet, plug-ins, charging | `-n`, `-plug-frac`, `-plug-spread`, `-charge`, `-duration` | `v2g.<Message>`: request → answer, per message type; `session.setup_total` |
| Activation stage | `-api`, `-api-key`, `-activate-at`, `-activate-n`, `-activate-limit`; `-activate-groups`, `-activate-pool`, `-activate-spread` | API call time; per device: API call → new pair (`cl/push`, ISO) or `power/set` (PWM) on the device; time to the last delivery |
| Mass reconnect | `-reconnect-at`, `-reconnect-jitter` | drop → last device connected; drop → last re-sent pair |
| Time without the device clock (ADR-003) | `-clock-skew -30s`, `-late-unplug 20s`, `-legacy` | the server's plug-in summaries (`session_summaries`): no orphans, real durations |

The activation stage goes through the operator API in one of three ways:
one `POST /v1/devices/{id}/limit` per device (default, `-activate-conc`
calls in parallel); `-activate-groups N`: N group activations
(`POST /v1/activations`) over disjoint sets of devices; `-activate-pool`: one
activation by criteria (charging, count), the server picks the devices from
its pool, optionally spread over `-activate-spread` seconds. Charging devices
are picked first. After the stage the generator cancels its limits or
activations again: a limit left active would make the next run's command
"the same answer" for the EV, so nothing would be pushed or measured.

EV profiles (`-profiles iso=0.7,pwm=0.3`):
- `iso` — ISO 15118-2 AC: digital session, cache, limit via a new pair;
- `pwm` — no HLC, basic charging: limit via `power/set`, confirmed by a `cp` report.

A V2G answer later than the EV would wait counts as a timeout and ends that
EV's session: `V2G_EVCC_Msg_Timeout` of ISO 15118-2, table 109 — 2 s, for
PowerDelivery 5 s (`-timeout N` sets one value for all, for experiments).
Until 10.11.2026 the generator waited 5 s for every message, so earlier
reports count only answers later than 5 s as timeouts.

Every pair and `power/set` is acknowledged on `cmd/ack` as a device does it:
pair `installed`; power `applied` (PWM profile in a session), `held`
(ISO: the pair carries the limit) or `cleared`.

## Server side (test installations only)

1. Devices connect with client certificates (CN = `fleet-NNNNN`, `-prefix`), which
   fleetgen signs itself with a **test** CA: `-client-ca ca.crt -client-ca-key ca.key`.
   On a deployment in `PKI_MODE=own` that is the deployment's CA (`docker compose run --rm
   pki-init ca`, the key in the `pki` volume); for a test with the service layer,
   the test copy of the layer's cloud root. No accounts on the server.
2. An operator key for the activation: `OPERATOR_API_KEYS="fleet:<key of 16+ chars>"`.
3. Mosquitto: the default `MQTT_MEM_LIMIT` (128m in `docker-compose.yml`) is too
   small for more than a few thousand connections; raise it in `.env` for the test
   (see the larger-machine values in `.env.example`).

`-password` (one shared password) is for a broker with password accounts; a deployment admits certificates only.

## Run

```sh
# from engine/
docker build -f cmd/fleetgen/Dockerfile -t fleetgen .
docker run --rm -v "$PWD:/out" -e FLEET_API_KEY=... fleetgen \
  -broker ssl://deployment.example:8883 -ca /out/ca.crt \
  -client-ca /out/ca.crt -client-ca-key /out/ca.key \
  -n 1000 -plug-frac 0.65 -plug-spread 90s -charge 3m -duration 5m \
  -api https://deployment.example -activate-at 150s -activate-n 500 \
  -report /out/base-1k.json
```

Run the generator on a separate VM, not on the server under test. One process
handles about 10 000 connections; split a bigger fleet with `-start`/`-n`
over several processes or hosts and merge the reports.

Without Docker: build libcbv2g v0.3.2 (see the engine `Dockerfile`), then
`CGO_CFLAGS=-I<cbv2g>/include CGO_LDFLAGS=-L<cbv2g>/lib go build ./cmd/fleetgen`.

## Typical runs

| Goal | Flags |
|---|---|
| Night background | `-n 10000 -plug-frac 0.65 -plug-spread 10m -charge 30m -duration 30m` |
| Evening peak | `-n 10000 -plug-frac 0.5 -plug-spread 5m -charge 10m -duration 15m` |
| Activation stage | add `-activate-at 8m -activate-n 2000 -activate-limit 8` |
| Mass reconnect | `-reconnect-at 5m -reconnect-jitter 2m` |

Server-side settings (`loop_sample_s`, `telemetry_s`, `heartbeat_s`) come from
the `config` message as on a real device; `-sample-s` and `-telemetry-s`
override them for a run.
