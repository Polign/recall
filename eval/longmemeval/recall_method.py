"""The Recall methods for run.py. Each question gets a fresh local store: the
haystack is written into it in date order, one round (a user turn and the
reply after it) at a time, and the question is answered from what recall()
returns.

  recall-notes  every round is kept as a note (text with no statements), so
                this measures Recall as a plain note store with its built-in
                lexical search. No model is needed to ingest.
  recall-typed  an extractor model turns each session into typed statements
                over registry.json (see extract.py); the reader sees beliefs,
                with the values a single-valued belief replaced.

Two stopgaps until remember accepts an observation time:
  - observed_at is the wall clock at ingest, so recall() is called without
    as_of (the question dates are in 2023 and would hide every event).
  - The reader is shown each belief's session date from the ingest log, which
    is the date an observed_at override would have stored.
"""
from __future__ import annotations

import json
import os
import re
import shutil
import tempfile
import time
from pathlib import Path
from typing import Any

import lme

# LME_POLIGN_BIN runs a locally built polign CLI instead of the one the
# polign_db wheel installed, to measure an engine change before it ships.
if os.environ.get("LME_POLIGN_BIN"):
    from polign_recall import client as _client
    _client.polign_bin = lambda: os.environ["LME_POLIGN_BIN"]

MAX_TEXT_BYTES = 32768  # remember's text limit, in UTF-8 bytes


def render(turns: list[dict[str, Any]]) -> str:
    text = "\n\n".join(f"{t['role']}: {t['content'].strip()}" for t in turns)
    return text.encode()[:MAX_TEXT_BYTES].decode(errors="ignore")


def ingest_notes(client: Any, entry: dict[str, Any], rounds) -> list[dict[str, Any]]:
    """Writes every round as a note. Returns one ingest row per stored event,
    which maps it back to its session and date."""
    log = []
    for sid, date, turns in lme.sessions(entry):
        for r in rounds(turns):
            text = render(r)
            if not text.strip():
                continue
            result = client.remember(text=text, statements=[])
            for rr in result.results:
                log.append({"event_id": rr.stored.event_id, "session_id": sid, "date": date,
                            "predicate": rr.stored.predicate, "probe": str(rr.stored.value)})
    return log


def chunks_of(turns: list[dict[str, Any]], rounds) -> list[str]:
    """A session as texts under remember's byte limit, split between rounds."""
    out, cur = [], ""
    for r in rounds(turns):
        piece = render(r)
        if cur and len((cur + "\n\n" + piece).encode()) > MAX_TEXT_BYTES:
            out.append(cur)
            cur = piece
        else:
            cur = f"{cur}\n\n{piece}" if cur else piece
    if cur.strip():
        out.append(cur)
    return out


def exact_evidence(evidence: str, text: str) -> str | None:
    """The span of text an extractor's quote refers to. Models add a closing
    period or turn a newline into a space; remember needs the exact excerpt,
    so the quote is matched with trailing punctuation dropped and any run of
    whitespace allowed between words."""
    if evidence in text:
        return evidence
    trimmed = evidence.strip().rstrip(".!?,;: ")
    if trimmed and trimmed in text:
        return trimmed
    words = trimmed.split()
    if not words:
        return None
    m = re.search(r"\s+".join(re.escape(w) for w in words), text)
    return m.group(0) if m else None


def ingest_typed(client: Any, entry: dict[str, Any], rounds, extractor: str, stats: dict) -> list[dict[str, Any]]:
    """Extracts typed statements from each session in date order and writes
    them with remember(text, statements). Statements whose evidence is not an
    exact excerpt are dropped here rather than failing the whole batch."""
    import extract

    reg = extract.registry()
    cache = extract.Cache(extractor, entry["question_id"])
    subjects: list[str] = []
    log = []
    for sid, date, turns in lme.sessions(entry):
        for i, text in enumerate(chunks_of(turns, rounds)):
            key = f"{sid}#{i}"
            statements = cache.get(key)
            if statements is None:
                statements = extract.extract(extractor, date, text, subjects, reg)
                cache.put(key, statements)
            valid = []
            for st in statements:
                span = exact_evidence(st["evidence"], text)
                if span is not None:
                    valid.append({**st, "evidence": span})
            stats["proposed"] += len(statements)
            stats["bad_evidence"] += len(statements) - len(valid)
            stats["unregistered"] += sum(st["predicate"] not in reg for st in valid)
            for st in valid:
                if st["subject"] not in subjects:
                    subjects.append(st["subject"])
            for j in range(0, len(valid), 32):  # remember takes 32 statements per call
                result = client.remember(text=text, statements=valid[j:j + 32])
                for rr in result.results:
                    log.append({"event_id": rr.stored.event_id, "session_id": sid, "date": date,
                                "predicate": rr.stored.predicate,
                                "probe": f"{rr.stored.subject} {rr.stored.predicate} {rr.stored.value}"})
    stats["subjects"] = len(subjects)
    return log


