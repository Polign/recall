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
import urllib.error
import urllib.request
from dataclasses import dataclass, field, fields
from datetime import datetime
from typing import Any, Callable, Mapping, Sequence

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
    # Set when the value was held under another predicate (likes mangoes,
    # replaced by dislikes mangoes); empty for the same predicate.
    predicate: str = ""


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
    # The subject's current beliefs under other predicates that hold the same
    # value, such as a dislike beside a like; each PriorValue names its
    # predicate.
    elsewhere: tuple[PriorValue, ...] = ()
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
    # Current beliefs of the same subject, under other predicates, that may
    # contradict the statement. When it makes any of them untrue, remember it
    # again with those in `replaces`.
    related: tuple[Belief, ...] = ()
    # Beliefs named in `replaces` that were not believed, so nothing was
    # withdrawn for them, each as {"predicate": ..., "value": ...}.
    unreplaced: tuple[dict[str, Any], ...] = ()

    @classmethod
    def decode(cls, data: dict[str, Any]) -> RememberResult:
        return cls(_belief(data["stored"]), data.get("already_known", False),
                   tuple(_belief(b) for b in data.get("superseded") or []),
                   tuple(_belief(b) for b in data.get("related") or []),
                   tuple(data.get("unreplaced") or []))


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
    # What else is believed about the same thing in other words.
    elsewhere: tuple[Earlier, ...] = ()
    # The sentence the memory was drawn from, as it was said.
    said: str = ""


@dataclass(frozen=True)
class Step:
    """One statement in a Timeline: when it was made, what it did (`stated`,
    `replaced`, `withdrawn` or `no change`), and the sentences it `ended` and
    `began` being believed. `statement` is set when the step was caused by a
    statement about something else (a dislike ending a like) or changed
    nothing."""
    when: str
    change: str
    ended: tuple[str, ...] = ()
    began: tuple[str, ...] = ()
    said: str = ""
    statement: str = ""


@dataclass(frozen=True)
class Timeline:
    """How one remembered thing changed, oldest first, as history_text tells
    it, with what is believed `now`."""
    about: str
    now: tuple[str, ...]
    changes: tuple[Step, ...]
    # Set by the typed timeline; the text tools never name predicates.
    subject: str = ""
    predicate: str = ""


@dataclass(frozen=True)
class Change:
    """What stopped (`ended`) and started (`began`) being believed about one
    thing between the two instants of a diff."""
    ended: tuple[str, ...] = ()
    began: tuple[str, ...] = ()
    # Set by the typed diff, with what was believed at each end.
    subject: str = ""
    predicate: str = ""
    before: tuple[str, ...] = ()
    after: tuple[str, ...] = ()


def _known(cls: type, data: Any) -> dict[str, Any]:
    """The keys of `data` that `cls` declares. A newer server may send fields
    this client does not know yet; they are dropped instead of failing."""
    if not isinstance(data, dict):
        raise RecallError(f"malformed {cls.__name__}: {data!r}", code="protocol_error")
    names = {f.name for f in fields(cls)}
    return {k: v for k, v in data.items() if k in names}


def _belief(data: Any) -> Belief:
    known = _known(Belief, data)
    for name in ("replaced", "elsewhere"):
        known[name] = tuple(
            PriorValue(**{"value": "", "source": "", "observed_at": "", "event_id": "", **_known(PriorValue, p)})
            for p in known.get(name) or ())
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
    action_seq: int = 0
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


ACTION_CLASSES = ("pure", "idempotent", "external")


@dataclass(frozen=True)
class Action:
    """One tool call recorded around its run. `begin_action` writes it before
    the tool runs and `end_action` closes it; one a crash left open is listed
    in `ResumeContext.pending_actions` and at the top of the briefing.

    `class_` is what running the tool again would do: "pure" (reads only),
    "idempotent" (repeating with `key` does nothing more; pass `key` to the
    tool) or "external" (could happen twice). `status` is "started",
    "done", "failed", "abandoned" or "unknown"; started and unknown are open.
    """
    seq: int
    tool: str
    class_: str
    key: str
    status: str
    args: str = ""
    result: str = ""
    error: str = ""
    args_ref: str = ""
    result_ref: str = ""
    resolution: str = ""
    epoch: int = 0
    started_at: str = ""
    ended_at: str = ""

    @property
    def open(self) -> bool:
        return self.status in ("started", "unknown")

    @classmethod
    def decode(cls, data: Any) -> Action:
        data = dict(data or {})
        if "class" in data:
            data["class_"] = data.pop("class")
        return cls(**{"seq": 0, "tool": "", "class_": "", "key": "", "status": "", **_known(cls, data)})


