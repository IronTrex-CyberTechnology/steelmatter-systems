"""Operation rows, the commit-ordered journal and its head (D3).

steelmatter.operation holds one row per op_ref and is both the lock create and
fence serialise on and the tombstone. The journal is append-only. Neither can
be changed or deleted through the ORM by anyone: only the module's own code,
running as superuser inside the steelmatter.api methods, writes them, and
unlink is always refused. Direct SQL bypasses this (coverage limit).
"""

from odoo import _, fields, models
from odoo.exceptions import AccessError, UserError

OP_STATES = [
    ("new", "New"),          # only inside an uncommitted create or fence
    ("created", "Created"),
    ("fenced", "Fenced"),
    ("refused", "Refused"),
]

JOURNAL_EVENTS = [
    ("created", "Created"),
    ("fenced", "Fenced"),
    ("refused", "Refused"),
    ("po_edited", "Order edited"),
    ("po_cancelled", "Order cancelled"),
    ("po_deleted", "Order deleted"),
    ("foreign_created", "Order created outside Steelmatter"),
]


def _refuse_unless_superuser(records, what):
    if not records.env.su:
        raise AccessError(_("Steelmatter %s records can only be changed by the Steelmatter module.", what))


class SteelmatterOperation(models.Model):
    _name = "steelmatter.operation"
    _description = "Steelmatter operation (op_ref lock and tombstone)"
    _order = "id"

    op_ref = fields.Char(required=True, readonly=True, index=True)
    state = fields.Selection(OP_STATES, required=True, default="new", readonly=True)
    reason = fields.Char(readonly=True)
    permit_digest = fields.Char(readonly=True)
    po_id = fields.Many2one("purchase.order", ondelete="set null", readonly=True, index=True)
    seq = fields.Integer(readonly=True, help="Journal sequence of the operation's last decision")

    _sql_constraints = [
        ("op_ref_unique", "UNIQUE(op_ref)", "An operation reference can only be used once."),
    ]

    def create(self, vals_list):
        _refuse_unless_superuser(self, "operation")
        return super().create(vals_list)

    def write(self, vals):
        _refuse_unless_superuser(self, "operation")
        return super().write(vals)

    def unlink(self):
        raise UserError(_("Steelmatter operation records are permanent and cannot be deleted."))


class SteelmatterJournal(models.Model):
    _name = "steelmatter.journal"
    _description = "Steelmatter destination journal (append-only, commit order)"
    _order = "seq"

    seq = fields.Integer(required=True, readonly=True, index=True)
    event = fields.Selection(JOURNAL_EVENTS, required=True, readonly=True)
    op_ref = fields.Char(readonly=True, index=True)
    po_name = fields.Char(readonly=True)
    amount_minor = fields.Float(digits=(20, 0), readonly=True)
    currency = fields.Char(readonly=True)
    permit_digest = fields.Char(readonly=True)
    reason = fields.Char(readonly=True)

    _sql_constraints = [
        ("seq_unique", "UNIQUE(seq)", "Journal sequence numbers are unique."),
    ]

    def create(self, vals_list):
        _refuse_unless_superuser(self, "journal")
        return super().create(vals_list)

    def write(self, vals):
        raise UserError(_("The Steelmatter journal is append-only."))

    def unlink(self):
        raise UserError(_("The Steelmatter journal is append-only."))


class SteelmatterHead(models.Model):
    _name = "steelmatter.head"
    _description = "Steelmatter journal head"

    last_seq = fields.Integer(readonly=True)
    incarnation = fields.Char(readonly=True)

    def create(self, vals_list):
        raise UserError(_("The Steelmatter journal head is created on install."))

    def write(self, vals):
        _refuse_unless_superuser(self, "journal head")
        return super().write(vals)

    def unlink(self):
        raise UserError(_("The Steelmatter journal head cannot be deleted."))


def append_journal(cr, event, op_ref=None, po_name=None, amount_minor=None, currency=None, permit_digest=None, reason=None):
    """Locks the head row, takes the next sequence and appends a journal row
    in the caller's transaction, so sequence order equals commit order and a
    listing never skips a row. Returns the sequence.

    Known limit: every write queues on this one row; measure at pilot volume.
    """
    cr.execute("SELECT id, last_seq FROM steelmatter_head ORDER BY id LIMIT 1 FOR UPDATE")
    row = cr.fetchone()
    if row is None:
        raise UserError(_("Steelmatter journal head is missing; reinstall the module."))
    head_id, last_seq = row
    seq = last_seq + 1
    cr.execute("UPDATE steelmatter_head SET last_seq = %s WHERE id = %s", [seq, head_id])
    cr.execute(
        "INSERT INTO steelmatter_journal"
        " (seq, event, op_ref, po_name, amount_minor, currency, permit_digest, reason, create_uid, write_uid, create_date, write_date)"
        " VALUES (%s, %s, %s, %s, %s, %s, %s, %s, 1, 1, now() at time zone 'UTC', now() at time zone 'UTC')",
        [seq, event, op_ref, po_name, amount_minor, currency, permit_digest, reason],
    )
    return seq


def head(cr):
    """Returns (last_seq, incarnation) without locking."""
    cr.execute("SELECT last_seq, incarnation FROM steelmatter_head ORDER BY id LIMIT 1")
    row = cr.fetchone()
    return (row[0], row[1]) if row else (0, "")
