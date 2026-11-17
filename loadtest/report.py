#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
# Copyright Contributors to the project. See NOTICE.

"""One scenario's report (Markdown) from fleetgen.json
(generator), server.json and samples.jsonl (server, collect.py).

    python3 loadtest/report.py results/<label> > results/<label>/report.md
"""
import json
import os
import sys

# targets: ISO 15118-2 table 109 (EVCC 2 s, SECC 1.5 s), the stage plan
SECC_S = 1.5
STAGE_S = 2.0  # the stage of the activation delivered within (functional test task)


def load(d, name):
    try:
        with open(os.path.join(d, name)) as f:
            return json.load(f)
    except (OSError, ValueError):
        return {}


def samples(d):
    out = []
    try:
        with open(os.path.join(d, "samples.jsonl")) as f:
            for line in f:
                try:
                    out.append(json.loads(line))
                except ValueError:
                    pass
    except OSError:
        pass
    return out


def mb(b):
    return f"{b / 2**20:,.0f} MB".replace(",", " ")


def num(x, nd=0):
    if x is None:
        return "—"
    return f"{x:,.{nd}f}".replace(",", " ")


def pct(xs, p):
    xs = sorted(x for x in xs if x is not None)
    if not xs:
        return None
    return xs[min(len(xs) - 1, int(p * (len(xs) - 1) + 0.5))]


def ok(cond):
    return "✅" if cond else "❌"


