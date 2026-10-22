import unittest
from check_results import verify


def event(action, test=None):
    result = {"Package": "p", "Action": action}
    if test:
        result["Test"] = test
    return result


class ResultsTest(unittest.TestCase):
    def test_complete(self):
        self.assertEqual(verify([event("run", "TestA"), event("pass", "TestA"), event("pass")], ["p/TestA"]), 1)

    def test_bad_evidence(self):
        cases = {
            "empty": [],
            "missing": [event("pass")],
            "skip": [event("run", "TestA"), event("skip", "TestA"), event("pass")],
            "fail": [event("run", "TestA"), event("fail", "TestA"), event("fail")],
            "truncated": [event("run", "TestA"), event("pass", "TestA")],
            "unfinished": [event("run", "TestA"), event("pass")],
            "missing-start": [event("pass", "TestA"), event("pass")],
            "extra-skip": [event("run", "TestA"), event("pass", "TestA"), event("skip", "TestB"), event("pass")],
        }
        for name, events in cases.items():
            with self.subTest(name=name), self.assertRaises(ValueError):
                verify(events, ["p/TestA"])


if __name__ == "__main__":
    unittest.main()
