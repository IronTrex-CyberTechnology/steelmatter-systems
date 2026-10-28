"""Odoo recovery tests for the steelmatter_r1 module (D5 v3), against the
local sandbox with a real Odoo database restore and a real anchor outage.

    python3 -I destinations/odoo/tests/restore_test.py -v

Covers:
  - case 2: a delayed create after an Odoo restore erased the tombstone is
    refused by the anchor's BLOCK, whatever sequence number it carries
  - case E: a committed order erased by a restore; a retry gets
    REFUSED_PRIOR_ATTEMPT (CREATE_DONE establishes the order, no second order)
  - case G: CREATE_INTENT but no order and no CREATE_DONE; retry or fence is
    a block over a prior create, which the engine holds (HELD), never releases
  - intent written but the Odoo commit failed: HELD, never released
  - anchor down: create and fence are UNAVAILABLE and change nothing
  - the anchor is untouched by the Odoo restore

The engine-side decision is checked with the same rules as Uday's Go code
(connector.Result.ProvesNoOrder and BlockedOverPriorCreate, and settle's
CREATE_DONE path).
"""

import os
import sys
import unittest
import uuid

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from sandbox_client import (  # noqa: E402
    RPCError, action, anchor_count, anchor_records, executor, observer, odoo_sql, orders_for, restore_odoo,
    snapshot_odoo, start_anchor, stop_anchor,
)

RUN = uuid.uuid4().hex[:8]
EXECUTOR = OBSERVER = None
TOMBSTONES = ("REFUSED_FENCED", "REFUSED_SUPPLIER_STALE", "REFUSED_PRIOR_ATTEMPT")


def setUpModule():
    global EXECUTOR, OBSERVER
    EXECUTOR, OBSERVER = executor(), observer()


def ref(name):
    return "rst-%s-%s" % (RUN, name)


def fp(point, name):
    return "fp-%s-%s-%s" % (point, RUN, name)


# The engine's rules (core/connector/connector.go, core/engine/settle.go).
def proves_no_order(r):
    return (r["outcome"] == "FENCED" or r["outcome"] in TOMBSTONES) and r["block_committed"] is True \
        and r["prior_create"] is False and not r["create_done"]


def blocked_over_prior_create(r):
    return (r["outcome"] == "FENCED" or r["outcome"] in TOMBSTONES) and r["prior_create"] is True and not r["create_done"]


def engine_status(r, a):
    """What settle.next decides for result r of authorised action a."""
    def matches(o):
        return bool(o) and o["state"] == "purchase" and o["company_ref"] == a["company_ref"] \
            and o["delivery_ref"] == a["delivery_ref"] and o["supplier_ref"] == a["supplier_ref"] \
            and o["amount_minor"] == a["amount_minor"] and o["currency"] == a["currency"] \
            and o["permit_digest"] == a["permit_digest"] and o["lines"] == a["lines"]
    if r["create_done"] and r["destination"]:
        return "COMMITTED" if matches(r["destination"]) else "MISMATCH"
    if r["outcome"] in ("CREATED", "EXISTS", "FOUND"):
        return "COMMITTED" if matches(r["destination"]) else "MISMATCH"
    if r["outcome"] == "MISMATCH":
        return "MISMATCH"
    if proves_no_order(r):
        return "FENCED"
    if blocked_over_prior_create(r):
        return "HELD"
    return "UNRESOLVED"


def op_row(op_ref):
    return odoo_sql("SELECT state FROM steelmatter_operation WHERE op_ref = '%s'" % op_ref)


class AnchorDown(unittest.TestCase):
    def test_create_and_fence_with_anchor_down_change_nothing(self):
        op = ref("down")
        a = action(op)
        stop_anchor()
        try:
            c = EXECUTOR.api("sm_create", a)
            self.assertEqual((c["outcome"], c["reason"]), ("UNAVAILABLE", "ANCHOR_UNAVAILABLE"))
            self.assertIsNone(c["block_committed"])
            self.assertIsNone(c["prior_create"])
            self.assertEqual(engine_status(c, a), "UNRESOLVED")
            f = EXECUTOR.api("sm_fence", op, 0)
            self.assertEqual(f["outcome"], "UNAVAILABLE")
            self.assertFalse(proves_no_order(f))
            look = OBSERVER.api("sm_lookup", op)
            self.assertEqual(look["outcome"], "NOT_FOUND_NOW")
            self.assertIsNone(look["prior_create"], "unknown flags are null, never false")
            self.assertEqual(orders_for(op), 0)
            self.assertEqual(op_row(op), [], "nothing committed in Odoo while the anchor is down")
        finally:
            start_anchor()
        self.assertEqual(anchor_records(op), [])
        # Nothing was blocked either: the same op_ref can still be created.
        self.assertEqual(EXECUTOR.api("sm_create", a)["outcome"], "CREATED")
        self.assertEqual(orders_for(op), 1)


