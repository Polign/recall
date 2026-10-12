import json
import os
import tempfile
import threading
import unittest
import unittest.mock
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from polign_recall import Client, RecallError
from polign_recall.client import _seconds


class StubServer(ThreadingHTTPServer):
    """Stands in for `recall serve`: /mcp behind a key, token exchange, renewal."""

    def __init__(self, *, agent=True, text=False):
        super().__init__(("127.0.0.1", 0), StubHandler)
        self.lock = threading.Lock()
        self.valid = {"rcl_static_s"}
        self.agent = agent
        self.text = text
        self.exchanges = 0
        self.renews = 0
        self.memory = {}

    @property
    def url(self):
        return f"http://127.0.0.1:{self.server_address[1]}"


class StubHandler(BaseHTTPRequestHandler):
    server: StubServer

    def log_message(self, *args):
        pass

    def _send(self, status, payload=None):
        body = b"" if payload is None else json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _authorized(self):
        auth = self.headers.get("Authorization", "")
        with self.server.lock:
            return auth.startswith("Bearer ") and auth[7:] in self.server.valid

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        body = json.loads(self.rfile.read(length) or b"{}")
        if self.path == "/v1/auth/kubernetes":
            if body.get("token") != "pod-token":
                return self._send(401, {"error": {"code": "unauthenticated", "message": "token rejected"}})
            with self.server.lock:
                self.server.exchanges += 1
                key = f"rcl_k{self.server.exchanges}_s"
                self.server.valid.add(key)
            return self._send(201, {"key": key, "ttl": "15m0s", "expires_at": "2026-10-12T10:15:00Z", "namespace": "agents:sandbox-1"})
        if not self._authorized():
            return self._send(401, {"error": {"code": "unauthenticated", "message": "the key is not known or no longer valid"}})
        if self.path == "/v1/keys/renew":
            with self.server.lock:
                self.server.renews += 1
            return self._send(200, {"expires_at": "2026-10-12T10:30:00Z"})
        if self.path != "/mcp":
            return self._send(404, {"error": {"code": "not_found", "message": "no"}})
        if "id" not in body:
            return self._send(202)
        method = body["method"]
        if method == "initialize":
            result = {"protocolVersion": "2025-06-18", "capabilities": {"tools": {}}, "serverInfo": {"name": "stub"}}
        elif method == "tools/list":
            names = ["remember", "recall", "forget"] if self.server.text else ["list_predicates", "remember", "recall", "forget", "explain", "memory_history"]
            if self.server.agent:
                names += ["agent_resume", "agent_release"]
            result = {"tools": [{"name": n, "description": "", "inputSchema": {"type": "object"}} for n in names]}
        elif method == "tools/call":
            name, args = body["params"]["name"], body["params"]["arguments"]
            if name == "remember":
                belief = dict(subject=args["subject"], predicate=args["predicate"], value=args["value"], confidence=1,
                              source="user_stated", kind="fact", observed_at="2026-10-12T09:00:00Z", event_id="e1")
                with self.server.lock:
                    self.server.memory[(args["subject"], args["predicate"])] = belief
                text = json.dumps({"stored": belief})
            elif name == "recall":
                with self.server.lock:
                    found = [b for (s, p), b in self.server.memory.items() if s == args.get("subject") and p == args.get("predicate")]
                text = json.dumps(found)
            else:
                text = "[]"
            result = {"content": [{"type": "text", "text": text}], "isError": False}
        else:
            return self._send(200, {"jsonrpc": "2.0", "id": body["id"], "error": {"code": -32601, "message": "unsupported"}})
        self._send(200, {"jsonrpc": "2.0", "id": body["id"], "result": result})


class HTTPTransportTests(unittest.TestCase):
    def start(self, **kw):
        server = StubServer(**kw)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        return server

    def test_remember_and_recall_over_http(self):
        server = self.start()
        with Client(url=server.url, api_key="rcl_static_s") as client:
            self.assertTrue(client.agent_enabled)
            stored = client.remember("user", "prefers_editor", "vim")
            self.assertEqual(stored.stored.value, "vim")
            beliefs = client.recall("user", "prefers_editor")
            self.assertEqual([b.value for b in beliefs], ["vim"])
            self.assertEqual(client.predicates(), [])

    def test_an_unknown_key_is_unauthenticated(self):
        server = self.start()
        with self.assertRaises(RecallError) as ctx:
            Client(url=server.url, api_key="rcl_wrong")
        self.assertEqual(ctx.exception.code, "unauthenticated")

    def test_token_exchange_and_a_fresh_exchange_after_revocation(self):
        server = self.start()
        with tempfile.NamedTemporaryFile("w", suffix=".token", delete=False) as f:
            f.write("pod-token\n")
        self.addCleanup(os.unlink, f.name)
        with Client(url=server.url, token_file=f.name) as client:
            client.remember("user", "prefers_editor", "vim")
            self.assertEqual(server.exchanges, 1)
            # The server revokes the key; the next call exchanges again.
            with server.lock:
                server.valid.discard("rcl_k1_s")
            self.assertEqual([b.value for b in client.recall("user", "prefers_editor")], ["vim"])
            self.assertEqual(server.exchanges, 2)
            # Past half the lifetime the client renews instead.
            session = client._http
            session._expires_at = 0.0
            session.bearer()
            self.assertEqual(server.renews, 1)
            self.assertEqual(server.exchanges, 2)

    def test_a_rejected_token_is_unauthenticated(self):
        server = self.start()
        with tempfile.NamedTemporaryFile("w", suffix=".token", delete=False) as f:
            f.write("someone-elses-token\n")
        self.addCleanup(os.unlink, f.name)
        with self.assertRaises(RecallError) as ctx:
            Client(url=server.url, token_file=f.name)
        self.assertEqual(ctx.exception.code, "unauthenticated")

    def test_from_env_connects_to_recall_url(self):
        server = self.start()
        with unittest.mock.patch.dict(os.environ, {"RECALL_URL": server.url, "RECALL_API_KEY": "rcl_static_s"}):
            with Client.from_env() as client:
                self.assertIsNotNone(client._http)
                self.assertEqual(client.predicates(), [])

    def test_url_rejects_process_options(self):
        server = self.start()
        with self.assertRaises(ValueError):
            Client(url=server.url, api_key="rcl_static_s", local_dir="/tmp/x")
        with self.assertRaises(ValueError):
            Client(url=server.url, api_key="rcl_static_s", command=["x"])
        with self.assertRaises(ValueError):
            Client(url=server.url)
        with self.assertRaises(ValueError):
            Client(api_key="rcl_static_s")

    def test_the_server_decides_text_and_agent(self):
        server = self.start(agent=False, text=True)
        with Client(url=server.url, api_key="rcl_static_s") as client:
            self.assertFalse(client.agent_enabled)
            with self.assertRaises(ValueError):
                client.recall("user", "prefers_editor")  # a text client
        with self.assertRaises(ValueError):
            Client(url=server.url, api_key="rcl_static_s", agent=True)

    def test_go_durations_parse(self):
        self.assertEqual(_seconds("15m0s"), 900)
        self.assertEqual(_seconds("1h30m"), 5400)
        self.assertEqual(_seconds("250ms"), 0.25)
        self.assertIsNone(_seconds("soon"))
        self.assertIsNone(_seconds(None))


if __name__ == "__main__":
    unittest.main()
