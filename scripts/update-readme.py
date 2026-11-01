#!/usr/bin/env python3
"""Inject docs/REDTEAM.md and bench/RESULTS.md into README.md between markers."""
import pathlib
import re

root = pathlib.Path(__file__).resolve().parent.parent
readme = root / "README.md"
text = readme.read_text()


def inject(text, name, body):
    pat = re.compile(r"(<!-- %s:BEGIN -->\n).*?(<!-- %s:END -->)" % (name, name), re.S)
    return pat.sub(lambda m: m.group(1) + body.strip() + "\n" + m.group(2), text)


redteam = (root / "docs/REDTEAM.md").read_text()
bench = (root / "bench/RESULTS.md").read_text()
# Drop the results file's own title/timestamp; keep machine table + sections.
bench = re.sub(r"^# Benchmarks\n\n.*?\n\n", "", bench, count=1, flags=re.S).replace("## ", "### ")
text = inject(text, "REDTEAM", redteam)
text = inject(text, "BENCH", bench)
readme.write_text(text)
print("README updated")
