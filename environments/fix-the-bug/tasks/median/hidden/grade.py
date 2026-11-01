"""Hidden grader for fix-the-bug/median.

Runs as root in the grader sandbox. Each hidden case is executed in a fresh
Python subprocess running as an unprivileged uid (2000) that cannot read
/grader, with a timeout and bounded output. Results are compared here, in
the root process, so submitted code can neither see the expected answers
nor influence the comparison. Prints one JSON verdict line.
"""
import json
import math
import subprocess
import sys

UNTRUSTED = 2000
RUNNER = r"""
import json, sys
sys.path.insert(0, sys.argv[1])
case = json.loads(sys.stdin.read())
try:
    import stats
    out = {"value": getattr(stats, case["fn"])(*case["args"])}
except BaseException as e:  # noqa: BLE001 - report any failure as data
    out = {"raises": type(e).__name__}
sys.stdout.write("\n" + json.dumps(out) + "\n")
"""


def run_case(repo, case):
    try:
        p = subprocess.run(
            [sys.executable, "-I", "-c", RUNNER, repo],
            input=json.dumps({"fn": case["fn"], "args": case["args"]}),
            capture_output=True, text=True, timeout=5,
            user=UNTRUSTED, group=UNTRUSTED, extra_groups=[],
            cwd="/tmp", env={"PATH": "/usr/local/bin:/usr/bin:/bin"},
        )
    except subprocess.TimeoutExpired:
        return None, "timeout"
    lines = [l for l in p.stdout[-65536:].splitlines() if l.strip()]
    if not lines:
        return None, "no output (exit %d)" % p.returncode
    try:
        return json.loads(lines[-1]), None
    except ValueError:
        return None, "unparseable output"


def same(got, want):
    if isinstance(want, dict):
        return got == want
    if "value" not in got:
        return False
    v = got["value"]
    if isinstance(v, bool) or not isinstance(v, (int, float)):
        return False
    return math.isclose(v, want, rel_tol=1e-9, abs_tol=1e-12)


def main():
    repo = sys.argv[1]
    with open("/grader/cases.json") as f:
        cases = json.load(f)
    passed, failures = 0, []
    for i, case in enumerate(cases):
        got, err = run_case(repo, case)
        if err is None and same(got, case["want"]):
            passed += 1
        else:
            failures.append({"case": i, "error": err})
    print(json.dumps({
        "reward": passed / len(cases),
        "metadata": {"passed": passed, "total": len(cases), "failed_cases": [f["case"] for f in failures]},
    }))


if __name__ == "__main__":
    main()
