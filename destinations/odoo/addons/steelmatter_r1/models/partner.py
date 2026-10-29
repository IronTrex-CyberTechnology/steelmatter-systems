"""Supplier revision (D1 v1.1: one definition everywhere).

res.partner.sm_revision is kept on the commercial partner and raised whenever
a protected supplier field changes, including bank accounts. sm_create compares
it under FOR SHARE on that row, so a concurrent change either commits first
(and the create sees the new revision) or waits for the create to commit.

Protected fields are a proposal for D9 (to confirm with Uday and Aakhil).
"""

from odoo import _, api, fields, models
from odoo.exceptions import AccessError

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

    def init(self):
        super().init()
        # The revision only ever increases (R1-DEF-11). Writing it back down
        # (for example to 3 after a bank change took it to 4) would make an
        # authorisation for the old supplier details current again. Enforced
        # in the database, so it holds for every Odoo route.
        self.env.cr.execute("""
            CREATE OR REPLACE FUNCTION sm_partner_revision_forward() RETURNS trigger LANGUAGE plpgsql AS $$
            BEGIN
                IF NEW.sm_revision IS DISTINCT FROM OLD.sm_revision
                   AND (NEW.sm_revision IS NULL OR NEW.sm_revision < OLD.sm_revision) THEN
                    RAISE EXCEPTION 'Steelmatter supplier revision of partner % can only increase (% -> %)',
                        OLD.id, OLD.sm_revision, NEW.sm_revision;
                END IF;
                RETURN NEW;
            END $$;
            DROP TRIGGER IF EXISTS sm_partner_revision_forward ON res_partner;
            CREATE TRIGGER sm_partner_revision_forward BEFORE UPDATE OF sm_revision ON res_partner
                FOR EACH ROW EXECUTE FUNCTION sm_partner_revision_forward();
        """)

    @api.model_create_multi
    def create(self, vals_list):
        for vals in vals_list:
            vals.pop("sm_revision", None)  # always starts at 1
        return super().create(vals_list)

    def write(self, vals):
        if "sm_revision" in vals:
            # Only the module raises it, by its own update (bump_revision);
            # no user, administrator or RPC caller sets it (R1-DEF-11).
            raise AccessError(_("The Steelmatter supplier revision is maintained by Steelmatter and cannot be set."))
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
