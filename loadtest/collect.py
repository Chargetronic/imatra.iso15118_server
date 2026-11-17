#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
# Copyright Contributors to the project. See NOTICE.

"""Load test, server side: samples the stack while a scenario runs; on SIGTERM / Ctrl-C
writes the server side of the report and exits.

    python3 loadtest/collect.py --out results/<label>

Every --every s (default 5): host CPU (/proc/stat), memory, disk I/O, CPU and
memory of every container (docker stats), Valkey (INFO), Postgres
(pg_stat_database, connections), broker ($SYS), engine / relay / projector
metrics (their /metrics, read with `engine get` inside the containers).
At the end: the plug-in summaries, orphans, limits and reaction times of the
run window from Postgres. Writes samples.jsonl and server.json.
Standard library only; needs docker on the host.
"""
import argparse
import datetime as dt
import json
import os
import re
import signal
import subprocess
import sys
import time

P = os.environ.get("LT_PROJECT", "imatra-harness-server")  # compose project of the deployment
ENV = {}


def sh(args, timeout=20):
    try:
        r = subprocess.run(args, capture_output=True, text=True, timeout=timeout)
        return r.stdout if r.returncode == 0 else ""
    except Exception:
        return ""


def containers():
    out = sh(["docker", "ps", "--filter", f"label=com.docker.compose.project={P}", "--format", "{{.Names}}"])
    return sorted(n for n in out.split() if n)


def psql(q):
    return sh(["docker", "exec", f"{P}-postgres-1", "psql", "-U", ENV.get("POSTGRES_USER", "imatra"),
               "-d", ENV.get("POSTGRES_DB", "imatra"), "-AtF", "\t", "-c", q], timeout=60)


def cpu_ticks():
    with open("/proc/stat") as f:
        v = [int(x) for x in f.readline().split()[1:]]
    idle = v[3] + v[4]
    return sum(v), idle, v[4]


def disk():
    r = w = 0
    with open("/proc/diskstats") as f:
        for line in f:
            p = line.split()
            if re.fullmatch(r"(sd[a-z]+|vd[a-z]+|nvme\d+n\d+|xvd[a-z]+)", p[2]):
                r += int(p[5]) * 512
                w += int(p[9]) * 512
    return r, w


def meminfo():
    m = {}
    with open("/proc/meminfo") as f:
        for line in f:
            k, v = line.split(":")
            m[k] = int(v.split()[0]) * 1024
    return m["MemTotal"], m["MemTotal"] - m["MemAvailable"]


