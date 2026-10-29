"""Coverage report tests (build guide step 5): each test creates a real route
around Steelmatter in the sandbox, checks that sm_coverage reports it with the
right posture, then removes it.

    python3 -I destinations/odoo/tests/coverage_test.py -v
"""

import os
import sys
import unittest
import uuid

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from sandbox_client import Identity, RPCError, action, admin, executor, observer, odoo_sql  # noqa: E402

RUN = uuid.uuid4().hex[:8]
EXECUTOR = OBSERVER = ADMIN = None


def setUpModule():
    global EXECUTOR, OBSERVER, ADMIN
    EXECUTOR, OBSERVER, ADMIN = executor(), observer(), admin()


def coverage():
    return OBSERVER.api("sm_coverage")


def route(cov, rid):
    for r in cov["routes"]:
        if r["id"] == rid:
            return r
    return None


def xmlid(module, name):
    return ADMIN.call("ir.model.data", "search_read", [("module", "=", module), ("name", "=", name)], fields=["res_id"])[0]["res_id"]


def user_id(login):
    return ADMIN.call("res.users", "search", [("login", "=", login)])[0]


class Baseline(unittest.TestCase):
    def test_sandbox_posture(self):
        cov = coverage()
        self.assertFalse(cov["complete"], "coverage must never be claimed complete")
        self.assertTrue(cov["module"]["installed"] and cov["module"]["supported"])
        self.assertEqual(cov["module"]["version"], cov["module"]["expected_version"])
        self.assertEqual(route(cov, "steelmatter_api")["posture"], "enforced")
        self.assertEqual(route(cov, "direct_database")["posture"], "unknown")
        self.assertEqual(route(cov, "administrators")["posture"], "unprotected")
        self.assertIn("admin", [s["login"] for s in route(cov, "administrators")["subjects"]])
        direct = [s["login"] for s in route(cov, "direct_users")["subjects"]]
        self.assertNotIn("sm_executor", direct)
        self.assertNotIn("sm_observer", direct)
        self.assertIsNone(route(cov, "executor_bypass"))
        self.assertIsNone(route(cov, "observer_writes"))
        self.assertEqual(cov["identities"]["observer"][0]["read_only"], True)
        self.assertEqual(sum(cov["summary"].values()), len(cov["routes"]))

    def test_executor_cannot_call_it_to_write(self):
        # Read-only for every caller: nothing in the database changes.
        before = odoo_sql("SELECT last_seq FROM steelmatter_head")
        EXECUTOR.api("sm_coverage")
        coverage()
        self.assertEqual(odoo_sql("SELECT last_seq FROM steelmatter_head"), before)


