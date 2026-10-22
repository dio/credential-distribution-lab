"""Require behavioral failures from deliberately weakened model guards.

Only temporary copies are mutated. Compiler errors/timeouts never count as a
successful negative control. No runtime service code is changed.
"""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile

root = Path(__file__).resolve().parent.parent
mutations = [
    ("dependency", "r.Version == c.Binding.Version &&", "", "TestActivationCounterexample"),
    ("lease", "w.Now < s.Deadline &&", "", "TestRevocationAndDispatchBoundary"),
    ("session", "response.Session != s.Session ||", "", "TestRestartAndPublisherRollbackLimit"),
]
for name, before, after, test in mutations:
    with tempfile.TemporaryDirectory(prefix="credential-model-control-") as folder:
        target = Path(folder)
        for source in (root / "model").glob("*.go"):
            shutil.copyfile(source, target / source.name)
        (target / "go.mod").write_text("module model-control\n\ngo 1.25.0\n")
        source = target / "model.go"
        text = source.read_text()
        if text.count(before) != 1:
            raise SystemExit(f"mutation target changed: {name}; review the control")
        source.write_text(text.replace(before, after, 1))
        env = dict(os.environ, GOTOOLCHAIN="local")
        result = subprocess.run(["go", "test", "-count=1", "-timeout=20s", "-run", "^"+test+"$", "."],
                                cwd=target, env=env, capture_output=True, text=True, timeout=30)
        if result.returncode == 0 or f"--- FAIL: {test}" not in result.stdout:
            raise SystemExit(f"mutation {name} did not produce its required behavioral failure\n{result.stdout}\n{result.stderr}")
        print(f"EXPECTED FAILURE observed: removed {name} guard -> {test}")
