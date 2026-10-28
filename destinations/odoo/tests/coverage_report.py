"""Prints the sandbox's coverage report (sm_coverage, read with the observer
identity) as Markdown, or as JSON with --json.

    python3 -I destinations/odoo/tests/coverage_report.py [--json] [--out FILE]
"""

import argparse
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from sandbox_client import observer  # noqa: E402

ORDER = ["unprotected", "unknown", "observed", "enforced"]
MEANING = {
    "enforced": "orders go through Steelmatter",
    "observed": "not prevented, but recorded and reported",
    "unprotected": "a person, key or process can bypass Steelmatter",
    "unknown": "cannot be verified from inside Odoo",
}


def subjects_text(s):
    if not s:
        return ""
    out = []
    for x in s:
        if isinstance(x, dict):
            label = x.get("login") or x.get("name") or json.dumps(x)
            extra = []
            if x.get("create") or x.get("confirm_or_edit"):
                extra.append(", ".join(k for k in ("create", "confirm_or_edit") if x.get(k)).replace("confirm_or_edit", "confirm/edit"))
            if x.get("api_keys"):
                extra.append("%d API key(s)" % x["api_keys"])
            if x.get("groups"):
                extra.append("groups: " + ", ".join(x["groups"]))
            out.append(label + (" (" + "; ".join(extra) + ")" if extra else ""))
        else:
            out.append(str(x))
    return "; ".join(out)


def markdown(cov):
    m, a = cov["module"], cov["anchor"]
    lines = [
        "# Steelmatter coverage report: Odoo destination",
        "",
        "Generated %s from database `%s` (journal seq %s). Read-only, with the observer identity. "
        "**Coverage is never complete:** direct database access and server administrators cannot be seen from inside Odoo."
        % (cov["generated_at"], cov["database"], cov["seq"]),
        "",
        "| Module | Odoo | Anchor |",
        "|---|---|---|",
        "| %s %s (expected %s)%s | %s%s | %s |" % (
            m["name"], m["version"] or "not installed", m["expected_version"], "" if m["installed"] else ", **not installed**",
            m["odoo"], "" if m["supported"] else " (**unsupported**)",
            "reachable" if a["reachable"] else ("**unreachable**" if a["configured"] else "**not configured**")),
        "",
        "**Summary:** " + ", ".join("%d %s" % (cov["summary"].get(p, 0), p) for p in ORDER),
        "",
        "| Posture | Route | Count | Detail | Who or what |",
        "|---|---|---|---|---|",
    ]
    for r in sorted(cov["routes"], key=lambda r: ORDER.index(r["posture"])):
        count = "" if r.get("count") is None else str(r["count"])
        lines.append("| **%s** | %s | %s | %s | %s |" % (r["posture"], r["route"], count, r["detail"].replace("|", "/"),
                                                     subjects_text(r.get("subjects")).replace("|", "/")))
    lines += ["", "Postures: " + "; ".join("**%s**: %s" % (p, MEANING[p]) for p in ORDER), "", "## Steelmatter identities", ""]
    for i in cov["identities"].get("executor", []):
        lines.append("- executor `%s`: direct purchase rights %s" % (
            i["login"], "**yes (bypass)**" if i["direct_create"] or i["direct_write"] else "none (writes only through the module)"))
    for i in cov["identities"].get("observer", []):
        lines.append("- observer `%s`: %s" % (i["login"], "read-only" if i["read_only"] else "**can write: %s**" % ", ".join(i["writes"])))
    return "\n".join(lines) + "\n"


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--json", action="store_true")
    ap.add_argument("--out")
    args = ap.parse_args()
    cov = observer().api("sm_coverage")
    text = json.dumps(cov, indent=2) + "\n" if args.json else markdown(cov)
    if args.out:
        with open(args.out, "w") as f:
            f.write(text)
        print("wrote", args.out)
    else:
        sys.stdout.write(text)


if __name__ == "__main__":
    main()
