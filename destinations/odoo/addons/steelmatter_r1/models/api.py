"""steelmatter.api: the destination calls of D1 v1.2.

    sm_create(action)                          execute (executor identity)
    sm_fence(op_ref, expected_min_seq)         fence   (executor identity)
    sm_lookup(op_ref)                          lookup  (executor or observer)
    sm_list_since(checkpoint_seq, limit)       list    (observer; caller's own rights)
    sm_readiness(action)                       readiness, advisory (executor)
    sm_observe(action)                         observe (observer; caller's own rights)

Every call returns the D1 result envelope as a dict. The connector maps a
transport failure or an RPC error to UNKNOWN; this module never turns an
error into a refusal or NOT_FOUND_NOW.

sm_create and sm_fence run in their own READ COMMITTED transaction (Odoo's
cursors default to REPEATABLE READ, where a concurrent create on the same
op_ref or the journal head raises a serialisation failure and Odoo replays the
whole call; a replay after CREATE_INTENT landed would wrongly block the
op_ref). Both start with INSERT ... ON CONFLICT (op_ref) DO NOTHING and then
SELECT ... FOR UPDATE on that row, so they serialise on one lock and recheck
durable state under it. Anchor I/O happens before the journal head lock, so it
does not serialise creates for different op_refs.

Order of sm_create (agreed with Uday, D5 v3):
  1. restore gate: head behind expected_min_seq -> REFUSED_RESTORE_SUSPECTED, nothing written
  2. lock the operation row
  3. read the anchor (unreadable -> UNAVAILABLE, nothing written)
     BLOCK -> REFUSED_FENCED; fenced/refused row -> REFUSED_FENCED; created row -> EXISTS or MISMATCH;
     CREATE_INTENT and no row -> BLOCK, REFUSED_PRIOR_ATTEMPT
  4. scope, mappings and supplier revision (partner FOR SHARE); failure -> BLOCK, refusal
  5. CREATE_INTENT (write fails -> rollback, UNAVAILABLE)
  6. create and confirm the purchase order in this transaction
  7. head lock, journal row, COMMIT
  8. CREATE_DONE (best effort) -> CREATED
"""

import datetime
import os

from psycopg2.extensions import ISOLATION_LEVEL_READ_COMMITTED

from odoo import SUPERUSER_ID, _, api, models
from odoo.exceptions import AccessError

from . import anchor
from .operation import append_journal, head
from .purchase import minor

EXECUTOR = "steelmatter_r1.group_executor"
OBSERVER = "steelmatter_r1.group_observer"
MODULE_VERSION = "18.0.0.1.0"
SUPPORTED_SERIES = "18.0"

STATE_CONFIRMED = "purchase"

# Structured refusal for an action outside the configured scope (mapping
# missing, other currency, approval not one step, amount not the sum of the
# lines). Decided before CREATE_INTENT and written with a BLOCK, so it can
# release (block_committed, no prior create). Not yet in the Go
# connector.Outcome list: the engine holds it as unrecognised until the next
# fence returns FENCED. To agree with Uday (D1 v1.3).
REFUSED_OUT_OF_SCOPE = "REFUSED_OUT_OF_SCOPE"


def _failpoint(name, op_ref):
    """Test-only fault injection for the sandbox's recovery tests. Off unless
    the server environment sets STEELMATTER_FAILPOINTS (never in production),
    and then only for op_refs starting with "fp-<name>-".

        commit_after_intent  the create fails after CREATE_INTENT, before commit
        lose_create_done     the best-effort CREATE_DONE write is skipped
    """
    enabled = os.environ.get("STEELMATTER_FAILPOINTS", "")
    return bool(enabled) and name in enabled.split(",") and op_ref.startswith("fp-%s-" % name)


class FailpointError(Exception):
    pass


class _OutOfScope(Exception):
    def __init__(self, reason):
        super().__init__(reason)
        self.reason = reason


def _now_iso():
    return datetime.datetime.now(datetime.timezone.utc).isoformat().replace("+00:00", "Z")


