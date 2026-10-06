import json
import os
from pathlib import Path
import sys
import tempfile
import time
import unittest
import unittest.mock

from polign_recall import Client, PriorValue, RecallError, ResumeContext, WorkingState


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
            if args["subject"] == "future":
                belief["field_from_a_newer_server"] = True
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

    def test_observed_at_is_sent_as_an_instant(self):
        from datetime import datetime, timezone
        with Client(command=self.command) as memory:
            sent = []
            real = memory._tool
            memory._tool = lambda name, args: (sent.append(args), real(name, args))[1]
            memory.remember("user", "prefers_editor", "vim", observed_at=datetime(2023, 3, 1, 9, tzinfo=timezone.utc))
            memory.remember("user", "prefers_editor", "vim", observed_at="2023-03-01T09:00:00Z")
            memory.remember("user", "prefers_editor", "vim")
            self.assertEqual([a.get("observed_at") for a in sent],
                             ["2023-03-01T09:00:00+00:00", "2023-03-01T09:00:00Z", None])
            with self.assertRaises(ValueError):
                memory.remember("user", "prefers_editor", "vim", observed_at=datetime(2023, 3, 1))

    def test_text_alone_leaves_statements_to_the_server(self):
        with Client(command=self.command) as memory:
            sent = []
            memory._tool = lambda name, args: (sent.append(args), {"proposals": [], "results": []})[1]
            memory.remember(text="I use emacs now.")
            memory.remember(text="I use emacs now.", statements=[])
            self.assertEqual(sent, [{"text": "I use emacs now."}, {"text": "I use emacs now.", "statements": []}])
        with self.assertRaises(ValueError):
            Client(command=self.command, extract_model="anthropic")

    def test_unknown_fields_from_a_newer_server_are_ignored(self):
        with Client(command=self.command) as memory:
            result = memory.remember("future", "prefers_editor", "vim")
            self.assertEqual(result.stored.value, "vim")
            self.assertEqual(result.stored.evidence, "")

    def test_replaced_values_decode(self):
        from polign_recall.client import _belief
        b = _belief({"subject": "4812", "predicate": "refund_exception", "value": "revoked",
                     "replaced": [{"value": "approved", "source": "user_stated",
                                   "observed_at": "2026-09-12T00:00:00Z", "event_id": "e1", "later": 1}]})
        self.assertEqual(b.replaced, (PriorValue("approved", "user_stated", "2026-09-12T00:00:00Z", "e1"),))
        self.assertEqual(_belief({"subject": "4812"}).replaced, ())

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
            self.assertIn("note", {p["predicate"] for p in memory.predicates()})
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
            # An imported conversation, written out of order: the date decides.
            dated = subject + "-dated"
            memory.remember(text="I use emacs now.", statements=[{
                "subject": dated, "predicate": "prefers_editor", "value": "emacs", "evidence": "I use emacs now."
            }], observed_at="2023-05-01T09:00:00Z")
            memory.remember(dated, "prefers_editor", "vim", observed_at="2023-03-01T09:00:00Z")
            self.assertEqual(memory.recall(dated, "prefers_editor")[0].value, "emacs")
            self.assertEqual(memory.recall(dated, "prefers_editor", as_of="2023-04-01T00:00:00Z")[0].value, "vim")
            # A fact remembered from text keeps the text it came from.
            sourced = subject + "-sourced"
            said = "I bought it on sale for $24, down from $30."
            extraction = memory.remember(text=said, statements=[{
                "subject": sourced, "predicate": "prefers_editor", "value": "helix", "evidence": "on sale for $24"
            }])
            self.assertIsNotNone(extraction.episode)
            plain = memory.recall(sourced, "prefers_editor")[0]
            self.assertEqual((plain.evidence, plain.source_text), ("", ""))
            full = memory.recall(sourced, "prefers_editor", with_sources=True)[0]
            self.assertEqual(full.evidence, "on sale for $24")
            self.assertEqual(full.source_text, said)
            self.assertEqual(full.evidence_id, extraction.episode.stored.event_id)
            self.assertEqual(full.source, "agent_inferred")
        with self.connect() as memory:
            self.assertEqual(memory.recall(subject, "prefers_editor")[0].value, "neovim")
            self.assertEqual(memory.forget(subject, "prefers_editor", "neovim"), 1)
            self.assertEqual(memory.recall(subject, "prefers_editor"), [])
            self.assertGreaterEqual(len(memory.history(subject, "prefers_editor")), 3)

    def test_unregistered_proposals_are_kept_as_notes(self):
        subject = "python-notes"
        text = "I use fish as my shell."
        proposal = {"subject": subject, "predicate": "prefers_shell",
                    "value": "fish", "evidence": text}
        with self.connect() as memory:
            result = memory.remember(text=text, statements=[proposal])
            self.assertEqual(result.unfiled, (proposal,))
            self.assertEqual(result.results[0].stored.predicate, "note")
            self.assertEqual(result.results[0].stored.value, text)
        with self.connect() as memory:
            self.assertEqual(memory.recall(subject, "note")[0].value, text)
            self.assertEqual(memory.forget(subject, "note", text), 1)
            self.assertEqual(memory.recall(subject, "note"), [])


