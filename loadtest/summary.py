#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
# Copyright Contributors to the project. See NOTICE.
"""All runs in one table: results/summary.csv (one row per run).

    python3 loadtest/summary.py [results]

Reads each results/<label>/ (fleetgen.json, server.json, samples.jsonl,
run.env when gen-run.sh wrote it) and writes one CSV row per run, for
charts: V2G latency and timeouts, activation stage delivery, server CPU.
Also writes results/timeline-<label>.csv per run: the server samples over
time (host CPU, CPU per container) for time-series charts.
"""
import csv
import json
import os
import sys


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


def env(d):
    r = {}
    try:
        with open(os.path.join(d, "run.env")) as f:
            for line in f:
                k, _, v = line.strip().partition("=")
                if k:
                    r[k] = v
    except OSError:
        pass
    return r


def pct(xs, p):
    xs = sorted(xs)
    if not xs:
        return ""
    return xs[min(len(xs) - 1, int(round(p / 100 * (len(xs) - 1))))]


COLS = ["label", "scenario", "engine_build", "devices", "stage_n", "stage_spread_s", "settle", "conn_rate",
        "plug_spread", "sessions_started", "v2g_timeouts", "v2g_failed", "v2g_p50_ms", "v2g_p95_ms", "v2g_p99_ms", "v2g_max_ms",
        "mqtt_lost", "act_devices", "act_delivered", "act_last_s", "act_p50_ms", "act_p95_ms", "act_p99_ms",
        "fcr_d_stages_7_5s", "fcr_d_devices_7_5s", "fcr_d_mw_7_5s_at_250_per_mw",
        "host_cpu_p50", "host_cpu_p95", "host_cpu_max", "postgres_cpu_max", "mqtt_cpu_max", "engines_cpu_max"]


def row(d):
    fg, ss, e = load(d, "fleetgen.json"), samples(d), env(d)
    if not fg:
        return None
    lat, cnt, p = fg.get("latency", {}), fg.get("counts", {}), fg.get("params", {})
    allv = lat.get("v2g.all", {})
    act = fg.get("activation") or {}
    label = os.path.basename(os.path.abspath(d))
    r = {"label": label, "scenario": e.get("SCENARIO", label.split("-")[2] if label.count("-") >= 3 else ""),
         "engine_build": e.get("ENGINE_BUILD", ""), "devices": p.get("n", ""),
         "stage_n": e.get("STAGE_N", act.get("devices", "")), "stage_spread_s": e.get("STAGE_SPREAD", ""),
         "settle": e.get("SETTLE", ""), "conn_rate": e.get("CONN_RATE", ""), "plug_spread": p.get("plug_spread", ""),
         "sessions_started": cnt.get("session.started", 0),
         "v2g_timeouts": sum(v for k, v in cnt.items() if k.startswith("v2g.timeout.")),
         "v2g_failed": sum(v for k, v in cnt.items() if k.startswith("v2g.failed_code.")),
         "v2g_p50_ms": allv.get("p50_ms", ""), "v2g_p95_ms": allv.get("p95_ms", ""),
         "v2g_p99_ms": allv.get("p99_ms", ""), "v2g_max_ms": allv.get("max_ms", ""),
         "mqtt_lost": cnt.get("mqtt.connection_lost", 0)}
    if act:
        last = act.get("deliver_span_s") or 0
        r.update({"act_devices": act.get("devices"), "act_delivered": act.get("delivered"), "act_last_s": round(last, 2),
                  "act_p50_ms": act.get("per_device", {}).get("p50_ms"), "act_p95_ms": act.get("per_device", {}).get("p95_ms"),
                  "act_p99_ms": act.get("per_device", {}).get("p99_ms")})
        # FCR-D (Fingrid): 86 % of the capacity within 7.5 s; stages back to back.
        if last > 0 and act.get("delivered") == act.get("devices"):
            k = int(7.5 // last)
            r.update({"fcr_d_stages_7_5s": k, "fcr_d_devices_7_5s": k * act["devices"],
                      "fcr_d_mw_7_5s_at_250_per_mw": round(k * act["devices"] / 250, 1)})
    host = [s["host"]["cpu_pct"] for s in ss if "host" in s]

    def cmax(prefix):
        v = [x["cpu"] for s in ss for n, x in s.get("containers", {}).items() if n.startswith(prefix)]
        return max(v) if v else ""

    def engines_max():
        tot = [sum(x["cpu"] for n, x in s.get("containers", {}).items() if n.startswith("engine")) for s in ss]
        return round(max(tot)) if tot else ""

    r.update({"host_cpu_p50": pct(host, 50), "host_cpu_p95": pct(host, 95), "host_cpu_max": max(host) if host else "",
              "postgres_cpu_max": cmax("postgres"), "mqtt_cpu_max": cmax("mqtt"), "engines_cpu_max": engines_max()})
    return r


def timeline(d, out):
    ss = samples(d)
    if not ss:
        return
    names = sorted({n for s in ss for n in s.get("containers", {})})
    t0 = ss[0]["t"]
    with open(out, "w", newline="") as f:
        w = csv.writer(f)
        w.writerow(["t_s", "unix_s", "host_cpu_pct"] + [f"cpu_{n}" for n in names])
        for s in ss:
            c = s.get("containers", {})
            w.writerow([round(s["t"] - t0, 1), s["t"], s.get("host", {}).get("cpu_pct", "")] +
                       [c.get(n, {}).get("cpu", "") for n in names])


def main(root):
    rows = []
    for name in sorted(os.listdir(root)):
        d = os.path.join(root, name)
        if not os.path.isdir(d):
            continue
        r = row(d)
        if r:
            rows.append(r)
            timeline(d, os.path.join(root, f"timeline-{name}.csv"))
    with open(os.path.join(root, "summary.csv"), "w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=COLS, extrasaction="ignore")
        w.writeheader()
        w.writerows(rows)
    print(f"{len(rows)} runs -> {os.path.join(root, 'summary.csv')}")


if __name__ == "__main__":
    main(sys.argv[1] if len(sys.argv) > 1 else "results")
