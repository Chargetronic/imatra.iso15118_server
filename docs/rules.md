# Business rules of a device

What a device is and what it allows is not built into the server. The service
layer gives each device an open set of **business rules** in its registry
message ([registry.md](registry.md)); without a layer the operator sets them
(`scripts/registry.sh … RULES`). The server:

- keeps the whole set as given (`devices.rules`) and shows it in
  `GET /v1/devices` and `GET /v1/devices/{id}` (`registry.rules`);
- applies the keys it has a handler for; a missing key means the default
  behaviour, the same as for a device with no rules;
- skips a key of the wrong type (metric `engine_rules_invalid_total{key}`),
  the other keys still apply;
- keeps a key it has no handler for, unchanged, and lets it change nothing.

A new rule is a new handler in the engine (`engine/cmd/engine/rules.go`,
`engine/internal/rules`) and a line in this catalogue; the schema and the
device protocol do not change. The catalogue is the same for every server
built on this protocol.

No rule names a product, and the server's behaviour never depends on the
device type (`type` in the registry is for reports only).

## Catalogue

| Key | Value | Default | What the server does |
|---|---|---|---|
| `rated_current_a` | number, 6–1000 | none | the device's rating: the EV is never offered more, operator limits above it are refused |
| `install_max_current_a` | number, 6–1000 | none | the installation's limit (the breaker): the same, with the rating and the station limit — the lowest wins |
| `phases` | 1 or 3 | the station settings | fewer phases than the station settings: the power is computed with these |
| `meter` | `clamp`, `certified` | `clamp` | quality of the energy in `session.ended`: `measured` (clamp) or `certified` |
| `supply` | `{"below_pct", "min_current_a", "minutes"}` | none | charging windows for the device's own battery, below |
| `topology` | `inline` (between a charging station and the EV), `integrated` (in the charging station) | `inline` | validated and kept; no handler yet: both are served alike until a station with the module built in needs its own handling |

Reserved (kept and shown, no handler until the engine speaks ISO 15118-20):
`v2g` (`{"hw", "enabled", "max_discharge_kw"}`), `flex` (participation in
flexibility).

### `supply`

For a device that runs on a battery it can charge only while current flows to
the EV. The thresholds are the layer's: they change with the battery and the
device's revision.

| Field | Meaning | Default |
|---|---|---|
| `below_pct` | open a window when the battery reported by the device is below this, percent | — (mandatory) |
| `min_current_a` | the EV's current during the window, A (6 at least, IEC 61851-1) | 6 |
| `minutes` | length of the window | 20 |

The device reports its battery in its heartbeat (`dev/<id>/status`,
`"battery_pct"`). When it is below `below_pct` and an EV is plugged in that
draws no current, the server opens a window: a limit of `min_current_a` for
`minutes` (source `device_supply`, priority 0 — any other limit in force wins,
current flows with it as well) and a CP restart pulse, after which the EV
starts a new session. Event `device.supply_window`, metric
`engine_supply_windows_total`. The next window comes after three window
lengths at the earliest. An EV that does not take current (full) gives
nothing; once the battery is empty the device lets the EV charge on the
station directly (its bypass), and the server loses control of that session.

This is the default; an operator may change it (the server is open).