COLLECTION = "recall_lexical_v1"  # the MCP memory server's default


def wait_for_text_index(store: Path, event_id: str, probe: str, timeout: float = 180) -> float:
    """Waits until the text index holds the last event written, so every
    session is searchable lexically when the question is asked, as it would
    be days later in real use. Returns the seconds waited, or -1 on timeout
    (the question is then answered with part of the log unindexed)."""
    import urllib.error
    import urllib.request

    url = json.loads((store / "runtime.json").read_text())["url"]
    key = (store / "local-key").read_text().strip()
    words = " ".join(re.findall(r"[A-Za-z0-9]+", probe)[:24])
    body = json.dumps({"text": words, "k": 10}).encode()
    t0 = time.monotonic()
    while time.monotonic() - t0 < timeout:
        req = urllib.request.Request(f"{url}/v1/collections/{COLLECTION}/query", data=body, method="POST",
                                     headers={"Authorization": f"Bearer {key}", "Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(req, timeout=10) as r:
                if any(h["id"] == event_id for h in json.loads(r.read()).get("hits") or []):
                    return round(time.monotonic() - t0, 1)
        except urllib.error.HTTPError:
            pass  # 404 until the first segment is published
        time.sleep(1)
    return -1.0


def describe(b: Any, where: dict[str, dict], client: Any, reg: dict[str, Any]) -> str:
    """One belief as the reader sees it. A single-valued belief also lists the
    values it replaced, which is what "what was it before" questions need."""
    if b.predicate == "note":
        return str(b.value)
    line = f"{b.subject} | {b.predicate}: {b.value}"
    if reg.get(b.predicate, {}).get("cardinality") == "single":
        earlier = [e for e in client.history(b.subject, b.predicate)
                   if not e.retraction and e.id != b.event_id and e.value is not None]
        if earlier:
            line += " (earlier: " + "; ".join(
                f"{e.value} as of {where.get(e.id, {}).get('date', '?')}" for e in earlier[-3:]) + ")"
    return line


def context(entry: dict[str, Any], method: str, k: int, rounds, cache_dir: Path,
            extractor: str = "") -> tuple[list, list[str], dict]:
    from polign_recall.client import Client

    stats: dict[str, Any] = {"proposed": 0, "bad_evidence": 0, "unregistered": 0}
    env = {}
    reg: dict[str, Any] = {}
    if method == "recall-typed":
        import extract
        env["POLIGN_PREDICATES"] = str(extract.REGISTRY_PATH)
        reg = extract.registry()
    store = Path(tempfile.mkdtemp(prefix=f"lme-{entry['question_id']}-"))
    try:
        with Client(local_dir=store, write=True, timeout=600, env=env) as client:
            t0 = time.monotonic()
            if method == "recall-notes":
                log = ingest_notes(client, entry, rounds)
            elif method == "recall-typed":
                log = ingest_typed(client, entry, rounds, extractor, stats)
            else:
                raise SystemExit(f"unknown recall method {method!r}")
            ingest_s = time.monotonic() - t0
            if log:
                stats["index_wait_s"] = wait_for_text_index(store, log[-1]["event_id"], log[-1]["probe"])

            t1 = time.monotonic()
            beliefs = client.recall(query=entry["question"], limit=k)
            recall_ms = (time.monotonic() - t1) * 1000

            where = {r["event_id"]: r for r in log}
            chunks, order = [], []
            for b in beliefs:
                src = where.get(b.event_id, {})
                chunks.append((src.get("date", ""), [{"role": "memory", "content": describe(b, where, client, reg)}]))
                sid = src.get("session_id")
                if sid and sid not in order:
                    order.append(sid)
    finally:
        shutil.rmtree(store, ignore_errors=True)

    (cache_dir / f"{entry['question_id']}.ingest.jsonl").write_text("".join(json.dumps(r) + "\n" for r in log))
    stats.update({"events": len(log), "notes": sum(r["predicate"] == "note" for r in log),
                  "ingest_s": round(ingest_s, 1), "recall_ms": round(recall_ms, 1), "beliefs": len(beliefs)})
    return chunks, order, stats