def _check_action(action):
    """Shape check only; a malformed action is a programming fault."""
    if not isinstance(action, dict):
        raise ValueError("action must be an object")
    for k, t in (("op_ref", str), ("company_ref", str), ("delivery_ref", str), ("supplier_ref", str),
                 ("currency", str), ("permit_digest", str)):
        if not isinstance(action.get(k), t) or not action[k]:
            raise ValueError("action.%s is required" % k)
    for k in ("action_revision", "supplier_revision", "amount_minor", "expected_min_seq"):
        if not isinstance(action.get(k, 0), int) or isinstance(action.get(k, 0), bool):
            raise ValueError("action.%s must be an integer" % k)
    lines = action.get("lines")
    if not isinstance(lines, list) or not lines:
        raise ValueError("action.lines must be a non-empty list")
    for ln in lines:
        if not isinstance(ln, dict) or not isinstance(ln.get("item_ref"), str) \
                or not all(isinstance(ln.get(k), int) and not isinstance(ln.get(k), bool) for k in ("quantity", "unit_price_minor")):
            raise ValueError("action.lines entries need item_ref, quantity and unit_price_minor")
    return action


def order_facts(po):
    """The destination facts the engine verifies against the authorised
    action (connector.Order)."""
    cur = po.currency_id
    lines = []
    for ln in po.order_line.filtered(lambda l: not l.display_type).sorted(lambda l: (l.sequence, l.id)):
        qty = ln.product_qty
        lines.append({
            "item_ref": ln.product_id.sm_ref or "",
            "quantity": int(qty) if qty == int(qty) else -1,  # a fractional quantity never matches
            "unit_price_minor": minor(ln.price_unit, cur),
        })
    return {
        "id": po.name,
        "state": po.state,
        "company_ref": po.company_id.sm_ref or "",
        "delivery_ref": po.dest_address_id.sm_ref or "",
        "supplier_ref": po.partner_id.sm_ref or "",
        "supplier_revision": po.sm_supplier_revision,
        "lines": lines,
        "amount_minor": minor(po.amount_total, cur),
        "currency": cur.name,
        "permit_digest": po.sm_permit_digest or "",
    }


def _same_order(facts, action):
    return (facts["company_ref"] == action["company_ref"] and facts["delivery_ref"] == action["delivery_ref"]
            and facts["supplier_ref"] == action["supplier_ref"] and facts["supplier_revision"] == action["supplier_revision"] and facts["amount_minor"] == action["amount_minor"]
            and facts["currency"] == action["currency"] and facts["permit_digest"] == action["permit_digest"]
            and facts["lines"] == [{"item_ref": l["item_ref"], "quantity": l["quantity"], "unit_price_minor": l["unit_price_minor"]}
                                   for l in action["lines"]])


