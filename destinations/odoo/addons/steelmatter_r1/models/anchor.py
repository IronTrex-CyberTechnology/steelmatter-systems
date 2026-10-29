"""Restore-proof record: the recovery anchor's destination stream (D5 v3).

The anchor is a separate PostgreSQL outside Odoo's rollback domain. The module
appends CREATE_INTENT, BLOCK and CREATE_DONE with the anchor_dest_writer
identity through anchor.append_destination (append-only, hash-chained,
idempotent per op_ref and record type) and reads its own stream.

Configuration comes from the server environment only, never from the Odoo
database, so it is not readable through ir.config_parameter:

    STEELMATTER_ANCHOR_DEST_URL    postgres:// URL of the anchor_dest_writer login
    STEELMATTER_DESTINATION        destination name in the anchor (the engine's company_ref)
    STEELMATTER_ANCHOR_TIMEOUT_MS  connect and statement timeout (default 2000)
    STEELMATTER_ANCHOR_POOL        idle connections kept for reuse (default 8; 0 = none)

Connections are reused from a small per-process pool: opening one costs about
2.5 ms and, under concurrency, far more (measured 28 Oct 2026, 47 ms p50 with
8 concurrent writers against 0.9 ms on reused connections). A pooled
connection that has gone stale (the anchor restarted) is replaced once; that
retry is safe because reads change nothing and appends are idempotent per
op_ref and record type. Any other failure, including a timeout, raises
AnchorUnavailable and the caller fails closed.

The URL contains a password: never log it.
"""

import json
import os
import threading

import psycopg2
import psycopg2.errors
import psycopg2.extensions

CREATE_INTENT = "CREATE_INTENT"
BLOCK = "BLOCK"
CREATE_DONE = "CREATE_DONE"


class AnchorUnavailable(Exception):
    """The restore-proof record could not be read or written."""


def destination():
    name = os.environ.get("STEELMATTER_DESTINATION", "")
    if not name:
        raise AnchorUnavailable("STEELMATTER_DESTINATION is not set")
    return name


def _dsn():
    dsn = os.environ.get("STEELMATTER_ANCHOR_DEST_URL", "")
    if not dsn:
        raise AnchorUnavailable("STEELMATTER_ANCHOR_DEST_URL is not set")
    return dsn


def _connect(dsn=None):
    dsn = dsn or _dsn()
    timeout_ms = int(os.environ.get("STEELMATTER_ANCHOR_TIMEOUT_MS", "2000"))
    try:
        conn = psycopg2.connect(dsn, connect_timeout=max(1, timeout_ms // 1000),
                                options="-c statement_timeout=%d" % timeout_ms)
        conn.autocommit = True
        return conn
    except psycopg2.Error as e:
        raise AnchorUnavailable(type(e).__name__) from None


# ------------------------------------------------------------------ pool
# Odoo's threaded server runs each request on a new thread and its prefork
# workers fork the process, so the pool is per process (reset after a fork)
# and shared by threads, not thread-local.

_lock = threading.Lock()
_idle = []  # (dsn, connection)
_pid = os.getpid()


def _close(conn):
    try:
        conn.close()
    except psycopg2.Error:
        pass


def _checkout(dsn):
    """Returns (connection, reused)."""
    global _pid, _idle
    stale = []
    with _lock:
        if _pid != os.getpid():
            # Forked: the parent's sockets are not ours to use or close.
            _pid, _idle = os.getpid(), []
        while _idle:
            d, conn = _idle.pop()
            if d == dsn and not conn.closed:
                break
            stale.append(conn)  # closed, or opened with rotated credentials
        else:
            conn = None
    for c in stale:
        _close(c)
    if conn is not None:
        return conn, True
    return _connect(dsn), False


def _checkin(dsn, conn):
    keep = int(os.environ.get("STEELMATTER_ANCHOR_POOL", "8"))
    if not conn.closed and conn.status == psycopg2.extensions.STATUS_READY:
        with _lock:
            if _pid == os.getpid() and len(_idle) < keep:
                _idle.append((dsn, conn))
                return
    _close(conn)


def _run(fn):
    """Runs fn(connection) on a pooled connection; fails closed."""
    dsn = _dsn()
    conn, reused = _checkout(dsn)
    try:
        result = fn(conn)
    except psycopg2.errors.QueryCanceled:
        # The statement timeout: the anchor is slow, not gone. No retry.
        _close(conn)
        raise AnchorUnavailable("QueryCanceled") from None
    except (psycopg2.OperationalError, psycopg2.InterfaceError) as e:
        _close(conn)
        if not reused:
            raise AnchorUnavailable(type(e).__name__) from None
        # A pooled connection went stale (anchor restarted): one retry on a
        # fresh connection.
        conn = _connect(dsn)
        try:
            result = fn(conn)
        except psycopg2.Error as e2:
            _close(conn)
            raise AnchorUnavailable(type(e2).__name__) from None
    except psycopg2.Error as e:
        _close(conn)
        raise AnchorUnavailable(type(e).__name__) from None
    _checkin(dsn, conn)
    return result


# ------------------------------------------------------------------ records

def proof(op_ref):
    """Returns {create_intent, blocked, create_done} for op_ref, where
    create_done is the CREATE_DONE order facts or None."""
    dest = destination()

    def read(conn):
        with conn.cursor() as cr:
            cr.execute(
                "SELECT record_type, payload FROM anchor.destination_records"
                " WHERE destination = %s AND op_ref = %s ORDER BY seq",
                [dest, op_ref],
            )
            return cr.fetchall()

    p = {"create_intent": False, "blocked": False, "create_done": None}
    for record_type, payload in _run(read):
        if record_type == CREATE_INTENT:
            p["create_intent"] = True
        elif record_type == BLOCK:
            p["blocked"] = True
        elif record_type == CREATE_DONE:
            p["create_done"] = payload
    return p


def append(record_type, op_ref, payload):
    """Appends one destination record and returns its anchor sequence. A
    repeat for the same op_ref and type returns the stored sequence."""
    dest = destination()
    body = json.dumps(payload)

    def write(conn):
        with conn.cursor() as cr:
            cr.execute("SELECT anchor.append_destination(%s, %s, %s, %s::jsonb)", [record_type, dest, op_ref, body])
            return cr.fetchone()[0]

    return _run(write)
