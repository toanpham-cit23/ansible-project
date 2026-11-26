"""Prove the new CSS guards fire on broken input and pass on the real file."""
import importlib.util
import sys

spec = importlib.util.spec_from_file_location("aud", "files/ops/prepush-audit.py")
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)

REAL = open("files/dashboard/docker/api/static/css/app.css", encoding="utf-8").read()

# 1. Real file: must be clean.
real = m.css_layout_problems(REAL)
print("real file           :", "PASS" if not real else "FAIL %s" % real)

# 2. The shipped-broken phone table rule: overflow-x on the table, no box.
broken_table = """
html.device-phone .log-table { overflow-x: auto; -webkit-overflow-scrolling: touch; }
html.device-phone table { min-width: 0; }
html.device-phone .shell { flex-direction: column; }
"""
r = m.css_layout_problems(broken_table)
hit = [x for x in r if "scroll box" in x[1]]
print("clipped table       :", "PASS (caught)" if hit else "FAIL (missed)")

# 3. Scaled topology svg with no min-width.
broken_topo = """
html.device-phone .shell { flex-direction: column; }
.table-scroll { overflow-x: auto; }
html.device-phone .topo-box { overflow: auto; }
"""
r = m.css_layout_problems(broken_topo)
hit = [x for x in r if "topology" in x[1]]
print("scaled topology     :", "PASS (caught)" if hit else "FAIL (missed)")

# 4. Fixed topology must not fire.
fixed_topo = """
html.device-phone .shell { flex-direction: column; }
.table-scroll { overflow-x: auto; }
html.device-phone .topo-box { overflow: auto; }
html.device-phone .topo-box > svg { width: 980px; min-width: 980px; max-width: none; }
"""
r = m.css_layout_problems(fixed_topo)
hit = [x for x in r if "topology" in x[1]]
print("fixed topology      :", "PASS (quiet)" if not hit else "FAIL (false positive)")

sys.exit(0)
