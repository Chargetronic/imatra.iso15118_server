# Metrics and probes

Every engine, relay and projector serves `/metrics` (Prometheus text format),
`/live` (the process runs) and `/ready` (it takes traffic) on `HEALTH_ADDR`
(`:8090` by default). The API serves `/health` (database, broker, lag of the
operator tables). The deployment ships no dashboards or alert rules: scrape these
endpoints with your own monitoring.

Each resource has its own signal and its own remedy. More engines do not
relieve Postgres or Valkey: their load follows the number of devices and
events, not the number of engines.

The thresholds below are **starting points derived from the ISO 15118 timing
budget**; the reference measurement (2 vCPU, 5 000 devices, see
[architecture.md](architecture.md)) ran within them. Treat them as
recommendations.

| Resource | Metric | What it shows | Remedy | Starting threshold |
|---|---|---|---|---|
| Engine | `engine_v2g_server_seconds` (histogram, p95 / p99) | time of a V2G request in the engine: queue + step + answer; ISO budget 1.5 s, EV timeout 2 s | add an engine (`--scale engine=N`) | p95 > 0.3 s: add; p99 > 1 s: alarm |
| Engine | `engine_worker_wait_seconds` (p95) | wait of a message in its device lane — first sign an instance falls behind | add an engine | 0.1 s / alarm 0.2 s |
| Engine | `process_cpu_seconds_total` | CPU of the instance | add an engine | 70 % of its cores |
| Engine | `engine_worker_backlog`, `engine_sender_backlog` | queued messages and activation sends | watch | — |
| Activations | `engine_activation_send_wait_seconds`, `engine_activation_send_seconds` | wait and duration of the sends | `ACT_SENDERS`, `ACT_RATE` | a 2 000-device stage within 1 s |
| Correctness | `engine_v2g_failed_total`, `engine_v2g_unknown_session_total` | ISO failures and engine errors; requests of lost sessions | investigate; a lost Valkey | growth |
| Admission | `engine_blocked_messages_total{dir}` | messages of devices the operator blocked, dropped | — | — |
| Registry | `engine_registry_applied_total{state}`, `engine_registry_ignored_total`, `engine_registry_mismatch_total`, `engine_registry_unknown_devices` | the service layer's registry: applied, dropped (repeats, late, malformed), digest mismatches, devices admitted with no entry | [registry.md](registry.md) | mismatch > 0 |
| Rules | `engine_rules_invalid_total{key}`, `engine_supply_windows_total` | business rules of a known key with an unusable value (skipped); charging windows for a device's own battery | [rules.md](rules.md) | invalid > 0 |
| Valkey | `engine_valkey_pool{stat="timeouts"}`; Valkey CPU and memory (e.g. redis_exporter) | Valkey too slow or full | a faster core (Valkey 8 io-threads), `VALKEY_MAXMEMORY`; beyond: another deployment | timeouts > 0; CPU > 60 % of a core |
| Postgres | `relay_lag_seconds`, `projector_lag_seconds` | freshness of the feed and of the operator tables | a stronger Postgres, fewer writes | > 10 s |
| Postgres | `relay_backlog_entries` | events waiting in Valkey for Postgres | as above; check `EVENTS_STREAM_MAX` and Valkey memory | > 50 % of `EVENTS_STREAM_MAX` |
| Postgres | `engine_write_queue{queue}`, `engine_db_wait_seconds_total`, `engine_db_connections` | background writes behind; waits for a connection | `BG_DB_CONNS`, Postgres | growth |
| Roles | `engine_ready`, `engine_leader`, `engine_mqtt_connected`, `projector_active`, `relay_healthy` | readiness and roles | — | exactly one leader, one active projector |
| Broker | connections, dropped messages (`$SYS/#`, every 10 s) | the broker is one process | — (one broker per deployment) | dropped > 0 |

**When to add an engine:** the p95 lane wait or V2G time stays above its
threshold for 1–2 minutes, or the CPU does. Remove engines slowly, one at a
time. Stopping an instance loses no requests: it reports not ready, drops
its shared subscription, finishes what it took and hands over the leader
lease.
