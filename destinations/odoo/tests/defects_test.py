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


def taxed_product(name):
    """A product whose configured purchase tax is the seeded 18% GST."""
    item = "%s-%s" % (name, RUN)
    tax = ADMIN.call("account.tax", "search", [("sm_ref", "=", "gst18-purchase")])[0]
    ADMIN.call("product.product", "create", {"name": "Taxed chair %s" % name, "sm_ref": item, "purchase_ok": True, "type": "consu",
                                             "supplier_taxes_id": [(6, 0, [tax])], "taxes_id": [(6, 0, [])]})
    return item


def taxed_action(op, item, **over):
    # 10 chairs at INR 6,000: untaxed 60,000.00, GST 18% 10,800.00, total 70,800.00 (in paise).
    a = action(op, lines=[{"item_ref": item, "quantity": 10, "unit_price_minor": 600000, "tax_refs": ["gst18-purchase"]}],
               amount_untaxed_minor=6000000, tax_minor=1080000, amount_minor=7080000)
    a.update(over)
    return a


class Tax(unittest.TestCase):
    """R1-DEF-21 / R1 decision 1: authorised tax details and full total."""

    def test_def21_authorised_tax_is_applied_and_the_full_total_verified(self):
        item, op = taxed_product("tax-ok"), ref("tax-ok")
        a = taxed_action(op, item)
        r = EXECUTOR.api("sm_create", a)
        self.assertEqual(r["outcome"], "CREATED", r)
        d = r["destination"]
        self.assertEqual((d["amount_untaxed_minor"], d["tax_minor"], d["amount_minor"]), (6000000, 1080000, 7080000))
        self.assertEqual(d["lines"][0]["tax_refs"], ["gst18-purchase"])
        row = odoo_sql("SELECT amount_untaxed, amount_tax, amount_total FROM purchase_order WHERE sm_op_ref = '%s'" % op)[0]
        self.assertEqual([float(x) for x in row.split("|")], [60000.0, 10800.0, 70800.0], "Odoo holds the taxed order")
        self.assertEqual(OBSERVER.api("sm_observe", a)["outcome"], "MATCH")

    def test_def21_tax_is_never_silently_dropped(self):
        item = taxed_product("tax-legacy")
        # An action with no tax details (an engine before D1 v1.4) for a taxed
        # product is refused before any create attempt: no order, nothing held
        # over an earlier create.
        op = ref("tax-legacy")
        legacy = action(op, lines=[{"item_ref": item, "quantity": 10, "unit_price_minor": 600000}], amount_minor=6000000)
        r = EXECUTOR.api("sm_create", legacy)
        self.assertEqual((r["outcome"], r["reason"]), ("REFUSED_OUT_OF_SCOPE", "TAX_NOT_AUTHORISED"))
        self.assertEqual((r["block_committed"], r["prior_create"]), (True, False))
        self.assertEqual(int(odoo_sql("SELECT count(*) FROM purchase_order WHERE sm_op_ref = '%s'" % op)[0]), 0)
        # Authorising a tax the product does not carry is refused as well.
        untaxed = ref("tax-extra")
        r = EXECUTOR.api("sm_create", taxed_action(untaxed, "chair-ergo"))
        self.assertEqual((r["outcome"], r["reason"]), ("REFUSED_OUT_OF_SCOPE", "TAX_NOT_AUTHORISED"))

    def test_def21_total_that_differs_from_odoo_is_refused_before_any_create(self):
        item, op = taxed_product("tax-wrong"), ref("tax-wrong")
        r = EXECUTOR.api("sm_create", taxed_action(op, item, tax_minor=1000000, amount_minor=7000000))
        self.assertEqual((r["outcome"], r["reason"]), ("REFUSED_OUT_OF_SCOPE", "TOTAL_DIFFERS_FROM_ODOO"))
        self.assertEqual(anchor_records(op), ["BLOCK"], "refused before CREATE_INTENT, so it can be released")
        self.assertEqual(int(odoo_sql("SELECT count(*) FROM purchase_order WHERE sm_op_ref = '%s'" % op)[0]), 0)


class Units(unittest.TestCase):
    """R1-DEF-22 / R1 decision 1: the authorised unit, never converted."""

    def test_def22_authorised_unit_is_used_exactly(self):
        op = ref("unit-dozen")
        a = action(op, lines=[{"item_ref": "chair-ergo", "quantity": 1, "unit_price_minor": 7200000, "unit_ref": "dozen"}],
                   amount_minor=7200000)
        r = EXECUTOR.api("sm_create", a)
        self.assertEqual(r["outcome"], "CREATED", r)
        self.assertEqual(r["destination"]["lines"], a["lines"])
        dozens = xmlid("uom", "product_uom_dozen")
        row = odoo_sql("SELECT l.product_qty, l.product_uom FROM purchase_order_line l JOIN purchase_order o ON o.id = l.order_id"
                       " WHERE o.sm_op_ref = '%s'" % op)[0].split("|")
        self.assertEqual((float(row[0]), int(row[1])), (1.0, dozens))
        line = int(odoo_sql("SELECT l.id FROM purchase_order_line l JOIN purchase_order o ON o.id = l.order_id"
                            " WHERE o.sm_op_ref = '%s'" % op)[0])
        ADMIN.call("purchase.order.line", "write", [line], {"product_uom": xmlid("uom", "product_uom_unit")})
        self.assertEqual(OBSERVER.api("sm_lookup", op)["destination"]["lines"][0]["quantity"], -1)

    def test_def22_unit_of_another_kind_is_refused(self):
        r = EXECUTOR.api("sm_create", action(ref("unit-kg"), lines=[{"item_ref": "chair-ergo", "quantity": 10,
                                                                      "unit_price_minor": 600000, "unit_ref": "no-such-unit"}]))
        self.assertEqual((r["outcome"], r["reason"]), ("REFUSED_OUT_OF_SCOPE", "MAPPING_MISSING:unit"))


