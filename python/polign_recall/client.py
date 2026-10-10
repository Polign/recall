from __future__ import annotations

import json
import math
import os
import queue
import shutil
import subprocess
import sys
import sysconfig
import threading
import time
from dataclasses import dataclass, field, fields
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
class PriorValue:
    """A value that a newer statement replaced."""
    value: Value
    source: str
    observed_at: str
    event_id: str


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
    # Set by recall(with_sources=True) for a belief remembered from text: the
    # excerpt it was drawn from, the event holding the whole text, and that
    # text. `source` above is how the belief was come by, not this text.
    evidence: str = ""
    evidence_id: str = ""
    source_text: str = ""
    # What this belief replaced when it was stated, one step back: the
    # correction, delivered with the answer. Empty when it replaced nothing.
    replaced: tuple[PriorValue, ...] = ()
    # Whole days between when this was stated and the moment asked about
    # (as_of, or today). None from a server that does not send it.
    days_ago: int | None = None


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
    evidence: str = ""
    evidence_id: str = ""


@dataclass(frozen=True)
class RememberResult:
    stored: Belief
    already_known: bool
    superseded: tuple[Belief, ...]

    @classmethod
    def decode(cls, data: dict[str, Any]) -> RememberResult:
        return cls(_belief(data["stored"]), data.get("already_known", False),
                   tuple(_belief(b) for b in data.get("superseded", [])))


@dataclass(frozen=True)
class ExtractionResult:
    proposals: tuple[dict[str, Any], ...]
    results: tuple[RememberResult, ...]
    # Proposals whose predicate is not registered. Their text was kept as a
    # note instead of being refused.
    unfiled: tuple[dict[str, Any], ...] = ()
    # The note holding the whole text, which every statement names as its
    # evidence. None from a server that predates it.
    episode: RememberResult | None = None


@dataclass(frozen=True)
class Remembered:
    """One statement a text-mode remember filed, told in words."""
    text: str
    replaces: tuple[str, ...] = ()
    already_known: bool = False


@dataclass(frozen=True)
class TextResult:
    """What a text-mode remember did. `kept_as_text` is True when the text
    itself was kept as the record; with no extraction model on the server,
    that is all that happens and `remembered` is empty."""
    remembered: tuple[Remembered, ...]
    kept_as_text: bool


@dataclass(frozen=True)
class Earlier:
    """A value a Memory replaced, with the day it was stated."""
    text: str
    since: str


@dataclass(frozen=True)
class Memory:
    """One current memory as the text tools tell it: a sentence, the day it
    was stated, how many days before the question that was, and what it
    replaced."""
    text: str
    since: str
    days_ago: int
    before: tuple[Earlier, ...] = ()


def _known(cls: type, data: Any) -> dict[str, Any]:
    """The keys of `data` that `cls` declares. A newer server may send fields
    this client does not know yet; they are dropped instead of failing."""
    if not isinstance(data, dict):
        raise RecallError(f"malformed {cls.__name__}: {data!r}", code="protocol_error")
    names = {f.name for f in fields(cls)}
    return {k: v for k, v in data.items() if k in names}


def _belief(data: Any) -> Belief:
    known = _known(Belief, data)
    known["replaced"] = tuple(
        PriorValue(**{"value": "", "source": "", "observed_at": "", "event_id": "", **_known(PriorValue, p)})
        for p in known.get("replaced") or ())
    return Belief(**{"subject": "", "predicate": "", "value": "", "confidence": 0.0, "source": "",
                     "kind": "", "observed_at": "", "event_id": "", **known})


@dataclass(frozen=True)
class WorkingState:
    """The agent's note to its next instance. Every resume includes it whole."""
    goal: str = ""
    plan: tuple[str, ...] = ()
    progress: str = ""
    focus: str = ""
    decisions: tuple[str, ...] = ()
    open_questions: tuple[str, ...] = ()
    notes: str = ""
    step: int = 0
    last_milestone: str = ""
    # Written by the server.
    version: int = 0
    turn_seq: int = 0
    updated_at: str = ""

    @classmethod
    def decode(cls, data: Any) -> WorkingState:
        known = _known(cls, data)
        for key in ("plan", "decisions", "open_questions"):
            known[key] = tuple(known.get(key) or ())
        return cls(**known)


