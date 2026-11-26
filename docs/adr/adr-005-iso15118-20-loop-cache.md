# ADR-005: Charging Loop cache for ISO 15118-20: prefix compare, the device's time stamp

Date: 2026-11-23; revised 2026-11-26 against the text of ISO 15118-20:2022.
Status: proposed (with ADR-004).
Context: the device answers the Charging Loop from a pair the engine gives it
and compares bytes, it decodes no EXI (device architecture, F6).

## Context

ISO 15118-2: the ChargingStatusReq of a session is constant byte for byte
(empty body, the session ID in the header), and so is the answer as long as the
limits do not change. The device compares the whole request and sends the
cached answer as it is.

ISO 15118-20: every message header carries a time stamp (mandatory), and the
AC_ChargeLoopReq carries the EV's present power (mandatory in both control
modes) and, in Dynamic mode, its energy requests and power limits. The request
differs every time. The answer also carries a time stamp; its body is
otherwise constant (Scheduled: ResponseCode only; Dynamic: plus the target
power) until the limits change.

## Decision

1. **Compare the prefix.** In ISO 15118-20 every message is its own root
   element, so the message type is in the first bits of the document; the
   header follows with the session ID (8 bytes) and then the time stamp. The
   device compares the first `req_bits` bits of the request (message type and
   session ID) and the payload type. Any AC_ChargeLoopReq of the session hits,
   any other request misses and goes to the server.
2. **Write the time into the answer.** The time stamp is microseconds of SECC
   time (V2G20-1534), an EXI unsigned integer; for every value from 2^49
   (1987) to 2^56−1 (year 4253) it is eight 7-bit groups, 64 bits, at a bit
   offset that is fixed for an encoded message. The engine gives the offset of
   the answer (`res_ts_bit`) and of each termination answer (`term_ts_bit`);
   the device writes the time there before sending. Length and every other
   bit stay the same.
3. **The engine finds the offsets** by decoding the message and encoding it
   again with two time stamps (2^49 and 2^56−1) of the same length: the first
   differing bit less one is the field's start. For the EV's request it checks
   that the EV's bits before the field are the re-encoding's; for its own
   answers, that the re-encoding is the same bit for bit. A loop request that
   fails the check gets no pair: the loop goes to the server like every other
   request (correct, more traffic).
4. **Change detection without the time.** A new pair goes to the device only
   when the answer changes; the engine compares the answer with a fixed time
   written in (`Pair.Key`).
5. **Data from the loop.** The sample the device forwards once per
   `loop_sample_s` is decoded by the engine (SoC, Dynamic-mode needs) and
   journalled; nothing is decoded on the device.
6. **Rising time stamps.** Every time stamp the EV gets is higher than the one
   before (V2G20-1537), whoever sent it. The pair carries the time stamp it
   was encoded with (`ts`); the device writes max(its clock, `ts` + 1, its
   last one + 1) and reports its last one in `last_ts` of every `v2g/up`; the
   engine keeps the highest one of the session (`State.LastTS`) and answers
   above it. The device's clock (UTC, from the network or the server's
   `t_srv_*` fields) is used only for this, never for the server's events
   (ADR-003).
7. **MeterInfo.** When the EV sets MeterInfoRequested, every answer carries
   MeterInfo (V2G20-1082), the cached one too: the engine puts the energy of
   the digital session from the device's readings into the pair and pushes a
   new pair when the whole Wh changes (about once per `telemetry_s`). The
   value is informational (current at the nominal voltage), not a certified
   meter.
8. **Schedule renegotiation** runs on the side stream 0x8101 next to the loop
   (V2G20-1045): its messages have another payload type and always miss.
9. **Compatibility.** The pair has `kind: prefix` and the new fields. A device
   that does not know them compares all bytes, never hits and forwards every
   loop request: no wrong answer, only traffic.

Reference implementations: `exi.PrefixMatch` and `exi.WriteTimeStamp` (Go);
the device firmware does the same in a few lines without an EXI library.

## Consequences

- The device stays a byte-level proxy; the only new logic is a bit compare and
  a 64-bit write.
- Traffic and server load stay those of ISO 15118-2: one loop request per
  `loop_sample_s`, plus misses.
- The EV's changing values reach the operator at the sample rate, not per
  loop request.

## Open

- How closely vehicles check the station's time stamp against their own
  clock (the standard requires only that it rises).
- Bit offsets in the recordings of real EVs and other encoders (EXI options
  are fixed by the standard; to be confirmed on recorded sessions).
