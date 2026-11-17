# Deployment guide

How to install, run, update, back up and move one deployment of the server.
What the deployment is made of: [architecture.md](architecture.md). What the
operator of a deployment is responsible for: [operator.md](operator.md).

## 1. What you need

| | Minimum | Reference (measured) |
|---|---|---|
| Machine | 1 vCPU, 2 GB RAM, 20 GB disk: a test or a few hundred devices | **2 dedicated vCPU, 8 GB RAM, NVMe 80 GB**: 5 000 devices, 3 200 sessions at once ([architecture.md](architecture.md), "Configurations") |
| System | Linux x86_64 with Docker Engine 24+ and the Compose plugin | Ubuntu 24.04 LTS |
| Time | the host clock synchronised (chrony or systemd-timesyncd) | |
| Network | a DNS name for the deployment; ports open from the internet: **80, 443** (API over HTTPS, Let's Encrypt) and **8883** (devices, MQTT over TLS) | the same |
| Engines | one | two (`--scale engine=2`) |

Use dedicated vCPUs for a fleet: on shared vCPUs the answer time to the
vehicles depends on the neighbours. Behind a TLS proxy of your own the
gateway needs no ports 80/443 (section 3.4).

## 2. Install

```sh
git clone https://github.com/chargetronic/imatra.iso15118_server.git
cd imatra.iso15118_server
git checkout v0.1.0                # a release tag, not main
cp .env.example .env
```

Fill in `.env` (every setting is explained in the file):

| Setting | What to put |
|---|---|
| `SITE_ADDRESS` | the deployment's DNS name; the gateway gets its HTTPS certificate for it |
| `BROKER_DNS` | the DNS name devices connect to (usually the same name) |
| `POSTGRES_PASSWORD`, `MQTT_SERVER_PASSWORD` | `openssl rand -hex 24` each |
| `OPERATOR_API_KEYS` | `admin:<openssl rand -hex 24>` — the first key, with all rights (section 4) |
| `PKI_MODE` | `own` (the deployment's own CA) or `layer` (certificates from a service layer), section 3 |
| for the reference machine | uncomment the values under "the reference machine" |

Values containing `$` must be in single quotes. The deployment does not
start while a password or key is empty, shorter than 16 characters or still
an example value (`pki/preflight.sh` stops it with a message).

```sh
docker compose up -d --build                      # one engine
docker compose up -d --build --scale engine=2     # reference: two engines
docker compose ps                                  # all services "healthy" or "running"
curl -s https://$SITE_ADDRESS/health               # {"status":"ok", ...}
```

The first start builds the images (5–15 minutes) and applies the database
migrations (the API does it; engines, relay and projector wait for it).

## 3. Device admission

Devices connect to `BROKER_DNS:8883` with a client certificate; the
certificate's CN is the device id. There are no passwords for devices.

### 3.1. Own CA (`PKI_MODE=own`)

The deployment creates its CA on the first start (volume `pki`).

```sh
docker compose run --rm -T pki-init ca > ca.crt                         # devices pin this CA
docker compose run --rm -T pki-init device dev-01 > dev-01.pem          # key + certificate
docker compose run --rm -T pki-init sign dev-01 < dev-01.csr > dev-01.crt   # the key stays on the device
```

Certificates are valid for `DEVICE_CERT_DAYS` (365). Renewal, revocation and
which device belongs to which customer are yours to organise. The registry is
optional: a device with a valid certificate is admitted, its row appears at
the first connection. To keep a registry by hand: `scripts/registry.sh`
([registry.md](registry.md)).

### 3.2. Certificates from a service layer (`PKI_MODE=layer`)

A service layer issues the deployment's CA and broker certificate, gives the
devices 7-day certificates, renews them daily and publishes the device
registry into the deployment's broker ([registry.md](registry.md)). Put the
files the layer gave you (`ca.crt` and `broker.pem`, or `broker.crt` and
`broker.key`) in a folder and import them:

```sh
docker compose run --rm -v "$PWD/from-layer:/in:ro" pki-init import
docker compose restart mqtt
```

The broker certificate is renewed by the layer; import the new one the same
way before the old one expires (a start warns 7 days ahead).

### 3.3. First device

1. Issue the device a certificate (3.1) or bind it in the layer (3.2).
2. Point the device to `BROKER_DNS:8883` with that certificate and the CA.
3. `GET /v1/devices/dev-01` with an operator key shows it `online`; plugging
   in an EV shows a session in `GET /v1/sessions`.

The device protocol is `spec/asyncapi-device.yaml` (AsyncAPI 3.0).

### 3.4. Behind a TLS proxy of your own

```sh
docker compose -f docker-compose.yml -f compose.behind-proxy.yml up -d --build
```

The gateway then serves plain HTTP on `127.0.0.1:PUBLIC_PORT`; your proxy
terminates TLS and forwards `/v1` and `/health`. The rate limits use the
client address from `X-Forwarded-For` of the proxies in `TRUSTED_PROXIES`.
Port 8883 (MQTT over TLS with client certificates) must reach the broker
directly: a proxy must not terminate that TLS.

## 4. Operator keys

The key in `OPERATOR_API_KEYS` has all rights and is meant for the first
start. Create keys with the rights each system needs, then remove the first
key from `.env`:

```sh
docker compose exec api python -m app.keys create vpp --rights read,control
docker compose exec api python -m app.keys list
docker compose exec api python -m app.keys revoke vpp
```

Rights: `read` (devices, sessions, event feed), `control` (limits,
activations, device settings, block), `debug` (V2G journal of devices with
the debug flag). Every call with `control` rights is in the audit log. The
API description: `GET /v1/openapi.json`.

## 5. Check the installation

| Check | How |
|---|---|
| Services | `docker compose ps`: all healthy |
| API | `curl -s https://$SITE_ADDRESS/health` — `status` ok, database and broker reachable, lag of the operator tables |
| Engines | `/live`, `/ready` and `/metrics` on port 8090 inside the compose network ([metrics.md](metrics.md)): exactly one leader |
| A device end to end | section 3.3 |
| Under load | `engine/cmd/fleetgen` (a virtual fleet with real ISO 15118-2 sessions), e.g. 500 devices for 5 minutes; for a fleet on a second host: `loadtest/` ([loadtest/README.md](../loadtest/README.md)) |

The separate test environment `imatra.test_harness` (a test service layer, a
test station and a test EV) runs the end-to-end scenarios of this server: a
session with TLS and without, operator commands, the layer away, the registry.

## 6. Update

```sh
git fetch --tags && git checkout v0.1.1           # the new release
scripts/export.sh /root/backup                    # a backup first (section 7)
docker compose up -d --build --scale engine=2
docker compose ps
```

Read the release notes (`CHANGELOG.md`) first: a change marked **contract**
may need a change in your settings or systems. Migrations are applied by the
API at its start. The engines are stateless and can be restarted one at a
time without stopping the charging; the other services restart once (a few
seconds: devices answer the vehicles from their cache meanwhile).

## 7. Backups and moving to another host

`scripts/export.sh [dir]` packs everything the deployment needs into one archive:
the database (`pg_dump`), the PKI (CA key and broker certificate), the broker's
retained messages, the gateway's certificates, `.env` and the running commit.

```sh
scripts/export.sh /root/backup                    # on the old host (a daily cron job is a good backup)
scp /root/backup/deployment-export-*.tar new-host:
# on the new host:
git clone https://github.com/chargetronic/imatra.iso15118_server.git && cd imatra.iso15118_server
git checkout <commit from the archive>
scripts/import.sh ~/deployment-export-*.tar [--behind-proxy]
```

Then point the DNS names to the new host. The archive holds the CA key and
all passwords: keep it `chmod 600`, off the host, and delete copies you no
longer need. `import.sh` refuses to overwrite existing volumes or `.env`.

What is not in a backup: the event stream entries not yet stored in Postgres
(a few seconds) and the working state of running sessions in Valkey; after a
restore the running sessions start again.

## 8. Data kept

| Data | Kept | Setting |
|---|---|---|
| Event feed (`/v1/events`) | 30 days | `EVENTS_RETENTION_DAYS` |
| Telemetry, the device journal, the V2G journal | 30 days | `HISTORY_RETENTION_DAYS` |
| Full V2G journal | only for devices with the debug flag | `JOURNAL_MODE` (`flag`) |
| Devices, sessions, plug-in summaries, limits, activations, audit log | until deleted by you | — |

The engine's leader deletes the expired rows once an hour.

## 9. When something fails

There is one of each component (no replica); what the vehicles and the
operator see:

| Component | Effect | What to do |
|---|---|---|
| One of two engines | the other takes over; the leader changes within 5–8 s | none; Docker restarts it |
| The only engine | no answers to the vehicles until it is back; devices answer charging from their cache until the TTL (20 min) | `docker compose up -d` |
| Valkey restarts (AOF) | up to ~1 s lost; sessions go on; a gap in the feed is marked | none |
| Valkey data lost | running digital sessions restart; limits and the pool come back from Postgres | none |
| Postgres | answers to the vehicles go on; the API is down; the feed catches up | restore the service; from a backup if the data is lost |
| Broker | every device reconnects (measured: 5 000 devices in 61 s) | none |
| relay, projector | the operator's tables lag, then catch up without loss | none |
| Service layer away (`PKI_MODE=layer`) | charging and admission go on while certificates are valid (7 days); registry changes arrive when it is back | contact the layer |

Logs: `docker compose logs -f engine` (one line per event, `level=WARN` and
`level=ERROR` worth a look). Metrics and starting thresholds for alerts:
[metrics.md](metrics.md).