@dataclass(frozen=True)
class Turn:
    """One message of the agent's run, kept verbatim. When the content was too
    large, it holds only the opening and `output_ref` names the full output."""
    seq: int
    role: str
    content: str
    name: str = ""
    output_ref: str = ""
    # The harness's id for the message, when record_turn was given one.
    message_id: str = ""
    # The shorter form the resume briefing shows, when record_turn was given one.
    brief: str = ""
    at: str = ""

    @classmethod
    def decode(cls, data: Any) -> Turn:
        return cls(**{"seq": 0, "role": "", "content": "", **_known(cls, data)})


@dataclass(frozen=True)
class OutputRef:
    """A stored output, without its content."""
    ref: str
    tool: str = ""
    summary: str = ""
    tokens: int = 0
    at: str = ""

    @classmethod
    def decode(cls, data: Any) -> OutputRef:
        return cls(**{"ref": "", **_known(cls, data)})


@dataclass(frozen=True)
class Output:
    """A stored output with its full content."""
    ref: str
    content: str
    tool: str = ""
    summary: str = ""
    at: str = ""

    @classmethod
    def decode(cls, data: Any) -> Output:
        return cls(**{"ref": "", "content": "", **_known(cls, data)})


@dataclass(frozen=True)
class Pointer:
    """Where a piece of the agent's work lives: a branch, an object, an image."""
    name: str
    type: str
    fields: Mapping[str, str]
    note: str = ""
    at: str = ""

    @classmethod
    def decode(cls, data: Any) -> Pointer:
        known = {"name": "", "type": "", **_known(cls, data)}
        known["fields"] = dict(known.get("fields") or {})
        return cls(**known)


@dataclass(frozen=True)
class ResumeContext:
    """What `Client.resume` returns: the agent's records, trimmed to the token
    budget, and `briefing`, the same content as one text to hand the model."""
    agent_id: str
    fresh: bool
    briefing: str
    epoch: int = 0
    # False after resume(defer_lease=True) until the agent's acquire() succeeds.
    lease_held: bool = True
    working_state: WorkingState | None = None
    pointers: tuple[Pointer, ...] = ()
    outputs: tuple[OutputRef, ...] = ()
    memories: tuple[Belief, ...] = ()
    recent_turns: tuple[Turn, ...] = ()
    # How many of each were left out to fit the budget.
    omitted: Mapping[str, int] = field(default_factory=dict)
    turn_seq: int = 0
    token_budget: int = 0
    tokens: int = 0

    @classmethod
    def decode(cls, data: Any) -> ResumeContext:
        known = {"agent_id": "", "fresh": False, "briefing": "", **_known(cls, data)}
        ws = known.get("working_state")
        known["working_state"] = WorkingState.decode(ws) if ws else None
        known["pointers"] = tuple(Pointer.decode(p) for p in known.get("pointers") or ())
        known["outputs"] = tuple(OutputRef.decode(o) for o in known.get("outputs") or ())
        known["memories"] = tuple(_belief(b) for b in known.get("memories") or ())
        known["recent_turns"] = tuple(Turn.decode(t) for t in known.get("recent_turns") or ())
        omitted = known.get("omitted") or {}
        known["omitted"] = {k: int(omitted.get(k, 0)) for k in ("turns", "memories", "outputs")}
        return cls(**known)


def _instant(value: str | datetime, name: str) -> str:
    """An instant for the server: a timezone-aware datetime, or a string the
    server parses as RFC3339."""
    if isinstance(value, datetime):
        if value.tzinfo is None:
            raise ValueError(f"{name} datetime must include a timezone")
        return value.isoformat()
    return value


def _lease_error(exc: RecallError) -> RecallError:
    """Give lease failures their own code, so a caller can tell "someone else
    is running this agent" from a broken call."""
    text = str(exc)
    for marker, code in (("lease is held", "lease_held"), ("lease was lost", "lease_lost"),
                         ("lease not acquired", "lease_not_held")):
        if marker in text:
            return RecallError(text, code=code, partial=exc.partial)
    return exc


