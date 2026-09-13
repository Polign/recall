from __future__ import annotations

import json
import math
import os
import queue
import subprocess
import threading
from dataclasses import dataclass
from datetime import datetime
from typing import Any, Mapping, Sequence

Value = str | float | bool
_MISSING = object()


class RecallError(Exception):
    """A tool or transport failure. Writes are never retried automatically."""

    def __init__(self, message: str, *, code: str = "tool_error", partial: Any = None):
        super().__init__(message)
        self.code = code
        self.partial = partial


@dataclass(frozen=True)
class Belief:
    subject: str
    predicate: str
    value: Value
    confidence: float
    source: str
    kind: str
    observed_at: str
    event_id: str


@dataclass(frozen=True)
class Event:
    id: str
    kind: str
    subject: str
    predicate: str
    value: Value | None
    confidence: float
    source: str
    observed_at: str
    retraction: bool = False


@dataclass(frozen=True)
class RememberResult:
    stored: Belief
    already_known: bool
    superseded: tuple[Belief, ...]

    @classmethod
    def decode(cls, data: dict[str, Any]) -> RememberResult:
        return cls(Belief(**data["stored"]), data.get("already_known", False),
                   tuple(Belief(**b) for b in data.get("superseded", [])))


@dataclass(frozen=True)
class ExtractionResult:
    proposals: tuple[dict[str, Any], ...]
    results: tuple[RememberResult, ...]