class DeliveryAddress(unittest.TestCase):
    """R1-DEF-11 / R1 decision 2: the delivery address is protected."""

    def delivery(self):
        return ADMIN.call("res.partner", "search", [("sm_ref", "=", "wh-chennai")])[0]

    def test_def11_address_change_after_authorisation_needs_a_fresh_one(self):
        rev = supplier_revision("wh-chennai")
        stale_op = ref("dlv-stale")
        a = action(stale_op, delivery_revision=rev)
        ADMIN.call("res.partner", "write", [self.delivery()], {"street": "%s Diverted Road" % RUN})
        self.assertEqual(supplier_revision("wh-chennai"), rev + 1)
        ready = EXECUTOR.api("sm_readiness", a)
        self.assertEqual((ready["outcome"], ready["reason"]), ("NOT_READY", "DELIVERY_CHANGED"))
        r = EXECUTOR.api("sm_create", a)
        self.assertEqual((r["outcome"], r["reason"]), ("REFUSED_SUPPLIER_STALE", "DELIVERY_CHANGED"))
        self.assertEqual(int(odoo_sql("SELECT count(*) FROM purchase_order WHERE sm_op_ref = '%s'" % stale_op)[0]), 0)
        fresh = EXECUTOR.api("sm_create", action(ref("dlv-fresh"), delivery_revision=rev + 1))
        self.assertEqual((fresh["outcome"], fresh["destination"]["delivery_revision"]), ("CREATED", rev + 1))

    def test_def11_missing_delivery_revision_is_refused(self):
        # R1 decision 2: the check is mandatory in Odoo too, not only in the
        # engine. No revision means never authorised against the address.
        op = ref("dlv-missing")
        a = action(op)
        del a["delivery_revision"]
        ready = EXECUTOR.api("sm_readiness", a)
        self.assertEqual((ready["outcome"], ready["reason"]), ("NOT_READY", "DELIVERY_CHANGED"))
        r = EXECUTOR.api("sm_create", a)
        self.assertEqual((r["outcome"], r["reason"]), ("REFUSED_SUPPLIER_STALE", "DELIVERY_CHANGED"))
        self.assertEqual(int(odoo_sql("SELECT count(*) FROM purchase_order WHERE sm_op_ref = '%s'" % op)[0]), 0)
        # The refusal is permanent for this op_ref, even with the revision added.
        self.assertEqual(EXECUTOR.api("sm_create", action(op))["outcome"], "REFUSED_FENCED")

    def test_def11_delivery_revision_cannot_be_set(self):
        with self.assertRaises(RPCError):
            ADMIN.call("res.partner", "write", [self.delivery()], {"sm_revision": 1})


class OnlySteelmatterWritesOrders(unittest.TestCase):
    """R1-DEF-10 / R1 decision 6."""

    def test_def10_purchase_user_cannot_create_or_change_orders(self):
        login, pw = "buyer-%s" % RUN, uuid.uuid4().hex
        uid = ADMIN.call("res.users", "create", {"login": login, "name": "Synthetic buyer", "email": login + "@example.invalid",
                                                 "password": pw, "groups_id": [(6, 0, [xmlid("purchase", "group_purchase_user")])]},
                         context={"no_reset_password": True})
        try:
            from sandbox_client import Identity
            buyer = Identity(login, pw)
            op = ref("guard")
            self.assertEqual(EXECUTOR.api("sm_create", action(op))["outcome"], "CREATED")
            po = po_of(op)
            partner = ADMIN.call("res.partner", "search", [("sm_ref", "=", "sup-desks")])[0]
            for call in (lambda: buyer.call("purchase.order", "create", {"partner_id": partner}),
                         lambda: buyer.call("purchase.order", "write", [po], {"notes": "x"}),
                         lambda: buyer.call("purchase.order", "button_cancel", [po]),
                         lambda: buyer.call("purchase.order", "unlink", [po])):
                with self.assertRaises(RPCError):
                    call()
            self.assertEqual(odoo_sql("SELECT state FROM purchase_order WHERE id = %d" % po), ["purchase"])
        finally:
            ADMIN.call("res.users", "write", [uid], {"active": False})

    def test_def10_a_caller_context_cannot_skip_the_journal(self):
        op = ref("ctx")
        self.assertEqual(EXECUTOR.api("sm_create", action(op))["outcome"], "CREATED")
        line = int(odoo_sql("SELECT l.id FROM purchase_order_line l JOIN purchase_order o ON o.id = l.order_id"
                            " WHERE o.sm_op_ref = '%s'" % op)[0])
        ADMIN.call("purchase.order.line", "write", [line], {"price_unit": 1.0}, context={"steelmatter_create": True})
        self.assertIn("po_edited", odoo_sql("SELECT event FROM steelmatter_journal WHERE op_ref = '%s'" % op))

    def test_one_odoo_company_per_deployment(self):
        other = ADMIN.call("res.company", "create", {"name": "Second synthetic company %s" % RUN})
        try:
            r = EXECUTOR.api("sm_create", action(ref("two-co")))
            self.assertEqual((r["outcome"], r["reason"]), ("REFUSED_OUT_OF_SCOPE", "MULTI_COMPANY"))
            cov = OBSERVER.api("sm_coverage")
            self.assertEqual([x["posture"] for x in cov["routes"] if x["id"] == "single_company"], ["unknown"])
        finally:
            ADMIN.call("res.company", "write", [other], {"active": False})
        self.assertEqual(EXECUTOR.api("sm_create", action(ref("one-co")))["outcome"], "CREATED")


if __name__ == "__main__":
    unittest.main()
