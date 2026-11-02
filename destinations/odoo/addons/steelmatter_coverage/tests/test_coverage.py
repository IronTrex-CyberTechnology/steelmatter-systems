from odoo.exceptions import AccessError
from odoo.tests import TransactionCase, tagged
from odoo.tests.common import new_test_user


@tagged("post_install", "-at_install")
class TestCoverageInstall(TransactionCase):
    def test_purchasing_and_read_only_inventory(self):
        self.assertNotIn("steelmatter.api", self.env)
        buyer = new_test_user(self.env, login="coverage-buyer", groups="purchase.group_purchase_user")
        coverage = self.env["steelmatter.coverage"]
        with self.assertRaises(AccessError):
            coverage.with_user(buyer).report()

        supplier = self.env["res.partner"].create({"name": "Coverage test supplier"})
        product = self.env["product.product"].create({"name": "Coverage test item", "purchase_ok": True})
        orders = self.env["purchase.order"].with_user(buyer)
        po = orders.create({"partner_id": supplier.id, "order_line": [(0, 0, {
            "product_id": product.id, "name": product.name, "product_qty": 1,
            "product_uom": product.uom_po_id.id, "price_unit": 10,
            "date_planned": "2026-01-01 12:00:00",
        })]})
        po.order_line.write({"product_qty": 2})
        po.button_confirm()
        self.assertEqual(po.state, "purchase")
        before = orders.search_count([])
        report = coverage.report()
        self.assertEqual(report["mode"], "coverage-only")
        self.assertTrue(any(s["login"] == buyer.login for s in report["routes"][0]["subjects"]))
        self.assertFalse(any(r["posture"] == "enforced" for r in report["routes"]))
        self.assertEqual(orders.search_count([]), before)
        self.assertEqual(po.order_line.product_qty, 2)