@dataclass(frozen=True)
class ActionEnd:
    """How a resolver settled an action. `status` is "done", "failed",
    "abandoned" or "unknown"."""
    status: str
    result: str = ""
    error: str = ""
    resolution: str = ""


@dataclass(frozen=True)
class ReconcileReport:
    """What `ResumedAgent.reconcile` closed, and what is still open."""
    closed: tuple[Action, ...] = ()
    open: tuple[Action, ...] = ()


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
    # Tool calls a crash left open. Settle them before anything else.
    pending_actions: tuple[Action, ...] = ()
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
        known["pending_actions"] = tuple(Action.decode(a) for a in known.get("pending_actions") or ())
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
    """The `recall` binary to run: RECALL_BIN when set, then the one the
    polign-recall platform wheel installed next to this interpreter, then
    whatever `recall` is on PATH. None when none exists."""
    exe = "recall" + (".exe" if sys.platform == "win32" else "")
    candidates = [os.environ.get("RECALL_BIN") or "", os.path.join(sysconfig.get_path("scripts"), exe)]
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


# The variables that name a database, in the spelling recall 0.23 reads and
# the ones each backend had before it. A client that names a database strips
# all of them from the environment it hands the binary, so the binary resolves
# exactly what the caller gave.
_DATABASE_VARS = ("RECALL_BACKEND", "RECALL_BACKEND_URL", "RECALL_BACKEND_KEY",
                  "POLIGN_URL", "POLIGN_API_KEY", "DATABASE_URL", "PGPASSWORD",
                  "MONGODB_URI", "MONGODB_PASSWORD", "QDRANT_URL", "QDRANT_API_KEY")

# Connection settings that belong to some other server. A managed local
# database must not inherit them, from the caller or from the environment.
_CONNECTION = _DATABASE_VARS + ("RECALL_COLLECTION", "RECALL_PREDICATES", "POLIGN_COLLECTION", "POLIGN_PREDICATES")

_versions: dict[str, tuple[int, ...]] = {}


def _version(binary: str) -> tuple[int, ...]:
    """The version `recall -version` prints, as a tuple, cached per binary.
    A development build ("dev") counts as the newest. A binary that does not
    answer counts as older than every release."""
    if binary in _versions:
        return _versions[binary]
    version: tuple[int, ...] = (0,)
    try:
        out = subprocess.run([binary, "-version"], capture_output=True, text=True, timeout=10,
                             stdin=subprocess.DEVNULL).stdout.strip()
        if out == "dev":
            version = (999,)
        else:
            version = tuple(int(part) for part in out.lstrip("v").split("-")[0].split("."))
    except (OSError, ValueError, subprocess.TimeoutExpired):
        pass
    _versions[binary] = version
    return version


def _database_env(backend: str | None, url: str | os.PathLike[str] | None, key: str | None,
                  serve: Sequence[str], setup: Sequence[str], timeout: float) -> dict[str, str]:
    """The environment that names the database to the binary.

    recall 0.23 and later read RECALL_BACKEND, RECALL_BACKEND_URL and
    RECALL_BACKEND_KEY and resolve the rest themselves, local database
    included. An older binary gets the same database in the names it knows:
    for the local backend that means running `recall setup -local` for the
    directory, as this client always did.
    """
    if url is not None:
        url = os.fspath(url)
    if "-memory-only" not in serve and _version(serve[0]) >= (0, 23):
        env = {}
        if backend is not None:
            env["RECALL_BACKEND"] = backend
        if url is not None:
            env["RECALL_BACKEND_URL"] = url
        if key is not None:
            env["RECALL_BACKEND_KEY"] = key
        return env
    # An older binary: what -url meant to it depends on its form.
    if backend is None:
        backend = "polign" if url and "://" in url else "local"
    if url and "?" in url and backend in ("local", "polign", "qdrant"):
        raise RecallError(f"options on the {backend} URL ({url!r}) need recall 0.23 or later; "
                          "this environment runs an older recall binary", code="unsupported")
    if backend == "local":
        directory = url or os.environ.get("RECALL_HOME") or os.environ.get("POLIGN_RECALL_HOME") \
            or os.path.join(os.path.expanduser("~"), ".config", "polign", "recall")
        return _local_server(setup, directory, timeout)
    names = {"polign": ("POLIGN_URL", "POLIGN_API_KEY"), "pgvector": ("DATABASE_URL", "PGPASSWORD"),
             "mongodb": ("MONGODB_URI", "MONGODB_PASSWORD"), "qdrant": ("QDRANT_URL", "QDRANT_API_KEY")}
    if backend not in names:
        raise RecallError(f"backend {backend!r} needs recall 0.23 or later; this environment runs an older recall binary",
                          code="unsupported")
    url_var, key_var = names[backend]
    env = {"RECALL_BACKEND": backend}
    if url is not None:
        env[url_var] = url
    if key is not None:
        env[key_var] = key
    return env


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


