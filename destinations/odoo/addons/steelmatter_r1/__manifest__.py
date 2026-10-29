{
    "name": "Steelmatter R1 destination",
    "summary": "Steelmatter operation reference, fence and restore-proof records for purchase orders",
    "description": "Steelmatter R1 destination module (D1 v1.2, D3, D5 v3). One locked operation row per "
                   "op_ref serialises create and fence. Restore-proof CREATE_INTENT, BLOCK and CREATE_DONE records "
                   "go to the separate PostgreSQL recovery anchor; its URL comes from the server environment only.",
    "version": "18.0.0.2.0",
    # Odoo accepts only its own licence keys. The module's licence is open
    # (D16, with counsel); the repository is Apache-2.0.
    "license": "Other OSI approved licence",
    "author": "IronTrex",
    "category": "Purchase",
    "depends": ["purchase"],
    "data": [
        "security/groups.xml",
        "security/ir.model.access.csv",
    ],
    "post_init_hook": "post_init_hook",
    "installable": True,
    "application": False,
}
