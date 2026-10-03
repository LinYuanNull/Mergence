import os
import re
import sys

sys.stdout.reconfigure(encoding="utf-8")
d = os.path.join(os.path.expanduser("~"), "Desktop")
pat = re.compile(r"[A-Za-z]:\\[^\x00-\x1f\"<>|*?]{3,150}")
for f in sorted(os.listdir(d)):
    if not f.endswith(".lnk"):
        continue
    raw = open(os.path.join(d, f), "rb").read()
    exes = set()
    for enc in ("utf-16-le", "latin-1"):
        for m in pat.findall(raw.decode(enc, "ignore")):
            if m.lower().endswith(".exe"):
                exes.add(m)
    print("  %-40s -> %s" % (f, sorted(exes)[0] if exes else "(未解析出 exe)"))
