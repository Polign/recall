"""Generate hypotheses for LongMemEval with one of the baseline methods.

    python run.py --method full --reader gpt-4o-2024-08-06 --run full-4o
    python run.py --method bm25-session --k 10 --limit 50 --run bm25s-k10-50

Writes out/<run>/hyp.jsonl (one row per question: question_id, hypothesis,
retrieved session ids) and out/<run>/manifest.json. A rerun with the same
--run skips questions that already have a hypothesis.

Methods:
  full          every haystack session (upstream's full-context setting)
  oracle        only the answer sessions: the ceiling for any retriever
  bm25-session  top-k sessions by BM25 over session text
  bm25-turn     top-k rounds (a user turn and the reply after it) by BM25
  recall-notes  Recall as a note store; see recall_method.py
  recall-typed  Recall over typed statements from an extractor model
  recall-linked recall-typed with each fact linked to its source round
"""
from __future__ import annotations

import argparse
import json
import os
import re
import sys
import threading
from concurrent.futures import ThreadPoolExecutor, as_completed
from typing import Any

import lme
from bm25 import BM25

METHODS = ("full", "oracle", "bm25-session", "bm25-turn", "recall-notes", "recall-typed", "recall-linked")


def rounds(turns: list[dict[str, Any]]) -> list[list[dict[str, Any]]]:
    out: list[list[dict[str, Any]]] = []
    for t in turns:
        if t["role"] == "user" or not out:
            out.append([t])
        else:
            out[-1].append(t)
    return out


# Questions whose answer spans several memories: an order, a total, "all",
# "each" or "both", or a count of days or times between events. They need
# every match, not the top k.
LIST_QUESTION = re.compile(r"\b(order|in total|total|list|all (the|of|my)|each|every|both|which (two|three|four)|"
                           r"how many (days|weeks|months|times|different))\b", re.I)


def k_for(entry: dict[str, Any], k: int, list_k: int) -> int:
    return list_k if list_k and LIST_QUESTION.search(entry["question"]) else k


