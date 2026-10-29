"""Coverage posture inputs (build guide step 5) for Uday's coverage view.

sm_coverage reports, for each known route to a purchase order in this Odoo
database, whether Steelmatter enforces it, only observes it, leaves it
unprotected, or cannot tell:

    enforced     orders go through steelmatter.api: authorised action, op_ref
                 lock, fence, anchor records
    observed     not prevented, but recorded in the journal (foreign orders,
                 edits, cancels and deletes of Steelmatter orders)
    unprotected  a person, key or process can create or change orders
                 without Steelmatter
    unknown      cannot be verified from inside Odoo (direct SQL, server
                 administrators, unlisted integrations, orders older than the
                 journal)

It is read-only. It inspects users, groups, access rules, API keys, server
actions and installed modules, which the observer cannot read on its own, so
the inspection runs as superuser; it never writes. It never claims complete
coverage: direct database access and server administrators are always
reported as unknown.
"""

import datetime

from odoo import SUPERUSER_ID, api, models, release

from . import anchor
from .api import EXECUTOR, MODULE_VERSION, OBSERVER, SUPPORTED_SERIES
from .operation import head

ENFORCED, OBSERVED, UNPROTECTED, UNKNOWN = "enforced", "observed", "unprotected", "unknown"

# Installed modules that make purchase orders on their own (replenishment,
# agreements, subcontracting, drop-shipping from sales). Each is an
# unprotected route when installed.
ORDER_MAKING_MODULES = {
    "purchase_stock": "reordering rules and procurement create purchase orders",
    "purchase_requisition": "purchase agreements and calls for tenders create purchase orders",
    "sale_purchase": "confirmed sales of service products create purchase orders",
    "purchase_mrp": "manufacturing procurement creates purchase orders",
    "mrp_subcontracting_purchase": "subcontracting creates purchase orders",
    "stock_dropshipping": "drop-ship sales create purchase orders",
}

# Steelmatter records an engine-side write should never touch directly.
STEELMATTER_MODELS = ("steelmatter.operation", "steelmatter.journal", "steelmatter.head")


def _route(rid, route, posture, detail, subjects=None, count=None):
    r = {"id": rid, "route": route, "posture": posture, "detail": detail}
    if subjects is not None:
        r["subjects"] = subjects
    if count is not None:
        r["count"] = count
    return r


