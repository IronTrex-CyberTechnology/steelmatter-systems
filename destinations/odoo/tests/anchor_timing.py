"""Anchor latency baseline for the module (D5 v3, "Kaif measures p50/p95";
the anchor timeout is a contract value to agree with Uday).

Measures, on the local sandbox:
  inside the Odoo container, with the module's own anchor.py (exactly as
  sm_create and sm_fence use it, with its connection pool):
    - anchor read  (proof of one op_ref)
    - anchor write (append_destination; takes the destination stream head lock)
    - anchor write with 8 concurrent writers (they queue on that lock)
    - for comparison: connect only, and read/write on a reused connection
  from the host over JSON-RPC (what the engine sees):
    - sm_create, sm_fence and sm_lookup end to end

Benchmark appends go to a separate destination name (bench-<run>), never to
the sandbox's real destination. Synthetic sandbox only.

    python3 -I destinations/odoo/tests/anchor_timing.py [--n 300]
"""

import argparse
import json
import os
import statistics
import sys
import time
import uuid

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from sandbox_client import action, compose, executor, observer  # noqa: E402

IN_CONTAINER = r'''
import importlib.util, json, os, statistics, sys, threading, time
import psycopg2
spec = importlib.util.spec_from_file_location("sm_anchor", "/mnt/extra-addons/steelmatter_r1/models/anchor.py")
anchor = importlib.util.module_from_spec(spec); spec.loader.exec_module(anchor)
N, RUN = int(sys.argv[1]), sys.argv[2]

def timed(fn, n):
    out = []
    for i in range(n):
        t = time.perf_counter(); fn(i); out.append((time.perf_counter() - t) * 1000)
    return out

res = {}
res["connect only"] = timed(lambda i: anchor._connect().close(), N)
res["read (module)"] = timed(lambda i: anchor.proof("bench-missing-%d" % i), N)
res["write (module)"] = timed(lambda i: anchor.append("BLOCK", "w-%s-%d" % (RUN, i), {"op_ref": "x", "reason": "bench"}), N)

conn = anchor._connect()
def read_reused(i):
    with conn.cursor() as cr:
        cr.execute("SELECT record_type, payload FROM anchor.destination_records WHERE destination = %s AND op_ref = %s",
                   [anchor.destination(), "bench-missing-%d" % i]); cr.fetchall()
def write_reused(i):
    with conn.cursor() as cr:
        cr.execute("SELECT anchor.append_destination(%s, %s, %s, %s::jsonb)",
                   ["BLOCK", anchor.destination(), "r-%s-%d" % (RUN, i), json.dumps({"op_ref": "x", "reason": "bench"})]); cr.fetchone()
res["read (reused connection)"] = timed(read_reused, N)
res["write (reused connection)"] = timed(write_reused, N)
conn.close()

conc, lock = [], threading.Lock()
def worker(w):
    for i in range(N // 8):
        t = time.perf_counter()
        anchor.append("BLOCK", "c-%s-%d-%d" % (RUN, w, i), {"op_ref": "x", "reason": "bench"})
        with lock: conc.append((time.perf_counter() - t) * 1000)
ts = [threading.Thread(target=worker, args=(w,)) for w in range(8)]
t0 = time.perf_counter()
[t.start() for t in ts]; [t.join() for t in ts]
res["write (module), 8 concurrent"] = conc
res["_throughput_8_writers_per_s"] = [len(conc) / (time.perf_counter() - t0)]

# Same 8 writers, each on its own reused connection: separates the cost of
# connecting from queueing on the destination stream head lock.
conc2 = []
def worker_reused(w):
    c = anchor._connect()
    for i in range(N // 8):
        t = time.perf_counter()
        with c.cursor() as cr:
            cr.execute("SELECT anchor.append_destination(%s, %s, %s, %s::jsonb)",
                       ["BLOCK", anchor.destination(), "cr-%s-%d-%d" % (RUN, w, i), json.dumps({"op_ref": "x", "reason": "bench"})])
            cr.fetchone()
        with lock: conc2.append((time.perf_counter() - t) * 1000)
    c.close()
ts = [threading.Thread(target=worker_reused, args=(w,)) for w in range(8)]
t0 = time.perf_counter()
[t.start() for t in ts]; [t.join() for t in ts]
res["write, 8 writers, reused connections"] = conc2
res["_throughput_8_reused_per_s"] = [len(conc2) / (time.perf_counter() - t0)]
print(json.dumps(res))
'''


