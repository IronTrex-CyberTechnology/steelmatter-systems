"""Restore-proof record: the recovery anchor's destination stream (D5 v3).

The anchor is a separate PostgreSQL outside Odoo's rollback domain. The module
appends CREATE_INTENT, BLOCK and CREATE_DONE with the anchor_dest_writer
identity through anchor.append_destination (append-only, hash-chained,
idempotent per op_ref and record type) and reads its own stream.

Configuration comes from the server environment only, never from the Odoo
database, so it is not readable through ir.config_parameter:

    STEELMATTER_ANCHOR_DEST_URL   postgres:// URL of the anchor_dest_writer login
    STEELMATTER_DESTINATION       destination name in the anchor (the engine's company_ref)
    STEELMATTER_ANCHOR_TIMEOUT_MS connect and statement timeout (default 2000)

Any failure raises AnchorUnavailable and the caller fails closed. The URL
contains a password: never log it.
"""

import json
import os

import psycopg2

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


def _connect():
    dsn = os.environ.get("STEELMATTER_ANCHOR_DEST_URL", "")
    if not dsn:
        raise AnchorUnavailable("STEELMATTER_ANCHOR_DEST_URL is not set")
    timeout_ms = int(os.environ.get("STEELMATTER_ANCHOR_TIMEOUT_MS", "2000"))
    try:
        conn = psycopg2.connect(dsn, connect_timeout=max(1, timeout_ms // 1000),
                                options="-c statement_timeout=%d" % timeout_ms)
        conn.autocommit = True
        return conn
    except psycopg2.Error as e:
        raise AnchorUnavailable(type(e).__name__) from None


def proof(op_ref):
    """Returns {create_intent, blocked, create_done} for op_ref, where
    create_done is the CREATE_DONE order facts or None."""
    dest = destination()
    conn = _connect()
    try:
        with conn.cursor() as cr:
            cr.execute(
                "SELECT record_type, payload FROM anchor.destination_records"
                " WHERE destination = %s AND op_ref = %s ORDER BY seq",
                [dest, op_ref],
            )
            rows = cr.fetchall()
    except psycopg2.Error as e:
        raise AnchorUnavailable(type(e).__name__) from None
    finally:
        conn.close()
    p = {"create_intent": False, "blocked": False, "create_done": None}
    for record_type, payload in rows:
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
    conn = _connect()
    try:
        with conn.cursor() as cr:
            cr.execute("SELECT anchor.append_destination(%s, %s, %s, %s::jsonb)",
                       [record_type, dest, op_ref, json.dumps(payload)])
            return cr.fetchone()[0]
    except psycopg2.Error as e:
        raise AnchorUnavailable(type(e).__name__) from None
    finally:
        conn.close()
