import json
import os
from pathlib import Path
import sys
import tempfile
import unittest

from polign_recall import Client, RecallError


FAKE = r'''
import json, sys, time
for line in sys.stdin:
    request = json.loads(line)
    if "id" not in request:
        continue
    result = {}
    if request["method"] == "tools/call":
        args = request["params"]["arguments"]
        if args.get("subject") == "timeout":
            time.sleep(30)
        if args.get("subject") == "partial":
            result = {"isError": True, "content": [{"type":"text", "text":json.dumps({"error":"failed", "partial":{"results":[1]}})}]}
        elif request["params"]["name"] == "remember":
            belief = dict(subject=args["subject"], predicate=args["predicate"], value=args["value"], confidence=args.get("confidence",1), source="user_stated", kind="fact", observed_at="2026-09-12T00:00:00Z", event_id="a")
            result = {"content": [{"type":"text", "text":json.dumps({"stored":belief})}]}
        elif request["params"]["name"] == "forget":
            result = {"content": [{"type":"text", "text":json.dumps({"withdrawn": 1 if args.get("value") is False or args.get("value") == 0 else 0})}]}
        else:
            result = {"content": [{"type":"text", "text":"[]"}]}
    print(json.dumps({"jsonrpc":"2.0","id":request["id"],"result":result}),flush=True)
'''


class TransportTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        script = Path(self.directory.name) / "fake.py"
        script.write_text(FAKE)
        self.command = [sys.executable, "-u", str(script)]

    def test_false_zero_and_cleanup(self):
        with Client(command=self.command) as memory:
            result = memory.remember("user", "enabled", False, confidence=0)
            self.assertIs(result.stored.value, False)
            self.assertEqual(result.stored.confidence, 0)
            self.assertEqual(memory.forget("user", "enabled", False), 1)
            self.assertEqual(memory.forget("project", "port", 0), 1)
            with self.assertRaises(ValueError):
                memory.forget("user", "enabled")
            with self.assertRaises(ValueError):
                memory.forget("user", "enabled", None)
            with self.assertRaises(ValueError):
                memory.remember("user", "score", float("nan"))
        self.assertIsNotNone(memory._process.poll())
        with self.assertRaises(RecallError):
            memory.predicates()

    def test_partial_results_are_preserved(self):
        with Client(command=self.command) as memory:
            with self.assertRaises(RecallError) as failure:
                memory.remember("partial", "p", "v")
            self.assertEqual(failure.exception.partial, {"results": [1]})

    def test_timeout_closes_without_retry(self):
        with Client(command=self.command, timeout=0.2) as memory:
            with self.assertRaises(RecallError) as failure:
                memory.recall("timeout")
            self.assertEqual(failure.exception.code, "timeout")
            self.assertIsNotNone(memory._process.poll())


@unittest.skipUnless(os.environ.get("RECALL_TEST_POLIGN"), "set RECALL_TEST_POLIGN and POLIGN_URL for integration")
class IntegrationTests(unittest.TestCase):
    def connect(self):
        return Client(command=[os.environ["RECALL_TEST_POLIGN"], "mcp", "-memory-only", "-write"])

    def test_sessions_extraction_history_and_retraction(self):
        subject = "python-integration"
        with self.connect() as memory:
            self.assertEqual(len(memory.predicates()), 15)
            first = memory.remember(subject, "prefers_editor", "vim")
            text = "I now prefer neovim."
            extraction = memory.remember(text=text, statements=[{
                "subject": subject, "predicate": "prefers_editor", "value": "neovim", "evidence": text
            }])
            self.assertEqual(extraction.results[0].stored.value, "neovim")
            self.assertEqual(memory.recall(subject, "prefers_editor")[0].value, "neovim")
            self.assertEqual(memory.recall(subject, "prefers_editor", as_of=first.stored.observed_at)[0].value, "vim")
            with self.assertRaises(RecallError):
                memory.remember(text="valid text", statements=[{
                    "subject": subject, "predicate": "name", "value": "made up", "evidence": "fabricated"
                }])
        with self.connect() as memory:
            self.assertEqual(memory.recall(subject, "prefers_editor")[0].value, "neovim")
            self.assertEqual(memory.forget(subject, "prefers_editor", "neovim"), 1)
            self.assertEqual(memory.recall(subject, "prefers_editor"), [])
            self.assertGreaterEqual(len(memory.history(subject, "prefers_editor")), 3)


if __name__ == "__main__":
    unittest.main()