def recall_bin() -> str | None:
    """The `recall` binary to run: the one the polign-recall platform wheel
    installed next to this interpreter, then whatever `recall` is on PATH.
    None when neither exists."""
    exe = "recall" + (".exe" if sys.platform == "win32" else "")
    candidates = [os.path.join(sysconfig.get_path("scripts"), exe)]
    # `pip install --user` uses a different scheme from the interpreter's own.
    if sys.version_info >= (3, 10):
        candidates.append(os.path.join(sysconfig.get_path("scripts", scheme=sysconfig.get_preferred_scheme("user")), exe))
    # `pip install --target` puts scripts in <target>/bin next to the package.
    candidates.append(os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "bin", exe))
    for path in candidates:
        if os.path.isfile(path) and os.access(path, os.X_OK):
            return path
    return shutil.which("recall")


def polign_bin() -> str:
    """The `polign` CLI from polign_db 0.13 and earlier, which hosted Recall's
    server before it moved into the `recall` binary. Used only when `recall`
    is not installed."""
    try:
        import polign_db
        return polign_db.find_bin("polign")
    except (ImportError, OSError, ValueError):
        return shutil.which("polign") or "polign"


def _server() -> tuple[list[str], list[str]]:
    """The command prefixes that serve memory and set up a local database:
    `recall mcp` and `recall setup`, or the same server under its old names,
    `polign mcp -memory-only` and `polign recall setup`."""
    recall = recall_bin()
    if recall:
        return [recall, "mcp"], [recall, "setup"]
    polign = polign_bin()
    return [polign, "mcp", "-memory-only"], [polign, "recall", "setup"]


# Connection settings that belong to some other server. A managed local
# database must not inherit them, from the caller or from the environment.
_CONNECTION = ("POLIGN_URL", "POLIGN_API_KEY", "POLIGN_COLLECTION", "POLIGN_PREDICATES")


def _local_server(setup: Sequence[str], directory: str | os.PathLike[str], timeout: float) -> dict[str, str]:
    """Start (or find) the managed local database kept in `directory` and
    return the POLIGN_URL and POLIGN_API_KEY that reach it.

    `recall setup -local` does the work: it starts one detached polign-server
    for the directory, shared by every process that opens it, and is safe to
    run again while that server is up.
    """
    directory = os.path.abspath(directory)
    env = {k: v for k, v in os.environ.items() if k not in _CONNECTION}
    argv = [*setup, "-local", "-no-plugin", "-config-dir", directory]
    try:
        done = subprocess.run(argv, env=env, stdin=subprocess.DEVNULL, capture_output=True,
                              text=True, timeout=timeout)
    except subprocess.TimeoutExpired as exc:
        raise RecallError(f"local database in {directory} did not start within {timeout:g}s",
                          code="timeout") from exc
    except OSError as exc:
        raise RecallError(f"could not start {argv[0]!r}: {exc}", code="transport_error") from exc
    if done.returncode != 0:
        raise RecallError(f"local database in {directory} did not start: "
                          f"{(done.stderr or done.stdout).strip()}", code="transport_error")
    try:
        with open(os.path.join(directory, "runtime.json")) as f:
            url = json.load(f)["url"]
        with open(os.path.join(directory, "local-key")) as f:
            key = f.read().strip()
    except (OSError, ValueError, KeyError) as exc:
        raise RecallError(f"local database in {directory} left no connection details: {exc}",
                          code="transport_error") from exc
    return {"POLIGN_URL": url, "POLIGN_API_KEY": key}


