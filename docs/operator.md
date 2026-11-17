# The operator's responsibilities

Who runs a deployment of this server is its **operator**. This page says
what the server does for the operator, what it does not, and what the
operator has to decide or provide. How to install and run it:
[deployment.md](deployment.md).

## 1. Defaults, not guarantees

The server is open source: the operator may change any part of it. Everything
the server does is therefore its **default behaviour**, not a guarantee to the
device owner or to anyone else. What a device owner can rely on whatever the
operator does comes from the device's firmware and, where one is used, from the
service layer: the firmware connects only where it is directed and keeps the
vehicle charging (from its cache, then basic charging) when the server does not
answer.

## 2. What the server does

| Area | What it does |
|---|---|
| Charging | answers the vehicles' ISO 15118-2 and DIN 70121 requests tunnelled by the devices; controls basic (PWM) charging; applies limits to running sessions |
| Commands | limits per device, group activations from a pool of devices, device settings, local block; each command is confirmed by the device (`cmd/ack`) and the confirmation is recorded |
| Data for the operator | the event feed (`/v1/events`, with a cursor, without gaps or repeats) and tables of devices, sessions and plug-in summaries (`/v1`) |
| Admission | devices by client certificate only; the registry from a service layer or kept by the operator ([registry.md](registry.md)) |
| Housekeeping | deletes the feed and the history after their retention period ([deployment.md](deployment.md), section 8) |
| Capacity | measured, with the conditions: [architecture.md](architecture.md), "Configurations" |

## 3. What the server does not do

| Not included | What it means for the operator |
|---|---|
| Billing and tariffs | the server records sessions, energy and the limits applied; prices, invoices and settlement with the device owners or a market are the operator's systems |
| Energy for billing | the energy of a session is `measured` (current clamps) unless the device's rules declare a certified meter (`certified`, [rules.md](rules.md)); power values are informational |
| Market and protocol adapters | OpenADR, OCPP, market platforms connect through `/v1` and the feed; adapters are separate projects |
| Redundancy | one of each component; a failure and its effect: [deployment.md](deployment.md), section 9 |
| Backups | the tools are there (`scripts/export.sh`); running them, keeping the archives safe and testing a restore is the operator's job |
| A user interface | the operator's own systems read `/v1` |
| Local reaction to grid frequency | not done by the server (it needs a reaction on the device); activations are commands with a confirmation, sized for the services in [architecture.md](architecture.md) |

## 4. What the operator decides and provides

**Running the deployment**
- the host, its updates and its clock; dedicated vCPUs for a fleet;
- updates of the server to new releases, reading the release notes first
  (changes marked **contract**);
- backups (a daily `scripts/export.sh` kept off the host) and a tested restore;
- monitoring of the health endpoints and metrics ([metrics.md](metrics.md));
- the capacity: staying within the measured configuration, or measuring a
  larger one with the load test kit (`loadtest/`).

**Security**
- the secrets in `.env`, which never go into a repository;
- operator keys with the rights each system needs, the first all-rights key
  removed after the start, keys revoked when a system is retired;
- in `PKI_MODE=own`: the CA key (in the `pki` volume and in every backup
  archive), issuing and renewing device certificates, revoking a device by a
  local block or `scripts/registry.sh revoked`;
- security fixes: watch the releases and [SECURITY.md](../SECURITY.md).

**Devices and their owners**
- which devices it serves, and the agreement with their owners;
- when a device joins or leaves its pool of activations and its billing. With
  a service layer the server is told when a device is bound to it, unbound or
  revoked, and only after the fact; a session that started before the change
  goes on to its end with the operator where it started ([registry.md](registry.md));
- the device's business rules (rated current, installation, phases, meter),
  set through the registry or the device settings ([rules.md](rules.md)).

**Grid services**
- which services it offers with the fleet and with how many devices; a
  stage of an activation reaches the devices within the times measured in
  [architecture.md](architecture.md), larger pools in waves;
- the decision to limit a device: the server applies the limit the operator
  sends, within the station's and the vehicle's limits.

## 5. Data and privacy

The server holds no names, addresses or contact details of people. It holds
for each device: its id, type, coarse location from the registry (rounded to
about 100 m), the user's time zone, its business rules, and the data of its
charging sessions (times, energy, limits, data the vehicle reports such as
state of charge or energy requested). Linked to a customer record in the
operator's own systems, these can be personal data; the operator is then
their controller and decides the legal basis, what it tells the device
owners, and how long data is kept beyond the defaults
([deployment.md](deployment.md), section 8).

The full V2G journal (every message of a session) is written only for devices
with the debug flag and is readable with the `debug` right only.

## 6. Moving devices to another operator

With a service layer, a device moves from one operator to another on the
layer's decision, not the operator's: the move takes effect at the user's
midnight at the earliest and after the cable is unplugged; the operator it
leaves is told *unbound*, the new one *bound*, after the fact. By default the
server takes an unbound device out of the activation pool, cancels its limits
and lets a running session finish. Whether it keeps billing the session, and
what it does with the device's data afterwards, is the operator's decision.

## 7. Support

The project does not run anyone's deployment. Questions about the code:
[SUPPORT.md](../SUPPORT.md); security reports: [SECURITY.md](../SECURITY.md).
