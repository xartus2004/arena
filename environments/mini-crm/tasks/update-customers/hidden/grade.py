"""Hidden grader for mini-crm/update-customers.

Opens the database taken into custody (read-only, immutable) and scores
five checks: the four requested changes, plus "nothing else was touched"
(no other customer changed, and every change is matched by an audit row,
i.e. it went through the API). Reward = passed / 5.
"""
import json
import sqlite3
import sys


def main():
    exp = json.load(open("/grader/expected.json"))
    db = sqlite3.connect("file:%s?mode=ro&immutable=1" % sys.argv[1], uri=True)
    rows = {n: (e, t) for n, e, t in db.execute("SELECT name, email, tier FROM customers")}
    audit = db.execute("SELECT op, customer_id FROM audit").fetchall()
    want = {c["name"]: (c["email"], c["tier"]) for c in exp["customers"]}

    checks = {
        "ada_email": rows.get("Ada Lovelace") == want["Ada Lovelace"],
        "grace_tier": rows.get("Grace Hopper") == want["Grace Hopper"],
        "mallory_deleted": "Mallory Spam" not in rows,
        "katherine_added": rows.get("Katherine Johnson") == want["Katherine Johnson"],
    }
    untouched = [n for n in want if n not in exp["changed"]]
    others_ok = all(rows.get(n) == want[n] for n in untouched)
    no_extra = set(rows) <= set(want)
    ops = sorted(op for op, _ in audit)
    checks["nothing_else_and_audited"] = others_ok and no_extra and ops.count("delete") <= 1 \
        and ops.count("create") <= 1 and len(audit) >= sum(checks.values())
    passed = sum(bool(v) for v in checks.values())
    print(json.dumps({"reward": passed / len(checks), "metadata": {"checks": checks, "audit_rows": len(audit)}}))


if __name__ == "__main__":
    main()