_DEFAULT_TOKEN_FILE = "/var/run/secrets/recall/token"


class _HTTPSession:
    """One `recall serve`: JSON-RPC over HTTP at /mcp, behind a Recall key.

    The key is given, or obtained by exchanging a projected Kubernetes token at
    /v1/auth/kubernetes and renewed at /v1/keys/renew once past half its
    lifetime. A 401 part-way through triggers one fresh exchange and a retry.
    """

    def __init__(self, url: str, api_key: str | None, token_file: str | None, timeout: float):
        if not api_key and not token_file:
            raise ValueError("a url needs api_key or token_file")
        self.url = url.rstrip("/")
        self.timeout = timeout
        self._api_key = api_key
        self._token_file = token_file
        self._lock = threading.Lock()
        self._key: str | None = api_key
        self._expires_at: float | None = None
        self._ttl: float | None = None

    def _post(self, path: str, body: bytes | None, bearer: str | None) -> tuple[int, Any]:
        headers = {"Content-Type": "application/json", "Accept": "application/json"}
        if bearer:
            headers["Authorization"] = "Bearer " + bearer
        request = urllib.request.Request(self.url + path, data=body, headers=headers, method="POST")
        try:
            with urllib.request.urlopen(request, timeout=self.timeout) as response:  # noqa: S310 (operator-supplied URL)
                raw = response.read()
                status = response.status
        except urllib.error.HTTPError as exc:
            with exc:
                raw = exc.read()
                status = exc.code
        except (urllib.error.URLError, OSError, TimeoutError) as exc:
            raise RecallError(f"could not reach {self.url}: {exc}", code="transport_error") from exc
        if status == 202 or not raw.strip():
            return status, None
        try:
            return status, json.loads(raw)
        except ValueError as exc:
            raise RecallError(f"{path}: malformed answer: {exc}", code="protocol_error") from exc

    @staticmethod
    def _failure(status: int, data: Any, what: str) -> RecallError:
        message = what
        if isinstance(data, dict) and isinstance(data.get("error"), dict):
            message = data["error"].get("message", what)
        code = {401: "unauthenticated", 403: "forbidden", 404: "not_found"}.get(status, "transport_error")
        return RecallError(f"{message} (HTTP {status})", code=code)

    def _exchange(self) -> str:
        assert self._token_file is not None
        try:
            with open(self._token_file, encoding="utf-8") as f:
                token = f.read().strip()
        except OSError as exc:
            raise RecallError(f"token file: {exc}", code="transport_error") from exc
        status, data = self._post("/v1/auth/kubernetes", json.dumps({"token": token}).encode(), None)
        if status != 201 or not isinstance(data, dict) or not data.get("key"):
            raise self._failure(status, data, "the server refused the token")
        self._key = data["key"]
        self._ttl = _seconds(data.get("ttl"))
        self._expires_at = time.monotonic() + self._ttl if self._ttl else None
        return self._key

    def _renew(self) -> bool:
        status, data = self._post("/v1/keys/renew", None, self._key)
        if status != 200 or self._ttl is None:
            return False
        self._expires_at = time.monotonic() + self._ttl
        return True

    def bearer(self) -> str:
        with self._lock:
            if self._key is None:
                return self._exchange()
            if self._expires_at is not None and self._ttl and time.monotonic() > self._expires_at - self._ttl / 2:
                if not self._renew():
                    return self._exchange()
            return self._key

    def refresh(self) -> bool:
        if self._token_file is None:
            return False
        with self._lock:
            self._key = None
            self._exchange()
            return True

    def rpc(self, message: dict[str, Any]) -> Any:
        body = json.dumps(message, allow_nan=False).encode()
        for attempt in (0, 1):
            status, data = self._post("/mcp", body, self.bearer())
            if status == 401 and attempt == 0 and self.refresh():
                continue
            if status != 200:
                raise self._failure(status, data, "the server refused the call")
            return data
        raise AssertionError("unreachable")

    def notify(self, message: dict[str, Any]) -> None:
        self._post("/mcp", json.dumps(message, allow_nan=False).encode(), self.bearer())


