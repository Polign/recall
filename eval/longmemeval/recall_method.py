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
  recall-linked recall-typed plus every round kept whole as a note, with each
                statement written from the round it quotes; the reader sees
                each fact with its source round, and matching rounds directly.

Every write carries its session date as observed_at, and recall() is asked
as_of the question date, so supersession and "when" come from Recall itself.
The reader sees each belief's observed_at in the dataset's date format.
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

# LME_RECALL_BIN runs a locally built `recall` binary instead of the one pip
# installed, to measure an engine change before it ships. LME_POLIGN_BIN does
# the same with a polign CLI from polign_db 0.13 or earlier, which hosted the
# server before it moved into `recall`.
if os.environ.get("LME_RECALL_BIN") or os.environ.get("LME_POLIGN_BIN"):
    from polign_recall import client as _client
    _client.recall_bin = lambda: os.environ.get("LME_RECALL_BIN")
    if os.environ.get("LME_POLIGN_BIN"):
        _client.polign_bin = lambda: os.environ["LME_POLIGN_BIN"]

MAX_TEXT_BYTES = 32768
MAX_EVIDENCE_BYTES = 2048  # recall.MaxEvidenceBytes  # remember's text limit, in UTF-8 bytes


def instant(date: str) -> str:
    """A LongMemEval date, "2023/05/20 (Sat) 02:21", as RFC3339 UTC."""
    day, _, clock = date.partition(" (")
    clock = clock.split(") ")[-1] if ") " in clock else "00:00"
    return f"{day.replace('/', '-')}T{clock}:00Z"


def dataset_date(observed_at: str) -> str:
    """An RFC3339 instant back in the dataset's date format."""
    from datetime import datetime
    t = datetime.fromisoformat(observed_at.replace("Z", "+00:00"))
    return t.strftime("%Y/%m/%d (%a) %H:%M")


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
            result = client.remember(text=text, statements=[], observed_at=instant(date))
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


def write(client: Any, text: str, statements: list, sid: str, date: str, log: list) -> None:
    """remember(text, statements), 32 statements per call, logging every event
    written: the episode note and each statement."""
    for j in range(0, max(len(statements), 1), 32):
        result = client.remember(text=text, statements=statements[j:j + 32], observed_at=instant(date))
        written = list(result.results)
        if result.episode is not None and all(r.stored.event_id != result.episode.stored.event_id for r in written):
            written.insert(0, result.episode)
        for rr in written:
            log.append({"event_id": rr.stored.event_id, "session_id": sid, "date": date,
                        "predicate": rr.stored.predicate,
                        "probe": f"{rr.stored.subject} {rr.stored.predicate} {rr.stored.value}"})


def ingest_typed(client: Any, entry: dict[str, Any], rounds, extractor: str, stats: dict,
                 link: bool = False) -> list[dict[str, Any]]:
    """Extracts typed statements from each session in date order and writes
    them with remember(text, statements). Statements whose evidence is not an
    exact excerpt are dropped here rather than failing the whole batch.

    With link, every round goes through remember(text=round, statements=...)
    on its own, so Recall keeps the round whole as the episode note and links
    each statement to it (evidence, evidence_id). This is the product path; a
    round with no statements is still kept as its episode."""
    import extract

    reg = extract.registry()
    cache = extract.Cache(extractor, entry["question_id"])
    subjects: list[str] = []
    log = []
    for sid, date, turns in lme.sessions(entry):
        round_texts = [t for t in (render(r) for r in rounds(turns)) if t.strip()]
        by_round: dict[str, list] = {t: [] for t in round_texts}
        for i, text in enumerate(chunks_of(turns, rounds)):
            key = f"{sid}#{i}"
            statements = cache.get(key)
            if statements is None:
                statements = extract.extract(extractor, date, text, subjects, reg)
                cache.put(key, statements)
            valid = []
            for st in statements:
                span = exact_evidence(st["evidence"], text)
                if span is None:
                    continue
                if len(span.strip().encode()) > MAX_EVIDENCE_BYTES:  # Recall refuses a longer excerpt
                    stats["long_evidence"] += 1
                    continue
                valid.append({**st, "evidence": span})
            stats["proposed"] += len(statements)
            stats["bad_evidence"] += len(statements) - len(valid)
            stats["unregistered"] += sum(st["predicate"] not in reg for st in valid)
            for st in valid:
                if st["subject"] not in subjects:
                    subjects.append(st["subject"])
            if link:
                for st in valid:
                    home = next((t for t in round_texts if st["evidence"] in t), None)
                    if home is None:  # the quote spans two rounds
                        stats["cross_round"] += 1
                        continue
                    by_round[home].append(st)
                continue
            write(client, text, valid, sid, date, log)
        if link:
            for body in round_texts:
                write(client, body, by_round[body], sid, date, log)
    stats["subjects"] = len(subjects)
    return log


# LME_EMBED_URL gives the memory server a model embedder (see embed_proxy.py),
# which needs its own collection; without it Recall uses lexical hashing.
EMBED_URL = os.environ.get("LME_EMBED_URL", "")
COLLECTION = "recall_embed_v1" if EMBED_URL else "recall_lexical_v1"  # the MCP memory server's default


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