def pct(xs, p):
    xs = sorted(xs)
    return xs[min(len(xs) - 1, int(round(p / 100 * (len(xs) - 1))))]


def row(name, xs):
    return "| %-34s | %5d | %7.2f | %7.2f | %7.2f | %7.2f |" % (name, len(xs), pct(xs, 50), pct(xs, 95), pct(xs, 99), max(xs))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--n", type=int, default=300, help="samples per anchor measurement")
    ap.add_argument("--rpc", type=int, default=60, help="samples per JSON-RPC call")
    args = ap.parse_args()
    run = uuid.uuid4().hex[:8]

    out = compose("exec", "-T", "-e", "STEELMATTER_DESTINATION=bench-" + run, "odoo",
                  "python3", "-c", IN_CONTAINER, str(args.n), run)
    res = json.loads(out.strip().splitlines()[-1])
    throughput = res.pop("_throughput_8_writers_per_s")[0]
    throughput_reused = res.pop("_throughput_8_reused_per_s")[0]

    ex, ob = executor(), observer()

    def rpc_timed(fn, n):
        xs = []
        for i in range(n):
            t = time.perf_counter()
            r = fn(i)
            xs.append((time.perf_counter() - t) * 1000)
            assert isinstance(r, dict) and r.get("outcome"), r
        return xs

    a0 = action("t-%s-0" % run)
    rpc = {
        "sm_create (CREATED)": rpc_timed(lambda i: ex.api("sm_create", dict(a0, op_ref="t-%s-c%d" % (run, i),
                                                                              permit_digest="sha256:t%d" % i)), args.rpc),
        "sm_create retry (EXISTS)": rpc_timed(lambda i: ex.api("sm_create", dict(a0, op_ref="t-%s-c%d" % (run, i),
                                                                                   permit_digest="sha256:t%d" % i)), args.rpc),
        "sm_fence (FENCED)": rpc_timed(lambda i: ex.api("sm_fence", "t-%s-f%d" % (run, i), 0), args.rpc),
        "sm_lookup (FOUND)": rpc_timed(lambda i: ob.api("sm_lookup", "t-%s-c%d" % (run, i)), args.rpc),
    }

    print("Anchor and module latency, local sandbox (ms)\n")
    print("| %-34s | %5s | %7s | %7s | %7s | %7s |" % ("measurement", "n", "p50", "p95", "p99", "max"))
    print("|" + "-" * 36 + "|" + "-" * 7 + "|" + "-" * 9 + "|" + "-" * 9 + "|" + "-" * 9 + "|" + "-" * 9 + "|")
    for k, xs in res.items():
        print(row(k, xs))
    for k, xs in rpc.items():
        print(row(k, xs))
    print("\n8 concurrent anchor writers: %.0f appends/s through the module, %.0f/s on dedicated connections"
          % (throughput, throughput_reused))
    print(json.dumps({"anchor": {k: {"p50": pct(v, 50), "p95": pct(v, 95), "p99": pct(v, 99), "max": max(v)} for k, v in res.items()},
                      "rpc": {k: {"p50": pct(v, 50), "p95": pct(v, 95), "p99": pct(v, 99), "max": max(v)} for k, v in rpc.items()},
                      "writers_8_per_s": throughput, "writers_8_reused_per_s": throughput_reused}, indent=None))


if __name__ == "__main__":
    main()
