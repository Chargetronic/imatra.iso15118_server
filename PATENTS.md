# Patents

## The software licence

Imatra is licensed under the Apache License, Version 2.0 ([LICENSE](LICENSE)).
Section 3 of that licence gives every user a patent licence from each
contributor for the patent claims that the contributor's contribution infringes
by itself or in combination with Imatra. This notice does not change, limit or
add to the Apache License 2.0.

## Patents of Salute Solutions Oy

Salute Solutions Oy (Chargetronic) holds patent applications on smart charging
of electric vehicles with cloud computing: Finnish application FI 20246111 A1
and European application EP 4711188 A1, and any patents granted on them. Their
claims describe a system of a cloud server, a charging station or charging
device and an electric vehicle, and methods carried out in it.

## What the Apache licence covers

- Using, changing and distributing Imatra, including running it in
  production, is covered by the Apache License 2.0, without payment.
- **Charging equipment** is not part of Imatra: any device at a charging
  point that carries the communication with the vehicle (an adapter, a
  charging station, a charge controller), whoever makes it; Chargetronic's
  SmartPlug is one example. Publishing Imatra grants no licence, under
  Section 3 or by implication, to develop, make, sell, import or deploy
  charging equipment. Where such equipment, alone or in combination with
  Imatra, falls under the claims, it needs a licence from Salute Solutions Oy.
- Charging equipment from manufacturers licensed by Chargetronic needs no
  further licence to be used with Imatra.
- **Protocol adapters** are software, not charging equipment: programs that
  use only the operator API `/v1` and the event feed and take no part in the
  communication with the vehicle, such as OCPP, OpenADR or market adapters
  ([adapters/README.md](adapters/README.md)). They need no licence from
  Salute Solutions Oy.

## Licensing equipment

Manufacturers who want to build or sell charging equipment on this architecture:
cto@chargetronic.fi.
