"""Shared helpers for the direct Odoo tests: JSON-RPC identities, read
queries against the sandbox databases, and the sandbox's failure controls
(Odoo database snapshot and restore, anchor stop and start)."""

import json
import os
import subprocess
import threading
import time
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.abspath(os.path.join(HERE, "..", "..", ".."))
SANDBOX = os.path.join(ROOT, "destinations", "odoo", "sandbox")
PORT = os.environ.get("STEELMATTER_ODOO_PORT", "18069")
URL = "http://127.0.0.1:%s/jsonrpc" % PORT
DB = os.environ.get("ODOO_DB_NAME", "smr1")
DESTINATION = os.environ.get("STEELMATTER_DESTINATION", "acme-in")
ANCHOR_DB = "steelmatter_anchor_odoo"
SNAPSHOT_DB = DB + "_snapshot"


def _secrets():
    out = {}
    with open(os.path.join(ROOT, ".local", "odoo-sandbox.env")) as f:
        for line in f:
            line = line.strip()
            if line and not line.startswith("#") and "=" in line:
                k, v = line.split("=", 1)
                out[k] = v.strip("'")
    return out


SECRETS = _secrets()


class RPCError(Exception):
    pass


def rpc(service, method, *args):
    body = json.dumps({"jsonrpc": "2.0", "method": "call", "params": {"service": service, "method": method, "args": list(args)},
                       "id": 1}).encode()
    req = urllib.request.Request(URL, body, {"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=60) as resp:
        r = json.load(resp)
    if "error" in r:
        d = r["error"].get("data", {})
        raise RPCError("%s: %s" % (d.get("name"), d.get("message")))
    return r.get("result")  # absent when the method returns None


class Identity:
    def __init__(self, login, password):
        self.login, self.password = login, password
        self.uid = rpc("common", "login", DB, login, password)
        if not self.uid:
            raise RuntimeError("login failed for %s" % login)

    def call(self, model, method, *args, **kw):
        return rpc("object", "execute_kw", DB, self.uid, self.password, model, method, list(args), kw)

    def api(self, method, *args):
        return self.call("steelmatter.api", method, *args)


def executor():
    return Identity("sm_executor", SECRETS["SM_EXECUTOR_PASSWORD"])


def observer():
    return Identity("sm_observer", SECRETS["SM_OBSERVER_PASSWORD"])


def admin():
    return Identity("admin", SECRETS["ODOO_ADMIN_PASSWORD"])


# Compose needs its variables to parse the file; these commands only exec into,
# stop or start existing containers, so placeholders are enough.
_COMPOSE_ENV = dict(os.environ, POSTGRES_IMAGE="unused", ODOO_IMAGE="unused", ODOO_DB_PASSWORD="", ANCHOR_PG_PASSWORD="",
                    ANCHOR_DEST_URL_IN_NETWORK="", STEELMATTER_DESTINATION=DESTINATION, ODOO_DB_NAME=DB,
                    ODOO_PORT=PORT, ANCHOR_PORT="55442")


def compose(*args):
    return subprocess.run(["docker", "compose", "-f", os.path.join(SANDBOX, "compose.yaml"), *args],
                          capture_output=True, text=True, check=True, env=_COMPOSE_ENV).stdout


def sql(service, user, database, query):
    out = compose("exec", "-T", service, "psql", "-v", "ON_ERROR_STOP=1", "-U", user, "-d", database, "-tAc", query)
    return [ln for ln in out.splitlines() if ln]


def odoo_sql(query):
    return sql("odoo-db", "odoo", DB, query)


def anchor_records(op_ref):
    q = ("SELECT record_type FROM anchor.destination_records WHERE destination = '%s' AND op_ref = '%s' ORDER BY seq"
         % (DESTINATION, op_ref.replace("'", "''")))
    return sql("anchor-db", "postgres", ANCHOR_DB, q)


def anchor_count():
    return int(sql("anchor-db", "postgres", ANCHOR_DB, "SELECT count(*) FROM anchor.destination_records")[0])


def orders_for(op_ref):
    return int(odoo_sql("SELECT count(*) FROM purchase_order WHERE sm_op_ref = '%s'" % op_ref)[0])


def supplier_revision(ref="sup-chairs"):
    return int(odoo_sql("SELECT sm_revision FROM res_partner WHERE sm_ref = '%s'" % ref)[0])


def action(op_ref, **over):
    a = {
        "op_ref": op_ref, "action_revision": 1, "company_ref": DESTINATION, "delivery_ref": "wh-chennai",
        "supplier_ref": "sup-chairs", "supplier_revision": supplier_revision(),
        "lines": [{"item_ref": "chair-ergo", "quantity": 10, "unit_price_minor": 600000}],
        "amount_minor": 6000000, "currency": "INR", "permit_digest": "sha256:" + op_ref, "expected_min_seq": 0,
    }
    a.update(over)
    return a


def run_together(*fns):
    """Starts every fn at the same moment; returns results (an exception is
    returned, not raised)."""
    barrier = threading.Barrier(len(fns))
    results = [None] * len(fns)

    def go(i, fn):
        barrier.wait()
        try:
            results[i] = fn()
        except Exception as e:  # noqa: BLE001 - reported to the test
            results[i] = e

    threads = [threading.Thread(target=go, args=(i, fn)) for i, fn in enumerate(fns)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    return results


def outcome(r):
    return r["outcome"] if isinstance(r, dict) else "ERROR(%s)" % r


# ------------------------------------------------------------ failure controls

def wait_odoo(timeout=90):
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            with urllib.request.urlopen("http://127.0.0.1:%s/web/health" % PORT, timeout=2):
                return
        except OSError:
            time.sleep(1)
    raise RuntimeError("Odoo did not come back")


def snapshot_odoo():
    """Takes an Odoo database backup (a template copy). Odoo is stopped while
    it is taken, so no connection holds the database."""
    compose("stop", "odoo")
    try:
        sql("odoo-db", "odoo", "postgres", "DROP DATABASE IF EXISTS %s" % SNAPSHOT_DB)
        sql("odoo-db", "odoo", "postgres", "CREATE DATABASE %s TEMPLATE %s" % (SNAPSHOT_DB, DB))
    finally:
        compose("start", "odoo")
        wait_odoo()


def restore_odoo():
    """Restores Odoo's database from the snapshot, as an operator restoring an
    old backup would. The anchor is a separate database and is untouched."""
    compose("stop", "odoo")
    try:
        sql("odoo-db", "odoo", "postgres", "DROP DATABASE %s WITH (FORCE)" % DB)
        sql("odoo-db", "odoo", "postgres", "CREATE DATABASE %s TEMPLATE %s" % (DB, SNAPSHOT_DB))
    finally:
        compose("start", "odoo")
        wait_odoo()


def stop_anchor():
    compose("stop", "anchor-db")


def start_anchor(timeout=60):
    compose("start", "anchor-db")
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            compose("exec", "-T", "anchor-db", "pg_isready", "-U", "postgres")
            return
        except subprocess.CalledProcessError:
            time.sleep(1)
    raise RuntimeError("anchor did not come back")
