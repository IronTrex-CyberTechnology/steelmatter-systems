"""Engine reference keys on Odoo records.

The engine never sends Odoo IDs. company_ref, supplier_ref, delivery_ref and
item_ref are mapped through these sm_ref fields, each unique where set.
"""

from odoo import fields, models


class ResCompany(models.Model):
    _inherit = "res.company"

    sm_ref = fields.Char("Steelmatter company ref", copy=False, index=True)

    _sql_constraints = [
        ("sm_ref_unique", "UNIQUE(sm_ref)", "A Steelmatter company ref can only be used once."),
    ]


class ProductProduct(models.Model):
    _inherit = "product.product"

    sm_ref = fields.Char("Steelmatter item ref", copy=False, index=True)

    _sql_constraints = [
        ("sm_ref_unique", "UNIQUE(sm_ref)", "A Steelmatter item ref can only be used once."),
    ]