class Client:
    """Owns one MCP subprocess. Thread-safe, with serialized calls and timeouts.

    Environment overrides extend the inherited environment. Credentials belong
    in POLIGN_API_KEY, not command-line arguments. The default enables memory
    writes; pass write=False to expose only read operations.

    The `recall` binary comes in this package's platform wheel, and
    polign-server with polign_db. With
    `local_dir`, the client also runs the database: it keeps a local
    polign-server for that directory and connects to it, ignoring any
    POLIGN_URL or POLIGN_API_KEY. Without it, `recall` connects to POLIGN_URL,
    or to what `recall setup` configured when POLIGN_URL is unset.

    `agent=True` also turns on the agent resume tools, for `resume`. It needs
    write=True. Agents resumed through this client stay held until they are
    released or the client closes.

    `extract_model` names the model that works out the statements in text
    remembered without any, as "provider:model": "anthropic:claude-opus-5-5",
    "openai:<model>", or "ollama:<model>". Keys come from ANTHROPIC_API_KEY or
    OPENAI_API_KEY. POLIGN_EXTRACT_MODEL in the environment does the same.

    `text=True` uses the text tools instead of the typed ones (`recall mcp
    -text`): `remember(text=...)`, `ask(question)` and `forget_text(...)`,
    with no predicates anywhere. The server works out the facts with
    `extract_model`; without one, remembered text is kept as written and
    replaces nothing. With `command`, the command must start the server with
    -text itself. Needs recall 0.14 or later.
    """

    def __init__(self, *, command: Sequence[str] | None = None,
                 env: Mapping[str, str] | None = None, timeout: float = 120,
                 write: bool = True, local_dir: str | os.PathLike[str] | None = None,
                 agent: bool = False, extract_model: str | None = None, text: bool = False):
        if not math.isfinite(timeout) or timeout <= 0:
            raise ValueError("timeout must be finite and positive")
        if agent and not write:
            raise ValueError("agent=True needs write=True: resuming an agent writes its records")
        if extract_model is not None and command is not None:
            raise ValueError("extract_model configures the recall server and cannot be combined with command")
        if local_dir is not None and command is not None:
            raise ValueError("local_dir runs the recall server itself and cannot be combined with command")
        self.timeout = timeout
        self._lock = threading.RLock()
        self._responses: queue.Queue[Any] = queue.Queue()
        self._id = 0
        self._closed = False
        self._agent = agent
        self._text = text
        serve, setup = _server() if command is None else ([], [])
        argv = list(command) if command is not None else (
            serve + (["-write"] if write else []) + (["-agent"] if agent else [])
            + (["-text"] if text else [])
            + (["-extract-model", extract_model] if extract_model else []))
        if local_dir is not None:
            env = {**(env or {}), **_local_server(setup, local_dir, timeout)}
        try:
            self._process = subprocess.Popen(argv, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                             stderr=None, env={**os.environ, **(env or {})})
        except OSError as exc:
            # The documented contract is that failures arrive as RecallError.
            # A missing binary is the most common first run, and it must not
            # escape as a bare OSError with no hint about what to install.
            raise RecallError(f"could not start {argv[0]!r}: {exc}",
                              code="transport_error") from exc
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
        # The write has to be bounded like the read. A server that stops
        # draining its stdin blocks this call forever once the payload passes
        # the pipe buffer, and the blocked call holds the lock close() needs, so
        # no other thread can even shut the client down.
        failure: list[BaseException] = []
        done = threading.Event()

        def write() -> None:
            try:
                self._process.stdin.write(payload)
                self._process.stdin.flush()
            except BaseException as exc:  # re-raised on the calling thread
                failure.append(exc)
            finally:
                done.set()

        threading.Thread(target=write, daemon=True).start()
        if not done.wait(self.timeout):
            # close() ends the child, which unblocks the writer. The lock is
            # reentrant and held by this thread, so this cannot deadlock.
            self.close()
            raise RecallError("MCP write timed out; the server stopped reading its input",
                              code="timeout")
        if failure:
            exc = failure[0]
            if isinstance(exc, (OSError, ValueError)):
                raise RecallError(str(exc), code="transport_error") from exc
            raise exc

    def _rpc(self, method: str, params: dict[str, Any]) -> Any:
        with self._lock:
            if self._closed:
                raise RecallError("client is closed", code="closed")
            self._id += 1
            self._send({"jsonrpc": "2.0", "id": self._id, "method": method, "params": params})
            deadline = time.monotonic() + self.timeout
            while True:
                try:
                    response = self._responses.get(timeout=max(0.0, deadline - time.monotonic()))
                except queue.Empty as exc:
                    self.close()
                    raise RecallError("MCP call timed out; write outcome may be unknown", code="timeout") from exc
                if isinstance(response, Exception):
                    self.close()
                    raise RecallError(str(response), code="transport_error") from response
                # Anything carrying a method is a notification or a request the
                # server started, not an answer to this call, and an older id is
                # a reply to a call already abandoned. Neither is a protocol
                # violation, and neither should destroy the session.
                if "method" in response or response.get("id") != self._id:
                    continue
                break
            if "error" in response:
                raise RecallError(response["error"]["message"], code="protocol_error")
            return response["result"]

    def _tool(self, name: str, arguments: dict[str, Any]) -> Any:
        result = self._rpc("tools/call", {"name": name, "arguments": arguments})
        try:
            text = "\n".join(c["text"] for c in result["content"] if c["type"] == "text")
        except (KeyError, TypeError) as exc:
            raise RecallError(f"malformed {name} result: {exc}", code="protocol_error") from exc
        if result.get("isError"):
            partial = None
            try:
                partial = json.loads(text).get("partial")
            except (ValueError, AttributeError):
                pass
            raise RecallError(text, partial=partial)
        try:
            return json.loads(text)
        except ValueError as exc:
            raise RecallError(f"malformed {name} result: {exc}", code="protocol_error") from exc

    def remember(self, subject: str | None = None, predicate: str | None = None,
                 value: Any = _MISSING, *, text: str | None = None,
                 statements: Sequence[Mapping[str, Any]] | None = None,
                 kind: str | None = None, confidence: float | None = None,
                 source: str | None = None,
                 observed_at: str | datetime | None = None) -> RememberResult | ExtractionResult | TextResult:
        """Record a typed statement, or the statements in a piece of text.

        In text mode, pass `statements` your agent proposes, or leave them out
        and the server's extraction model (`extract_model`) proposes them.

        On a client opened with text=True, pass `text` alone; the result is a
        TextResult saying what was filed and what each statement replaced.

        `observed_at` dates a statement made earlier, such as a line of an
        imported conversation; omitted means now. A statement dated before a
        later one for the same subject and predicate is kept as history and
        does not replace it. The server refuses times in the future.
        """
        dated = {"observed_at": _instant(observed_at, "observed_at")} if observed_at is not None else {}
        if self._text:
            if text is None or statements is not None or value is not _MISSING or any(
                    x is not None for x in (subject, predicate, kind, confidence, source)):
                raise ValueError("a text client remembers text alone: remember(text=...)")
            data = self._tool("remember", {"text": text, **dated})
            return TextResult(tuple(Remembered(r.get("text", ""), tuple(r.get("replaces") or ()),
                                               bool(r.get("already_known")))
                                    for r in data.get("remembered") or ()),
                              bool(data.get("kept_as_text")))
        if text is not None:
            if any(x is not None for x in (subject, predicate, kind, confidence, source)) or value is not _MISSING:
                raise ValueError("text mode cannot be combined with typed fields")
            proposed = {"statements": list(statements)} if statements is not None else {}
            data = self._tool("remember", {"text": text, **proposed, **dated})
            return ExtractionResult(tuple(data.get("proposals") or []),
                                    tuple(RememberResult.decode(r) for r in data["results"]),
                                    tuple(data.get("unfiled") or []),
                                    RememberResult.decode(data["episode"]) if data.get("episode") else None)
        if statements is not None:
            raise ValueError("statements requires original text")
        if subject is None or predicate is None or value is _MISSING:
            raise ValueError("typed remember requires subject, predicate, and value")
        args = {"subject": subject, "predicate": predicate, "value": value, **dated}
        args.update({k: v for k, v in {"kind": kind, "confidence": confidence, "source": source}.items() if v is not None})
        return RememberResult.decode(self._tool("remember", args))

    def ask(self, question: str, *, as_of: str | datetime | None = None) -> list[Memory]:
        """What is believed about `question`, as sentences (text=True only).
        Each Memory lists under `before` what it replaced. `as_of` asks about
        an earlier time."""
        self._need_text("ask")
        args: dict[str, Any] = {"question": question}
        if as_of is not None:
            args["as_of"] = _instant(as_of, "as_of")
        data = self._tool("recall", args)
        return [Memory(m.get("text", ""), m.get("since", ""), int(m.get("days_ago", 0)),
                       tuple(Earlier(e.get("text", ""), e.get("since", "")) for e in m.get("before") or ()))
                for m in data.get("memories") or ()]

    def forget_text(self, text: str) -> list[str]:
        """Withdraws the facts a description names, such as "my editor"
        (text=True only), and returns them as sentences. The texts they came
        from stay as the record. Needs an extraction model on the server."""
        self._need_text("forget_text")
        return list(self._tool("forget", {"text": text}).get("forgotten") or [])

    def _need_text(self, method: str) -> None:
        if not self._text:
            raise ValueError(f"{method} needs a client opened with text=True")

    def _need_typed(self, method: str) -> None:
        if self._text:
            raise ValueError(f"{method} uses the typed tools; this client was opened with text=True "
                             "(use ask, remember(text=...) and forget_text)")

    def recall(self, subject: str | None = None, predicate: str | None = None, *,
               query: str | None = None, as_of: str | datetime | None = None,
               min_confidence: float | None = None, limit: int | None = None,
               with_sources: bool = False, observed_after: str | datetime | None = None,
               observed_before: str | datetime | None = None) -> list[Belief]:
        """What is believed. With `with_sources`, a belief remembered from text
        also carries `evidence`, `evidence_id` and `source_text`, the excerpt
        and the whole text it was drawn from. It needs a polign CLI whose
        recall tool accepts with_sources.

        `observed_after` and `observed_before` keep only beliefs stated in
        that span. A query that names a time ("two weeks ago", "last
        Saturday") already searches that span first without them."""
        self._need_typed("recall")
        if as_of is not None:
            as_of = _instant(as_of, "as_of")
        if observed_after is not None:
            observed_after = _instant(observed_after, "observed_after")
        if observed_before is not None:
            observed_before = _instant(observed_before, "observed_before")
        args = {"subject": subject, "predicate": predicate, "query": query,
                "as_of": as_of, "min_confidence": min_confidence, "limit": limit,
                "with_sources": True if with_sources else None,
                "observed_after": observed_after, "observed_before": observed_before}
        return [_belief(b) for b in self._tool("recall", {k: v for k, v in args.items() if v is not None}) or []]

    def forget(self, subject: str, predicate: str, value: Any = _MISSING, *, all: bool = False) -> int:
        self._need_typed("forget")
        if all == (value is not _MISSING) or value is None:
            raise ValueError("forget requires exactly one typed value or all=True")
        args = {"subject": subject, "predicate": predicate}
        if value is not _MISSING:
            args["value"] = value
        return self._tool("forget", args)["withdrawn"]

    def history(self, subject: str, predicate: str) -> list[Event]:
        self._need_typed("history")
        return [Event(**_known(Event, e)) for e in self._tool("memory_history", {"subject": subject, "predicate": predicate}) or []]

    def predicates(self) -> list[dict[str, Any]]:
        self._need_typed("predicates")
        return self._tool("list_predicates", {})

    @property
    def agent_enabled(self) -> bool:
        """True when the client was opened with agent=True, so `resume` works."""
        return self._agent

    def resume(self, agent_id: str, *, token_budget: int | None = None,
               lease_ttl: float | None = None, holder: str | None = None,
               output_threshold: int | None = None, defer_lease: bool = False) -> ResumedAgent:
        """Take the agent's lease and return it with the context to start from.

        This is also how an agent starts the first time; `context.fresh` is
        then true. It raises RecallError with code "lease_held" while another
        process holds the agent. `lease_ttl` is in seconds; the server renews
        the lease in the background until release.

        With `defer_lease=True` it returns the context at once without taking
        the lease, even while a crashed process's lease is still live. Writes
        then raise RecallError with code "lease_not_held" until the agent's
        `acquire()` returns True.
        """
        if not self._agent:
            raise ValueError("resume needs a client opened with agent=True")
        args: dict[str, Any] = {"agent_id": agent_id}
        if token_budget is not None:
            args["token_budget"] = token_budget
        if lease_ttl is not None:
            if not math.isfinite(lease_ttl) or lease_ttl <= 0:
                raise ValueError("lease_ttl must be finite and positive")
            args["lease_ttl_seconds"] = max(1, math.ceil(lease_ttl))
        if holder is not None:
            args["holder"] = holder
        if output_threshold is not None:
            args["output_threshold"] = output_threshold
        if defer_lease:
            args["defer_lease"] = True
        try:
            data = self._tool("agent_resume", args)
        except RecallError as exc:
            raise _lease_error(exc) from None
        return ResumedAgent(self, agent_id, ResumeContext.decode(data))

    def close(self) -> None:
        with self._lock:
            if self._closed:
                return
            self._closed = True
            if self._agent and self._process.poll() is None and self._process.stdin is not None:
                # Closing stdin ends the session cleanly, and the server then
                # hands every lease over. A terminated server leaves them to
                # expire, so the next process would wait out the TTL.
                try:
                    self._process.stdin.close()
                    self._process.wait(timeout=min(self.timeout, 15))
                except (OSError, ValueError, subprocess.TimeoutExpired):
                    pass
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
                    try:
                        stream.close()
                    except OSError:
                        pass

    def __enter__(self) -> Client:
        return self

    def __exit__(self, *args: Any) -> None:
        self.close()


