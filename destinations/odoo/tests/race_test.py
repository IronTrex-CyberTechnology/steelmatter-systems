"""Direct Odoo race tests for the steelmatter_r1 module (D3).

Runs against the local sandbox (destinations/odoo/sandbox/up) over Odoo's
JSON-RPC, with the real executor, observer and admin identities, and checks
the actual Odoo and anchor records, not only the replies.

    python3 -I destinations/odoo/tests/race_test.py -v

Pass conditions: never an order together with FENCED, never two orders for
one op_ref, the tombstone survives deletion, and the journal sequence has no
gaps or duplicates under concurrent writes.
"""

import os
import sys
import unittest
import uuid

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from sandbox_client import (  # noqa: E402
    RPCError, action, admin, anchor_records, executor, observer, odoo_sql, orders_for, outcome, run_together,
)

ROUNDS = int(os.environ.get("RACE_ROUNDS", "25"))
EXECUTOR = OBSERVER = ADMIN = None
RUN = uuid.uuid4().hex[:8]


def setUpModule():
    global EXECUTOR, OBSERVER, ADMIN
    EXECUTOR, OBSERVER, ADMIN = executor(), observer(), admin()


def ref(name, i=0):
    return "race-%s-%s-%d" % (RUN, name, i)


class CreateVersusCreate(unittest.TestCase):
    def test_concurrent_creates_make_one_order(self):
        for i in range(ROUNDS):
            op = ref("cc", i)
            a = action(op)
            res = run_together(*[lambda: EXECUTOR.api("sm_create", a)] * 6)
            outs = sorted(outcome(r) for r in res)
            self.assertEqual(outs.count("CREATED"), 1, (op, outs))
            self.assertEqual(outs.count("EXISTS"), 5, (op, outs))
            self.assertEqual(orders_for(op), 1, op)
            ids = {r["destination"]["id"] for r in res}
            self.assertEqual(len(ids), 1, (op, ids))

    def test_same_reference_different_request_never_makes_a_second_order(self):
        for i in range(ROUNDS // 2):
            op = ref("cd", i)
            res = run_together(lambda: EXECUTOR.api("sm_create", action(op)),
                               lambda: EXECUTOR.api("sm_create", action(op, permit_digest="sha256:other")))
            outs = sorted(outcome(r) for r in res)
            self.assertIn(outs, (["CREATED", "MISMATCH"],), (op, outs))
            self.assertEqual(orders_for(op), 1, op)


class CreateVersusFence(unittest.TestCase):
    def test_order_and_fenced_never_both(self):
        seen = set()
        for i in range(ROUNDS * 2):
            op = ref("cf", i)
            a = action(op)
            c, f = run_together(lambda: EXECUTOR.api("sm_create", a), lambda: EXECUTOR.api("sm_fence", op, 0))
            pair = (outcome(c), outcome(f))
            seen.add(pair)
            n = orders_for(op)
            look = OBSERVER.api("sm_lookup", op)
            if pair == ("CREATED", "EXISTS"):
                self.assertEqual(n, 1, op)
                self.assertEqual(look["outcome"], "FOUND", op)
                self.assertEqual(anchor_records(op), ["CREATE_INTENT", "CREATE_DONE"], op)
            elif pair == ("REFUSED_FENCED", "FENCED"):
                self.assertEqual(n, 0, op)
                self.assertEqual(look["outcome"], "FENCED", op)
                self.assertTrue(f["block_committed"] and f["prior_create"] is False, (op, f))
                self.assertEqual(anchor_records(op), ["BLOCK"], op)
            else:
                self.fail("unexpected outcome pair %s for %s" % (pair, op))
        print("\n  create/fence orderings seen: %s" % sorted(seen))

    def test_delayed_create_after_fence_is_refused(self):
        op = ref("late")
        f = EXECUTOR.api("sm_fence", op, 0)
        self.assertEqual(f["outcome"], "FENCED")
        for _ in range(3):
            self.assertEqual(EXECUTOR.api("sm_create", action(op))["outcome"], "REFUSED_FENCED")
        self.assertEqual(orders_for(op), 0)

    def test_lost_fence_reply_then_retry_is_fenced_again(self):
        op = ref("refence")
        first, second = EXECUTOR.api("sm_fence", op, 0), EXECUTOR.api("sm_fence", op, 0)
        self.assertEqual((first["outcome"], second["outcome"]), ("FENCED", "FENCED"))
        self.assertTrue(second["block_committed"] and second["prior_create"] is False)
        self.assertEqual(anchor_records(op), ["BLOCK"])
        self.assertEqual(int(odoo_sql("SELECT count(*) FROM steelmatter_journal WHERE op_ref = '%s'" % op)[0]), 1)


class Tombstone(unittest.TestCase):
    def test_deleted_order_keeps_history_and_blocks_a_second_order(self):
        op = ref("del")
        c = EXECUTOR.api("sm_create", action(op))
        self.assertEqual(c["outcome"], "CREATED")
        po_id = int(odoo_sql("SELECT id FROM purchase_order WHERE sm_op_ref = '%s'" % op)[0])
        ADMIN.call("purchase.order", "button_cancel", [po_id])
        ADMIN.call("purchase.order", "unlink", [po_id])
        self.assertEqual(orders_for(op), 0)
        self.assertEqual(odoo_sql("SELECT state, po_id IS NULL FROM steelmatter_operation WHERE op_ref = '%s'" % op), ["created|t"])
        events = odoo_sql("SELECT event FROM steelmatter_journal WHERE op_ref = '%s' ORDER BY seq" % op)
        self.assertEqual(events, ["created", "po_cancelled", "po_deleted"])
        retry = EXECUTOR.api("sm_create", action(op))
        self.assertEqual(retry["outcome"], "EXISTS")
        self.assertEqual(orders_for(op), 0, "a retry after deletion must never recreate the order")
        fence = EXECUTOR.api("sm_fence", op, 0)
        self.assertEqual((fence["outcome"], fence["reason"]), ("EXISTS", "ORDER_DELETED"))
        self.assertEqual(OBSERVER.api("sm_lookup", op)["outcome"], "MISMATCH")

    def test_no_identity_can_delete_or_edit_the_tombstone(self):
        op = ref("tomb")
        EXECUTOR.api("sm_fence", op, 0)
        oid = int(odoo_sql("SELECT id FROM steelmatter_operation WHERE op_ref = '%s'" % op)[0])
        for who in (EXECUTOR, OBSERVER, ADMIN):
            with self.assertRaises(RPCError):
                who.call("steelmatter.operation", "unlink", [oid])
            with self.assertRaises(RPCError):
                who.call("steelmatter.operation", "write", [oid], {"state": "created"})
        self.assertEqual(odoo_sql("SELECT state FROM steelmatter_operation WHERE id = %d" % oid), ["fenced"])


class SupplierChange(unittest.TestCase):
    def test_supplier_change_racing_create(self):
        for i in range(ROUNDS // 2):
            rev = int(odoo_sql("SELECT sm_revision FROM res_partner WHERE sm_ref = 'sup-desks'")[0])
            sup = int(odoo_sql("SELECT id FROM res_partner WHERE sm_ref = 'sup-desks'")[0])
            op = ref("sup", i)
            a = action(op, supplier_ref="sup-desks", supplier_revision=rev)
            c, _w = run_together(lambda: EXECUTOR.api("sm_create", a),
                                 lambda: ADMIN.call("res.partner", "write", [sup], {"vat": "29BBBBB%04dB1Z6" % (i + 2000)}))
            self.assertIn(outcome(c), ("CREATED", "REFUSED_SUPPLIER_STALE"), op)
            self.assertEqual(orders_for(op), 1 if outcome(c) == "CREATED" else 0, op)
            self.assertEqual(int(odoo_sql("SELECT sm_revision FROM res_partner WHERE id = %d" % sup)[0]), rev + 1)
        stale = EXECUTOR.api("sm_create", action(ref("sup-stale"), supplier_ref="sup-desks", supplier_revision=rev))
        self.assertEqual(stale["outcome"], "REFUSED_SUPPLIER_STALE")
        self.assertTrue(stale["block_committed"] and stale["prior_create"] is False)


class Journal(unittest.TestCase):
    def test_sequence_has_no_gaps_under_concurrent_writes(self):
        fns = []
        for i in range(12):
            op = ref("jr", i)
            fns.append(lambda op=op: EXECUTOR.api("sm_create", action(op)))
            fns.append(lambda op=op: EXECUTOR.api("sm_fence", ref("jf", int(op.rsplit("-", 1)[1])), 0))
        res = run_together(*fns)
        self.assertFalse([r for r in res if not isinstance(r, dict)], res)
        seqs = [int(s) for s in odoo_sql("SELECT seq FROM steelmatter_journal ORDER BY seq")]
        self.assertEqual(seqs, list(range(1, len(seqs) + 1)), "journal sequence must be contiguous")
        head = int(odoo_sql("SELECT last_seq FROM steelmatter_head")[0])
        self.assertEqual(head, seqs[-1])
        listed, cp = [], 0
        while True:
            page = OBSERVER.api("sm_list_since", cp, 7)
            if not page["rows"]:
                break
            listed += [r["seq"] for r in page["rows"]]
            cp = page["next_checkpoint"]
        self.assertEqual(listed, seqs, "paged listing must return every journal row once, in order")


class Identities(unittest.TestCase):
    def test_observer_is_read_only(self):
        with self.assertRaises(RPCError):
            OBSERVER.api("sm_create", action(ref("obs")))
        with self.assertRaises(RPCError):
            OBSERVER.api("sm_fence", ref("obs"), 0)
        with self.assertRaises(RPCError):
            OBSERVER.call("purchase.order", "write", [1], {"notes": "x"})
        self.assertEqual(orders_for(ref("obs")), 0)

    def test_executor_cannot_bypass_the_module(self):
        with self.assertRaises(RPCError):
            EXECUTOR.call("purchase.order", "create", {"partner_id": 1})


if __name__ == "__main__":
    unittest.main()
