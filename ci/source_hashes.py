"""Identify tested source even before extraction/commit; never hash runtime secrets."""
import hashlib
from pathlib import Path

root = Path(__file__).resolve().parent.parent
for path in sorted(root.rglob('*')):
    relative = path.relative_to(root)
    if set(relative.parts) & {'evidence', '.tools', '__pycache__', '.git'}:
        continue
    if path.is_file() and (path.suffix in {'.go', '.py', '.sh', '.json', '.yml', '.env', '.md', '.mod', '.sum'} or relative.as_posix() == 'fixture/mcp-main.go.txt'):
        print(hashlib.sha256(path.read_bytes()).hexdigest(), relative.as_posix())