def _seconds(duration: Any) -> float | None:
    """Parse a Go duration such as "15m0s" or "1h30m" into seconds."""
    if not isinstance(duration, str) or not duration:
        return None
    total, number = 0.0, ""
    units = {"h": 3600.0, "m": 60.0, "s": 1.0, "ms": 0.001}
    i = 0
    while i < len(duration):
        c = duration[i]
        if c.isdigit() or c == ".":
            number += c
            i += 1
            continue
        unit = c
        if duration[i:i + 2] == "ms":
            unit, i = "ms", i + 1
        i += 1
        if not number or unit not in units:
            return None
        total += float(number) * units[unit]
        number = ""
    return total if not number else None


class Client:
    """Owns one MCP subprocess, or one HTTP session with a `recall serve`.
    Thread-safe, with serialized calls and timeouts.

    With `url`, the client talks to a `recall serve` over HTTP instead of
    starting a process. It needs `api_key` (a Recall key) or `token_file` (a
    projected Kubernetes service account token the server exchanges for one).
    The server decides what it serves: text or typed tools, and whether the
    resume tools are there; the client finds out at connect time, so `text`
    and `agent` are read from the server rather than chosen here. See also
    `Client.from_env()`, which reads RECALL_URL and the credential variables.

    Environment overrides extend the inherited environment. Credentials belong
    in POLIGN_API_KEY, not command-line arguments. The default enables memory
    writes; pass write=False to expose only read operations.

    The `recall` binary comes in this package's platform wheel, and
    polign-server with polign_db. With
    `backend`, `backend_url` and `backend_key` say where memory lives, the
    same three names as `recall -backend`, `-url` and `-key` and the
    RECALL_BACKEND, RECALL_BACKEND_URL and RECALL_BACKEND_KEY variables:
    `backend` is "local", "polign", "pgvector", "mongodb" or "qdrant", the URL
    is in that backend's own form (a directory for local), and the key is the
    credential when the URL does not carry one. Given any of them, the client
    hands the binary exactly those and nothing the environment says about a
    database. Given none, the binary resolves it: the environment, then what
    `recall setup` saved, then a local database in RECALL_HOME it runs itself.
    `local_dir=X` is the short spelling of `backend="local", backend_url=X`.
    The first client to open a directory starts its polign-server and later
    ones share it. With a recall binary older than 0.23 the client says the
    same thing in the names that binary knows.

    `agent=True` also turns on the agent resume tools, for `resume`. It needs
    write=True. Agents resumed through this client stay held until they are
    released or the client closes.

    `extract_model` names the model that works out the statements in text
    remembered without any, as "provider:model": "anthropic:claude-opus-5-5",
    "openai:<model>", or "ollama:<model>". Keys come from ANTHROPIC_API_KEY or
    OPENAI_API_KEY. POLIGN_EXTRACT_MODEL in the environment does the same.

    The client serves the text tools unless told otherwise: `remember(text=...)`,
    `ask(question)` and `forget_text(...)`, with no predicates anywhere. The
    server works out the facts with `extract_model`; without one, remembered
    text is kept as written and replaces nothing. `typed=True` serves the typed
    tools instead (`recall mcp -typed`, recall 0.18 or later): `remember(subject,
    predicate, value)`, `recall`, `predicates`, `history` and `forget`.
    `text=True` insists on the text tools (recall 0.14 or later). With `command`
    or `url`, the server decides, and the client follows what it lists; asking
    for the other surface is an error.
    """

    def __init__(self, *, command: Sequence[str] | None = None,
                 env: Mapping[str, str] | None = None, timeout: float = 120,
                 write: bool = True, local_dir: str | os.PathLike[str] | None = None,
                 agent: bool = False, extract_model: str | None = None, text: bool | None = None,
                 typed: bool = False, url: str | None = None, api_key: str | None = None,
                 token_file: str | None = None, backend: str | None = None,
                 backend_url: str | os.PathLike[str] | None = None, backend_key: str | None = None):
        if not math.isfinite(timeout) or timeout <= 0:
            raise ValueError("timeout must be finite and positive")
        if agent and not write:
            raise ValueError("agent=True needs write=True: resuming an agent writes its records")
        if text and typed:
            raise ValueError("text=True and typed=True name different tools; pass one")
        self.server_version = ""
        self._http: _HTTPSession | None = None
        self._process: subprocess.Popen[bytes] | None = None
        if local_dir is not None:
            if backend not in (None, "local"):
                raise ValueError(f"local_dir keeps memory in the local backend; it cannot be combined with backend={backend!r}")
            if backend_url is not None:
                raise ValueError("local_dir and backend_url both name the directory; pass one")
            backend, backend_url = "local", local_dir
        names_database = backend is not None or backend_url is not None or backend_key is not None
        if url is not None:
            if command is not None or names_database or extract_model is not None or env is not None:
                raise ValueError("url connects to a recall serve; command, backend, backend_url, backend_key, local_dir, env and extract_model configure a local process and cannot be combined with it")
            self.timeout = timeout
            self._lock = threading.RLock()
            self._id = 0
            self._closed = False
            self._http = _HTTPSession(url, api_key, token_file, timeout)
            self._initialize()
            self._http.notify({"jsonrpc": "2.0", "method": "notifications/initialized"})
            names = self._tool_names()
            self._agent = "agent_resume" in names
            if agent and not self._agent:
                raise ValueError("agent=True, but this server does not serve the resume tools (its backend has no leases)")
            self._adopt_surface(names, text, typed)
            return
        if api_key is not None or token_file is not None:
            raise ValueError("api_key and token_file go with url")
        if extract_model is not None and command is not None:
            raise ValueError("extract_model configures the recall server and cannot be combined with command")
        if names_database and command is not None:
            raise ValueError("backend, backend_url, backend_key and local_dir tell the recall server where memory is and cannot be combined with command")
        self.timeout = timeout
        self._lock = threading.RLock()
        self._responses: queue.Queue[Any] = queue.Queue()
        self._id = 0
        self._closed = False
        self._agent = agent
        self._text = text is not False and not typed
        serve, setup = _server() if command is None else ([], [])
        if command is not None:
            surface: list[str] = []
        elif "-memory-only" in serve:
            # polign mcp predates -typed and serves the typed tools unasked.
            surface = ["-text"] if text else []
        else:
            surface = ["-typed"] if typed else ["-text"]
        argv = list(command) if command is not None else (
            serve + (["-write"] if write else []) + (["-agent"] if agent else []) + surface
            + (["-extract-model", extract_model] if extract_model else []))
        inherited = dict(os.environ)
        if names_database:
            # The binary resolves exactly what was given: nothing the
            # environment says about a database may fill in or override it.
            # A local database also keeps no collection or registry meant
            # for some other server.
            strip = _CONNECTION if backend == "local" else _DATABASE_VARS
            inherited = {k: v for k, v in inherited.items() if k not in strip}
            env = {**(env or {}), **_database_env(backend, backend_url, backend_key, serve, setup, timeout)}
        try:
            self._process = subprocess.Popen(argv, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                             stderr=None, env={**inherited, **(env or {})})
        except OSError as exc:
            # The documented contract is that failures arrive as RecallError.
            # A missing binary is the most common first run, and it must not
            # escape as a bare OSError with no hint about what to install.
            raise RecallError(f"could not start {argv[0]!r}: {exc}",
                              code="transport_error") from exc
        self._reader = threading.Thread(target=self._read, daemon=True)
        self._reader.start()
        try:
            self._initialize()
            self._send({"jsonrpc": "2.0", "method": "notifications/initialized"})
            self._adopt_surface(self._tool_names(), text, typed)
        except RecallError as exc:
            self.close()
            if typed and exc.code == "transport_error":
                raise RecallError(f"{exc} (typed=True passes -typed to recall mcp, which needs recall 0.18 or later)",
                                  code=exc.code) from exc
            raise
        except BaseException:
            self.close()
            raise

    def _initialize(self) -> None:
        result = self._rpc("initialize", {"protocolVersion": "2025-06-18", "capabilities": {},
                                          "clientInfo": {"name": "polign-recall-python", "version": "0.1.0"}})
        info = result.get("serverInfo") if isinstance(result, dict) else None
        self.server_version = str((info or {}).get("version") or "")

    def _tool_names(self) -> set[str]:
        return {t.get("name") for t in self._rpc("tools/list", {}).get("tools", [])}

    def _adopt_surface(self, names: set[str], text: bool | None, typed: bool) -> None:
        """Follow the tools the server lists: the typed tools always include
        list_predicates, the text tools never do. A server that lists nothing
        (a stub) is taken at the caller's word."""
        if names:
            self._text = "list_predicates" not in names
        if typed and self._text:
            raise ValueError("typed=True, but this server serves the text tools; start it with -typed (recall 0.18 or later)")
        if text and not self._text:
            raise ValueError("text=True, but this server serves the typed tools; start it with -text (recall 0.14 or later)")

    @classmethod
    def from_env(cls, **kwargs: Any) -> Client:
        """A client for wherever the environment says memory is.

        With RECALL_URL set, it connects there with RECALL_API_KEY, or with
        the projected token at RECALL_TOKEN_FILE (default
        /var/run/secrets/recall/token when it exists). Otherwise it starts a
        local `recall` process as `Client(**kwargs)` does. The same code
        therefore runs on a laptop and inside a sandbox.
        """
        url = os.environ.get("RECALL_URL")
        if not url:
            return cls(**kwargs)
        api_key = os.environ.get("RECALL_API_KEY") or None
        token_file = os.environ.get("RECALL_TOKEN_FILE") or None
        if not api_key and not token_file and os.path.exists(_DEFAULT_TOKEN_FILE):
            token_file = _DEFAULT_TOKEN_FILE
        for forbidden in ("command", "local_dir", "backend", "backend_url", "backend_key", "extract_model", "env", "url", "api_key", "token_file"):
            kwargs.pop(forbidden, None)
        return cls(url=url, api_key=api_key, token_file=token_file, **kwargs)

    def _read(self) -> None:
        try:
            assert self._process is not None and self._process.stdout is not None
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
        if self._http is not None:
            self._http.notify(message)
            return
        payload = (json.dumps(message, allow_nan=False) + "\n").encode()
        assert self._process is not None and self._process.stdin is not None
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
            if self._http is not None:
                response = self._http.rpc({"jsonrpc": "2.0", "id": self._id, "method": method, "params": params})
                if not isinstance(response, dict):
                    raise RecallError("malformed answer from the server", code="protocol_error")
                if "error" in response:
                    raise RecallError(response["error"].get("message", "error"), code="protocol_error")
                return response.get("result")
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
                 messages: Sequence[Mapping[str, str]] | None = None,
                 statements: Sequence[Mapping[str, Any]] | None = None,
                 kind: str | None = None, confidence: float | None = None,
                 source: str | None = None,
                 observed_at: str | datetime | None = None,
                 replaces: Sequence[Mapping[str, Any]] | None = None) -> RememberResult | ExtractionResult | TextResult:
        """Record a typed statement, or the statements in a piece of text.

        In text mode, pass `statements` your agent proposes, or leave them out
        and the server's extraction model (`extract_model`) proposes them.

        On a client opened with text=True, pass `text` alone, or `messages`, a
        conversation as {"role": "user" | "assistant", "content": ...} turns,
        of which only what the user said or agreed to is kept; the result is a
        TextResult saying what was filed and what each statement replaced.

        `observed_at` dates a statement made earlier, such as a line of an
        imported conversation; omitted means now. A statement dated before a
        later one for the same subject and predicate is kept as history and
        does not replace it. The server refuses times in the future.

        `replaces` names beliefs of the same subject, under any predicate,
        that a typed statement makes untrue, each as {"predicate": ...,
        "value": ...}: remember("user", "dislikes", "mangoes",
        replaces=[{"predicate": "likes", "value": "mangoes"}]). A typed
        result lists under `related` the beliefs that may need it. In text
        mode each proposed statement may carry its own "replaces".
        """
        dated = {"observed_at": _instant(observed_at, "observed_at")} if observed_at is not None else {}
        if self._text:
            if (text is None) == (messages is None) or statements is not None or value is not _MISSING or any(
                    x is not None for x in (subject, predicate, kind, confidence, source, replaces)):
                raise ValueError("a text client remembers text or messages alone: remember(text=...) or remember(messages=[...])")
            said = {"text": text} if text is not None else {
                "messages": [{"role": m["role"], "content": m["content"]} for m in messages]}
            data = self._tool("remember", {**said, **dated})
            return TextResult(tuple(Remembered(r.get("text", ""), tuple(r.get("replaces") or ()),
                                               bool(r.get("already_known")))
                                    for r in data.get("remembered") or ()),
                              bool(data.get("kept_as_text")))
        if messages is not None:
            raise ValueError("messages need a text client: Client(text=True)")
        if text is not None:
            if any(x is not None for x in (subject, predicate, kind, confidence, source, replaces)) or value is not _MISSING:
                raise ValueError("text mode cannot be combined with typed fields; put replaces on each statement")
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
        if replaces is not None:
            args["replaces"] = [{"predicate": r["predicate"], "value": r["value"]} for r in replaces]
        return RememberResult.decode(self._tool("remember", args))

    def ask(self, question: str | None = None, *, as_of: str | datetime | None = None,
            since: str | datetime | None = None) -> list[Memory]:
        """What is believed about `question`, as sentences (text=True only).
        Each Memory lists under `before` what it replaced. `as_of` asks about
        an earlier time. Without a question, everything believed about the
        user (a profile); with `since`, only what was stated after then,
        newest first."""
        self._need_text("ask")
        args: dict[str, Any] = {}
        if question:
            args["question"] = question
        if as_of is not None:
            args["as_of"] = _instant(as_of, "as_of")
        if since is not None:
            args["since"] = _instant(since, "since")
        data = self._tool("recall", args)
        return [Memory(m.get("text", ""), m.get("since", ""), int(m.get("days_ago", 0)),
                       tuple(Earlier(e.get("text", ""), e.get("since", "")) for e in m.get("before") or ()),
                       tuple(Earlier(e.get("text", ""), e.get("since", "")) for e in m.get("elsewhere") or ()),
                       m.get("said", ""))
                for m in data.get("memories") or ()]

    def forget_text(self, text: str) -> list[str]:
        """Withdraws the facts a description names, such as "my editor"
        (text=True only), and returns them as sentences. The texts they came
        from stay as the record. Needs an extraction model on the server."""
        self._need_text("forget_text")
        return list(self._tool("forget", {"text": text}).get("forgotten") or [])

    def history_text(self, question: str) -> list[Timeline]:
        """How each thing `question` reaches changed, statement by statement,
        oldest first (text=True only). Things since forgotten are included.
        Each step agrees with what ask(as_of=...) answered at that instant."""
        self._need_text("history_text")
        data = self._new_tool("history", {"question": question})
        return [Timeline(t.get("about", ""), tuple(t.get("now") or ()),
                         tuple(Step(**{"when": "", "change": "", **{k: tuple(v) if k in ("ended", "began") else v
                                                                    for k, v in _known(Step, c).items()}})
                               for c in t.get("changes") or ()))
                for t in data.get("history") or ()]

    def diff(self, since: str | datetime, until: str | datetime | None = None, *,
             subject: str | None = None) -> list[Change]:
        """What was believed differently at `until` (default now) than at
        `since`: for each thing a statement touched in between, the sentences
        that ended and began being believed. On the typed tools each Change
        also names its subject and predicate with the beliefs at each end, and
        `subject` limits the diff to one subject."""
        args = {"from": _instant(since, "since")}
        if until is not None:
            args["to"] = _instant(until, "until")
        if subject is not None:
            if self._text:
                raise ValueError("diff(subject=...) uses the typed tools; the text tools diff every subject")
            args["subject"] = subject
        data = self._new_tool("diff", args)
        if self._text:
            return [Change(tuple(c.get("ended") or ()), tuple(c.get("began") or ())) for c in data.get("changes") or ()]
        return [Change(tuple(c.get("removed") or ()), tuple(c.get("added") or ()), c.get("subject", ""),
                       c.get("predicate", ""), tuple(c.get("before") or ()), tuple(c.get("after") or ()))
                for c in data.get("changes") or ()]

    def timeline(self, subject: str, predicate: str) -> Timeline:
        """How one subject and predicate changed, statement by statement,
        oldest first (typed tools only; history_text is the words version).
        Each step agrees with recall(as_of=...) at that instant."""
        self._need_typed("timeline")
        t = self._new_tool("timeline", {"subject": subject, "predicate": predicate})
        name = t.get("predicate", predicate)
        steps = []
        for st in t.get("steps") or ():
            # Name what caused a step that changed nothing, withdrew, or came
            # from another predicate, as the text tools do.
            caused = st.get("change") in ("no change", "withdrawn") or st.get("predicate", name) != name
            steps.append(Step(st.get("at", ""), st.get("change", ""), tuple(st.get("removed") or ()),
                              tuple(st.get("added") or ()), st.get("evidence", ""),
                              st.get("statement", "") if caused else ""))
        return Timeline(f"{t.get('subject', subject)} {name.replace('_', ' ')}", tuple(t.get("now") or ()),
                        tuple(steps), t.get("subject", subject), name)

    def _new_tool(self, name: str, arguments: dict[str, Any]) -> Any:
        """Calls a tool newer servers serve, saying so when this one does not."""
        try:
            return self._tool(name, arguments)
        except RecallError as exc:
            if "unknown" in str(exc) and f'tool "{name}"' in str(exc):
                raise RecallError(f"this server has no {name} tool; it needs a recall newer than 0.21",
                                  code="unsupported") from exc
            raise

    def _need_text(self, method: str) -> None:
        if not self._text:
            raise ValueError(f"{method} uses the text tools; this client serves the typed tools "
                             "(open it without typed=True, or use recall, remember(subject, predicate, value) and forget)")

    def _need_typed(self, method: str) -> None:
        if self._text:
            raise ValueError(f"{method} uses the typed tools; this client serves the text tools "
                             "(open it with typed=True, or use ask, remember(text=...) and forget_text)")

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
            if self._process is None:
                # An HTTP session holds nothing open. Agents resumed through
                # it stay held until released or their lease expires.
                return
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
        # Acquiring settled what it could; the context it read before did not
        # see that, nor what the previous holder wrote after it.
        self.refresh()
        return True

    def refresh(self) -> ResumeContext:
        """Rebuild `context` from the records as they stand now, for example
        after settling unfinished actions, and return it."""
        self.context = ResumeContext.decode(self._call("agent_context"))
        return self.context

    def begin_action(self, tool: str, class_: str, args: Any = None) -> Action:
        """Record a tool call before it runs; run the tool only if this
        returns. `args` may be a string or anything JSON can encode. Pass the
        returned `key` to an idempotent tool, so that running it again after
        a crash does nothing more."""
        if class_ not in ACTION_CLASSES:
            raise ValueError(f"class_ must be one of {', '.join(ACTION_CLASSES)}, got {class_!r}")
        if args is not None and not isinstance(args, str):
            args = json.dumps(args, sort_keys=True, default=str)
        return Action.decode(self._call("begin_action", tool=tool, **{"class": class_}, args=args))

    def end_action(self, seq: int, status: str, result: Any = None, error: str | None = None,
                   resolution: str | None = None) -> Action:
        """Close an open action: "done" or "failed" after it ran, or, settling
        one a crash left open, "abandoned" or "unknown"."""
        if result is not None and not isinstance(result, str):
            result = json.dumps(result, sort_keys=True, default=str)
        return Action.decode(self._call("end_action", seq=seq, status=status, result=result,
                                        error=error, resolution=resolution))

    def pending_actions(self) -> list[Action]:
        """Actions begun and never ended, or ended unknown, oldest first."""
        return [Action.decode(a) for a in self._call("pending_actions") or []]

    def reconcile(self, resolver: Callable[[Action], ActionEnd | None] | None = None) -> ReconcileReport:
        """Settle the actions a crash left open, then rebuild `context`. Each
        action settled done or failed is also recorded as a tool turn, so the
        model sees the outcome in its history.

        The resume already closed interrupted pure actions and marked
        external ones "unknown". `resolver` is called for each action still
        open: for an idempotent one it should run the tool again with
        `action.key` and return ActionEnd("done", result); for an external
        one it should check the outside system and return "done" or
        "failed" if it can tell, or None (or "unknown") if it cannot. Without
        a resolver, nothing changes and every open action stays in the
        briefing. Recall never runs a tool itself.
        """
        closed: list[Action] = []
        still: list[Action] = []
        for action in self.pending_actions():
            if action.class_ == "pure":
                end: ActionEnd | None = ActionEnd("abandoned", resolution="interrupted; pure, not run again")
            elif resolver is None:
                still.append(action)
                continue
            else:
                end = resolver(action)
            if end is None or end.status == "unknown":
                if action.status != "unknown":
                    action = self.end_action(action.seq, "unknown",
                                             resolution=(end.resolution if end else "") or "could not tell whether it happened")
                still.append(action)
                continue
            ended = self.end_action(action.seq, end.status, result=end.result or None,
                                    error=end.error or None, resolution=end.resolution or None)
            if ended.status in ("done", "failed"):
                # Put the outcome in the history the model reads; a closed
                # action with no visible result invites the same call again.
                text = ended.result if ended.status == "done" else f"failed: {ended.error}".strip()
                text = text or ended.status
                brief = f"(settled after a crash: {ended.status}; {ended.resolution or '-'}) {text[:300]}"
                self.record_turn("tool", text, ended.tool, brief=brief[:4000])
            closed.append(ended)
        if closed:
            self.refresh()
        return ReconcileReport(closed=tuple(closed), open=tuple(still))

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
