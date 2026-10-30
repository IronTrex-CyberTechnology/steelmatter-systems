"""op_ref on purchase orders, the purchase-write guard, and journal rows for
changes made in Odoo.

Only Steelmatter writes purchase orders (R1 decision 6, R1-DEF-10): creating,
editing or deleting an order or its lines is refused for every Odoo user except
the module's own execution path (superuser, reached only through
steelmatter.api) and Odoo administrators. Administrators keep that ability so
Odoo stays administrable; the coverage report lists them as unprotected.

Edits, cancels and deletes of Steelmatter orders, and orders created outside
Steelmatter (foreign, a coverage gap), write journal rows so the restore check
and observation see them. Direct SQL bypasses this (coverage limit).
"""

from odoo import _, api, fields, models
from odoo.exceptions import AccessError

from .operation import append_journal

# Order fields whose change makes an order differ from what was authorised.
MATERIAL_FIELDS = {"partner_id", "company_id", "currency_id", "dest_address_id", "order_line", "sm_op_ref", "sm_permit_digest",
                   "sm_supplier_revision", "sm_delivery_revision"}
LINE_FIELDS = {"product_id", "product_qty", "product_uom", "price_unit", "taxes_id", "order_id", "display_type", "sm_unit_ref"}

# Bookkeeping Odoo itself writes on an order when someone posts in its chatter.
CHATTER_FIELDS = {"message_main_attachment_id"}

# Odoo 18's Lock and Unlock buttons move a confirmed order between these two
# states (button_done also resets priority). Neither changes what was
# authorised, so neither is an edit (NEW-QA-01); the order stays FOUND as
# confirmed (R1-DEF-19).
CONFIRMED_STATES = ("purchase", "done")
LOCK_FIELDS = {"state", "priority"}


def internal(env):
    """The module's own path: superuser, never an RPC caller's context flag."""
    return env.su and env.context.get("steelmatter_create")


def guard(records, vals=None):
    env = records.env
    if env.su or env.user.has_group("base.group_system"):
        return
    if vals is not None and set(vals) <= CHATTER_FIELDS:
        return
    raise AccessError(_("Purchase orders in this company are created and changed only by Steelmatter. "
                        "Ask an administrator if an order must be changed."))


class PurchaseOrder(models.Model):
    _inherit = "purchase.order"

    sm_op_ref = fields.Char("Steelmatter op_ref", readonly=True, copy=False, index=True)
    sm_supplier_revision = fields.Integer("Steelmatter supplier revision at purchase", readonly=True, copy=False)
    sm_delivery_revision = fields.Integer("Steelmatter delivery address revision at purchase", readonly=True, copy=False)
    sm_permit_digest = fields.Char("Steelmatter permit digest", readonly=True, copy=False)

    def init(self):
        super().init()
        self.env.cr.execute(
            "CREATE UNIQUE INDEX IF NOT EXISTS purchase_order_sm_op_ref_uniq"
            " ON purchase_order (sm_op_ref) WHERE sm_op_ref IS NOT NULL"
        )

    @api.model_create_multi
    def create(self, vals_list):
        guard(self)
        orders = super().create(vals_list)
        for o in orders.filtered(lambda o: not o.sm_op_ref):
            append_journal(self.env.cr, "foreign_created", po_name=o.name, currency=o.currency_id.name,
                           amount_minor=minor(o.amount_total, o.currency_id))
        return orders

    def write(self, vals):
        guard(self, vals)
        lock_only = (set(vals) <= LOCK_FIELDS and vals.get("state") in CONFIRMED_STATES
                     and all(o.state in CONFIRMED_STATES for o in self))
        res = super().write(vals)
        sm = self.filtered("sm_op_ref")
        if sm and not internal(self.env) and not lock_only:
            if vals.get("state") == "cancel":
                for o in sm:
                    append_journal(self.env.cr, "po_cancelled", op_ref=o.sm_op_ref, po_name=o.name)
            elif MATERIAL_FIELDS & set(vals) or "state" in vals:
                for o in sm:
                    append_journal(self.env.cr, "po_edited", op_ref=o.sm_op_ref, po_name=o.name,
                                   reason=",".join(sorted(set(vals) & (MATERIAL_FIELDS | {"state"}))))
        return res

    def unlink(self):
        guard(self)
        gone = [(o.sm_op_ref, o.name) for o in self if o.sm_op_ref]
        res = super().unlink()
        for op_ref, name in gone:
            append_journal(self.env.cr, "po_deleted", op_ref=op_ref, po_name=name)
        return res


class PurchaseOrderLine(models.Model):
    _inherit = "purchase.order.line"

    # The unit the line was authorised in ("" = the product's base unit),
    # reported back so the engine compares it (D1 v1.4).
    sm_unit_ref = fields.Char("Steelmatter authorised unit", readonly=True, copy=False)

    def _sm_journal(self, orders, why):
        if internal(self.env):
            return
        for o in orders.filtered("sm_op_ref"):
            append_journal(self.env.cr, "po_edited", op_ref=o.sm_op_ref, po_name=o.name, reason=why)

    @api.model_create_multi
    def create(self, vals_list):
        guard(self)
        lines = super().create(vals_list)
        self._sm_journal(lines.order_id, "line added")
        return lines

    def write(self, vals):
        guard(self, vals)
        res = super().write(vals)
        if LINE_FIELDS & set(vals):
            self._sm_journal(self.order_id, "line changed")
        return res

    def unlink(self):
        guard(self)
        orders = self.order_id
        res = super().unlink()
        self._sm_journal(orders.exists(), "line removed")
        return res


def minor(amount, currency):
    """Converts an Odoo amount to integer minor units of currency."""
    return int(round(amount * 10 ** currency.decimal_places))
