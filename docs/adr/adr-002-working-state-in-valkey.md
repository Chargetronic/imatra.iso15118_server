# ADR-002: Working state in Valkey, Postgres from the event feed

Date: 2026-11-10. Status: accepted (Dmitrii Ershov).
Context: group activations; load test of 2026-11-10
(end of a run: `SessionStop` timeouts while Postgres was busy).

## Context

With Valkey (several engine instances) the engine still
wrote to Postgres on the path of an answer to the EV:

- a `sessions` row at the start of every digital session (synchronous);
- a copy of the session state after every step (write-behind every 200 ms,
  held in the engine's memory);
- at unplug, on the device's worker: the time of the plug-in from the `events`
  table, the energy from `telemetry` (twice), `UPDATE sessions` (close, energy);
- `Close` of the session when the connection closed;
- a session missing in Valkey was looked up, and restored, from Postgres.

A device's messages go through one worker (order, ADR-001), shared with about
30 other devices. When ~1 900 EVs stopped at once, each unplug held its worker
for 7–8 Postgres round trips; the `SessionStop` of the next EV on that worker
waited and missed the EVCC timeout of 2 s (ISO 15118-2, Table 109): 369 to
1 038 timeouts per run.

## Decision

1. **Everything needed to run a charging session lives in Valkey**: the state
   of the digital session, the station settings with the limit in force
   (`st:`), the plug-in record (`hs:`: mode, charging, power, departure, need,
   energy of the plug-in, reaction to limits, location) and the activation
   pool. The engine's answer path touches Valkey and MQTT only.
2. **Postgres holds records and results, not steps.** What the operator API
   and the reports need — the feed (`domain_events`), the operator tables, the
   `sessions` rows, energy, history — is built from the event feed by
   background consumers (`relay`, `projector`), in batches, in order per
   device, idempotently. No snapshot of an ISO step is written.
3. **Commands go the other way:** accepted by the API into Postgres first
   (durable), then their value into Valkey; a send reads Valkey only.
4. **Losing Valkey ends the running digital sessions.** There is no copy of
   their state elsewhere. A request of a session the server does not know is
   answered by the standard: `FAILED_UnknownSession` and the connection closed
   (V2G2-460, V2G2-539); a `SessionSetupReq` opens a new session
   (`OK_NewSessionEstablished`, V2G2-756). The EV starts over. Until an EV's
   next request to the server it is answered from the device's cache.
   Limits, settings, the pool (`pool:seeded`) and the plug-in records are
   rebuilt from Postgres.
5. **No Valkey replica for now.** Reads and writes of the answer path go to the
   primary only (a lagging replica would break the compare-and-set and the
   order of ADR-001); Valkey uses under a third of one core at the stage
   peak. Replication with failover (Sentinel or a managed service, on another
   host) belongs to high availability (later); a replica may then serve reads that
   tolerate lag (pool selection, analytics).
6. **Deployment:** Valkey with AOF every second, restart policy, a memory limit
   of its own (`maxmemory`, `noeviction`: a full Valkey refuses writes with an
   error rather than evicting keys of running sessions), a health check.
7. **One Valkey per deployment, no Valkey Cluster** (added 12.11.2026, design review).
   The Lua scripts build keys of a device (`hs:`, `s:`, `st:`, `act:`) inside
   the script instead of receiving them in `KEYS`, the activation pool
   (`poolPick`) chooses over all devices of the deployment, and the feed is one
   stream `events`. Cluster is excluded by the model, not by the
   implementation: the pool and the feed are per deployment. Growth beyond one
   Valkey is another deployment; spreading a fleet over several deployments
   is not part of this edition. Headroom from the 19k run:
   Valkey ≤ 28 % of one core at the peak, about 3–4×. Rule for new scripts:
   keys of one device go through `KEYS`; a key built inside a script is for
   operations over the whole deployment only, with a comment. No rework for Cluster
   is planned.

Amendment (16.11.2026): the single-engine mode with Postgres as the working
store (`STATE_STORE=postgres`) has since been removed; Valkey is the only
working store of the sessions.

## Consequences

- `sessions` is the projector's: a row appears with `v2g.started` (a few
  hundred ms after the session starts) and is closed by `v2g.ended`, or by the
  next plug-in boundary of the device; `phase` is the event-level phase
  (`WaitServiceDiscovery`, `Charging`, `ChargingStopped`, `Ended`), not the
  step; `energy_wh` comes with `v2g.ended` (the counter in `hs:`; telemetry
  table only when the event has none). `engine_state` is empty.
  The twin timeline, the message journal and the telemetry refer to a
  session by id without a foreign key (in `001_initial.sql` of the first release).
- The energy of a plug-in in `session.ended` comes from the counter in `hs:`
  (sum of the device counter's non-negative increments). A record rebuilt
  from Postgres is marked `seeded`; its unplug reads the telemetry table.
- A process restart of Valkey with AOF loses at most about a second: the
  sessions go on; the relay reports the possible gap (`system.events_gap`),
  the plug-ins overlapping it are flagged `data_gap`.
- **Known limit:** after a total loss, an EV in the Charging Loop keeps its
  cached answer (and the limit in it) until its next request to the server —
  at most the pair's TTL (`DEVICE_TTL_S`, 1 200 s), longer for a device with a
  limit, whose TTL the leader renews from Postgres. A limit cannot reach such
  a session. A device command to drop the cached pair after a Valkey loss
  would close the window; it changes the device protocol (open).

## Verification (bench, 2026-11-10; 3 000 devices, two engines, 2 vCPU)

| Scenario | Result |
|---|---|
| ~1 900 EVs stop at once (end of run) | `SessionStop` timeouts 369–1 038 before; 0 and 16 after (two runs) |
| Activation stage of ~1 900 | 2.6–2.7 s, all confirmed (no change) |
| Valkey killed (`kill -9`) and restarted with AOF while 1 885 EVs charge | sessions go on, 0 timeouts; gap reported and flagged |
| Valkey emptied (`FLUSHALL`) while 1 886–1 938 EVs charge | 1 326–1 328 ISO sessions get `FAILED_UnknownSession` at their next request (p95 46–50 ms), 0 timeouts; pool re-seeded with all plugged devices; plug-in energy as without the loss (273.0 vs 273.2 Wh) |
| `sessions` from the feed | 1 391 of 1 391 rows, ended, with energy |