class SteelmatterApiCoverage(models.AbstractModel):
    _inherit = "steelmatter.api"

    @api.model
    def sm_coverage(self):
        self._require(OBSERVER, EXECUTOR)
        env = api.Environment(self.env.cr, SUPERUSER_ID, {"active_test": True})
        cr = env.cr
        seq, incarnation = head(cr)
        Access = env["ir.model.access"]

        users = env["res.users"].search([("share", "=", False)])

        def can(user, model, mode):
            return Access.with_user(user).check(model, mode, raise_exception=False)

        def granting_groups(model, mode):
            return sorted(Access.group_names_with_access(model, mode))

        executor_group, observer_group = env.ref(EXECUTOR), env.ref(OBSERVER)
        steelmatter_users = executor_group.users | observer_group.users

        # --- the module itself -------------------------------------------
        mod = env["ir.module.module"].search([("name", "=", "steelmatter_r1")], limit=1)
        module = {
            "name": "steelmatter_r1",
            "installed": mod.state == "installed",
            "version": mod.latest_version or "",
            "expected_version": MODULE_VERSION,
            "odoo": release.version,
            "supported": release.serie == SUPPORTED_SERIES,
        }
        module_ok = module["installed"] and module["supported"] and module["version"] == MODULE_VERSION
        try:
            anchor.proof("coverage-check")
            anchor_state = {"configured": True, "reachable": True}
        except anchor.AnchorUnavailable as e:
            anchor_state = {"configured": "not set" not in str(e), "reachable": False}

        routes = []
        if module_ok:
            routes.append(_route("steelmatter_api", "Orders through Steelmatter (steelmatter.api)", ENFORCED,
                                 "authorised canonical action only; op_ref lock and fence; restore-proof anchor records"
                                 + ("" if anchor_state["reachable"] else "; anchor unreachable now, so creates fail closed")))
        else:
            routes.append(_route("steelmatter_api", "Orders through Steelmatter (steelmatter.api)", UNKNOWN,
                                 "module %s, version %s (expected %s), Odoo %s%s" % (
                                     "installed" if module["installed"] else "not installed", module["version"] or "none",
                                     MODULE_VERSION, release.version, "" if module["supported"] else " (unsupported series)")))

        # --- people and keys that can make or change orders directly -----
        # The module refuses purchase-order writes from everyone but its own
        # execution path and administrators (R1 decision 6, R1-DEF-10). Users
        # whose Odoo groups would otherwise allow it are listed as blocked;
        # administrators keep the ability and are the unprotected route.
        direct, blocked = [], []
        for u in users - steelmatter_users:
            c, w = can(u, "purchase.order", "create"), can(u, "purchase.order", "write")
            if c or w:
                entry = {"login": u.login, "name": u.name, "create": c, "confirm_or_edit": w,
                         "groups": sorted(g.full_name for g in u.groups_id if g.full_name.startswith(("Purchase", "Administration", "Inventory")))}
                (direct if u.has_group("base.group_system") else blocked).append(entry)
        direct.sort(key=lambda s: s["login"])
        blocked.sort(key=lambda s: s["login"])
        routes.append(_route(
            "direct_users", "Odoo users who can create or confirm purchase orders without Steelmatter",
            UNPROTECTED if direct else ENFORCED,
            ("administrators only; %d other users with purchase rights are refused by the module" % len(blocked)) if direct
            else "no one: %d users with purchase rights are refused by the module" % len(blocked),
            subjects=direct, count=len(direct)))
        routes[-1]["blocked_by_module"] = blocked

        keyed = []
        for u in users - steelmatter_users:
            n = env["res.users.apikeys"].search_count([("user_id", "=", u.id)])
            if n and u.has_group("base.group_system"):
                keyed.append({"login": u.login, "api_keys": n})
        routes.append(_route("api_keys", "API keys of administrators (the only users who can write purchase orders)",
                             UNPROTECTED if keyed else ENFORCED,
                             "an integration holding one of these keys writes orders without Steelmatter" if keyed
                             else "none found", subjects=keyed, count=len(keyed)))

        companies = env["res.company"].search_count([])
        routes.append(_route("single_company", "One Odoo company per deployment", ENFORCED if companies == 1 else UNKNOWN,
                             "one company" if companies == 1
                             else "%d companies: Steelmatter refuses every create until there is one" % companies, count=companies))

        admins = [{"login": u.login, "name": u.name} for u in users if u.has_group("base.group_system")]
        routes.append(_route("administrators", "Odoo administrators (Settings)", UNPROTECTED,
                             "can change access rights, install or remove modules, or uninstall steelmatter_r1",
                             subjects=admins, count=len(admins)))

        # --- identity hygiene: the two Steelmatter identities -------------
        identities = {"executor": [], "observer": []}
        for u in executor_group.users:
            identities["executor"].append({
                "login": u.login, "direct_create": can(u, "purchase.order", "create"), "direct_write": can(u, "purchase.order", "write"),
                "also_observer": u in observer_group.users})
        for u in observer_group.users:
            writes = [m for m in ("purchase.order", "purchase.order.line", "res.partner") + STEELMATTER_MODELS
                      if can(u, m, "write") or can(u, m, "create") or can(u, m, "unlink")]
            identities["observer"].append({"login": u.login, "read_only": not writes and u not in executor_group.users,
                                           "writes": writes})
        bad_exec = [i["login"] for i in identities["executor"] if i["direct_create"] or i["direct_write"]]
        bad_obs = [i["login"] for i in identities["observer"] if not i["read_only"]]
        if bad_exec:
            routes.append(_route("executor_bypass", "Executor identity can write orders without the module", UNPROTECTED,
                                 "the executor must have no purchase rights of its own", subjects=bad_exec, count=len(bad_exec)))
        if bad_obs:
            routes.append(_route("observer_writes", "Observer identity is not read-only", UNPROTECTED,
                                 "the observer must have no write, create or delete access", subjects=bad_obs, count=len(bad_obs)))

        # --- processes that make orders on their own ----------------------
        installed = env["ir.module.module"].search([("name", "in", list(ORDER_MAKING_MODULES)), ("state", "=", "installed")])
        for m in installed.sorted("name"):
            routes.append(_route("module:" + m.name, "Module %s" % m.name, UNPROTECTED, ORDER_MAKING_MODULES[m.name]))

        po_model = env["ir.model"]._get("purchase.order")
        recs = env["ir.actions.server"].search([("model_id", "=", po_model.id)])
        recs |= env["ir.cron"].search([("model_id", "=", po_model.id)]).ir_actions_server_id
        if "base.automation" in env:
            recs |= env["base.automation"].search([("model_id", "=", po_model.id)]).action_server_ids
        # Standard actions shipped by installed Odoo modules are buttons that
        # run with the clicking user's rights (covered by direct_users).
        # Anything else was added in this database and may run as anyone.
        standard_modules = set(env["ir.module.module"].search([("state", "=", "installed"), ("author", "ilike", "Odoo")]).mapped("name"))
        xmlids = recs.get_external_id()
        custom = sorted(r.name for r in recs if xmlids.get(r.id, "").split(".")[0] not in standard_modules)
        standard = sorted(r.name for r in recs if r.name not in custom)
        routes.append(_route("automation", "Custom server actions, scheduled actions and automations on purchase orders",
                             UNKNOWN,
                             ("added in this database; they may create or change orders as their own user, review each"
                              if custom else "none found; integrations outside Odoo (imports, EDI, scripts) cannot be listed")
                             + ("; %d standard Odoo actions run with the user's own rights" % len(standard) if standard else ""),
                             subjects=custom, count=len(custom)))

        # --- what the journal sees ----------------------------------------
        def journal_count(event):
            return env["steelmatter.journal"].search_count([("event", "=", event)])

        foreign = journal_count("foreign_created")
        routes.append(_route("foreign_orders", "Orders created outside Steelmatter", OBSERVED,
                             "recorded in the journal when created through Odoo; never authorised by Steelmatter", count=foreign))
        cr.execute("SELECT count(*) FROM purchase_order WHERE sm_op_ref IS NULL")
        no_ref = cr.fetchone()[0]
        unjournalled = max(0, no_ref - foreign)
        if unjournalled:
            routes.append(_route("unjournalled_orders", "Orders without an op_ref and no journal row", UNKNOWN,
                                 "created before the module was installed, or written around the ORM (direct SQL)",
                                 count=unjournalled))
        changes = {e: journal_count(e) for e in ("po_edited", "po_cancelled", "po_deleted")}
        routes.append(_route("order_changes", "Edits, cancels and deletes of Steelmatter orders in Odoo", OBSERVED,
                             "recorded in the journal and flagged by observe; never refunded automatically",
                             count=sum(changes.values())))
        routes[-1]["by_event"] = changes

        routes.append(_route("direct_database", "Direct database access and Odoo server administrators", UNKNOWN,
                             "SQL, the Odoo shell and server file access bypass every Odoo control and cannot be seen from inside Odoo"))

        summary = {p: sum(1 for r in routes if r["posture"] == p) for p in (ENFORCED, OBSERVED, UNPROTECTED, UNKNOWN)}
        return {
            "generated_at": datetime.datetime.now(datetime.timezone.utc).isoformat().replace("+00:00", "Z"),
            "database": cr.dbname,
            "seq": seq,
            "incarnation": incarnation,
            "module": module,
            "anchor": anchor_state,
            "identities": identities,
            "routes": routes,
            "summary": summary,
            "complete": False,  # never claimed: see direct_database
        }
