# Device registry

The service layer (the network that issues device identities and decides which
operator a device belongs to) tells a deployment which devices are its own.
It never calls the deployment's API, and the deployment never calls the layer:
the layer publishes into the deployment's MQTT broker.

## Who is responsible for what

| Party | Responsible for |
|---|---|
| Service layer | which operator a device belongs to; when it moves; revocation; device type; coarse location (rounded to ~100 m) |
| Device firmware | connecting to one operator at a time; finishing a running session with the operator it began with; renewing its 7-day certificate daily |
| This server | applying the registry with the defaults below; admitting devices by certificate |

The behaviour of this server described here is its **default**. The server is
open source and an operator may change it; the guarantees to users and to
other operators come from the layer and the firmware, not from this code.

## Rule

An operator hears only of facts. Until a device is reported *unbound* it is
this operator's: full data and control. After that it is not. A device is
connected to one operator's server at any moment, and a session belongs
entirely to the operator it began with.

A move to another operator takes effect from the user's midnight; a session
running at that time goes on to its end (the device switches when the cable is
unplugged), and only then are *unbound* and *bound* published.

## Messages

The layer connects to the broker on the device port (8883) with a certificate
under this deployment's root whose CN is `layer` (reserved: no device may use
it). It may write `registry/#` and nothing else; devices may not write it.

`registry/dev/<device_id>` — retained, QoS 1, one per device:

| Field | Meaning |
|---|---|
| `v` | schema version, 1 |
| `device_id` | the device (its certificate's CN) |
| `state` | `bound`, `unbound` or `revoked` |
| `type` | device type, for reports and selection in the API only: the server's behaviour does not depend on it (values are neutral, e.g. `adapter`, `station`, `v2g-station`) |
| `lat`, `lon` | coarse location (3 decimals); may be absent |
| `binding_ver` | grows with every change; the server applies a message only if it is greater than the stored one |
| `at` | time of the fact, UTC |
| `tz` | the user's time zone |
| `rules` | the device's business rules: an open set of parameters the server applies ([rules.md](rules.md)); absent — the stored set stays |

Unknown fields are ignored: the list will grow. Unknown keys **inside** `rules`
are kept as given and shown to the operator. There is no personal data.

`registry/digest` — once a day: `{"v":1,"bound":N,"max_binding_ver":M,"at":…}`.
If the stored registry differs, the server reads the registry again and writes
`system.registry_mismatch` to the feed.

### What to expect during a move

- A device that moved here connects **before** this server hears *bound*: the
  layer publishes once the device reports it has moved. For those seconds the
  device is admitted by its certificate and counted in
  `engine_registry_unknown_devices`.
- The server it left keeps it as *bound* until then; a session it serves goes
  on to its end.
- A device that is off is reported by the layer a day after the change.
- A revocation during a move goes to both operators.
- *bound* again with a greater `binding_ver` and the same state is an update of
  its fields (location, time zone, type, rules).
- If you kept the registry yourself with `scripts/registry.sh` before joining
  a layer, its versions (milliseconds) are greater than the layer's: clear the
  entries (`registry.sh clear`), set `devices.binding_ver` back to 0, then ask
  the layer to send everything again.

## What the server does (defaults)

| Message | Action | Feed event |
|---|---|---|
| `bound` | the device's row (it may not have connected yet), type, location, rules; back into the activation pool | `device.bound` |
| `unbound` | out of the activation pool, its limits in force cancelled; a running session goes on to its end | `device.unbound` |
| `revoked` | served no more, at once (the same barrier as the operator's local block); a later `bound` lifts it | `device.revoked` |

Only the leading engine reads the registry, with an ordinary subscription (a
shared one gets no retained messages). A new leader reads it all again; since
a message is applied only with a greater `binding_ver`, repeats, late messages
and re-reads change nothing.

A device with a valid certificate and no registry entry is **admitted**: the
certificate decides, the registry is reference data. Such devices are counted
in `engine_registry_unknown_devices` and listed with `GET /v1/devices?binding=none`.

`GET /v1/devices` and `GET /v1/devices/{id}` show the binding (`binding`,
`binding_ver`, `type`, `rules`, `connected_ever`) and filter by it
(`?binding=bound|unbound|revoked|none`, `?type=…`).

Revocations in bulk: the engines refuse a revoked device at once; the broker's
ACL is updated in batches (`BLOCKED_BATCH_MS`, 3 s), one configuration reload
for many devices.

## Without a service layer (`PKI_MODE=own`)

The registry is optional: a device's row appears at its first connection.
`scripts/registry.sh` publishes the same messages through the broker's
internal listener, e.g.
`scripts/registry.sh bound dev-01 adapter 61.172 28.753 Europe/Helsinki '{"rated_current_a":16}'`.
