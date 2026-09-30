"""The Recall methods for run.py. Each question gets a fresh local store: the
haystack is written into it in date order, one round (a user turn and the
reply after it) at a time, and the question is answered from what recall()
returns.

  recall-notes  every round is kept as a note (text with no statements), so
                this measures Recall as a plain note store with its built-in
                lexical search. No model is needed to ingest.

Two stopgaps until remember accepts an observation time:
  - observed_at is the wall clock at ingest, so recall() is called without
    as_of (the question dates are in 2023 and would hide every event).
  - The reader is shown each belief's session date from the ingest log, which
    is the date an observed_at override would have stored.
"""
from __future__ import annotations

import json
import os
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
                            "predicate": rr.stored.predicate})
    return log


def context(entry: dict[str, Any], method: str, k: int, rounds, cache_dir: Path) -> tuple[list, list[str], dict]:
    from polign_recall.client import Client

    store = Path(tempfile.mkdtemp(prefix=f"lme-{entry['question_id']}-"))
    try:
        with Client(local_dir=store, write=True, timeout=600) as client:
            t0 = time.monotonic()
            if method == "recall-notes":
                log = ingest_notes(client, entry, rounds)
            else:
                raise SystemExit(f"unknown recall method {method!r}")
            ingest_s = time.monotonic() - t0

            t1 = time.monotonic()
            beliefs = client.recall(query=entry["question"], limit=k)
            recall_ms = (time.monotonic() - t1) * 1000
    finally:
        shutil.rmtree(store, ignore_errors=True)

    (cache_dir / f"{entry['question_id']}.ingest.jsonl").write_text("".join(json.dumps(r) + "\n" for r in log))
    where = {r["event_id"]: r for r in log}
    chunks, order = [], []
    for b in beliefs:
        src = where.get(b.event_id, {})
        text = b.value if b.predicate == "note" else f"{b.subject} {b.predicate}: {b.value}"
        chunks.append((src.get("date", ""), [{"role": "memory", "content": str(text)}]))
        sid = src.get("session_id")
        if sid and sid not in order:
            order.append(sid)
    stats = {"events": len(log), "notes": sum(r["predicate"] == "note" for r in log),
             "ingest_s": round(ingest_s, 1), "recall_ms": round(recall_ms, 1), "beliefs": len(beliefs)}
    return chunks, order, stats