class AnchorRestart(unittest.TestCase):
    def test_restarted_anchor_leaves_no_stale_connection(self):
        # Warm the module's anchor connection pool, restart the anchor, then
        # write again: a stale pooled connection must be replaced, not turned
        # into UNAVAILABLE once the anchor is back.
        for i in range(3):
            self.assertEqual(EXECUTOR.api("sm_create", action(ref("warm-%d" % i)))["outcome"], "CREATED")
        stop_anchor()
        start_anchor()
        for i in range(3):
            op = ref("after-restart-%d" % i)
            r = EXECUTOR.api("sm_create", action(op))
            self.assertEqual(r["outcome"], "CREATED", r)
            self.assertEqual(anchor_records(op), ["CREATE_INTENT", "CREATE_DONE"])
            f = EXECUTOR.api("sm_fence", ref("after-restart-f%d" % i), 0)
            self.assertTrue(proves_no_order(f), f)


class CommitFailedAfterIntent(unittest.TestCase):
    """Case G without a restore: CREATE_INTENT landed, the Odoo commit failed."""

    def _fail_create(self, op):
        a = action(op)
        with self.assertRaises(RPCError):  # the connector reports this as UNKNOWN
            EXECUTOR.api("sm_create", a)
        self.assertEqual(orders_for(op), 0)
        self.assertEqual(op_row(op), [])
        self.assertEqual(anchor_records(op), ["CREATE_INTENT"])
        look = OBSERVER.api("sm_lookup", op)
        self.assertEqual((look["outcome"], look["prior_create"]), ("NOT_FOUND_NOW", True))
        return a

    def test_fence_after_failed_commit_is_held(self):
        op = fp("commit_after_intent", "fence")
        a = self._fail_create(op)
        f = EXECUTOR.api("sm_fence", op, 0)
        self.assertEqual((f["outcome"], f["block_committed"], f["prior_create"]), ("FENCED", True, True))
        self.assertEqual(engine_status(f, a), "HELD", "a fence over a prior create never releases")
        self.assertEqual(EXECUTOR.api("sm_create", a)["outcome"], "REFUSED_FENCED")
        self.assertEqual(orders_for(op), 0)

    def test_retry_after_failed_commit_is_refused_prior_attempt(self):
        op = fp("commit_after_intent", "retry")
        a = self._fail_create(op)
        r = EXECUTOR.api("sm_create", a)
        self.assertEqual((r["outcome"], r["block_committed"], r["prior_create"]), ("REFUSED_PRIOR_ATTEMPT", True, True))
        self.assertEqual(engine_status(r, a), "HELD")
        self.assertEqual(anchor_records(op), ["CREATE_INTENT", "BLOCK"])
        self.assertEqual(orders_for(op), 0, "a retry must never create the order the failed attempt may have made")


