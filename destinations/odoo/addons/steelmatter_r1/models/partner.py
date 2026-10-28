"""Supplier revision (D1 v1.1: one definition everywhere).

res.partner.sm_revision is kept on the commercial partner and raised whenever
a protected supplier field changes, including bank accounts. sm_create compares
it under FOR SHARE on that row, so a concurrent change either commits first
(and the create sees the new revision) or waits for the create to commit.

Protected fields are a proposal for D9 (to confirm with Uday and Aakhil).
"""

from odoo import api, fields, models

PROTECTED_FIELDS = {
    "name", "vat", "active", "company_id", "is_company", "parent_id", "country_id",
    "bank_ids", "property_purchase_currency_id", "property_supplier_payment_term_id",
}


def bump_revision(env, partners):
    ids = tuple({p.commercial_partner_id.id for p in partners if p.commercial_partner_id})
    if ids:
        # Plain SQL: the UPDATE takes the row lock that sm_create's FOR SHARE
        # waits on, and needs no write rights for the user editing the partner.
        env.cr.execute("UPDATE res_partner SET sm_revision = sm_revision + 1 WHERE id IN %s", [ids])
        env["res.partner"].browse(ids).invalidate_recordset(["sm_revision"])


class ResPartner(models.Model):
    _inherit = "res.partner"

    sm_ref = fields.Char("Steelmatter ref", copy=False, index=True,
                         help="Engine supplier_ref (on a supplier) or delivery_ref (on a delivery address)")
    sm_revision = fields.Integer("Steelmatter supplier revision", default=1, readonly=True, copy=False)

    _sql_constraints = [
        ("sm_ref_unique", "UNIQUE(sm_ref)", "A Steelmatter ref can only be used once."),
    ]

    def write(self, vals):
        before = self.mapped("commercial_partner_id") if PROTECTED_FIELDS & set(vals) else None
        res = super().write(vals)
        if before is not None:
            bump_revision(self.env, self | before)
        return res


class ResPartnerBank(models.Model):
    _inherit = "res.partner.bank"

    @api.model_create_multi
    def create(self, vals_list):
        banks = super().create(vals_list)
        bump_revision(self.env, banks.partner_id)
        return banks

    def write(self, vals):
        before = self.partner_id
        res = super().write(vals)
        bump_revision(self.env, before | self.partner_id)
        return res

    def unlink(self):
        partners = self.partner_id
        res = super().unlink()
        bump_revision(self.env, partners)
        return res