def context(entry: dict[str, Any], method: str, k: int) -> tuple[list[tuple[str, list]], list[str]]:
    """The chunks the reader sees, as (date, turns), and the session ids they
    came from in retrieval rank order (deduplicated)."""
    sess = lme.sessions(entry)
    if method == "full":
        return [(d, t) for _, d, t in sess], [s for s, _, _ in sess]
    if method == "oracle":
        want = set(entry["answer_session_ids"])
        picked = [(s, d, t) for s, d, t in sess if s in want]
        return [(d, t) for _, d, t in picked], [s for s, _, _ in picked]

    if method == "bm25-session":
        units = [(s, d, t) for s, d, t in sess]
    elif method == "bm25-turn":
        units = [(s, d, r) for s, d, t in sess for r in rounds(t)]
    else:
        raise SystemExit(f"unknown method {method!r}")
    index = BM25(["\n".join(x["content"] for x in turns) for _, _, turns in units])
    ranked = [units[i] for i in index.rank(entry["question"])]
    top = ranked[:k]
    order: list[str] = []
    for s, _, _ in ranked:
        if s not in order:
            order.append(s)
    return [(d, t) for _, d, t in top], order


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--run", required=True, help="output folder name under out/")
    ap.add_argument("--method", choices=METHODS, required=True)
    ap.add_argument("--reader", default="gpt-4o-2024-08-06", help="provider:model; bare name = OpenAI")
    ap.add_argument("--k", type=int, default=10, help="chunks or beliefs passed to the reader")
    ap.add_argument("--as-of", choices=("question", "none"), default="question",
                    help="recall methods: answer as of the question date, or over all history as the baselines do")
    ap.add_argument("--extractor", default="gpt-4o-mini-2024-07-18", help="recall-typed extraction model")
    ap.add_argument("--cot", action="store_true", help="upstream's step-by-step reader prompt")
    ap.add_argument("--ages", action="store_true", help="say how many days before the question each session date is")
    ap.add_argument("--list-k", type=int, default=0,
                    help="use this k instead for questions that ask for a set, an order or a count")
    ap.add_argument("--profile", type=int, default=0,
                    help="recall-*: also show up to this many preference beliefs matching the question")
    ap.add_argument("--max-tokens", type=int, help="reader output budget (upstream: 500, or 800 with --cot)")
    ap.add_argument("--context", type=int, default=128000,
                    help="reader context window; history is cut to context - max-tokens - 1000, as upstream")
    ap.add_argument("--limit", type=int, help="stratified subset of this many questions")
    ap.add_argument("--subset", choices=tuple(lme.SUBSETS), default="all",
                    help="valid: drop questions answered by a session dated after them; "
                         "user-facts: valid questions about the user (no assistant-recall or multi-session)")
    ap.add_argument("--only", help="file of question ids, one per line: run just those (after --subset)")
    ap.add_argument("--data", default="longmemeval_s_cleaned.json")
    ap.add_argument("--workers", type=int, default=8)
    ap.add_argument("--dry-run", action="store_true", help="build prompts and retrieval, skip the reader")
    args = ap.parse_args()
    if args.max_tokens is None:
        args.max_tokens = 800 if args.cot else 500
    history_budget = args.context - args.max_tokens - 1000

    entries = lme.sample(lme.subset(lme.load(args.data), args.subset), args.limit)
    if args.only:
        wanted = set(open(args.only).read().split())
        entries = [e for e in entries if e["question_id"] in wanted]
    out = lme.run_dir(args.run)
    manifest = {**vars(args), "data_sha256": lme.file_sha256(lme.DATA / args.data),
                "questions": len(entries), "argv": sys.argv,
                "recall_bin": os.environ.get("LME_RECALL_BIN") or os.environ.get("LME_POLIGN_BIN", "wheel")}
    (out / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")

    hyp_path = out / "hyp.jsonl"
    done = {r["question_id"] for r in lme.read_jsonl(hyp_path)}
    todo = [e for e in entries if e["question_id"] not in done]
    print(f"{args.run}: {len(entries)} questions, {len(done)} done, {len(todo)} to go", flush=True)

    lock = threading.Lock()

    def one(entry: dict[str, Any]) -> dict[str, Any]:
        stats: dict[str, Any] = {}
        if args.method.startswith("recall-"):
            import recall_method
            chunks, retrieved, stats = recall_method.context(entry, args.method, k_for(entry, args.k, args.list_k), rounds, out, args.extractor,
                                                              args.as_of, profile=args.profile)
        else:
            chunks, retrieved = context(entry, args.method, k_for(entry, args.k, args.list_k))
        history, history_tokens = lme.truncate_history(
            lme.format_history(chunks, entry["question_date"] if args.ages else None), history_budget)
        prompt = lme.reader_prompt(history, entry, args.cot, facts=args.method == "recall-typed",
                                   merge=args.method == "recall-linked")
        hyp = "" if args.dry_run else lme.complete(args.reader, prompt, args.max_tokens)
        return {"question_id": entry["question_id"], "hypothesis": hyp,
                "retrieved_session_ids": retrieved, "history_tokens": history_tokens,
                "truncated": history_tokens > history_budget, **stats}

    with ThreadPoolExecutor(args.workers) as pool:
        futures = [pool.submit(one, e) for e in todo]
        failed = 0
        for n, f in enumerate(as_completed(futures), 1):
            try:
                row = f.result()
            except Exception as exc:  # one bad question must not lose the rest; rerun resumes it
                failed += 1
                print(f"  question failed: {type(exc).__name__}: {str(exc)[:200]}", flush=True)
                continue
            with lock:
                lme.append_jsonl(hyp_path, row)
            if n % 10 == 0 or n == len(todo):
                print(f"  {n}/{len(todo)}", flush=True)
    if failed:
        raise SystemExit(f"{args.run}: {failed} questions failed; rerun the same command to retry them")


if __name__ == "__main__":
    main()