class ResumedAgent:
    """An agent this client holds. Its writes are what the next resume reads.

    Every method is a call on the client's subprocess, so they are blocking
    and serialized like the memory calls. Use it as a context manager, or call
    `release` on a clean shutdown so the next process need not wait for the
    lease to expire.
    """

    def __init__(self, client: Client, agent_id: str, context: ResumeContext):
        self.client = client
        self.agent_id = agent_id
        self.context = context
        self._lease_held = context.lease_held

    @property
    def lease_held(self) -> bool:
        """False after a deferred resume until `acquire` succeeds."""
        return self._lease_held

    def acquire(self) -> bool:
        """Take the lease after resume(defer_lease=True). Tries once: returns
        False while another process still holds it, so call it again later.
        Returns True once held, and at once if it already was."""
        if self._lease_held:
            return True
        try:
            self._call("agent_acquire")
        except RecallError as exc:
            if exc.code == "lease_held":
                return False
            raise
        self._lease_held = True
        return True

    def _call(self, tool: str, /, **args: Any) -> Any:
        try:
            return self.client._tool(tool, {"agent_id": self.agent_id,
                                            **{k: v for k, v in args.items() if v is not None}})
        except RecallError as exc:
            raise _lease_error(exc) from None

    def update_working_state(self, **fields: Any) -> WorkingState:
        """Supersede the working state. Fields you pass replace the current
        ones and the rest are kept: goal, plan, progress, focus, decisions,
        open_questions, notes, step."""
        for key in ("plan", "decisions", "open_questions"):
            if key in fields and fields[key] is not None:
                fields[key] = list(fields[key])
        return WorkingState.decode(self._call("update_working_state", **fields))

    def milestone(self, name: str, progress: str | None = None) -> WorkingState:
        """Record a durable point. A crash costs at most the work since the last one."""
        return WorkingState.decode(self._call("agent_milestone", name=name, progress=progress))

    def record_turn(self, role: str, content: str, name: str | None = None,
                    message_id: str | None = None, brief: str | None = None) -> Turn:
        """Append one message, verbatim. role is system, user, assistant or tool.
        `message_id` is your harness's id for the message, kept with the turn,
        so that after a resume you can tell messages already recorded from
        new ones. `brief` is a shorter form for the resume briefing (a tool
        call with its long arguments elided, say); the record keeps `content`
        whole."""
        return Turn.decode(self._call("record_turn", role=role, content=content, name=name,
                                      message_id=message_id, brief=brief))

    def recent_turns(self, limit: int | None = None) -> list[Turn]:
        """The newest turns, oldest first (20 by default)."""
        return [Turn.decode(t) for t in self._call("recent_turns", limit=limit) or []]

    def store_output(self, content: str, tool: str | None = None, summary: str | None = None,
                     ref: str | None = None) -> OutputRef:
        """Keep a large output whole, to fetch by reference instead of carrying it."""
        return OutputRef.decode(self._call("store_output", content=content, tool=tool,
                                           summary=summary, ref=ref))

    def fetch_output(self, ref: str) -> Output:
        return Output.decode(self._call("fetch_output", ref=ref))

    def set_pointer(self, name: str, type: str, fields: Mapping[str, str],
                    note: str | None = None) -> Pointer:
        """Record where a piece of work lives, replacing a pointer of the same
        name. type is git_ref, object, env, external or process."""
        return Pointer.decode(self._call("set_pointer", name=name, type=type,
                                         fields=dict(fields), note=note))

    def remove_pointer(self, name: str) -> None:
        self._call("remove_pointer", name=name)

    def pointers(self) -> list[Pointer]:
        return [Pointer.decode(p) for p in self._call("list_pointers") or []]

    def working_state_history(self, limit: int | None = None) -> list[WorkingState]:
        """Earlier versions of the working state, newest first."""
        return [WorkingState.decode(w) for w in self._call("working_state_history", limit=limit) or []]

    def release(self) -> bool:
        """Hand the lease over now. Returns False if this client no longer
        held the agent, including when the client is already closed (closing
        releases every agent it held)."""
        try:
            return bool(self._call("agent_release").get("released"))
        except RecallError as exc:
            if exc.code == "closed":
                return False
            raise

    def __enter__(self) -> ResumedAgent:
        return self

    def __exit__(self, *args: Any) -> None:
        self.release()
