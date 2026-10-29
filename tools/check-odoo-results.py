#!/usr/bin/env python3
"""Fail the real-destination gate if its test was omitted, skipped or failed."""
import json
import sys

passed = False
for line in open(sys.argv[1], encoding="utf-8"):
    row = json.loads(line)
    if row.get("Action") in ("skip", "fail"):
        raise SystemExit("Real Odoo gate contains a skipped or failed result")
    if row.get("Test") == "TestR1OdooEndToEnd" and row.get("Action") == "pass":
        passed = True
if not passed:
    raise SystemExit("TestR1OdooEndToEnd did not pass")
print("Real Odoo recovery gate passed")
