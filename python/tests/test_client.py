import json
import os
from pathlib import Path
import sys
import tempfile
import unittest
import unittest.mock

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


# A server that logs over the protocol, and one that stops reading its input.
CHATTY = r'''
import json, sys
for line in sys.stdin:
    request = json.loads(line)
    if "id" not in request:
        continue
    print(json.dumps({"jsonrpc":"2.0","method":"notifications/message","params":{"level":"info","data":"working"}}),flush=True)
    print(json.dumps({"jsonrpc":"2.0","id":request["id"],"result":{"content":[{"type":"text","text":"[]"}]}}),flush=True)
'''

# Answers the handshake, then never reads its input again.
DEAF = r'''
import json, sys, time
request = json.loads(sys.stdin.readline())
print(json.dumps({"jsonrpc":"2.0","id":request["id"],"result":{"protocolVersion":"2025-06-18"}}),flush=True)
time.sleep(60)
'''


class ResilienceTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)

    def _command(self, source):
        script = Path(self.directory.name) / "server.py"
        script.write_text(source)
        return [sys.executable, "-u", str(script)]

    def test_server_notifications_do_not_end_the_session(self):
        with Client(command=self._command(CHATTY)) as memory:
            self.assertEqual(memory.predicates(), [])
            self.assertEqual(memory.recall("user", "prefers_editor"), [])

    def test_a_server_that_never_reads_times_out_instead_of_hanging(self):
        memory = Client(command=self._command(DEAF), timeout=2)
        self.addCleanup(memory.close)
        with self.assertRaises(RecallError) as caught:
            memory.remember("user", "note", "x" * (1 << 20))
        self.assertEqual(caught.exception.code, "timeout")

    def test_a_missing_binary_is_a_recall_error(self):
        with self.assertRaises(RecallError) as caught:
            Client(command=[str(Path(self.directory.name) / "definitely-absent")])
        self.assertEqual(caught.exception.code, "transport_error")


# Stands in for the `polign` CLI: `recall setup` writes the two files a managed
# local database leaves behind, and `mcp` answers list_predicates with the
# connection it was handed, so a test can see what the client passed on.
FAKE_POLIGN = r'''#!%s
import json, os, sys
args = sys.argv[1:]
if args[:2] == ["recall", "setup"]:
    if os.environ.get("POLIGN_URL") or os.environ.get("POLIGN_API_KEY"):
        sys.exit("setup inherited a connection meant for another server")
    directory = args[args.index("-config-dir") + 1]
    if os.path.basename(directory) == "broken":
        sys.exit("local Recall database did not become ready")
    os.makedirs(directory, exist_ok=True)
    json.dump({"url": "http://127.0.0.1:4242", "pid": 1}, open(os.path.join(directory, "runtime.json"), "w"))
    open(os.path.join(directory, "local-key"), "w").write("plgn_local_secret\n")
    sys.exit(0)
for line in sys.stdin:
    request = json.loads(line)
    if "id" not in request:
        continue
    seen = [{"url": os.environ.get("POLIGN_URL"), "key": os.environ.get("POLIGN_API_KEY"), "argv": args}]
    result = {"content": [{"type": "text", "text": json.dumps(seen)}]} if request["method"] == "tools/call" else {}
    print(json.dumps({"jsonrpc": "2.0", "id": request["id"], "result": result}), flush=True)
''' % sys.executable


@unittest.skipIf(sys.platform == "win32", "managed local databases are Unix only")
class LocalDirTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.polign = Path(self.directory.name) / "polign"
        self.polign.write_text(FAKE_POLIGN)
        self.polign.chmod(0o755)
        patcher = unittest.mock.patch("polign_recall.client.polign_bin", return_value=str(self.polign))
        patcher.start()
        self.addCleanup(patcher.stop)

    def test_local_dir_connects_to_the_managed_server(self):
        # A connection aimed at some other server must not leak into either step.
        with unittest.mock.patch.dict(os.environ, {"POLIGN_URL": "http://elsewhere:23000", "POLIGN_API_KEY": "plgn_other"}):
            with Client(local_dir=Path(self.directory.name) / "data", write=False) as memory:
                (seen,) = memory.predicates()
        self.assertEqual(seen, {"url": "http://127.0.0.1:4242", "key": "plgn_local_secret",
                                "argv": ["mcp", "-memory-only"]})

    def test_a_failed_local_start_is_a_recall_error(self):
        with self.assertRaises(RecallError) as caught:
            Client(local_dir=Path(self.directory.name) / "broken")
        self.assertEqual(caught.exception.code, "transport_error")
        self.assertIn("did not become ready", str(caught.exception))

    def test_local_dir_and_command_are_exclusive(self):
        with self.assertRaises(ValueError):
            Client(local_dir=self.directory.name, command=["polign", "mcp"])