class SteelmatterApi(models.AbstractModel):
    _name = "steelmatter.api"
    _description = "Steelmatter R1 destination API"

    # ------------------------------------------------------------------ helpers

    def _require(self, *groups):
        if not any(self.env.user.has_group(g) for g in groups):
            raise AccessError(_("This Steelmatter call needs the %s group.", " or ".join(groups)))

    def _own_cursor(self):
        cr = self.env.registry.cursor()
        # No transaction has started on a fresh cursor yet, so the level can
        # still change.
        cr.connection.set_isolation_level(ISOLATION_LEVEL_READ_COMMITTED)
        return cr

    def _sm_env(self, cr):
        return api.Environment(cr, SUPERUSER_ID, {
            "tracking_disable": True, "mail_create_nolog": True, "mail_notrack": True,
            "mail_auto_subscribe_no_notify": True, "steelmatter_create": True,
        })

    @staticmethod
    def _lock(cr, op_ref):
        cr.execute(
            "INSERT INTO steelmatter_operation (op_ref, state, create_uid, write_uid, create_date, write_date)"
            " VALUES (%s, 'new', 1, 1, now() at time zone 'UTC', now() at time zone 'UTC')"
            " ON CONFLICT (op_ref) DO NOTHING",
            [op_ref],
        )
        cr.execute("SELECT id, state, permit_digest, po_id, seq FROM steelmatter_operation WHERE op_ref = %s FOR UPDATE", [op_ref])
        oid, state, digest, po_id, seq = cr.fetchone()
        return {"id": oid, "state": state, "permit_digest": digest, "po_id": po_id, "seq": seq}

    @staticmethod
    def _decide(env, op, state, reason=None, digest=None, po=None):
        """Records the operation's decision and its journal row (takes the
        head lock, so call it last before commit)."""
        facts = order_facts(po) if po else None
        seq = append_journal(env.cr, state, op_ref=None if op is None else op["op_ref"],
                             po_name=facts["id"] if facts else None,
                             amount_minor=facts["amount_minor"] if facts else None,
                             currency=facts["currency"] if facts else None,
                             permit_digest=digest, reason=reason)
        env.cr.execute(
            "UPDATE steelmatter_operation SET state = %s, reason = %s, permit_digest = COALESCE(%s, permit_digest),"
            " po_id = %s, seq = %s, write_date = now() at time zone 'UTC' WHERE id = %s",
            [state, reason, digest, po.id if po else op["po_id"], seq, op["id"]],
        )
        op.update(state=state, seq=seq)
        return seq

    def _result(self, env, op_ref, call, outcome, reason="", proof_known=True, block_committed=None):
        """Builds the D1 result envelope from durable state, reading the
        restore-proof record for the flags (unknown if it cannot be read)."""
        cr = env.cr
        last_seq, incarnation = head(cr)
        cr.execute("SELECT now() at time zone 'UTC'")
        tx_time = cr.fetchone()[0].replace(tzinfo=datetime.timezone.utc).isoformat().replace("+00:00", "Z")
        cr.execute("SELECT seq, po_id FROM steelmatter_operation WHERE op_ref = %s AND state <> 'new'", [op_ref])
        row = cr.fetchone()
        r = {
            "op_ref": op_ref, "call": call, "outcome": outcome, "reason": reason,
            "seq": row[0] if row else last_seq, "incarnation": incarnation,
            "odoo_tx_time": tx_time, "connector_received_at": None,
            "source": "odoo/steelmatter_r1@%s/%s" % (MODULE_VERSION, env.context.get("sm_identity", "executor")),
            "destination": None, "block_committed": None, "prior_create": None, "create_done": False,
        }
        po = env["purchase.order"].sudo().browse(row[1]) if row and row[1] else None
        if po:
            r["destination"] = order_facts(po)
        if proof_known:
            try:
                p = anchor.proof(op_ref)
            except anchor.AnchorUnavailable:
                p = None
            if p is not None:
                r["block_committed"], r["prior_create"] = p["blocked"], p["create_intent"]
                if p["create_done"] and not po:
                    # The order was erased (restore or deletion); CREATE_DONE
                    # still establishes it.
                    r["create_done"], r["destination"] = True, p["create_done"]
                    r["reason"] = r["reason"] or "ORDER_ERASED_CREATE_DONE"
        if block_committed is not None:
            r["block_committed"] = block_committed
        return r

    # ------------------------------------------------------------------ execute

    @api.model
    def sm_create(self, action):
        self._require(EXECUTOR)
        a = _check_action(action)
        cr = self._own_cursor()
        try:
            env = self._sm_env(cr)
            try:
                outcome, reason, done = self._create(env, a)
                cr.commit()
            except anchor.AnchorUnavailable:
                cr.rollback()
                outcome, reason, done = "UNAVAILABLE", "ANCHOR_UNAVAILABLE", None
            except Exception:
                cr.rollback()
                raise
            if done and not _failpoint("lose_create_done", a["op_ref"]):
                try:
                    anchor.append(anchor.CREATE_DONE, a["op_ref"], done)
                except anchor.AnchorUnavailable:
                    pass  # best effort: without it, case G applies after a restore (HELD)
            if outcome == "UNAVAILABLE":
                res = self._result(env, a["op_ref"], "execute", outcome, reason, proof_known=False)
            else:
                res = self._result(env, a["op_ref"], "execute", outcome, reason)
            cr.commit()
            return res
        finally:
            cr.close()

    def _create(self, env, a):
        cr, op_ref = env.cr, a["op_ref"]
        last_seq, _inc = head(cr)
        if a.get("expected_min_seq", 0) > last_seq:
            # Defence in depth only; the BLOCK check below is the guarantee.
            return "REFUSED_RESTORE_SUSPECTED", "HEAD_BEHIND_EXPECTED", None
        op = self._lock(cr, op_ref)
        op["op_ref"] = op_ref
        p = anchor.proof(op_ref)
        if p["blocked"]:
            # Checked first, so a delayed create cannot pass after a restore
            # erased the in-Odoo tombstone, whatever sequence it carries.
            if op["state"] == "new":
                self._decide(env, op, "fenced", "anchor block", a["permit_digest"])
            return "REFUSED_FENCED", "", None
        if op["state"] in ("fenced", "refused"):
            return "REFUSED_FENCED", "", None
        if op["state"] == "created":
            return ("EXISTS" if op["permit_digest"] == a["permit_digest"] else "MISMATCH"), "", None
        if p["create_intent"]:
            # An earlier attempt left CREATE_INTENT but no row: the order was
            # erased by a restore or its commit failed. Never create again.
            anchor.append(anchor.BLOCK, op_ref, {"op_ref": op_ref, "reason": "prior attempt"})
            self._decide(env, op, "refused", "prior attempt", a["permit_digest"])
            return "REFUSED_PRIOR_ATTEMPT", "", None
        try:
            scope = self._resolve(env, a)
        except _OutOfScope as e:
            anchor.append(anchor.BLOCK, op_ref, {"op_ref": op_ref, "reason": "out of scope: " + e.reason})
            self._decide(env, op, "refused", e.reason, a["permit_digest"])
            return REFUSED_OUT_OF_SCOPE, e.reason, None
        cr.execute("SELECT sm_revision FROM res_partner WHERE id = %s FOR SHARE", [scope["supplier"].commercial_partner_id.id])
        if cr.fetchone()[0] != a["supplier_revision"]:
            anchor.append(anchor.BLOCK, op_ref, {"op_ref": op_ref, "reason": "supplier stale"})
            self._decide(env, op, "refused", "supplier stale", a["permit_digest"])
            return "REFUSED_SUPPLIER_STALE", "", None
        # Written before the create commits. If it fails the transaction rolls
        # back and the answer is UNAVAILABLE, never "not sent".
        anchor.append(anchor.CREATE_INTENT, op_ref, {
            "op_ref": op_ref, "permit_digest": a["permit_digest"], "company_ref": a["company_ref"],
            "supplier_ref": a["supplier_ref"], "supplier_revision": a["supplier_revision"],
            "amount_minor": a["amount_minor"], "currency": a["currency"],
        })
        po = self._create_order(env, a, scope)
        facts = order_facts(po)
        if facts["state"] != STATE_CONFIRMED or not _same_order(facts, a):
            # Rolls back; CREATE_INTENT stays, so the operation goes HELD.
            raise ValueError("created order does not match the authorised action")
        self._decide(env, op, "created", None, a["permit_digest"], po)
        if _failpoint("commit_after_intent", op_ref):
            raise FailpointError("commit_after_intent")
        return "CREATED", "", facts

    def _resolve(self, env, a):
        """Maps the engine refs and checks the configured scope. Runs before
        CREATE_INTENT, so a refusal here carries prior_create = false."""

        def one(model, domain, what):
            recs = env[model].search(domain, limit=2)
            if len(recs) != 1:
                raise _OutOfScope("MAPPING_MISSING:" + what)
            return recs

        company = one("res.company", [("sm_ref", "=", a["company_ref"])], "company")
        if company.po_double_validation != "one_step":
            raise _OutOfScope("APPROVAL_NOT_ONE_STEP")
        supplier = one("res.partner", [("sm_ref", "=", a["supplier_ref"]), ("company_id", "in", [False, company.id])], "supplier")
        delivery = one("res.partner", [("sm_ref", "=", a["delivery_ref"]), ("company_id", "in", [False, company.id])], "delivery")
        currency = env["res.currency"].search([("name", "=", a["currency"])], limit=1)
        if not currency or currency != company.currency_id:
            raise _OutOfScope("CURRENCY_OUT_OF_SCOPE")
        products = []
        total = 0
        for ln in a["lines"]:
            if ln["quantity"] <= 0 or ln["unit_price_minor"] < 0:
                raise _OutOfScope("LINE_OUT_OF_SCOPE")
            prod = one("product.product", [("sm_ref", "=", ln["item_ref"]), ("purchase_ok", "=", True),
                                           ("company_id", "in", [False, company.id])], "item")
            products.append(prod)
            total += ln["quantity"] * ln["unit_price_minor"]
        if total != a["amount_minor"]:
            raise _OutOfScope("AMOUNT_NOT_SUM_OF_LINES")
        return {"company": company, "supplier": supplier, "delivery": delivery, "currency": currency, "products": products}

    def _create_order(self, env, a, scope):
        cur = scope["currency"]
        env = env(context=dict(env.context, allowed_company_ids=[scope["company"].id]))
        po = env["purchase.order"].with_company(scope["company"]).create({
            "company_id": scope["company"].id,
            "partner_id": scope["supplier"].id,
            "dest_address_id": scope["delivery"].id,
            "currency_id": cur.id,
            "sm_op_ref": a["op_ref"],
            "sm_permit_digest": a["permit_digest"],
            "sm_supplier_revision": a["supplier_revision"],
            "order_line": [(0, 0, {
                "product_id": prod.id,
                "name": prod.display_name,
                "product_qty": ln["quantity"],
                "price_unit": ln["unit_price_minor"] / 10 ** cur.decimal_places,
                "taxes_id": [(6, 0, [])],
            }) for prod, ln in zip(scope["products"], a["lines"])],
        })
        po.button_confirm()
        return po

    # ------------------------------------------------------------------ fence

    @api.model
    def sm_fence(self, op_ref, expected_min_seq=0):
        self._require(EXECUTOR)
        if not isinstance(op_ref, str) or not op_ref:
            raise ValueError("op_ref is required")
        cr = self._own_cursor()
        try:
            env = self._sm_env(cr)
            block_committed = None
            try:
                outcome, reason, block_committed, commit = self._fence(env, op_ref, expected_min_seq or 0)
                if commit:
                    cr.commit()
                else:
                    cr.rollback()
            except anchor.AnchorUnavailable:
                cr.rollback()
                outcome, reason = "UNAVAILABLE", "ANCHOR_UNAVAILABLE"
            except Exception:
                cr.rollback()
                raise
            res = self._result(env, op_ref, "fence", outcome, reason, proof_known=outcome != "UNAVAILABLE",
                               block_committed=block_committed)
            cr.commit()
            return res
        finally:
            cr.close()

    def _fence(self, env, op_ref, expected_min_seq):
        cr = env.cr
        last_seq, _inc = head(cr)
        if expected_min_seq > last_seq:
            return "REFUSED_RESTORE_SUSPECTED", "HEAD_BEHIND_EXPECTED", None, False
        op = self._lock(cr, op_ref)
        op["op_ref"] = op_ref
        p = anchor.proof(op_ref)
        if op["state"] == "created":
            # Create won the lock: never FENCED.
            return "EXISTS", "" if op["po_id"] else "ORDER_DELETED", None, False
        if op["state"] == "new" and p["create_done"]:
            # The order was erased by a restore but CREATE_DONE establishes
            # it: block any re-create and report the order (case E).
            anchor.append(anchor.BLOCK, op_ref, {"op_ref": op_ref, "reason": "fenced"})
            self._decide(env, op, "fenced", "created before restore")
            return "EXISTS", "", None, True
        try:
            # Recorded before FENCED is reported.
            anchor.append(anchor.BLOCK, op_ref, {"op_ref": op_ref, "reason": "fenced"})
        except anchor.AnchorUnavailable:
            return "UNAVAILABLE", "BLOCK_WRITE_FAILED", False, False
        if op["state"] == "new":
            self._decide(env, op, "fenced", "fenced")
            return "FENCED", "", None, True
        return "FENCED", "", None, False  # already fenced or refused; idempotent

    # ------------------------------------------------------------------ reads

    @api.model
    def sm_lookup(self, op_ref):
        self._require(EXECUTOR, OBSERVER)
        if not isinstance(op_ref, str) or not op_ref:
            raise ValueError("op_ref is required")
        identity = "executor" if self.env.user.has_group(EXECUTOR) else "observer"
        cr = self._own_cursor()
        try:
            env = self._sm_env(cr)(context={"sm_identity": identity})
            cr.execute("SELECT state, po_id FROM steelmatter_operation WHERE op_ref = %s AND state <> 'new'", [op_ref])
            row = cr.fetchone()
            if row is None:
                res = self._result(env, op_ref, "lookup", "NOT_FOUND_NOW")
                if res["create_done"]:
                    res["outcome"] = "FOUND"  # order erased; CREATE_DONE establishes it
                elif res["prior_create"] is None:
                    res["reason"] = "ANCHOR_UNAVAILABLE"
                return res
            state, po_id = row
            if state in ("fenced", "refused"):
                return self._result(env, op_ref, "lookup", "FENCED")
            if not po_id:
                return self._result(env, op_ref, "lookup", "MISMATCH", "ORDER_DELETED")
            po = env["purchase.order"].browse(po_id)
            outcome = "FOUND" if po.state == STATE_CONFIRMED else "MISMATCH"
            return self._result(env, op_ref, "lookup", outcome)
        finally:
            cr.rollback()
            cr.close()

    @api.model
    def sm_list_since(self, checkpoint_seq=0, limit=500):
        """Journal rows with seq > checkpoint_seq in commit order, under the
        caller's own rights (no sudo)."""
        self._require(OBSERVER, EXECUTOR)
        limit = max(1, min(int(limit or 500), 5000))
        journal = self.env["steelmatter.journal"]
        recs = journal.search([("seq", ">", int(checkpoint_seq or 0))], order="seq", limit=limit)
        hd = self.env["steelmatter.head"].search([], limit=1)
        state_of = {"created": "created", "fenced": "fenced", "refused": "refused", "foreign_created": "foreign",
                    "po_edited": "created", "po_cancelled": "created", "po_deleted": "created"}
        rows = []
        for j in recs:
            rows.append({
                "seq": j.seq, "op_ref": j.op_ref or "", "state": state_of[j.event], "event": j.event,
                "order_id": "" if j.event == "po_deleted" else (j.po_name or ""),
                "amount_minor": int(j.amount_minor or 0), "currency": j.currency or "",
                "permit_digest": j.permit_digest or "", "tombstoned": j.event in ("fenced", "refused"),
                "foreign": j.event == "foreign_created",
            })
        return {
            "rows": rows,
            "next_checkpoint": rows[-1]["seq"] if rows else int(checkpoint_seq or 0),
            "head_seq": hd.last_seq, "incarnation": hd.incarnation or "",
        }

    @api.model
    def sm_readiness(self, action):
        """Advisory only: READY cannot rule out a change before the write."""
        self._require(EXECUTOR)
        a = _check_action(action)
        env = self._sm_env(self.env.cr)
        res = {"op_ref": a["op_ref"], "call": "readiness", "outcome": "READY", "reason": "",
               "seq": head(self.env.cr)[0], "source": "odoo/steelmatter_r1@%s/executor" % MODULE_VERSION}
        try:
            scope = self._resolve(env, a)
            if scope["supplier"].commercial_partner_id.sm_revision != a["supplier_revision"]:
                res.update(outcome="NOT_READY", reason="SUPPLIER_CHANGED")
            else:
                anchor.proof(a["op_ref"])
        except _OutOfScope as e:
            res.update(outcome="NOT_READY", reason="MAPPING_MISSING" if e.reason.startswith("MAPPING_MISSING") else e.reason)
        except anchor.AnchorUnavailable:
            res.update(outcome="NOT_READY", reason="DESTINATION_UNAVAILABLE")
        return res

    @api.model
    def sm_observe(self, action):
        """Read-only comparison under the caller's own rights. Evidence only."""
        self._require(OBSERVER)
        a = _check_action(action)
        res = {"op_ref": a["op_ref"], "call": "observe", "outcome": "NOT_FOUND", "reason": "",
               "seq": self.env["steelmatter.head"].search([], limit=1).last_seq,
               "source": "odoo/steelmatter_r1@%s/observer" % MODULE_VERSION, "destination": None}
        po = self.env["purchase.order"].search([("sm_op_ref", "=", a["op_ref"])], limit=1)
        if not po:
            return res
        facts = order_facts(po)
        res["destination"] = facts
        if facts["state"] != STATE_CONFIRMED:
            res["outcome"] = "INSUFFICIENT_EVIDENCE"
        else:
            res["outcome"] = "MATCH" if _same_order(facts, a) else "MISMATCH"
        return res

    @api.model
    def sm_version(self):
        self._require(EXECUTOR, OBSERVER)
        from odoo import release
        return {"module": MODULE_VERSION, "odoo": release.version, "supported": release.serie == SUPPORTED_SERIES}
