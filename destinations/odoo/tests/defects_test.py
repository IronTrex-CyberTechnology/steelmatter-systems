"""Regression tests for the Odoo-side SDET defects (R1 defect report, 29 Oct
2026), against the local sandbox with real Odoo records:

    R1-DEF-11  the supplier revision cannot be set directly or moved backwards
    R1-DEF-18  a company that locks confirmed orders (po_lock = lock) still gets
               a verified order, not HELD
    R1-DEF-19  an order a user locks after confirmation is still FOUND
    R1-DEF-22  quantities are ordered in the product's base unit, never in a
               purchase unit such as Dozens

    python3 -I destinations/odoo/tests/defects_test.py -v
"""

import os
import subprocess
import sys
import unittest
import uuid

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from sandbox_client import (  # noqa: E402
    RPCError, action, admin, anchor_records, compose, executor, observer, odoo_sql, supplier_revision,
)

RUN = uuid.uuid4().hex[:8]
EXECUTOR = OBSERVER = ADMIN = None


def setUpModule():
    global EXECUTOR, OBSERVER, ADMIN
    EXECUTOR, OBSERVER, ADMIN = executor(), observer(), admin()


def ref(name):
    return "def-%s-%s" % (RUN, name)


def xmlid(module, name):
    return ADMIN.call("ir.model.data", "search_read", [("module", "=", module), ("name", "=", name)], fields=["res_id"])[0]["res_id"]


def po_of(op):
    return int(odoo_sql("SELECT id FROM purchase_order WHERE sm_op_ref = '%s'" % op)[0])


class LockedOrders(unittest.TestCase):
    def test_def18_company_that_locks_confirmed_orders(self):
        company = xmlid("base", "main_company")
        ADMIN.call("res.company", "write", [company], {"po_lock": "lock"})
        try:
            op = ref("lock-company")
            a = action(op)
            r = EXECUTOR.api("sm_create", a)
            self.assertEqual(r["outcome"], "CREATED", r)
            self.assertEqual(odoo_sql("SELECT state FROM purchase_order WHERE sm_op_ref = '%s'" % op), ["done"],
                             "the company setting locked the order on confirmation")
            self.assertEqual(r["destination"]["state"], "purchase", "a locked order is reported as confirmed")
            self.assertEqual(anchor_records(op), ["CREATE_INTENT", "CREATE_DONE"], "the create committed; nothing HELD")
            look = OBSERVER.api("sm_lookup", op)
            self.assertEqual(look["outcome"], "FOUND")
            self.assertEqual(OBSERVER.api("sm_observe", a)["outcome"], "MATCH")
        finally:
            ADMIN.call("res.company", "write", [company], {"po_lock": "edit"})

    def test_def19_order_locked_by_a_user_stays_found(self):
        op = ref("lock-user")
        a = action(op)
        self.assertEqual(EXECUTOR.api("sm_create", a)["outcome"], "CREATED")
        po = po_of(op)
        ADMIN.call("purchase.order", "button_done", [po])  # the Lock button
        self.assertEqual(odoo_sql("SELECT state FROM purchase_order WHERE id = %d" % po), ["done"])
        look = OBSERVER.api("sm_lookup", op)
        self.assertEqual((look["outcome"], look["destination"]["state"]), ("FOUND", "purchase"))
        retry = EXECUTOR.api("sm_create", a)
        self.assertEqual((retry["outcome"], retry["destination"]["state"]), ("EXISTS", "purchase"))
        # Locking is not cancelling: a cancelled order is still not confirmed.
        ADMIN.call("purchase.order", "button_unlock", [po])
        ADMIN.call("purchase.order", "button_cancel", [po])
        cancelled = OBSERVER.api("sm_lookup", op)
        self.assertEqual((cancelled["outcome"], cancelled["destination"]["state"]), ("MISMATCH", "cancel"))