@unittest.skipUnless(os.environ.get("RECALL_TEST_POLIGN"), "set RECALL_TEST_POLIGN and POLIGN_URL for integration")
class AgentIntegrationTests(unittest.TestCase):
    def connect(self):
        return Client(command=[os.environ["RECALL_TEST_POLIGN"], "mcp", "-memory-only", "-write", "-agent"],
                      agent=True)

    def test_resume_writes_records_and_a_second_client_sees_them(self):
        agent_id = "py-agent-" + os.urandom(4).hex()
        with self.connect() as first:
            agent = first.resume(agent_id, lease_ttl=30)
            self.assertTrue(agent.context.fresh)
            self.assertEqual(agent.context.agent_id, agent_id)
            agent.update_working_state(goal="port billing to v2", plan=["find call sites", "migrate"])
            state = agent.update_working_state(progress="call sites listed")
            self.assertEqual(state.goal, "port billing to v2")
            self.assertEqual(state.plan, ("find call sites", "migrate"))
            self.assertEqual(state.version, 2)
            self.assertEqual(agent.milestone("call sites found").last_milestone, "call sites found")
            self.assertEqual(agent.record_turn("user", "please continue", message_id="m-1").message_id, "m-1")
            self.assertEqual(agent.record_turn("assistant", "write(long)", brief="write(...)").brief, "write(...)")
            turn = agent.record_turn("tool", "billing.charge(\n" * 2000, name="grep")
            self.assertTrue(turn.output_ref)
            self.assertEqual(agent.fetch_output(turn.output_ref).content, "billing.charge(\n" * 2000)
            self.assertEqual([t.seq for t in agent.recent_turns()], [1, 2, 3])
            pointer = agent.set_pointer("wip", "git_ref", {"repo": "github.com/acme/billing", "branch": "agent/wip"})
            self.assertEqual(pointer.fields["branch"], "agent/wip")
            agent.set_pointer("scratch", "object", {"uri": "s3://b/scratch.tar"})
            agent.remove_pointer("scratch")
            self.assertEqual([p.name for p in agent.pointers()], ["wip"])
            self.assertGreaterEqual(len(agent.working_state_history()), 2)

            # Another process cannot take the agent while this one holds it.
            with self.connect() as second:
                with self.assertRaises(RecallError) as caught:
                    second.resume(agent_id)
                self.assertEqual(caught.exception.code, "lease_held")
            self.assertTrue(agent.release())
            self.assertFalse(agent.release())

        with self.connect() as later:
            with later.resume(agent_id) as agent:
                context = agent.context
                self.assertIsInstance(context, ResumeContext)
                self.assertFalse(context.fresh)
                self.assertIsInstance(context.working_state, WorkingState)
                self.assertEqual(context.working_state.progress, "call sites listed")
                self.assertEqual(context.turn_seq, 3)
                self.assertEqual([p.name for p in context.pointers], ["wip"])
                self.assertIn("port billing to v2", context.briefing)
                self.assertIn("please continue", context.briefing)
                self.assertEqual(agent.record_turn("assistant", "resumed").seq, 4)

    def test_deferred_resume_reads_at_once_and_writes_after_the_lease(self):
        agent_id = "py-agent-" + os.urandom(4).hex()
        crashed = self.connect()
        crashed.resume(agent_id, lease_ttl=5).update_working_state(goal="rebook the flight")
        # Kill the server process outright: no release, the lease stays live.
        crashed._process.kill()
        crashed.close()
        with self.connect() as client:
            started = time.monotonic()
            agent = client.resume(agent_id, defer_lease=True)
            self.assertLess(time.monotonic() - started, 3)
            self.assertFalse(agent.lease_held)
            self.assertIn("rebook the flight", agent.context.briefing)
            with self.assertRaises(RecallError) as caught:
                agent.record_turn("assistant", "sorry, we got cut off")
            self.assertEqual(caught.exception.code, "lease_not_held")
            self.assertFalse(agent.acquire())
            deadline = time.monotonic() + 15
            while not agent.acquire():
                self.assertLess(time.monotonic(), deadline, "lease never became free")
                time.sleep(0.5)
            self.assertTrue(agent.lease_held)
            self.assertEqual(agent.record_turn("assistant", "sorry, we got cut off").seq, 1)
            agent.release()

    def test_closing_the_client_hands_the_lease_over(self):
        agent_id = "py-agent-" + os.urandom(4).hex()
        with self.connect() as first:
            first.resume(agent_id, lease_ttl=60).record_turn("user", "hello")
        # No explicit release: closing the client ended the session, and the
        # server released the lease instead of leaving it to expire.
        with self.connect() as second:
            with second.resume(agent_id) as agent:
                self.assertEqual(agent.context.turn_seq, 1)


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

    def test_agent_mode_adds_the_agent_flag(self):
        with Client(local_dir=Path(self.directory.name) / "data", agent=True) as memory:
            (seen,) = memory.predicates()
        self.assertEqual(seen["argv"], ["mcp", "-memory-only", "-write", "-agent"])

    def test_a_failed_local_start_is_a_recall_error(self):
        with self.assertRaises(RecallError) as caught:
            Client(local_dir=Path(self.directory.name) / "broken")
        self.assertEqual(caught.exception.code, "transport_error")
        self.assertIn("did not become ready", str(caught.exception))

    def test_local_dir_and_command_are_exclusive(self):
        with self.assertRaises(ValueError):
            Client(local_dir=self.directory.name, command=["polign", "mcp"])