class Bypasses(unittest.TestCase):
    def test_a_purchase_user_is_blocked_by_the_module(self):
        # R1 decision 6 / R1-DEF-10: only Steelmatter writes purchase orders.
        # A user Odoo would let buy is refused by the module and reported as
        # blocked, not as a route around Steelmatter.
        login, pw = "buyer-%s" % RUN, uuid.uuid4().hex
        uid = ADMIN.call("res.users", "create", {"login": login, "name": "Synthetic buyer", "email": login + "@example.invalid",
                                                 "password": pw, "groups_id": [(6, 0, [xmlid("purchase", "group_purchase_user")])]},
                         context={"no_reset_password": True})
        try:
            r = route(coverage(), "direct_users")
            self.assertNotIn(login, [s["login"] for s in r["subjects"]], "a blocked user is not a route")
            me = [s for s in r["blocked_by_module"] if s["login"] == login]
            self.assertTrue(me and me[0]["create"] and me[0]["confirm_or_edit"], r)
            self.assertIn("Purchase / User", me[0]["groups"])
            buyer = Identity(login, pw)
            partner = ADMIN.call("res.partner", "search", [("sm_ref", "=", "sup-desks")])[0]
            with self.assertRaises(RPCError):
                buyer.call("purchase.order", "create", {"partner_id": partner})
        finally:
            ADMIN.call("res.users", "write", [uid], {"active": False})

    def test_executor_with_purchase_rights_is_flagged(self):
        uid, gid = user_id("sm_executor"), xmlid("purchase", "group_purchase_user")
        ADMIN.call("res.users", "write", [uid], {"groups_id": [(4, gid)]})
        try:
            r = route(coverage(), "executor_bypass")
            self.assertIsNotNone(r, "an executor with its own purchase rights must be reported")
            self.assertEqual((r["posture"], r["subjects"]), ("unprotected", ["sm_executor"]))
        finally:
            ADMIN.call("res.users", "write", [uid], {"groups_id": [(3, gid)]})
        self.assertIsNone(route(coverage(), "executor_bypass"))

    def test_observer_with_write_rights_is_flagged(self):
        uid, gid = user_id("sm_observer"), xmlid("purchase", "group_purchase_user")
        ADMIN.call("res.users", "write", [uid], {"groups_id": [(4, gid)]})
        try:
            cov = coverage()
            r = route(cov, "observer_writes")
            self.assertIsNotNone(r)
            self.assertEqual(r["posture"], "unprotected")
            self.assertIn("purchase.order", cov["identities"]["observer"][0]["writes"])
        finally:
            ADMIN.call("res.users", "write", [uid], {"groups_id": [(3, gid)]})
        self.assertIsNone(route(coverage(), "observer_writes"))

    def test_custom_server_action_is_unknown(self):
        name = "Synthetic automation %s" % RUN
        model = ADMIN.call("ir.model", "search", [("model", "=", "purchase.order")])[0]
        aid = ADMIN.call("ir.actions.server", "create", {"name": name, "model_id": model, "state": "code", "code": "pass"})
        try:
            r = route(coverage(), "automation")
            self.assertEqual(r["posture"], "unknown")
            self.assertIn(name, r["subjects"])
        finally:
            ADMIN.call("ir.actions.server", "unlink", [aid])
        self.assertNotIn(name, route(coverage(), "automation")["subjects"])


class Journal(unittest.TestCase):
    def test_foreign_order_is_observed(self):
        before = route(coverage(), "foreign_orders")["count"]
        partner = ADMIN.call("res.partner", "search", [("sm_ref", "=", "sup-desks")])[0]
        product = ADMIN.call("product.product", "search", [("sm_ref", "=", "desk-std")])[0]
        ADMIN.call("purchase.order", "create", {"partner_id": partner, "order_line": [(0, 0, {
            "product_id": product, "product_qty": 1, "price_unit": 100.0})]})
        r = route(coverage(), "foreign_orders")
        self.assertEqual((r["posture"], r["count"]), ("observed", before + 1))

    def test_edit_of_a_steelmatter_order_is_observed(self):
        op = "cov-%s-edit" % RUN
        self.assertEqual(EXECUTOR.api("sm_create", action(op))["outcome"], "CREATED")
        before = route(coverage(), "order_changes")["by_event"]["po_edited"]
        line = int(odoo_sql("SELECT l.id FROM purchase_order_line l JOIN purchase_order o ON o.id = l.order_id"
                            " WHERE o.sm_op_ref = '%s'" % op)[0])
        ADMIN.call("purchase.order.line", "write", [line], {"price_unit": 1.0})
        r = route(coverage(), "order_changes")
        self.assertEqual(r["posture"], "observed")
        self.assertEqual(r["by_event"]["po_edited"], before + 1)

    def test_order_changed_around_the_orm_is_unknown(self):
        # Direct SQL bypasses the journal: an order that loses its op_ref this
        # way has no journal row, and coverage must report it, not hide it.
        op = "cov-%s-sql" % RUN
        self.assertEqual(EXECUTOR.api("sm_create", action(op))["outcome"], "CREATED")
        before = (route(coverage(), "unjournalled_orders") or {"count": 0})["count"]
        from sandbox_client import compose  # noqa: E402 - a write, so not in the read helpers
        compose("exec", "-T", "odoo-db", "psql", "-U", "odoo", "-d", "smr1", "-c",
                "UPDATE purchase_order SET sm_op_ref = NULL WHERE sm_op_ref = '%s'" % op)
        r = route(coverage(), "unjournalled_orders")
        self.assertIsNotNone(r)
        self.assertEqual((r["posture"], r["count"]), ("unknown", before + 1))


if __name__ == "__main__":
    unittest.main()