class UnitOfMeasure(unittest.TestCase):
    def test_def22_quantity_is_in_the_base_unit_not_the_purchase_unit(self):
        units, dozens = xmlid("uom", "product_uom_unit"), xmlid("uom", "product_uom_dozen")
        item = "chair-dozen-%s" % RUN
        ADMIN.call("product.product", "create", {"name": "Chair sold by the dozen", "sm_ref": item, "purchase_ok": True,
                                                 "type": "consu", "uom_id": units, "uom_po_id": dozens,
                                                 "supplier_taxes_id": [(6, 0, [])], "taxes_id": [(6, 0, [])]})
        op = ref("uom")
        a = action(op, lines=[{"item_ref": item, "quantity": 10, "unit_price_minor": 600000}], amount_minor=6000000)
        r = EXECUTOR.api("sm_create", a)
        self.assertEqual(r["outcome"], "CREATED", r)
        row = odoo_sql("SELECT l.product_qty, l.product_uom FROM purchase_order_line l JOIN purchase_order o ON o.id = l.order_id"
                       " WHERE o.sm_op_ref = '%s'" % op)[0].split("|")
        self.assertEqual((float(row[0]), int(row[1])), (10.0, units), "10 units, not 10 dozen (120 units)")
        self.assertEqual(r["destination"]["lines"], a["lines"])
        self.assertEqual(r["destination"]["amount_minor"], 6000000)
        # An edit to the purchase unit afterwards no longer matches and is journalled.
        line = int(odoo_sql("SELECT l.id FROM purchase_order_line l JOIN purchase_order o ON o.id = l.order_id"
                            " WHERE o.sm_op_ref = '%s'" % op)[0])
        ADMIN.call("purchase.order.line", "write", [line], {"product_uom": dozens})
        look = OBSERVER.api("sm_lookup", op)
        self.assertEqual(look["destination"]["lines"][0]["quantity"], -1, "a line in another unit never matches")
        self.assertEqual(OBSERVER.api("sm_observe", a)["outcome"], "MISMATCH")
        self.assertIn("po_edited", odoo_sql("SELECT event FROM steelmatter_journal WHERE op_ref = '%s'" % op))


class SupplierRevision(unittest.TestCase):
    def supplier(self):
        return ADMIN.call("res.partner", "search", [("sm_ref", "=", "sup-desks")])[0]

    def test_def11_revision_cannot_be_set_by_anyone(self):
        sup, rev = self.supplier(), supplier_revision("sup-desks")
        for who in (ADMIN, EXECUTOR, OBSERVER):
            with self.assertRaises(RPCError):
                who.call("res.partner", "write", [sup], {"sm_revision": rev + 5})
        with self.assertRaises(RPCError):
            ADMIN.call("res.partner", "write", [sup], {"sm_revision": 1})
        self.assertEqual(supplier_revision("sup-desks"), rev)

    def test_def11_revision_never_moves_backwards_even_in_sql(self):
        sup, rev = self.supplier(), supplier_revision("sup-desks")
        ADMIN.call("res.partner", "write", [sup], {"vat": "29BBBBB%sB1Z6" % RUN[:4]})  # a protected change
        self.assertEqual(supplier_revision("sup-desks"), rev + 1)
        with self.assertRaises(subprocess.CalledProcessError):  # even a direct database update is refused
            compose("exec", "-T", "odoo-db", "psql", "-v", "ON_ERROR_STOP=1", "-U", "odoo", "-d", "smr1", "-tAc",
                    "UPDATE res_partner SET sm_revision = %d WHERE id = %d" % (rev, sup))
        self.assertEqual(supplier_revision("sup-desks"), rev + 1)
        # So an action authorised at the old revision stays stale.
        stale = EXECUTOR.api("sm_create", action(ref("stale"), supplier_ref="sup-desks", supplier_revision=rev))
        self.assertEqual(stale["outcome"], "REFUSED_SUPPLIER_STALE")

    def test_def11_new_partner_starts_at_revision_one(self):
        pid = ADMIN.call("res.partner", "create", {"name": "Synthetic new supplier", "is_company": True,
                                                   "sm_ref": "sup-new-%s" % RUN, "sm_revision": 99,
                                                   "email": "new-%s@example.invalid" % RUN})
        self.assertEqual(odoo_sql("SELECT sm_revision FROM res_partner WHERE id = %d" % pid), ["1"])


if __name__ == "__main__":
    unittest.main()
