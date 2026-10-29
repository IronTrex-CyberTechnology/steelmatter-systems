# Synthetic sandbox fixture, run inside `odoo shell` by sandbox/up.
# No customer data. Identities get generated passwords from the environment.
import os

company = env.ref("base.main_company")
inr = env.ref("base.INR")
inr.active = True
company.write({
    "name": "Acme Synthetic India",
    "currency_id": inr.id,
    "sm_ref": os.environ["STEELMATTER_DESTINATION"],
    # Sandbox only: Odoo's two-step purchase approval off; Steelmatter's
    # approval rules remain (Aakhil, 28 Oct 2026).
    "po_double_validation": "one_step",
    "po_lock": "edit",
})

# No outgoing mail servers: notifications cannot leave the sandbox.
env["ir.mail_server"].search([]).unlink()

Partner = env["res.partner"]
suppliers = [
    ("sup-chairs", "Synthetic Chairs Pvt Ltd", "27AAAAA0000A1Z5"),
    ("sup-desks", "Synthetic Desks LLP", "29BBBBB1111B1Z6"),
]
for ref, name, vat in suppliers:
    if not Partner.search([("sm_ref", "=", ref)]):
        Partner.create({"name": name, "is_company": True, "sm_ref": ref, "vat": vat,
                        "supplier_rank": 1, "email": "%s@example.invalid" % ref, "country_id": env.ref("base.in").id})

if not Partner.search([("sm_ref", "=", "wh-chennai")]):
    Partner.create({"name": "Acme Synthetic Chennai Warehouse", "type": "delivery", "sm_ref": "wh-chennai",
                    "parent_id": company.partner_id.id, "city": "Chennai", "country_id": env.ref("base.in").id})

Product = env["product.product"]
for ref, name, price in [("chair-ergo", "Ergonomic office chair", 6000.0), ("desk-std", "Standard desk", 12000.0)]:
    if not Product.search([("sm_ref", "=", ref)]):
        Product.create({"name": name, "sm_ref": ref, "purchase_ok": True, "type": "consu",
                        "standard_price": price, "supplier_taxes_id": [(6, 0, [])], "taxes_id": [(6, 0, [])]})

# Every address in the sandbox is on the reserved .invalid domain, so nothing
# could be delivered even if mail were sent. Done before any password is set,
# because Odoo queues a "password changed" email to the user's address.
for p in Partner.with_context(active_test=False).search([("email", "!=", False)]):
    if not p.email.endswith(".invalid"):
        p.email = p.email.split("@")[0] + "@example.invalid"

Users = env["res.users"].with_context(no_reset_password=True)
for login, name, group, pw in [
    ("sm_executor", "Steelmatter executor", "steelmatter_r1.group_executor", os.environ["SM_EXECUTOR_PASSWORD"]),
    ("sm_observer", "Steelmatter observer", "steelmatter_r1.group_observer", os.environ["SM_OBSERVER_PASSWORD"]),
]:
    u = Users.search([("login", "=", login)])
    if not u:
        u = Users.create({"login": login, "name": name, "email": "%s@example.invalid" % login,
                          "company_id": company.id, "company_ids": [(6, 0, [company.id])],
                          "groups_id": [(6, 0, [env.ref(group).id])]})
    u.password = pw

admin = env.ref("base.user_admin")
admin.password = os.environ["SM_ADMIN_PASSWORD"]

env.cr.commit()
print("seeded: company %s (%s), %d suppliers, delivery wh-chennai, products chair-ergo and desk-std, users sm_executor and sm_observer"
      % (company.name, company.sm_ref, len(suppliers)))
