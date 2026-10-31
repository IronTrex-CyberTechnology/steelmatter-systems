"""Engine reference keys on Odoo records.

The engine never sends Odoo IDs. company_ref, supplier_ref, delivery_ref,
item_ref, unit_ref and tax_refs are mapped through these sm_ref fields, each
unique where set.
"""

from odoo import fields, models


class ResCompany(models.Model):
    _inherit = "res.company"

    sm_ref = fields.Char("Steelmatter company ref", copy=False, index=True)

    _sql_constraints = [
        ("sm_ref_unique", "UNIQUE(sm_ref)", "A Steelmatter company ref can only be used once."),
    ]


class UomUom(models.Model):
    _inherit = "uom.uom"

    sm_ref = fields.Char("Steelmatter unit ref", copy=False, index=True)

    _sql_constraints = [
        ("sm_ref_unique", "UNIQUE(sm_ref)", "A Steelmatter unit ref can only be used once."),
    ]


class AccountTax(models.Model):
    _inherit = "account.tax"

    sm_ref = fields.Char("Steelmatter tax ref", copy=False, index=True)

    _sql_constraints = [
        ("sm_ref_unique", "UNIQUE(sm_ref)", "A Steelmatter tax ref can only be used once."),
    ]


class ProductProduct(models.Model):
    _inherit = "product.product"

    sm_ref = fields.Char("Steelmatter item ref", copy=False, index=True)

    _sql_constraints = [
        ("sm_ref_unique", "UNIQUE(sm_ref)", "A Steelmatter item ref can only be used once."),
    ]
