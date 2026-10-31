"""Supplier notifications in the sandbox (build guide step 1: "disable or
safely route outbound email; explicitly test the chosen configuration").

The chosen configuration: no outgoing mail server in Odoo, SMTP pointed at a
closed local port (127.0.0.1:9), and supplier addresses on the reserved
.invalid domain. These tests show that a Steelmatter order queues no email to
the supplier, and that even an email forced into the queue cannot leave.

    python3 -I destinations/odoo/tests/mail_test.py -v
"""

import json
import os
import subprocess
import sys
import unittest
import uuid

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from sandbox_client import action, admin, executor, odoo_sql  # noqa: E402

RUN = uuid.uuid4().hex[:8]
EXECUTOR = ADMIN = None


def setUpModule():
    global EXECUTOR, ADMIN
    EXECUTOR, ADMIN = executor(), admin()


def supplier():
    pid, email = odoo_sql("SELECT id, email FROM res_partner WHERE sm_ref = 'sup-chairs'")[0].split("|")
    return int(pid), email


def mails_to(partner_id, email, since_id):
    """Outgoing mail.mail rows after since_id addressed to the supplier."""
    q = ("SELECT count(*) FROM mail_mail m WHERE m.id > %d AND (m.email_to ILIKE '%%%s%%' OR EXISTS ("
         "SELECT 1 FROM mail_mail_res_partner_rel r WHERE r.mail_mail_id = m.id AND r.res_partner_id = %d))"
         % (since_id, email, partner_id))
    return int(odoo_sql(q)[0])


def last_mail_id():
    return int(odoo_sql("SELECT COALESCE(max(id), 0) FROM mail_mail")[0])


class OutboundMail(unittest.TestCase):
    def test_no_outgoing_mail_server_and_smtp_is_a_closed_local_port(self):
        self.assertEqual(odoo_sql("SELECT count(*) FROM ir_mail_server"), ["0"])
        args = json.loads(subprocess.run(["docker", "inspect", "steelmatter-odoo-odoo-1", "--format", "{{json .Config.Cmd}}"],
                                         capture_output=True, text=True, check=True).stdout)
        self.assertIn("--smtp=127.0.0.1", args)
        self.assertIn("--smtp-port=9", args)
        self.assertTrue(supplier()[1].endswith(".invalid"), "supplier addresses must be undeliverable")

    def test_every_address_in_the_sandbox_is_undeliverable(self):
        bad = odoo_sql("SELECT email FROM res_partner WHERE email IS NOT NULL AND email <> '' AND email NOT ILIKE '%.invalid'")
        self.assertEqual(bad, [], "deliverable addresses in the sandbox")

    def test_steelmatter_order_queues_no_email_to_the_supplier(self):
        pid, email = supplier()
        since = last_mail_id()
        op = "mail-%s-create" % RUN
        r = EXECUTOR.api("sm_create", action(op))
        self.assertEqual(r["outcome"], "CREATED")
        self.assertEqual(mails_to(pid, email, since), 0, "creating and confirming the order queued an email to the supplier")
        po = int(odoo_sql("SELECT id FROM purchase_order WHERE sm_op_ref = '%s'" % op)[0])
        notified = odoo_sql(
            "SELECT count(*) FROM mail_notification n JOIN mail_message m ON m.id = n.mail_message_id"
            " WHERE m.model = 'purchase.order' AND m.res_id = %d AND n.notification_type = 'email'" % po)
        self.assertEqual(notified, ["0"], "the order produced an email notification")

    def test_an_email_forced_into_the_queue_cannot_leave(self):
        pid, email = supplier()
        op = "mail-%s-forced" % RUN
        self.assertEqual(EXECUTOR.api("sm_create", action(op))["outcome"], "CREATED")
        po = int(odoo_sql("SELECT id FROM purchase_order WHERE sm_op_ref = '%s'" % op)[0])
        since = last_mail_id()
        # What a person might do by hand: message the supplier from the order.
        ADMIN.call("purchase.order", "message_post", [po], body="sandbox mail test", partner_ids=[pid],
                   message_type="comment", subtype_xmlid="mail.mt_comment")
        queued = odoo_sql("SELECT id FROM mail_mail WHERE id > %d ORDER BY id" % since)
        self.assertTrue(queued, "the forced message did not reach the queue; the test proves nothing")
        ADMIN.call("mail.mail", "process_email_queue", ids=[int(i) for i in queued])
        ids = ",".join(queued)
        states = odoo_sql("SELECT state FROM mail_mail WHERE id IN (%s) ORDER BY id" % ids)
        self.assertEqual(len(states), len(queued), "queued mail disappeared (auto-deleted after sending?)")
        self.assertEqual(set(states), {"exception"}, "a queued email left the sandbox")
        reasons = odoo_sql("SELECT count(*) FROM mail_mail WHERE id IN (%s) AND failure_reason ILIKE '%%connection refused%%'" % ids)
        self.assertEqual(reasons, [str(len(queued))], "the failure was not the closed SMTP port")
        self.assertEqual(odoo_sql("SELECT DISTINCT n.failure_type FROM mail_notification n JOIN mail_mail m ON m.mail_message_id = "
                                  "n.mail_message_id WHERE m.id IN (%s) AND n.notification_type = 'email'" % ids), ["mail_smtp"])


if __name__ == "__main__":
    unittest.main()
