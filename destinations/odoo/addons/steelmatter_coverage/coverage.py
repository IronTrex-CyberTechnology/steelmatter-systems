"""Read-only installation profile. No purchase overrides or execution identity."""
from odoo import _, api, models
from odoo.exceptions import AccessError, UserError


class PurchasingCoverage(models.AbstractModel):
    _name = "steelmatter.coverage"
    _description = "Steelmatter purchasing route inventory"

    @api.model
    def report(self):
        if not self.env.user.has_group("base.group_system"):
            raise AccessError(_("Settings access is required to inspect purchasing routes."))
        modules = self.env["ir.module.module"]
        if modules.search_count([("name", "=", "steelmatter_r1"), ("state", "in", ["installed", "to upgrade", "to remove"])]):
            raise UserError(_("Use steelmatter.api.sm_coverage for this enforced installation."))
        users = self.env["res.users"].search([("active", "=", True), ("share", "=", False)])
        access = self.env["ir.model.access"]
        buyers = []
        for user in users.sorted("login"):
            acl = access.with_user(user)
            modes = [mode for mode in ("create", "write", "unlink")
                     if acl.check("purchase.order", mode, raise_exception=False)]
            if modes:
                buyers.append({"login": user.login, "model_rights": modes})
        model = self.env["ir.model"]._get("purchase.order")
        actions = self.env["ir.actions.server"].search([("model_id", "=", model.id)])
        return {
            "database": self.env.cr.dbname,
            "mode": "coverage-only",
            "complete": False,
            "routes": [
                {"id": "odoo_ui_rpc", "posture": "unprotected", "subjects": buyers,
                 "detail": "Model-level purchase rights. Record rules and company restrictions can narrow access."},
                {"id": "automation", "posture": "unknown", "subjects": sorted(actions.mapped("name")),
                 "detail": "Configured purchase-order actions. Inspect their code and effective identity."},
                {"id": "administrators", "posture": "unprotected",
                 "subjects": sorted(u.login for u in users if u.has_group("base.group_system"))},
                {"id": "external_integrations", "posture": "unknown",
                 "detail": "Shared credentials, external scripts and unlisted integrations need separate review."},
                {"id": "direct_database", "posture": "unknown",
                 "detail": "Direct SQL and server-side access cannot be inventoried inside Odoo."},
            ],
        }