def size(s):
    m = re.match(r"([\d.]+)\s*([KMGT]?i?B)", s.strip())
    if not m:
        return 0
    f = {"B": 1, "KiB": 1024, "MiB": 1024**2, "GiB": 1024**3, "TiB": 1024**4,
         "kB": 1e3, "KB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12}.get(m.group(2), 1)
    return float(m.group(1)) * f


def docker_stats():
    out = sh(["docker", "stats", "--no-stream", "--format", "{{json .}}"], timeout=30)
    res = {}
    for line in out.splitlines():
        try:
            d = json.loads(line)
        except ValueError:
            continue
        if not d.get("Name", "").startswith(P + "-"):
            continue
        res[d["Name"][len(P) + 1:]] = {"cpu": float(d["CPUPerc"].rstrip("%") or 0),
                                       "mem": size(d["MemUsage"].split("/")[0])}
    return res


def valkey():
    out = sh(["docker", "exec", f"{P}-valkey-1", "valkey-cli", "info"])
    d = dict(line.split(":", 1) for line in out.splitlines() if ":" in line and not line.startswith("#"))
    keys = 0
    for k, v in d.items():
        if k.startswith("db"):
            keys += int(v.split(",")[0].split("=")[1])
    return {"used_memory": int(d.get("used_memory", 0)), "cpu_s": float(d.get("used_cpu_sys", 0)) + float(d.get("used_cpu_user", 0)),
            "ops": int(d.get("total_commands_processed", 0)), "clients": int(d.get("connected_clients", 0)), "keys": keys,
            "stream": int((sh(["docker", "exec", f"{P}-valkey-1", "valkey-cli", "xlen", "events"]).strip() or 0))}


def postgres():
    out = psql("select numbackends, xact_commit, tup_inserted, tup_updated, blks_read, blks_hit, "
               "(select count(*) from pg_stat_activity where state = 'active'), "
               "(select count(*) from pg_stat_activity where wait_event_type = 'Lock'), "
               "pg_database_size(current_database()) from pg_stat_database where datname = current_database()")
    p = out.strip().split("\t")
    if len(p) < 9:
        return {}
    k = ["backends", "commits", "inserted", "updated", "blks_read", "blks_hit", "active", "lock_waits", "db_bytes"]
    return dict(zip(k, (int(x) for x in p)))


def broker():
    pw = ENV.get("MQTT_SERVER_PASSWORD", "")
    res = {}
    for k, t in (("clients", "$SYS/broker/clients/connected"), ("rx_1min", "$SYS/broker/load/messages/received/1min"),
                 ("tx_1min", "$SYS/broker/load/messages/sent/1min")):
        v = sh(["docker", "exec", f"{P}-mqtt-1", "mosquitto_sub", "-h", "localhost", "-p", "1883", "-u", "server",
                "-P", pw, "-t", t, "-C", "1", "-W", "12"], timeout=15).strip()
        try:
            res[k] = float(v)
        except ValueError:
            pass
    return res


def metrics(name, port):
    out = sh(["docker", "exec", f"{P}-{name}", "/usr/local/bin/engine", "get", f"http://127.0.0.1:{port}/metrics"])
    m = {}
    for line in out.splitlines():
        if line.startswith("#") or " " not in line:
            continue
        k, v = line.rsplit(" ", 1)
        try:
            m[k] = float(v)
        except ValueError:
            pass
    return m


def app_metrics(names):
    res = {}
    for n in names:
        svc = n.rsplit("-", 1)[0]
        port = {"engine": 8090, "relay": 8091, "projector": 8092}.get(svc)
        if port:
            res[n] = metrics(n, port)
    return res


def load_env(path):
    try:
        with open(path) as f:
            for line in f:
                m = re.match(r"^([A-Z_][A-Z0-9_]*)=(.*)$", line.strip())
                if m:
                    ENV[m.group(1)] = m.group(2).strip("'\"")
    except OSError:
        pass


def finish(out, start, samples):
    t0 = start.strftime("%Y-%m-%d %H:%M:%S+00")
    q = lambda s: [r.split("\t") for r in psql(s).strip().splitlines() if r]
    db = {}
    r = q(f"""select count(*), count(*) filter (where plug_id is not null), count(*) filter (where ended_at is not null),
                 count(*) filter (where end_reason = 'no unplug seen'), count(*) filter (where v2g_sessions > 0),
                 count(*) filter (where energy_wh > 0), coalesce(sum(limits_applied), 0)
              from session_summaries where started_at >= '{t0}'""")
    if r:
        db["summaries"] = dict(zip(["plug_ins", "with_plug_id", "ended", "no_unplug_seen", "with_v2g", "with_energy", "limits_applied"],
                                   (int(x) for x in r[0])))
    r = q("select count(*) from projector_orphans")
    if r:
        db["orphans"] = int(r[0][0])
    r = q(f"""select count(*), count(acked_at), percentile_disc(0.5) within group (order by reaction_ms),
                 percentile_disc(0.95) within group (order by reaction_ms), max(reaction_ms)
              from limits where created_at >= '{t0}'""")
    if r:
        db["limits"] = dict(zip(["created", "acked", "reaction_p50_ms", "reaction_p95_ms", "reaction_max_ms"],
                                (int(x) if x else None for x in r[0])))
    r = q(f"select count(*) from domain_events where recorded_at >= '{t0}'")
    if r:
        db["feed_events"] = int(r[0][0])
    with open(os.path.join(out, "server.json"), "w") as f:
        json.dump({"start": start.isoformat(), "end": dt.datetime.now(dt.timezone.utc).isoformat(),
                   "samples": samples, "db": db, "host": {"vcpu": os.cpu_count(), "mem_bytes": meminfo()[0]}}, f, indent=1)


def main():
    a = argparse.ArgumentParser()
    a.add_argument("--out", required=True)
    a.add_argument("--every", type=float, default=5)
    a.add_argument("--env", default=os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", ".env"))
    o = a.parse_args()
    load_env(o.env)
    os.makedirs(o.out, exist_ok=True)
    start = dt.datetime.now(dt.timezone.utc)
    stop = []
    signal.signal(signal.SIGTERM, lambda *_: stop.append(1))
    signal.signal(signal.SIGINT, lambda *_: stop.append(1))
    prev_cpu, prev_disk, prev_t = cpu_ticks(), disk(), time.time()
    n = 0
    with open(os.path.join(o.out, "samples.jsonl"), "w") as f:
        while not stop:
            t = time.time()
            cpu, dk = cpu_ticks(), disk()
            dtot, didle, dwait = cpu[0] - prev_cpu[0], cpu[1] - prev_cpu[1], cpu[2] - prev_cpu[2]
            el = max(t - prev_t, 1e-3)
            s = {"t": round(t, 1),
                 "host": {"cpu_pct": round(100 * (1 - didle / dtot), 1) if dtot else 0,
                          "iowait_pct": round(100 * dwait / dtot, 1) if dtot else 0,
                          "mem_used": meminfo()[1],
                          "disk_r_bps": round((dk[0] - prev_disk[0]) / el), "disk_w_bps": round((dk[1] - prev_disk[1]) / el)},
                 "containers": docker_stats(), "valkey": valkey(), "postgres": postgres()}
            if n % 3 == 0:  # every 3rd sample: broker and application metrics (slower)
                names = containers()  # engines may be added or replaced during the run (scenario scale)
                s["broker"] = broker()
                s["metrics"] = app_metrics(names)
            prev_cpu, prev_disk, prev_t = cpu, dk, t
            f.write(json.dumps(s) + "\n")
            f.flush()
            n += 1
            while not stop and time.time() - t < o.every:
                time.sleep(0.2)
    finish(o.out, start, n)
    print(f"{n} samples in {o.out}", file=sys.stderr)


if __name__ == "__main__":
    main()
