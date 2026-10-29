"""op_ref on purchase orders, and journal rows for changes made in Odoo.

Edits, cancels and deletes of Steelmatter orders, and orders created outside
Steelmatter (foreign, a coverage gap), write journal rows so the restore check
and observation see them. Direct SQL bypasses this (coverage limit).
"""

from odoo import api, fields, models

from .operation import append_journal

# Order fields whose change makes an order differ from what was authorised.
MATERIAL_FIELDS = {"partner_id", "company_id", "currency_id", "dest_address_id", "order_line", "sm_op_ref", "sm_permit_digest", "sm_supplier_revision"}
LINE_FIELDS = {"product_id", "product_qty", "product_uom", "price_unit", "taxes_id", "order_id", "display_type"}


class PurchaseOrder(models.Model):
    _inherit = "purchase.order"

    sm_op_ref = fields.Char("Steelmatter op_ref", readonly=True, copy=False, index=True)
    sm_supplier_revision = fields.Integer("Steelmatter supplier revision at purchase", readonly=True, copy=False)
    sm_permit_digest = fields.Char("Steelmatter permit digest", readonly=True, copy=False)

    def init(self):
        super().init()
        self.env.cr.execute(
            "CREATE UNIQUE INDEX IF NOT EXISTS purchase_order_sm_op_ref_uniq"
            " ON purchase_order (sm_op_ref) WHERE sm_op_ref IS NOT NULL"
        )

    @api.model_create_multi
    def create(self, vals_list):
        orders = super().create(vals_list)
        for o in orders.filtered(lambda o: not o.sm_op_ref):
            append_journal(self.env.cr, "foreign_created", po_name=o.name, currency=o.currency_id.name,
                           amount_minor=minor(o.amount_total, o.currency_id))
        return orders

    def write(self, vals):
        res = super().write(vals)
        sm = self.filtered("sm_op_ref")
        if sm and not self.env.context.get("steelmatter_create"):
            if vals.get("state") == "cancel":
                for o in sm:
                    append_journal(self.env.cr, "po_cancelled", op_ref=o.sm_op_ref, po_name=o.name)
            elif MATERIAL_FIELDS & set(vals) or "state" in vals:
                for o in sm:
                    append_journal(self.env.cr, "po_edited", op_ref=o.sm_op_ref, po_name=o.name,
                                   reason=",".join(sorted(set(vals) & (MATERIAL_FIELDS | {"state"}))))
        return res

    def unlink(self):
        gone = [(o.sm_op_ref, o.name) for o in self if o.sm_op_ref]
        res = super().unlink()
        for op_ref, name in gone:
            append_journal(self.env.cr, "po_deleted", op_ref=op_ref, po_name=name)
        return res


class PurchaseOrderLine(models.Model):
    _inherit = "purchase.order.line"

    def _sm_journal(self, orders, why):
        if self.env.context.get("steelmatter_create"):
            return
        for o in orders.filtered("sm_op_ref"):
            append_journal(self.env.cr, "po_edited", op_ref=o.sm_op_ref, po_name=o.name, reason=why)

    @api.model_create_multi
    def create(self, vals_list):
        lines = super().create(vals_list)
        self._sm_journal(lines.order_id, "line added")
        return lines

    def write(self, vals):
        res = super().write(vals)
        if LINE_FIELDS & set(vals):
            self._sm_journal(self.order_id, "line changed")
        return res

    def unlink(self):
        orders = self.order_id
        res = super().unlink()
        self._sm_journal(orders.exists(), "line removed")
        return res


def minor(amount, currency):
    """Converts an Odoo amount to integer minor units of currency."""
    return int(round(amount * 10 ** currency.decimal_places))
