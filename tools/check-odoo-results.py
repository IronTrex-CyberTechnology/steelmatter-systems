#!/usr/bin/env python3
"""Fail the real-destination gate if its test was omitted, skipped or failed."""
import json
import sys

required = {"TestR1OdooEndToEnd", "TestR1OdooEndToEnd/withdrawal_after_permit_creates_zero_Odoo_orders"}
passed = set()
for line in open(sys.argv[1], encoding="utf-8"):
    row = json.loads(line)
    if row.get("Action") in ("skip", "fail"):
        raise SystemExit("Real Odoo gate contains a skipped or failed result")
    if row.get("Action") == "pass":
        passed.add(row.get("Test"))
missing = required - passed
if missing:
    raise SystemExit("Missing real Odoo results: " + ", ".join(sorted(missing)))
print("Real Odoo recovery gate passed")