def describe(b: Any, client: Any, reg: dict[str, Any]) -> str:
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
                f"{e.value} as of {dataset_date(e.observed_at)}" for e in earlier[-3:]) + ")"
    return line


def stop_local_server(store: Path) -> None:
    """Stops the polign-server the client started for this store. The client
    leaves it running on purpose, so other processes can share the directory;
    one question's store is never shared, and a server left behind per
    question exhausts the machine within a few hundred questions."""
    import subprocess
    subprocess.run(["pkill", "-f", f"fs:{store}/data"], check=False)


def context(entry: dict[str, Any], method: str, k: int, rounds, cache_dir: Path,
            extractor: str = "", as_of: str = "question", profile: int = 0) -> tuple[list, list[str], dict]:
    from polign_recall.client import Client

    stats: dict[str, Any] = {"proposed": 0, "bad_evidence": 0, "unregistered": 0, "cross_round": 0, "long_evidence": 0}
    env = {}
    if EMBED_URL:
        env["POLIGN_EMBED_URL"] = EMBED_URL
        env["POLIGN_COLLECTION"] = COLLECTION
    reg: dict[str, Any] = {}
    if method in ("recall-typed", "recall-linked"):
        import extract
        env["POLIGN_PREDICATES"] = str(extract.REGISTRY_PATH)
        reg = extract.registry()
    # LME_STORES keeps each question's ingested store, so a run that only
    # changes the read side (engine search, reader prompt) reuses it instead
    # of writing the whole haystack again.
    stores = os.environ.get("LME_STORES")
    if stores:
        store = Path(stores) / f"{method}-{extractor}{'-events' if os.environ.get('LME_EXTRACT_EVENTS') else ''}{'-embed' if EMBED_URL else ''}" / entry["question_id"]
        if store.exists() and not (store / "ingest.jsonl").exists():
            shutil.rmtree(store, ignore_errors=True)  # left by an ingest that failed
        store.mkdir(parents=True, exist_ok=True)
    else:
        store = Path(tempfile.mkdtemp(prefix=f"lme-{entry['question_id']}-"))
    done = store / "ingest.jsonl"
    try:
        with Client(local_dir=store, write=True, timeout=600, env=env) as client:
            t0 = time.monotonic()
            if done.exists():
                log = lme.read_jsonl(done)
                stats["reused_store"] = True
            elif method == "recall-notes":
                log = ingest_notes(client, entry, rounds)
            elif method in ("recall-typed", "recall-linked"):
                log = ingest_typed(client, entry, rounds, extractor, stats, link=method == "recall-linked")
            else:
                raise SystemExit(f"unknown recall method {method!r}")
            ingest_s = time.monotonic() - t0
            if log and not done.exists():
                stats["index_wait_s"] = wait_for_text_index(store, log[-1]["event_id"], log[-1]["probe"])
                if stores:
                    done.write_text("".join(json.dumps(r) + "\n" for r in log))

            t1 = time.monotonic()
            # as_of="question" asks what was known on the question date, so a
            # session dated after it is not seen; "none" sees all history, as
            # the upstream baselines do (44 questions have an answer session
            # dated after the question).
            when = instant(entry["question_date"]) if as_of == "question" else None
            beliefs = client.recall(query=entry["question"], limit=k, as_of=when,
                                    with_sources=method == "recall-linked")
            if profile and reg.get("preference"):
                # The user's stated preferences about what the question is
                # about, whatever subject they were filed under.
                have = {b.event_id for b in beliefs}
                prefs = client.recall(predicate="preference", query=entry["question"], limit=profile,
                                      as_of=when, with_sources=method == "recall-linked")
                beliefs += [b for b in prefs if b.event_id not in have]
            recall_ms = (time.monotonic() - t1) * 1000

            where = {r["event_id"]: r for r in log}
            chunks, order = [], []
            shown: set[str] = set()  # source rounds already in the context
            for b in beliefs:
                src = where.get(b.event_id, {})
                content = describe(b, client, reg)
                if method == "recall-linked":
                    source = b.source_text
                    if b.predicate == "note":
                        if content in shown:
                            continue
                        shown.add(content)
                    elif source and source not in shown:
                        shown.add(source)
                        content = f"{content}\nsource conversation:\n{source}"
                chunks.append((dataset_date(b.observed_at), [{"role": "memory", "content": content}]))
                sid = src.get("session_id")
                if sid and sid not in order:
                    order.append(sid)
    finally:
        stop_local_server(store)
        # A cached store is kept only once its ingest finished; a failed one
        # starts over on the retry rather than build on a partial log.
        if not stores or not done.exists():
            shutil.rmtree(store, ignore_errors=True)

    (cache_dir / f"{entry['question_id']}.ingest.jsonl").write_text("".join(json.dumps(r) + "\n" for r in log))
    stats.update({"events": len(log), "notes": sum(r["predicate"] == "note" for r in log),
                  "ingest_s": round(ingest_s, 1), "recall_ms": round(recall_ms, 1), "beliefs": len(beliefs)})
    return chunks, order, stats
