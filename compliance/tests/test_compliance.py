import importlib.util
import sqlite3
import tempfile
import unittest
from pathlib import Path

SCRIPT = Path(__file__).resolve().parents[1] / "scripts" / "run_compliance.py"
spec = importlib.util.spec_from_file_location("run_compliance", SCRIPT)
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class ComplianceTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.old_actual_dir = module.ACTUAL_DIR
        module.ACTUAL_DIR = Path(self.tmp.name) / "actual"
        module.ACTUAL_DIR.mkdir()

    def tearDown(self):
        module.ACTUAL_DIR = self.old_actual_dir
        self.tmp.cleanup()

    def test_compliant_router(self):
        (module.ACTUAL_DIR / "r1.conf").write_text("hostname r1\nrouter ospf\n network 1.1.1.1/32 area 0\n")
        desired = {"routers": {"r1": {"required_patterns": [r"^hostname r1$", r"^router ospf$"]}}}
        result = module.audit_devices(desired)
        self.assertEqual(result[0]["status"], "COMPLIANT")

    def test_missing_rule_is_noncompliant(self):
        (module.ACTUAL_DIR / "r1.conf").write_text("hostname r1\n")
        desired = {"routers": {"r1": {"required_patterns": [r"^router ospf$"]}}}
        result = module.audit_devices(desired)
        self.assertEqual(result[0]["status"], "NON_COMPLIANT")
        self.assertEqual(len(result[0]["findings"]), 1)

    def test_missing_snapshot_is_error(self):
        desired = {"routers": {"r1": {"required_patterns": []}}}
        result = module.audit_devices(desired)
        self.assertEqual(result[0]["status"], "ERROR")

    def test_persists_history_and_findings(self):
        db = Path(self.tmp.name) / "test.db"
        results = [{"hostname": "r1", "status": "NON_COMPLIANT", "findings": [{"rule": "router ospf", "detail": "missing"}]}]
        run_id = module.save_results(db, results, "2026-11-16T00:00:00+00:00")
        # Close explicitly: `with sqlite3.connect(...)` only commits, so the
        # handle survives into tearDown and blocks TemporaryDirectory.cleanup()
        # on Windows with WinError 32.
        con = sqlite3.connect(db)
        try:
            run = con.execute("SELECT status, devices_noncompliant FROM compliance_runs WHERE id=?", (run_id,)).fetchone()
            finding = con.execute("SELECT hostname, rule FROM compliance_findings WHERE run_id=?", (run_id,)).fetchone()
        finally:
            con.close()
        self.assertEqual(run, ("NON_COMPLIANT", 1))
        self.assertEqual(finding, ("r1", "router ospf"))


if __name__ == "__main__":
    unittest.main()
