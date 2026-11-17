# ADR-003: Event time is the server's; the device gives ids and durations

Date: 2026-11-12. Status: accepted (Dmitrii Ershov).
Context: design review of 2026-11-11 (device clocks); ADR-001 (order across engine instances).
Supersedes the device-time parts of ADR-001 (rules 2 and 3).

## Context

Events took their time from two clocks. The plug-in boundaries (`cp.changed`,
`session.started`, `session.ended`) and the meter readings used the
device's wall clock when it sent one; the V2G events (`v2g.started`,
`charging.*`, `limit.applied`) used the server's. The projector tied the two
together with a window: an event belonged to the plug-in from 5 s before its
start to 5 s after its end. Command reaction times used the device's
acknowledgement time, accepted only when it looked plausible.

A device whose clock is off by more than 5 s (before NTP, after a reboot
without an RTC) puts its V2G events outside its own plug-in: they wait as
orphans or join the neighbouring plug-in. Independently of clocks, the window
also fails on a quick re-plug: a V2G request of the old plug-in handled late
by another instance (ADR-001, R1) falls within 5 s of the new plug-in's start
and joins it.

What the server measures with time, and where it can come from:

| Measure | Before | Now |
|---|---|---|
| plug-in an event belongs to | ±5 s window | plug-in id from the device |
| plug-in duration | end − start | the device's own count (`plugged_ms`) |
| plug-in start and end | event time | arrival; end = min(arrival, start + duration), start = min(start, end − duration) |
| energy | meter counter | unchanged (a counter, no time) |
| order of meter readings | device time | message number within a boot |
| reaction to a command | device's ack time | ack arrival − command accepted by the API, both server |
| limit duration, pair TTL | relative (`duration_s`, `ttl_s`) | unchanged |
| EV departure | relative in ISO 15118-2 | unchanged, converted at arrival |
| presence, heartbeat, activation pace, API lag | server | unchanged |

## Decision

1. **The time of an event is the server's**, taken when the MQTT message
   reached the engine, before the device's worker queue (waiting there is not
   part of it). A device's wall clock is never an event time.
2. **What belongs together is told by ids the device gives.**
   - `boot`: a random id of this start of the device software.
   - `plug`: `"<boot>-<n>"`, the plug-in number since that start; in `cp`,
     `telemetry/energy`, `cmd/ack` and `v2g/up` while an EV is plugged in.
     A plug-in going on when the software starts gets an id of the new boot
     (no `plugged` event).
   - The TCP connection to the EV (`conn`) and the V2G session id, as before.
   The answers of the server always carry the V2G session id; an EV that
   re-plugged has a new session and rejects an answer of the old one, so a
   late request cannot reach the wrong session.
3. **Durations are counted by the device** on its monotonic clock:
   `plugged_ms` with `unplugged`. A report delivered late (the device was
   offline) keeps the plug-in's real span.
4. **Order of meter readings:** by `seq` within a `boot`; a reading of another
   boot replaces the last one.
5. **Reaction time** = arrival of the acknowledgement − acceptance of the
   command (`limits.created_at`), both on the server's clock: end to end, as
   the aggregator sees it.
6. **The projector ties an event to a plug-in by its id.** An id it does not
   know yet: the event waits for its plug-in (orphan); except when the
   device's open plug-in has an id of another boot (the software restarted
   with the EV plugged in) — that plug-in takes the new id.
7. **Compatibility:** a device without the ids (device software before 0.3.2) gets
   arrival times and the 5 s window as before; its telemetry is ordered by
   arrival.
8. The device's wall clock stays in the messages (`t`) for diagnostics only.
   The device still needs a correct clock for TLS certificates,
   not for data.

## Consequences

- No clock synchronisation between the device and the server is needed for
  correct data; the review finding on device clocks is closed.
- Protocol additions (compatible): `boot`, `plug`, `plugged_ms`. Feed events
  carry `data.plug`; `session.ended` carries `plugged_ms`
  (`api/schemas/events.v1.json`). `session_summaries.plug_id`
  (in `001_initial.sql` of the first release).
- `device_live` keeps last-writer-by-time, now on one clock (the server's).
- Bench timings of the message journal (`t_ev_rx`, `t_up_tx` against the
  server's receive time) still compare two clocks; they are diagnostics of the
  bench, not data.
- fleetgen sends the ids as a device does; `-clock-skew`, `-late-unplug` and
  `-legacy` check this decision under load.

## Verification

| Check | Result |
|---|---|
| engine: meter readings by boot and number (`TestPowerChanged`) | pass |
| feed payload with `plug` (`TestPayloadPlug`) | pass |
| reference device software: ids, `plugged_ms`, id after a restart with the EV in | pass |
| fleetgen 3 000 devices (2 engines, one host), device clock −30 s | 1 956 plug-ins, all with id, all ended by their unplug; 0 orphans, 0 V2G timeouts; duration 110–120 s (charge 120 s, the last ones cut by the end of the run) |
| fleetgen 1 000, device clock +45 s, unplug reports 20 s late | 650 plug-ins, 0 orphans; span 60 s (= charge), the unplug arrived 20.0 s after the end it reports |
| fleetgen 1 000 `-legacy` (no ids), device clock −30 s | 618 plug-ins by the window, 0 orphans, duration 59–60 s |
| fleetgen 1 000, activation stage of 300 from the pool | 300 limits applied, each counted in its plug-in; reaction p50 1 330 / p95 1 345 ms (server clock) |