def main(d):
    fg, sv, ss = load(d, "fleetgen.json"), load(d, "server.json"), samples(d)
    lat, cnt = fg.get("latency", {}), fg.get("counts", {})
    p = fg.get("params", {})
    timeouts = sum(v for k, v in cnt.items() if k.startswith("v2g.timeout."))
    failed = sum(v for k, v in cnt.items() if k.startswith("v2g.failed_code."))
    allv = lat.get("v2g.all", {})
    act, rec = fg.get("activation"), fg.get("reconnect")
    db = sv.get("db", {})
    summ = db.get("summaries", {})

    L = [f"# Load test run {os.path.basename(os.path.abspath(d))}", ""]
    L.append(f"Start: {p.get('date', '—')}; devices {num(p.get('n'))}; duration {p.get('duration', '—')}; "
             f"plug-ins over {p.get('plug_spread', '—')}, charging {p.get('charge', '—')}; "
             f"server {sv.get('host', {}).get('vcpu', '—')} vCPU / {mb(sv.get('host', {}).get('mem_bytes', 0))}.")
    try:
        with open(os.path.join(d, "events.txt")) as f:
            ev = [x.strip() for x in f if x.strip()]
    except OSError:
        ev = []
    if ev:
        L += ["", "Run events: " + "; ".join(ev) + "."]
    L += ["", "## Result against the targets", "", "| Criterion | Target | Result | |", "|---|---|---|---|"]
    L.append(f"| V2G timeouts (EVCC 2 s, PowerDelivery 5 s) | 0 | {num(timeouts)} | {ok(timeouts == 0)} |")
    if allv:
        L.append(f"| V2G answer p99 (SECC 1.5 s) | < {SECC_S} s | {num(allv.get('p99_ms'))} ms | {ok(allv.get('p99_ms', 1e9) < SECC_S * 1000)} |")
        L.append(f"| V2G answer max | < 2 s | {num(allv.get('max_ms'))} ms | {ok(allv.get('max_ms', 1e9) < 2000)} |")
    L.append(f"| V2G errors (FAILED_*) | 0 | {num(failed)} | {ok(failed == 0)} |")
    if act:
        L.append(f"| Activation stage: delivered | {num(act['devices'])} | {num(act['delivered'])} | {ok(act['delivered'] == act['devices'])} |")
        L.append(f"| Activation stage: last delivery | ≤ {STAGE_S} s | {act['deliver_span_s']:.2f} s | {ok(act['deliver_span_s'] <= STAGE_S)} |")
    if rec:
        L.append(f"| Reconnect: back | {num(rec['devices'])} | {num(rec['reconnected'])} | {ok(rec['reconnected'] == rec['devices'])} |")
    if summ:
        L.append(f"| Plug-in summaries without orphans | 0 | {num(db.get('orphans'))} | {ok(db.get('orphans') == 0)} |")
        L.append(f"| Plug-ins closed by their own unplug | all | {num(summ.get('ended') - summ.get('no_unplug_seen', 0))} of {num(summ.get('plug_ins'))} | "
                 f"{ok(summ.get('no_unplug_seen', 0) == 0)} |")

    L += ["", "## Generator", "", f"Sessions started {num(cnt.get('session.started'))}, ended {num(cnt.get('session.ended'))}; "
          f"loop requests from the cache {num(cnt.get('loop.cache_hit'))}, to the server {num(cnt.get('loop.to_server'))}; "
          f"MQTT connections lost {num(cnt.get('mqtt.connection_lost', 0))}, connect failures {num(cnt.get('mqtt.connect_failed', 0))}.", ""]
    L += ["| Message | n | p50, ms | p95, ms | p99, ms | max, ms |", "|---|---|---|---|---|---|"]
    for k in sorted(lat):
        s = lat[k]
        L.append(f"| {k} | {num(s['n'])} | {num(s['p50_ms'], 1)} | {num(s['p95_ms'], 1)} | {num(s['p99_ms'], 1)} | {num(s['max_ms'], 1)} |")
    if timeouts or failed:
        L += ["", "Timeouts and errors: " + ", ".join(f"{k} {v}" for k, v in sorted(cnt.items())
                                                   if k.startswith(("v2g.timeout.", "v2g.failed_code.")))]
    if act:
        L += ["", f"Activation: {num(act['devices'])} devices, API calls {act['api_accepted']} (errors {act['api_errors']}), "
              f"call to delivery p50 {num(act['per_device']['p50_ms'])} / p95 {num(act['per_device']['p95_ms'])} ms, "
              f"last delivery after {act['deliver_span_s']:.2f} s."]
    if rec:
        L += ["", f"Reconnect: {num(rec['reconnected'])} of {num(rec['devices'])} back within {rec['all_back_s']:.1f} s; "
              f"pairs re-sent to {num(rec['pairs_back'])} devices, the last after {rec['pairs_back_s']:.1f} s."]

    if ss:
        L += ["", "## Server", ""]
        host = [s["host"] for s in ss if "host" in s]
        L.append(f"Host: CPU p50 {num(pct([h['cpu_pct'] for h in host], .5))} %, p95 {num(pct([h['cpu_pct'] for h in host], .95))} %, "
                 f"max {num(max(h['cpu_pct'] for h in host))} %; iowait max {num(max(h['iowait_pct'] for h in host), 1)} %; "
                 f"memory max {mb(max(h['mem_used'] for h in host))}; disk writes max {mb(max(h['disk_w_bps'] for h in host))}/s.")
        L += ["", "| Container | CPU p50, % | CPU p95, % | CPU max, % | Memory max |", "|---|---|---|---|---|"]
        names = sorted({n for s in ss for n in s.get("containers", {})})
        for n in names:
            c = [s["containers"][n] for s in ss if n in s.get("containers", {})]
            cpu = [x["cpu"] for x in c]
            L.append(f"| {n} | {num(pct(cpu, .5))} | {num(pct(cpu, .95))} | {num(max(cpu))} | {mb(max(x['mem'] for x in c))} |")
        L.append("")
        L.append("100 % = one core.")
        vk = [s["valkey"] for s in ss if s.get("valkey")]
        if len(vk) > 1:
            span = ss[-1]["t"] - ss[0]["t"]
            L += ["", f"Valkey: memory max {mb(max(v['used_memory'] for v in vk))}, keys max {num(max(v['keys'] for v in vk))}, "
                  f"feed stream max {num(max(v['stream'] for v in vk))}; CPU on average {100 * (vk[-1]['cpu_s'] - vk[0]['cpu_s']) / span:.0f} % of a core, "
                  f"commands on average {num((vk[-1]['ops'] - vk[0]['ops']) / span)}/s."]
        pg = [s["postgres"] for s in ss if s.get("postgres")]
        if len(pg) > 1:
            span = ss[-1]["t"] - ss[0]["t"]
            L.append(f"Postgres: connections max {max(x['backends'] for x in pg)}, active max {max(x['active'] for x in pg)}, "
                     f"lock waits max {max(x['lock_waits'] for x in pg)}; transactions {num((pg[-1]['commits'] - pg[0]['commits']) / span)}/s, "
                     f"inserts {num((pg[-1]['inserted'] - pg[0]['inserted']) / span)}/s; database grew by {mb(pg[-1]['db_bytes'] - pg[0]['db_bytes'])}.")
        br = [s["broker"] for s in ss if s.get("broker")]
        if br:
            L.append(f"Broker: clients max {num(max(b.get('clients', 0) for b in br))}, messages received max "
                     f"{num(max(b.get('rx_1min', 0) for b in br) / 60)}/s, sent max {num(max(b.get('tx_1min', 0) for b in br) / 60)}/s (one-minute averages).")
        mt = [s["metrics"] for s in ss if s.get("metrics")]
        if mt:
            lag_r = [m[n].get("relay_lag_seconds") for m in mt for n in m if n.startswith("relay")]
            lag_p = [m[n].get("projector_lag_seconds") for m in mt for n in m if n.startswith("projector")]
            back = [m[n].get("engine_worker_backlog") for m in mt for n in m if n.startswith("engine")]
            L.append(f"Feed lag: relay max {num(max([x for x in lag_r if x is not None] or [0]), 2)} s, "
                     f"projector max {num(max([x for x in lag_p if x is not None] or [0]), 2)} s (API data target ≤ 2 s); "
                     f"engine lane backlog max {num(max([x for x in back if x is not None] or [0]))}.")
    if db:
        L += ["", "## Data (Postgres, the run window)", ""]
        if summ:
            L.append(f"Plug-in summaries {num(summ['plug_ins'])}, with a plug-in id {num(summ['with_plug_id'])}, closed {num(summ['ended'])} "
                     f"(no unplug seen {num(summ['no_unplug_seen'])}), with V2G {num(summ['with_v2g'])}, with energy {num(summ['with_energy'])}, "
                     f"limits applied {num(summ['limits_applied'])}; orphans {num(db.get('orphans'))}; feed events {num(db.get('feed_events'))}.")
        lm = db.get("limits", {})
        if lm.get("created"):
            L.append(f"Limits: created {num(lm['created'])}, confirmed {num(lm['acked'])}, reaction p50 {num(lm['reaction_p50_ms'])} / "
                     f"p95 {num(lm['reaction_p95_ms'])} / max {num(lm['reaction_max_ms'])} ms (server clock).")
    print("\n".join(L))


if __name__ == "__main__":
    main(sys.argv[1] if len(sys.argv) > 1 else ".")
