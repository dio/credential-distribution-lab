"""Check local links and named experiment references; semantic review stays human."""
import json
import re
from pathlib import Path

root = Path(__file__).resolve().parent.parent
sources = "\n".join(p.read_text() for p in root.rglob("*_test.go"))
tests = set(re.findall(r"^func ((?:Test|Example)\w+)\(", sources, re.M))
for name in ("RESEARCH.md", "SPEC.md", "README.md", "CI-DESIGN.md", "RESEARCH-RESULTS.md", "SECURITY.md", "THIRD_PARTY.md"):
    text = (root / name).read_text()
    for target in re.findall(r"\]\(([^)]+)\)", text):
        if "://" not in target and not (root / target.split("#")[0]).exists():
            raise SystemExit(f"broken local link in {name}: {target}")
    for test in re.findall(r"`((?:Test|Example)\w+)`", text):
        if test not in tests:
            raise SystemExit(f"missing experiment in {name}: {test}")
manifest = json.loads((root / "ci/experiments.json").read_text())
for lane in manifest.values():
    for name in lane["required"]:
        top = next((part for part in name.split("/") if part.startswith(("Test", "Example"))), "")
        if top not in tests:
            raise SystemExit(f"manifest references absent experiment: {name}")
print("local documentation links, experiment references and manifest names resolve")

# While this module lives inside notes, keep the executable root workflow and
# the extraction copy identical. A standalone checkout has no such parent file.
if len(root.parents) > 3:
    parent_workflow = root.parents[3] / ".github/workflows/credential-research.yml"
    if parent_workflow.exists() and parent_workflow.read_bytes() != (root / ".github/workflows/verify.yml").read_bytes():
        raise SystemExit("parent and standalone workflow copies differ")