class Client:
    """Owns one MCP subprocess. Thread-safe, with serialized calls and timeouts.

    Environment overrides extend the inherited environment. Credentials belong
    in POLIGN_API_KEY, not command-line arguments. The default enables memory
    writes; pass write=False to expose only read operations.
    """

    def __init__(self, *, command: Sequence[str] | None = None,
                 env: Mapping[str, str] | None = None, timeout: float = 120,
                 write: bool = True):
        if not math.isfinite(timeout) or timeout <= 0:
            raise ValueError("timeout must be finite and positive")
        self.timeout = timeout
        self._lock = threading.RLock()
        self._responses: queue.Queue[Any] = queue.Queue()
        self._id = 0
        self._closed = False
        argv = list(command) if command is not None else ["polign", "mcp", "-memory-only"] + (["-write"] if write else [])
        self._process = subprocess.Popen(argv, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                         stderr=None, env={**os.environ, **(env or {})})
        self._reader = threading.Thread(target=self._read, daemon=True)
        self._reader.start()
        try:
            self._rpc("initialize", {"protocolVersion": "2025-06-18", "capabilities": {},
                                     "clientInfo": {"name": "polign-recall-python", "version": "0.1.0"}})
            self._send({"jsonrpc": "2.0", "method": "notifications/initialized"})
        except BaseException:
            self.close()
            raise

    def _read(self) -> None:
        try:
            assert self._process.stdout is not None
            while True:
                line = self._process.stdout.readline((8 << 20) + 1)
                if not line:
                    raise EOFError("MCP server closed stdout")
                if len(line) > 8 << 20:
                    raise ValueError("MCP response exceeds 8 MiB")
                self._responses.put(json.loads(line))
        except Exception as exc:
            self._responses.put(exc)

    def _send(self, message: dict[str, Any]) -> None:
        payload = (json.dumps(message, allow_nan=False) + "\n").encode()
        assert self._process.stdin is not None
        try:
            self._process.stdin.write(payload)
            self._process.stdin.flush()
        except (OSError, ValueError) as exc:
            raise RecallError(str(exc), code="transport_error") from exc

    def _rpc(self, method: str, params: dict[str, Any]) -> Any:
        with self._lock:
            if self._closed:
                raise RecallError("client is closed", code="closed")
            self._id += 1
            self._send({"jsonrpc": "2.0", "id": self._id, "method": method, "params": params})
            try:
                response = self._responses.get(timeout=self.timeout)
            except queue.Empty as exc:
                self.close()
                raise RecallError("MCP call timed out; write outcome may be unknown", code="timeout") from exc
            if isinstance(response, Exception):
                self.close()
                raise RecallError(str(response), code="transport_error") from response
            if response.get("id") != self._id:
                self.close()
                raise RecallError("unexpected MCP response ID", code="protocol_error")
            if "error" in response:
                raise RecallError(response["error"]["message"], code="protocol_error")
            return response["result"]

    def _tool(self, name: str, arguments: dict[str, Any]) -> Any:
        result = self._rpc("tools/call", {"name": name, "arguments": arguments})
        text = "\n".join(c["text"] for c in result["content"] if c["type"] == "text")
        if result.get("isError"):
            partial = None
            try:
                partial = json.loads(text).get("partial")
            except (ValueError, AttributeError):
                pass
            raise RecallError(text, partial=partial)
        return json.loads(text)

    def remember(self, subject: str | None = None, predicate: str | None = None,
                 value: Any = _MISSING, *, text: str | None = None,
                 statements: Sequence[Mapping[str, Any]] | None = None,
                 kind: str | None = None, confidence: float | None = None,
                 source: str | None = None) -> RememberResult | ExtractionResult:
        if text is not None:
            if any(x is not None for x in (subject, predicate, kind, confidence, source)) or value is not _MISSING:
                raise ValueError("text mode cannot be combined with typed fields")
            if statements is None:
                raise ValueError("text mode requires statements proposed by your agent")
            data = self._tool("remember", {"text": text, "statements": list(statements)})
            return ExtractionResult(tuple(data.get("proposals") or []),
                                    tuple(RememberResult.decode(r) for r in data["results"]))
        if statements is not None:
            raise ValueError("statements requires original text")
        if subject is None or predicate is None or value is _MISSING:
            raise ValueError("typed remember requires subject, predicate, and value")
        args = {"subject": subject, "predicate": predicate, "value": value}
        args.update({k: v for k, v in {"kind": kind, "confidence": confidence, "source": source}.items() if v is not None})
        return RememberResult.decode(self._tool("remember", args))

    def recall(self, subject: str | None = None, predicate: str | None = None, *,
               query: str | None = None, as_of: str | datetime | None = None,
               min_confidence: float | None = None, limit: int | None = None) -> list[Belief]:
        if isinstance(as_of, datetime):
            if as_of.tzinfo is None:
                raise ValueError("as_of datetime must include a timezone")
            as_of = as_of.isoformat()
        args = {"subject": subject, "predicate": predicate, "query": query,
                "as_of": as_of, "min_confidence": min_confidence, "limit": limit}
        return [Belief(**b) for b in self._tool("recall", {k: v for k, v in args.items() if v is not None}) or []]

    def forget(self, subject: str, predicate: str, value: Any = _MISSING, *, all: bool = False) -> int:
        if all == (value is not _MISSING) or value is None:
            raise ValueError("forget requires exactly one typed value or all=True")
        args = {"subject": subject, "predicate": predicate}
        if value is not _MISSING:
            args["value"] = value
        return self._tool("forget", args)["withdrawn"]

    def history(self, subject: str, predicate: str) -> list[Event]:
        return [Event(**e) for e in self._tool("memory_history", {"subject": subject, "predicate": predicate}) or []]

    def predicates(self) -> list[dict[str, Any]]:
        return self._tool("list_predicates", {})

    def close(self) -> None:
        with self._lock:
            if self._closed:
                return
            self._closed = True
            if self._process.poll() is None:
                self._process.terminate()
                try:
                    self._process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    self._process.kill()
                    self._process.wait()
            self._reader.join(timeout=1)
            for stream in (self._process.stdin, self._process.stdout):
                if stream is not None:
                    stream.close()

    def __enter__(self) -> Client:
        return self

    def __exit__(self, *args: Any) -> None:
        self.close()