class OdooRestore(unittest.TestCase):
    """One Odoo backup, work after it, then a restore of that backup."""

    @classmethod
    def setUpClass(cls):
        cls.e_retry, cls.e_fence = ref("E-retry"), ref("E-fence")
        cls.g_retry, cls.g_fence = fp("lose_create_done", "G-retry"), fp("lose_create_done", "G-fence")
        cls.fenced = ref("case2")
        cls.actions = {op: action(op) for op in (cls.e_retry, cls.e_fence, cls.g_retry, cls.g_fence, cls.fenced)}

        snapshot_odoo()
        for op in (cls.e_retry, cls.e_fence, cls.g_retry, cls.g_fence):
            r = EXECUTOR.api("sm_create", cls.actions[op])
            assert r["outcome"] == "CREATED", (op, r)
        f = EXECUTOR.api("sm_fence", cls.fenced, 0)
        assert proves_no_order(f), f
        cls.head_before = int(odoo_sql("SELECT last_seq FROM steelmatter_head")[0])
        cls.anchor_before = anchor_count()
        cls.records_before = {op: anchor_records(op) for op in cls.actions}
        restore_odoo()
        # Measured before any test writes again: the tests below add anchor
        # records and journal rows of their own.
        cls.anchor_after = anchor_count()
        cls.head_after = int(odoo_sql("SELECT last_seq FROM steelmatter_head")[0])
        cls.odoo_after = {op: (orders_for(op), op_row(op)) for op in cls.actions}
        cls.gate_op = ref("gate")
        cls.gate_create = EXECUTOR.api("sm_create", action(cls.gate_op, expected_min_seq=cls.head_before))
        cls.gate_fence = EXECUTOR.api("sm_fence", cls.gate_op, cls.head_before)

    def test_restore_erased_odoo_but_not_the_anchor(self):
        self.assertEqual(self.anchor_after, self.anchor_before, "an Odoo restore must leave the anchor untouched")
        for op in (self.e_retry, self.e_fence, self.g_retry, self.g_fence):
            self.assertEqual(self.odoo_after[op], (0, []), "the restore erased the order and its operation row: " + op)
        self.assertEqual(self.odoo_after[self.fenced], (0, []), "the restore erased the in-Odoo tombstone")
        self.assertEqual(self.records_before[self.e_retry], ["CREATE_INTENT", "CREATE_DONE"])
        self.assertEqual(self.records_before[self.g_retry], ["CREATE_INTENT"])
        self.assertEqual(self.records_before[self.fenced], ["BLOCK"])
        self.assertLess(self.head_after, self.head_before, "the restore rolled the journal head back")

    def test_restore_gate_refuses_a_write_with_the_newer_sequence(self):
        self.assertEqual(self.gate_create["outcome"], "REFUSED_RESTORE_SUSPECTED")
        self.assertEqual(self.gate_fence["outcome"], "REFUSED_RESTORE_SUSPECTED")
        self.assertEqual(anchor_records(self.gate_op), [], "nothing written")
        self.assertEqual(op_row(self.gate_op), [])

    def test_case2_delayed_create_after_restore_is_refused_by_block(self):
        a = self.actions[self.fenced]  # the original request, delayed, old sequence (0)
        r = EXECUTOR.api("sm_create", a)
        self.assertEqual(r["outcome"], "REFUSED_FENCED")
        self.assertEqual(orders_for(self.fenced), 0)
        self.assertEqual(engine_status(r, a), "FENCED", "stays released: block committed, no prior create")
        self.assertEqual(op_row(self.fenced), ["fenced"], "the tombstone is re-established from the anchor")
        look = OBSERVER.api("sm_lookup", self.fenced)
        self.assertEqual((look["outcome"], look["block_committed"], look["prior_create"]), ("FENCED", True, False))

    def test_caseE_lookup_finds_the_erased_order_from_create_done(self):
        look = OBSERVER.api("sm_lookup", self.e_retry)
        self.assertEqual((look["outcome"], look["create_done"]), ("FOUND", True))
        self.assertEqual(engine_status(look, self.actions[self.e_retry]), "COMMITTED")

    def test_caseE_retry_gets_refused_prior_attempt_and_no_second_order(self):
        a = self.actions[self.e_retry]
        r = EXECUTOR.api("sm_create", a)
        self.assertEqual((r["outcome"], r["block_committed"], r["prior_create"], r["create_done"]),
                         ("REFUSED_PRIOR_ATTEMPT", True, True, True))
        self.assertEqual(r["destination"]["permit_digest"], a["permit_digest"])
        self.assertEqual(engine_status(r, a), "COMMITTED", "established by CREATE_DONE; stays committed, no refund")
        self.assertEqual(orders_for(self.e_retry), 0, "the retry must not create a second order")
        self.assertEqual(anchor_records(self.e_retry), ["CREATE_INTENT", "CREATE_DONE", "BLOCK"])
        self.assertEqual(EXECUTOR.api("sm_create", a)["outcome"], "REFUSED_FENCED")
        self.assertEqual(orders_for(self.e_retry), 0)

    def test_caseE_fence_reports_the_order_and_blocks_it(self):
        a = self.actions[self.e_fence]
        f = EXECUTOR.api("sm_fence", self.e_fence, 0)
        self.assertEqual((f["outcome"], f["create_done"]), ("EXISTS", True))
        self.assertEqual(engine_status(f, a), "COMMITTED")
        self.assertIn("BLOCK", anchor_records(self.e_fence))
        self.assertEqual(EXECUTOR.api("sm_create", a)["outcome"], "REFUSED_FENCED")
        self.assertEqual(orders_for(self.e_fence), 0)

    def test_caseG_retry_without_create_done_is_held(self):
        a = self.actions[self.g_retry]
        look = OBSERVER.api("sm_lookup", self.g_retry)
        self.assertEqual((look["outcome"], look["prior_create"], look["create_done"]), ("NOT_FOUND_NOW", True, False))
        r = EXECUTOR.api("sm_create", a)
        self.assertEqual((r["outcome"], r["block_committed"], r["prior_create"], r["create_done"]),
                         ("REFUSED_PRIOR_ATTEMPT", True, True, False))
        self.assertEqual(engine_status(r, a), "HELD")
        self.assertEqual(orders_for(self.g_retry), 0)

    def test_caseG_fence_without_create_done_is_held(self):
        a = self.actions[self.g_fence]
        f = EXECUTOR.api("sm_fence", self.g_fence, 0)
        self.assertEqual((f["outcome"], f["block_committed"], f["prior_create"], f["create_done"]), ("FENCED", True, True, False))
        self.assertEqual(engine_status(f, a), "HELD", "never released: an order may have existed")
        self.assertEqual(EXECUTOR.api("sm_create", a)["outcome"], "REFUSED_FENCED")
        self.assertEqual(orders_for(self.g_fence), 0)


if __name__ == "__main__":
    unittest.main()
