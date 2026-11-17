# ADR-001: Order of one device's messages across engine instances

Date: 2026-11-09. Status: accepted; rules 2 and 3 amended by ADR-003 (2026-11-12): event time is the server's, plug-ins are told by the device's ids, not by a time window.
Context: stateless engines with the working state in Valkey; architecture review of 2026-11-08.

## Context

Engines are stateless and share the device topics through MQTT 5 shared
subscriptions (`$share/engine/...`). Mosquitto hands each message to any
member of the group, with no affinity to the device. Within one instance the
device dispatcher keeps a device's messages in order; across instances it does
not. So `cp`, `status`, `telemetry` and `v2g/up` of one device can be handled
at the same moment by two instances.

The V2G exchange itself is safe: strict request/response per connection, and
the session state is saved with a compare-and-set. The risk is between topics,
for example:

| time | engine A | engine B |
|---|---|---|
| t0 | `dev/42/cp`: EV unplugged | |
| t0+1 ms | | `dev/42/v2g/up`: late request |
| t0+3 ms | session closed | session loaded |
| t0+5 ms | `ev.unplugged` | step of a closed session, answer and event after the unplug |

Options considered:

1. **Broker with per-device affinity** (sticky or hashed shared subscriptions,
   e.g. EMQX, HiveMQ). Engine unchanged. Rejected as the default: the server
   ships with OSI-licensed components only, and EMQX 5.9+ is
   under BSL 1.1; Mosquitto, NanoMQ and VerneMQ have no per-key affinity.
   The code must not depend on a specific broker.
2. **Owner forwarding**: every message goes to the device's owner (rendezvous
   hash), other instances forward it. Rejected: an extra hop on the hot path
   and a hand-over problem whenever the membership changes.
3. **Order-tolerant handling**: no ordering assumption between topics; the
   handlers make the result correct regardless of arrival order.

## Decision

Option 3, with these rules:

1. **Closing wins over a late step.** Ending a session (EV unplugged, connection
   closed) sets `ended` and increments the version in Valkey in one step. A
   step running elsewhere fails its compare-and-set, reloads, finds the session
   ended and answers nothing (`errEnded`). The compare-and-set script also
   refuses to save an ended session. *(Implemented, tests
   `TestCloseBeatsLateStep`, `TestCompareAndSet`.)*
2. **Events carry the device's order, not the arrival order.** Every event has
   the device timestamp (`time`) and a per-device sequence number from Valkey
   (`INCR dseq:<device>`) taken in the same Lua step that writes the event.
   The projector and consumers of the feed order a device's events by that
   number, not by `seq` of `domain_events`. *(Implemented first; the
   counter starts from the current time when Valkey lost it, so it keeps
   growing.)*
   **Limit found on 09.11.2026:** `dseq` is the order in which the
   instances *wrote* the device's events, which is not always the order in
   which the device *sent* them. The device reports the plug-in on `cp` and
   the EV's first V2G request on `v2g/up` within milliseconds; two instances
   handle them in parallel, and `v2g.started` can get a lower `dseq` than
   `session.started` of the same plug-in. Within one kind of source (the V2G
   session's own events, written with its state) the order holds. A consumer
   that assigns events to a plug-in therefore uses a time window (the
   projector: from 5 s before the plug-in to 5 s after its end) and holds an
   event that arrives before its plug-in until the plug-in appears.
   **Since ADR-003:** the time is the server's, and an event of a device
   0.3.2+ carries its plug-in id (`data.plug`); the projector ties it by the
   id, the window stays for older devices.
3. **Device-state events are last-writer-by-time.** `cp.changed`,
   `device.online/offline`, `power.changed` update `device_live` only if their
   timestamp is newer than the stored one (since ADR-003 the server's arrival
   time; meter readings by their number within a boot). *(For
   `power.changed` from the start: an older reading is ignored in the
   same Lua step that compares it with the last reported power.)*
4. **Commands are idempotent by `command_id`.** Delivery and confirmation of a
   limit or an activation stage are keyed by the command id; a duplicate or
   reordered confirmation does not change the result. *(Limits: implemented
   with command confirmation — the first "installed" / "applied" ack, or the CP report,
   sets the confirmation once; repeats, stale and foreign acks change
   nothing. Activations: a send is "the limit in force now", so a
   repeated, late or taken-over job changes nothing; a send not confirmed
   after 10 s goes out once more as a new pair version.)*
5. **Operators may still run a broker with per-device affinity.** It reduces
   conflicts and retries but is not required for correctness.

## Consequences

- Correctness does not depend on the broker; Mosquitto stays the default.
- Version conflicts are expected under load and counted as a metric; a rising
  conflict rate is a sign to look at the broker or the device's publishing.
- The activation executor relies on rules 2 and 4, not on arrival
  order.
- Test before scaling out: two engines on one Valkey, with cp/v2g races,
  owner change and leader lease.
- Found with two engines (09.11.2026): writing a new session into Valkey
  must not overwrite one already there — another instance may have stepped
  it meanwhile. The seed is conditional now (`TestSeedKeepsNewerState`).
