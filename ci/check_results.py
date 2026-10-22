"""Fail closed on missing, skipped, failed or incomplete Go test evidence."""
import json
import sys
from pathlib import Path


def verify(events, expected):
    running = set()
    passed = set()
    packages = set()
    complete = set()
    for event in events:
        package, action, test = event.get("Package"), event.get("Action"), event.get("Test")
        if not package:
            continue
        packages.add(package)
        if action in ("fail", "skip"):
            raise ValueError(f"{action}: {package}/{test or '(package)'}")
        if test:
            key = package + "/" + test
            if action == "run":
                running.add(key)
            elif action == "pass":
                if key not in running:
                    raise ValueError(f"completion without start: {key}")
                passed.add(key)
        elif action == "pass":
            complete.add(package)
    if not packages or packages != complete:
        raise ValueError("missing package completion")
    if running != passed:
        raise ValueError("unfinished test")
    missing = set(expected) - passed
    if missing:
        raise ValueError("missing required tests: " + ", ".join(sorted(missing)))
    return len(passed)


def main():
    lane, filename = sys.argv[1:]
    manifest = json.loads(Path(__file__).with_name("experiments.json").read_text())[lane]
    try:
        events = [json.loads(line) for line in Path(filename).read_text().splitlines()]
        count = verify(events, manifest["required"])
    except (ValueError, OSError) as error:
        print(f"FAIL {lane}: {error}", file=sys.stderr)
        return 1
    print(f"PASS {lane}: {count} completed tests/examples; all required experiments present")
    for name in manifest.get("limitations", []):
        print(f"EXPECTED LIMITATION / CONTROL: {name}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