# Stands in for `polign mcp -agent`: agent_resume on the id "busy" fails the
# way the real server does when another process holds the lease.
AGENT_FAKE = r'''
import json, sys
for line in sys.stdin:
    request = json.loads(line)
    if "id" not in request:
        continue
    result = {}
    if request["method"] == "tools/call":
        name, args = request["params"]["name"], request["params"]["arguments"]
        if name == "agent_resume" and args["agent_id"] == "busy":
            text = 'recall: agent lease is held by another process: held by "pod-a" until 2026-09-26T00:00:00Z'
            result = {"isError": True, "content": [{"type": "text", "text": text}]}
        elif name == "agent_resume":
            payload = {"agent_id": args["agent_id"], "fresh": False, "epoch": 3, "future_field": 1,
                       "working_state": {"goal": "g", "plan": ["a"], "version": 4, "extra": True},
                       "memories": [{"subject": "s", "predicate": "p", "value": "v", "confidence": 1,
                                     "source": "user_stated", "kind": "fact", "observed_at": "t", "event_id": "e"}],
                       "recent_turns": [{"seq": 7, "role": "user", "content": "hi", "at": "t"}],
                       "omitted": {"turns": 2}, "turn_seq": 7, "token_budget": 8000, "tokens": 50,
                       "briefing": "B", "args": args, "argv": sys.argv[1:]}
            result = {"content": [{"type": "text", "text": json.dumps(payload)}]}
        elif name == "agent_release":
            result = {"content": [{"type": "text", "text": json.dumps({"released": True})}]}
        else:
            result = {"content": [{"type": "text", "text": json.dumps(args)}]}
    print(json.dumps({"jsonrpc": "2.0", "id": request["id"], "result": result}), flush=True)
'''


class AgentTransportTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        script = Path(self.directory.name) / "fake.py"
        script.write_text(AGENT_FAKE)
        self.command = [sys.executable, "-u", str(script)]

    def test_resume_decodes_and_ignores_unknown_fields(self):
        with Client(command=self.command, agent=True) as client:
            self.assertTrue(client.agent_enabled)
            with client.resume("coder-1", token_budget=4000, lease_ttl=2.5, holder="me") as agent:
                context = agent.context
                self.assertEqual(context.epoch, 3)
                self.assertEqual(context.working_state.plan, ("a",))
                self.assertEqual(context.working_state.version, 4)
                self.assertEqual(context.memories[0].value, "v")
                self.assertEqual(context.recent_turns[0].seq, 7)
                self.assertEqual(context.omitted, {"turns": 2, "memories": 0, "outputs": 0})
                # The fake echoes nothing back for these, but the call shapes are checked.
                sent = agent._call("update_working_state", goal="x", plan=None)
                self.assertEqual(sent, {"agent_id": "coder-1", "goal": "x"})

    def test_a_held_lease_has_its_own_code(self):
        with Client(command=self.command, agent=True) as client:
            with self.assertRaises(RecallError) as caught:
                client.resume("busy")
            self.assertEqual(caught.exception.code, "lease_held")

    def test_agent_mode_needs_write_and_resume_needs_agent_mode(self):
        with self.assertRaises(ValueError):
            Client(command=self.command, agent=True, write=False)
        with Client(command=self.command) as client:
            with self.assertRaises(ValueError):
                client.resume("coder-1")

    def test_release_after_close_is_false(self):
        client = Client(command=self.command, agent=True)
        agent = client.resume("coder-1")
        client.close()
        self.assertFalse(agent.release())
